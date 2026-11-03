#!/usr/bin/env python3
"""Aperture server renderer: PhotoDoc v1 + base image -> pixels (MM96).

Ground truth for every preview (design docs/design/PHOTO_EDITOR.md section 3.3, ADR-0017).
Every function cites the design section it implements; the rung-1 per-pixel math is a
verbatim port of media-manipulator-ui/lib/photo/colorMath.ts (the golden source) and of
Darkroom's lib/studio/colorMath.ts (eqAdjust, colorBalanceMidtones, vibranceAdjust).

Usage
  photo_render.py --input base.png --doc doc.json --masks <dir> --out rendered.png
                  [--max-megapixels N] [--progress]
  photo_render.py --selftest <goldensDir>      (docs/, golden-docs/, images/ are siblings)

Stage order (design 6.3): geometry (flip/rotate -> perspective -> crop, fill) -> layers in
order (each lerp(in, adjust(in), mask*opacity), blend normal|luminosity|color) -> downscale
to --max-megapixels -> 16-bit PNG out (alpha kept). Float64 for images up to 4 MP (exact
parity with the JS reference), float32 above (memory).
"""
import argparse
import base64
import json
import math
import os
import sys

import cv2
import numpy as np

LUMA = (0.2126, 0.7152, 0.0722)  # BT.709, section 5 preamble
VIB_LUMA = (0.072186, 0.715158, 0.212656)  # vf_vibrance (r/b swapped, as Darkroom)
HSL_NEUTRAL_SAT = 0.1
COLOR_BALANCE_GAIN = 0.3
SPLIT_TONE_GAIN = 0.25
TINT_GAIN = 0.25
LUT_SIZE = 256
MIN_LUMA = 1e-4
F64_MAX_PIXELS = 4_000_000

_progress_on = False


def progress(frac):
    if _progress_on:
        print("progress %.3f" % max(0.0, min(1.0, frac)), flush=True)


# --------------------------------------------------------------------------------------
# Scalars and colour spaces (colorMath.ts)
# --------------------------------------------------------------------------------------

def clamp01(v):
    return np.clip(v, 0.0, 1.0)


def luma(c):
    return c[..., 0] * LUMA[0] + c[..., 1] * LUMA[1] + c[..., 2] * LUMA[2]


def smoothstep(e0, e1, x):
    if e1 == e0:
        return np.where(x < e0, 0.0, 1.0)
    t = clamp01((x - e0) / (e1 - e0))
    return t * t * (3 - 2 * t)


def srgb_to_linear(v):
    return np.where(v <= 0.04045, v / 12.92, np.power((np.maximum(v, 0) + 0.055) / 1.055, 2.4))


def linear_to_srgb(v):
    return np.where(v <= 0.0031308, v * 12.92, 1.055 * np.power(np.maximum(v, 0), 1 / 2.4) - 0.055)


def rgb_to_hsl(c):
    r, g, b = c[..., 0], c[..., 1], c[..., 2]
    mx = np.maximum(np.maximum(r, g), b)
    mn = np.minimum(np.minimum(r, g), b)
    l = (mx + mn) / 2
    d = mx - mn
    dz = d == 0
    dd = np.where(dz, 1.0, d)
    den = 1 - np.abs(2 * l - 1)
    s = np.where(dz | (den <= 0), 0.0, d / np.where(den <= 0, 1.0, den))
    h = np.where(mx == r, np.fmod((g - b) / dd, 6.0), np.where(mx == g, (b - r) / dd + 2, (r - g) / dd + 4))
    h = h * 60
    h = np.where(h < 0, h + 360, h)
    h = np.where(dz, 0.0, h)
    return h, clamp01(s), l


def hsl_to_rgb(h, s, l):
    hh = np.mod(np.mod(h, 360.0) + 360.0, 360.0) / 60
    c = (1 - np.abs(2 * l - 1)) * s
    x = c * (1 - np.abs(np.mod(hh, 2.0) - 1))
    m = l - c / 2
    z = np.zeros_like(c)
    conds = [hh < 1, hh < 2, hh < 3, hh < 4, hh < 5]
    r = np.select(conds, [c, x, z, z, x], c)
    g = np.select(conds, [x, c, c, x, z], z)
    b = np.select(conds, [z, z, x, c, c], x)
    return np.stack([r + m, g + m, b + m], axis=-1)


def rgb_to_hsv(c):
    h, _, _ = rgb_to_hsl(c)
    mx = c.max(axis=-1)
    mn = c.min(axis=-1)
    s = np.where(mx == 0, 0.0, (mx - mn) / np.where(mx == 0, 1.0, mx))
    return h, s, mx


def hue_to_rgb(deg):
    h = (((deg % 360) + 360) % 360) / 360
    return np.array([
        min(1, max(0, abs(h * 6 - 3) - 1)),
        min(1, max(0, 2 - abs(h * 6 - 2))),
        min(1, max(0, 2 - abs(h * 6 - 4))),
    ])


def rgb_to_lab(c):
    """sRGB -> CIE Lab D65 (colorMath.ts rgbToLab), used by the colorRange mask (5.2)."""
    lin = srgb_to_linear(c)
    r, g, b = lin[..., 0], lin[..., 1], lin[..., 2]
    x = (0.4124564 * r + 0.3575761 * g + 0.1804375 * b) / 0.95047
    y = 0.2126729 * r + 0.7151522 * g + 0.072175 * b
    z = (0.0193339 * r + 0.119192 * g + 0.9503041 * b) / 1.08883

    def f(t):
        return np.where(t > 216 / 24389, np.cbrt(t), (24389 / 27 * t + 16) / 116)

    fx, fy, fz = f(x), f(y), f(z)
    return 116 * fy - 16, 500 * (fx - fy), 200 * (fy - fz)


def range_weight(h, center, width, feather):
    d = np.abs(np.mod(np.mod(h - center, 360.0) + 540.0, 360.0) - 180)
    half = width / 2
    if feather <= 0:
        return np.where(d <= half, 1.0, 0.0)
    return np.where(d <= half, 1.0, np.where(d >= half + feather, 0.0, 1 - (d - half) / feather))


# --------------------------------------------------------------------------------------
# Darkroom formulas (lib/studio/colorMath.ts), verbatim
# --------------------------------------------------------------------------------------

def eq_adjust(c, brightness, contrast, saturation):
    l = luma(c)
    nl = (l - 0.5) * contrast + 0.5 + brightness
    return clamp01(nl[..., None] + (c - l[..., None]) * saturation)


def color_balance_midtone_weight(c):
    l = 0.5 * (c.max(axis=-1) + c.min(axis=-1))
    return clamp01((l - 0.333) * 4 + 0.5) * clamp01((1 - l - 0.333) * 4 + 0.5) * 0.7


def color_balance_midtones(c, rm, gm, bm):
    w = color_balance_midtone_weight(c)[..., None]
    return clamp01(c + w * np.array([rm, gm, bm]))


def vibrance_adjust(c, intensity):
    sat = c.max(axis=-1) - c.min(axis=-1)
    l = c[..., 0] * VIB_LUMA[0] + c[..., 1] * VIB_LUMA[1] + c[..., 2] * VIB_LUMA[2]
    f = 1 + intensity * (1 - np.sign(intensity) * sat)
    return clamp01(l[..., None] + (c - l[..., None]) * f[..., None])


# --------------------------------------------------------------------------------------
# Curves, levels, LUT (5.1)
# --------------------------------------------------------------------------------------

def build_curve_lut(points):
    """Fritsch-Carlson monotone cubic Hermite sampled to 256 entries (buildCurveLut)."""
    n = len(points)
    lut = np.zeros(LUT_SIZE)
    if n < 2:
        return np.arange(LUT_SIZE) / (LUT_SIZE - 1)
    xs = [p["x"] for p in points]
    ys = [p["y"] for p in points]
    h = [xs[i + 1] - xs[i] for i in range(n - 1)]
    d = [(ys[i + 1] - ys[i]) / h[i] for i in range(n - 1)]
    m = [0.0] * n
    m[0] = d[0]
    m[n - 1] = d[n - 2]
    for i in range(1, n - 1):
        m[i] = 0.0 if d[i - 1] * d[i] <= 0 else (d[i - 1] + d[i]) / 2
    for i in range(n - 1):
        if d[i] == 0:
            m[i] = 0.0
            m[i + 1] = 0.0
            continue
        a = m[i] / d[i]
        b = m[i + 1] / d[i]
        s = a * a + b * b
        if s > 9:
            t = 3 / math.sqrt(s)
            m[i] = t * a * d[i]
            m[i + 1] = t * b * d[i]
    seg = 0
    for i in range(LUT_SIZE):
        x = i / (LUT_SIZE - 1)
        if x <= xs[0]:
            lut[i] = ys[0]
            continue
        if x >= xs[n - 1]:
            lut[i] = ys[n - 1]
            continue
        while seg < n - 2 and x > xs[seg + 1]:
            seg += 1
        t = (x - xs[seg]) / h[seg]
        t2 = t * t
        t3 = t2 * t
        y = ((2 * t3 - 3 * t2 + 1) * ys[seg] + (t3 - 2 * t2 + t) * h[seg] * m[seg]
             + (-2 * t3 + 3 * t2) * ys[seg + 1] + (t3 - t2) * h[seg] * m[seg + 1])
        lut[i] = min(1.0, max(0.0, y))
    return lut


def sample_lut(lut, v):
    p = clamp01(v) * (LUT_SIZE - 1)
    i = np.minimum(LUT_SIZE - 2, np.floor(p)).astype(np.int64)
    return lut[i] + (lut[i + 1] - lut[i]) * (p - i)


def levels_channel(v, p):
    span = max(p["inWhite"] - p["inBlack"], 1e-6)
    t = clamp01((v - p["inBlack"]) / span)
    return clamp01(np.power(t, 1 / p["gamma"]) * (p["outWhite"] - p["outBlack"]) + p["outBlack"])


_lut_cache = {}


def parse_cube(text):
    """Port of lib/studio/lutParser.ts parseCubeLut: byte-quantised table, or None."""
    if text in _lut_cache:
        return _lut_cache[text]
    result = None
    try:
        size = 0
        entries = []
        for raw in text.splitlines():
            line = raw.strip()
            if line == "" or line.startswith("#"):
                continue
            if line.startswith("LUT_3D_SIZE"):
                size = int(line.split()[1])
                continue
            if line.startswith("LUT_1D_SIZE"):
                raise ValueError("1D")
            if line.startswith(("TITLE", "DOMAIN_MIN", "DOMAIN_MAX", "LUT_3D_INPUT_RANGE")):
                continue
            parts = line.split()
            if len(parts) >= 3:
                try:
                    entries.extend([float(parts[0]), float(parts[1]), float(parts[2])])
                except ValueError:
                    pass
        if size < 2 or size > 64:
            raise ValueError("size")
        expected = size ** 3
        if len(entries) / 3 < expected:
            raise ValueError("truncated")
        arr = np.floor(np.array(entries[: expected * 3]) * 255 + 0.5)  # Math.round
        arr = np.clip(arr, 0, 255)
        result = (size, arr.reshape(size, size, size, 3))  # [z=b][y=g][x=r][k]
    except (ValueError, IndexError):
        result = None
    if len(_lut_cache) > 8:
        _lut_cache.clear()
    _lut_cache[text] = result
    return result


def sample_lut3d(lut, c):
    n, data = lut
    fx, fy, fz = [clamp01(c[..., k]) * (n - 1) for k in range(3)]
    x0 = np.minimum(n - 2, np.floor(fx)).astype(np.int64)
    y0 = np.minimum(n - 2, np.floor(fy)).astype(np.int64)
    z0 = np.minimum(n - 2, np.floor(fz)).astype(np.int64)
    tx, ty, tz = (fx - x0)[..., None], (fy - y0)[..., None], (fz - z0)[..., None]

    def at(x, y, z):
        return data[z, y, x] / 255

    def lerp(a, b, t):
        return a + (b - a) * t

    c00 = lerp(at(x0, y0, z0), at(x0 + 1, y0, z0), tx)
    c10 = lerp(at(x0, y0 + 1, z0), at(x0 + 1, y0 + 1, z0), tx)
    c01 = lerp(at(x0, y0, z0 + 1), at(x0 + 1, y0, z0 + 1), tx)
    c11 = lerp(at(x0, y0 + 1, z0 + 1), at(x0 + 1, y0 + 1, z0 + 1), tx)
    return lerp(lerp(c00, c10, ty), lerp(c01, c11, ty), tz)


# --------------------------------------------------------------------------------------
# Rung 1 adjustments (5.1); each takes (H,W,3) and the layer params
# --------------------------------------------------------------------------------------

def rescale_to_luma(c, target):
    l = luma(c)
    k = np.where(l < MIN_LUMA, 1.0, target / np.where(l < MIN_LUMA, 1.0, l))
    return clamp01(c * k[..., None])


def ap_exposure(c, p):  # 5.1 exposure
    g = 2.0 ** p["ev"]
    lin = clamp01(srgb_to_linear(c) * g + p["offset"])
    return clamp01(np.power(linear_to_srgb(lin), 1 / p["gamma"]))


def ap_brightness_contrast(c, p):  # 5.1 brightnessContrast
    k = 2.0 ** (p["contrast"] / 50)
    return clamp01((clamp01(c + p["brightness"] / 200) - 0.5) * k + 0.5)


def ap_tone(c, p):  # 5.1 tone
    L = luma(c)
    w_sh = 1 - smoothstep(0, 0.6, L)
    w_hi = smoothstep(0.4, 1, L)
    w_bl = 1 - smoothstep(0, 0.25, L)
    w_wh = smoothstep(0.75, 1, L)
    dl = (0.25 * (p["shadows"] / 100) * w_sh + 0.25 * (p["highlights"] / 100) * w_hi
          + 0.15 * (p["blacks"] / 100) * w_bl + 0.15 * (p["whites"] / 100) * w_wh)
    k = clamp01(L + dl) / np.maximum(L, MIN_LUMA)
    return clamp01(c * k[..., None])


def ap_levels(c, p):  # 5.1 levels: channels first, then master
    out = [levels_channel(levels_channel(c[..., i], p[k]), p["master"]) for i, k in enumerate("rgb")]
    return np.stack(out, axis=-1)


def ap_curves(c, p, cache):  # 5.1 curves
    luts = cache.get("curves")
    if luts is None:
        luts = {k: build_curve_lut(p[k]["points"]) for k in ("master", "r", "g", "b")}
        cache["curves"] = luts
    out = [sample_lut(luts["master"], sample_lut(luts[k], c[..., i])) for i, k in enumerate("rgb")]
    return np.stack(out, axis=-1)


def ap_white_balance(c, p):  # 5.1 whiteBalance, linear light
    t = p["temperature"] / 100
    ti = p["tint"] / 100
    return clamp01(linear_to_srgb(color_balance_midtones(srgb_to_linear(c), t, -ti, -t)))


def ap_vibrance_saturation(c, p):  # 5.1 vibranceSaturation
    return eq_adjust(vibrance_adjust(c, p["vibrance"] / 100), 0, 1, 1 + p["saturation"] / 100)


def ap_hsl(c, p):  # 5.1 hsl (separate +/- lightness accumulators, as the reference)
    h, s, l = rgb_to_hsl(c)
    neutral = np.minimum(1.0, s / HSL_NEUTRAL_SAT)
    dh = np.full(h.shape, float(p["master"]["hue"]))
    s_mul = np.full(h.shape, 1 + p["master"]["saturation"] / 100)
    dl = p["master"]["lightness"] / 100
    l_pos = np.full(h.shape, dl if dl > 0 else 0.0)
    l_neg = np.full(h.shape, 0.0 if dl > 0 else dl)
    for r in p["ranges"]:
        w = range_weight(h, r["center"], r["width"], r["feather"]) * neutral
        dh = dh + w * r["hue"]
        s_mul = s_mul * (1 + (w * r["saturation"]) / 100)
        dlr = (w * r["lightness"]) / 100
        l_pos = l_pos + np.maximum(dlr, 0)
        l_neg = l_neg + np.minimum(dlr, 0)
    s2 = clamp01(s * s_mul)
    l2 = clamp01(l + l_pos * (1 - l) + l_neg * l)
    return clamp01(hsl_to_rgb(h + dh, s2, l2))


def ap_color_balance(c, p):  # 5.1 colorBalance
    L = luma(c)
    w_sh = 1 - smoothstep(0, 0.5, L)
    w_mid = color_balance_midtone_weight(c)
    w_hi = smoothstep(0.5, 1, L)
    out = []
    for i, k in enumerate("rgb"):
        d = COLOR_BALANCE_GAIN * ((w_sh * p["shadows"][k] + w_mid * p["midtones"][k] + w_hi * p["highlights"][k]) / 100)
        out.append(clamp01(c[..., i] + d))
    out = np.stack(out, axis=-1)
    return rescale_to_luma(out, L) if p["preserveLuminosity"] else out


_BW_KEYS = ("reds", "yellows", "greens", "cyans", "blues", "magentas")


def ap_black_white(c, p):  # 5.1 blackWhite (default 30/30 ranges)
    h, S, V = rgb_to_hsv(c)
    L = luma(c)
    mix = np.zeros_like(L)
    for i, k in enumerate(_BW_KEYS):
        mix = mix + range_weight(h, i * 60, 30, 30) * (p[k] / 100)
    gray = clamp01((1 - S) * L + S * mix * V)
    g3 = np.stack([gray, gray, gray], axis=-1)
    if not p.get("tint"):
        return g3
    t = hue_to_rgb(p["tint"]["hue"])
    a = (p["tint"]["saturation"] / 100) * TINT_GAIN
    return clamp01(g3 + a * (t - 0.5))


def ap_channel_mixer(c, p):  # 5.1 channelMixer
    def row(m):
        return clamp01((m["r"] * c[..., 0] + m["g"] * c[..., 1] + m["b"] * c[..., 2] + m["constant"]) / 100)

    r = row(p["r"])
    if p["monochrome"]:
        return np.stack([r, r, r], axis=-1)
    return np.stack([r, row(p["g"]), row(p["b"])], axis=-1)


def hex_to_rgb(h):
    n = int(h[1:], 16)
    return np.array([((n >> 16) & 255) / 255, ((n >> 8) & 255) / 255, (n & 255) / 255])


def ap_photo_filter(c, p):  # 5.1 photoFilter
    f = hex_to_rgb(p["color"])
    d = p["density"] / 100
    out = c + (c * f - c) * d
    return rescale_to_luma(out, luma(c)) if p["preserveLuminosity"] else out


def ap_split_tone(c, p):  # 5.1 splitTone
    L = luma(c)
    e = 0.5 + (p["balance"] / 100) * 0.25
    w_sh = 1 - smoothstep(0, e, L)
    w_hi = smoothstep(e, 1, L)
    s = hue_to_rgb(p["shadows"]["hue"])
    h = hue_to_rgb(p["highlights"]["hue"])
    a = SPLIT_TONE_GAIN * (p["shadows"]["saturation"] / 100) * w_sh
    b = SPLIT_TONE_GAIN * (p["highlights"]["saturation"] / 100) * w_hi
    return clamp01(c + a[..., None] * (s - 0.5) + b[..., None] * (h - 0.5))


def ap_lut(c, p):  # 5.1 lut
    lut = parse_cube(p["cube"]) if p["cube"] else None
    if lut is None:
        return c
    o = sample_lut3d(lut, c)
    return c + (o - c) * p["intensity"]


def ap_posterize(c, p):  # 5.1 posterize
    n = p["levels"] - 1
    return np.floor(clamp01(c) * n + 0.5) / n


def ap_threshold(c, p):  # 5.1 threshold
    v = np.where(luma(c) >= p["level"], 1.0, 0.0)
    return np.stack([v, v, v], axis=-1)


def ap_gamma(c, p):  # 5.1 gamma
    return clamp01(np.power(clamp01(c), 1 / p["gamma"]))


def ap_red_eye(c):  # 5.3 red-eye: r = min(r, (g+b)/2); the area comes from the layer mask
    out = c.copy()
    out[..., 0] = np.minimum(c[..., 0], (c[..., 1] + c[..., 2]) / 2)
    return out


# --------------------------------------------------------------------------------------
# Rung 4 detail (5.4). Mirrors colorMath.ts "Detail (design 5.4, MM99)" stage for stage
# (the UI reference is the math authority): every constant below has the same name there.
# --------------------------------------------------------------------------------------

CLARITY_RADIUS, CLARITY_GAIN = 0.02, 0.6
TEXTURE_RADIUS, TEXTURE_MIN_RADIUS, TEXTURE_GAIN = 0.003, 1.5, 0.5
BILATERAL_RADIUS = 4
NR_CHROMA_SIGMA = 6
DEHAZE_WINDOW_RADIUS = 7
DEHAZE_OMEGA, DEHAZE_PERCENTILE, DEHAZE_MIN_A, DEHAZE_MIN_T, DEHAZE_HAZE_GAIN = 0.95, 99.5, 0.05, 0.1, 0.5
GRAIN_GAIN = 0.2
GRAIN_Y_OFFSET = 0x9E3779B9
GRAIN_SALT = 0x85EBCA6B
GRAIN_ROUGHNESS_MIX = 0.5


def gauss(img, sigma):
    """Separable Gaussian: taps k=-r..r, r=ceil(3 sigma), normalised, edges replicated (gaussBlurPlanes)."""
    if sigma < 1e-3:
        return img
    r = int(math.ceil(3 * sigma))
    k = np.exp(-(np.arange(-r, r + 1) ** 2) / (2 * sigma * sigma))
    k = (k / k.sum()).astype(np.float64)
    src = np.ascontiguousarray(img, dtype=np.float64)
    out = cv2.sepFilter2D(src, -1, k, k, borderType=cv2.BORDER_REPLICATE)
    return out.reshape(src.shape).astype(img.dtype, copy=False)


def _unsharp_luma(c, sigma, gain, mid_weight=False, thr=0.0):
    L = luma(c)
    d = L - gauss(L, sigma)
    if thr > 0:
        d = np.where(np.abs(d) > thr, d, 0.0)
    shift = gain * d
    if mid_weight:
        shift = shift * (1 - np.abs(2 * L - 1))
    return clamp01(c + shift[..., None])


def ap_sharpen(c, p):  # 5.4 sharpen
    return _unsharp_luma(c, p["radius"], p["amount"] / 100, False, p["threshold"] / 255)


def ap_blur(c, p):  # 5.4 blur: sigma = radius / 2
    return clamp01(gauss(c, p["radius"] / 2))


def ap_clarity(c, p):  # 5.4 clarity
    long_side = max(c.shape[0], c.shape[1])
    return _unsharp_luma(c, CLARITY_RADIUS * long_side, p["amount"] / 100 * CLARITY_GAIN, True)


def ap_texture(c, p):  # 5.4 texture
    long_side = max(c.shape[0], c.shape[1])
    return _unsharp_luma(c, max(TEXTURE_MIN_RADIUS, TEXTURE_RADIUS * long_side), p["amount"] / 100 * TEXTURE_GAIN, False)


def lab_to_rgb(L, a, b):
    fy = (L + 16) / 116
    fx = fy + a / 500
    fz = fy - b / 200

    def finv(f):
        return np.where(f > 6 / 29, f * f * f, (116 * f - 16) * 27 / 24389)

    x, y, z = finv(fx) * 0.95047, finv(fy), finv(fz) * 1.08883
    r = 3.2404542 * x - 1.5371385 * y - 0.4985314 * z
    g = -0.969266 * x + 1.8760108 * y + 0.041556 * z
    bl = 0.0556434 * x - 0.2040259 * y + 1.0572252 * z
    out = np.stack([r, g, bl], axis=-1)
    return clamp01(linear_to_srgb(clamp01(out)))


def bilateral9(plane, sigma_s, sigma_r):
    """Fixed 9x9 bilateral, edges replicated (bilateral9 in colorMath.ts); 81 shifted multiply-adds."""
    R = BILATERAL_RADIUS
    h, w = plane.shape
    pad = np.pad(plane, R, mode="edge")
    acc = np.zeros_like(plane)
    ws = np.zeros_like(plane)
    inv2r = 1 / (2 * sigma_r * sigma_r)
    for dy in range(-R, R + 1):
        for dx in range(-R, R + 1):
            q = pad[R + dy:R + dy + h, R + dx:R + dx + w]
            wt = math.exp(-(dx * dx + dy * dy) / (2 * sigma_s * sigma_s)) * np.exp(-(q - plane) ** 2 * inv2r)
            acc += wt * q
            ws += wt
    return acc / ws


def ap_noise_reduction(c, p):  # 5.4 noiseReduction: 9x9 bilateral on Lab L, Gaussian on Lab a,b
    if p["luminance"] <= 0 and p["color"] <= 0:
        return c
    L, a, b = rgb_to_lab(c.astype(np.float64))
    Lp = L / 100
    if p["luminance"] > 0:
        l = p["luminance"] / 100
        Lp = bilateral9(np.ascontiguousarray(Lp), 2 + l * 6, max(l * 0.2, 1e-3))
    if p["color"] > 0:
        s = p["color"] / 100 * NR_CHROMA_SIGMA
        a = gauss(np.ascontiguousarray(a), s)
        b = gauss(np.ascontiguousarray(b), s)
    return lab_to_rgb(Lp * 100, a, b).astype(c.dtype, copy=False)


def ap_dehaze(c, p):  # 5.4 dehaze
    amt = p["amount"] / 100
    if amt == 0:
        return c
    dark = np.ascontiguousarray(c.min(axis=-1), dtype=np.float64)
    r = DEHAZE_WINDOW_RADIUS
    dark = cv2.erode(dark, np.ones((2 * r + 1, 2 * r + 1), np.uint8))  # clipped-window min
    A = max(float(np.percentile(dark, DEHAZE_PERCENTILE)), DEHAZE_MIN_A)
    if amt > 0:
        t = np.maximum(1 - DEHAZE_OMEGA * (dark / A), DEHAZE_MIN_T)[..., None].astype(c.dtype)
        out = clamp01((c - A) / t + A)
        return c + (out - c) * amt
    k = -amt * DEHAZE_HAZE_GAIN
    return clamp01(c * (1 - k) + A * k)


def ap_vignette(c, p):  # 5.4 vignette
    h, w = c.shape[:2]
    ny = ((np.arange(h) + 0.5) / h * 2 - 1)[:, None]
    nx = ((np.arange(w) + 0.5) / w * 2 - 1)[None, :]
    d_rect = np.maximum(np.abs(nx), np.abs(ny))
    d_circ = np.sqrt((nx * w / 2) ** 2 + (ny * h / 2) ** 2) / math.hypot(w / 2, h / 2)
    r = (p["roundness"] + 100) / 200
    d = d_rect * (1 - r) + d_circ * r
    mid = p["midpoint"] / 100
    v = smoothstep(mid, mid + p["feather"] / 100, d)
    amt = p["amount"] / 100
    prot = 1 - p["highlights"] / 100 * luma(c) if amt < 0 else 1.0
    k = 1 + amt * v * prot
    return clamp01(c * k[..., None])


def _pcg(v):
    """PCG output hash of uint32 words held in uint64 arrays (pcgHash in colorMath.ts)."""
    M = np.uint64(0xFFFFFFFF)
    s = (v.astype(np.uint64) * np.uint64(747796405) + np.uint64(2891336453)) & M
    sh = (s >> np.uint64(28)) + np.uint64(4)
    word = (((s >> sh) ^ s) * np.uint64(277803737)) & M
    return ((word >> np.uint64(22)) ^ word) & M


def grain_hash(x, y, seed):
    """grainHash: pcg(pcg(x ^ seed) ^ (y + 0x9E3779B9)), uint32 arrays (x row vector, y column vector)."""
    M = np.uint64(0xFFFFFFFF)
    h = _pcg(x.astype(np.uint64) ^ np.uint64(seed & 0xFFFFFFFF))
    return _pcg(h ^ ((y.astype(np.uint64) + np.uint64(GRAIN_Y_OFFSET)) & M))


def grain_noise(h, w, size, seed, roughness):
    xs = np.arange(w, dtype=np.uint64)[None, :]
    ys = np.arange(h, dtype=np.uint64)[:, None]
    sz = np.uint64(max(1, int(size)))
    n1 = grain_hash(xs // sz, ys // sz, int(seed)).astype(np.float64) / 4294967296.0
    q = roughness / 100 * GRAIN_ROUGHNESS_MIX
    if q <= 0:
        return n1
    n2 = grain_hash(xs, ys, (int(seed) ^ GRAIN_SALT) & 0xFFFFFFFF).astype(np.float64) / 4294967296.0
    return n1 * (1 - q) + n2 * q


def ap_grain(c, p):  # 5.4 grain
    h, w = c.shape[:2]
    n = grain_noise(h, w, p["size"], p["seed"], p["roughness"])
    return clamp01(c + (p["amount"] / 100 * GRAIN_GAIN * (n - 0.5))[..., None].astype(c.dtype))


# --------------------------------------------------------------------------------------
# Masks (5.2), aligned to MM98 "Mask and geometry math as implemented" (UI renderMaskCPU).
# Mask space = the geometry-corrected image (w x h); a layer's mask is computed ONCE on
# the geometry-corrected SOURCE (luminosity / colorRange never see earlier layers' output).
# Pixel (x, y) is sampled at (x + 0.5, y + 0.5); coordinates are normalised to w and h.
# --------------------------------------------------------------------------------------

def snapped_trig(deg):
    """cos/sin of `deg` degrees, exact for multiples of 90 (colorMath.ts snaps |v|<1e-12 -> 0, ~1 -> +-1)."""
    a = math.radians(deg)
    c, s = math.cos(a), math.sin(a)
    out = []
    for v in (c, s):
        if abs(v) < 1e-12:
            v = 0.0
        elif abs(abs(v) - 1) < 1e-12:
            v = math.copysign(1.0, v)
        out.append(v)
    return out[0], out[1]


def _alpha_plane(im, prefer_alpha):
    """8-bit-style coverage in [0,1] from a decoded PNG: alpha when asked (brush) or when the
    image is transparent anywhere (subject cut-out), else the grey/red channel."""
    if im is None:
        return None
    scale = 65535.0 if im.dtype == np.uint16 else 255.0
    if im.ndim == 2:
        return im.astype(np.float64) / scale
    if im.shape[2] == 4:
        a = im[..., 3].astype(np.float64) / scale
        if prefer_alpha or float(a.min()) < 1.0:
            return a
        return im[..., 2].astype(np.float64) / scale  # BGR -> red channel
    if prefer_alpha:
        return np.ones(im.shape[:2])
    return im[..., 2].astype(np.float64) / scale


def sample_alpha(A, w, h):
    """Bilinear with texel centres, indices clamped (u = nx*aw - 0.5), over the whole mask space."""
    ah, aw = A.shape
    xs = (np.arange(w) + 0.5) / w * aw - 0.5
    ys = (np.arange(h) + 0.5) / h * ah - 0.5
    x0 = np.floor(xs).astype(np.int64)
    y0 = np.floor(ys).astype(np.int64)
    tx = (xs - x0)[None, :]
    ty = (ys - y0)[:, None]
    xa, xb = np.clip(x0, 0, aw - 1), np.clip(x0 + 1, 0, aw - 1)
    ya, yb = np.clip(y0, 0, ah - 1), np.clip(y0 + 1, 0, ah - 1)
    top = A[ya][:, xa] * (1 - tx) + A[ya][:, xb] * tx
    bot = A[yb][:, xa] * (1 - tx) + A[yb][:, xb] * tx
    return top * (1 - ty) + bot * ty


class MaskCtx:
    def __init__(self, masks_dir):
        self.masks_dir = masks_dir
        self.cache = {}

    def subject_alpha(self, ref):
        if ref in self.cache:
            return self.cache[ref]
        plane = None
        path = os.path.join(self.masks_dir, os.path.basename(ref)) if self.masks_dir else None
        if path and os.path.isfile(path):
            plane = _alpha_plane(cv2.imread(path, cv2.IMREAD_UNCHANGED), False)
        self.cache[ref] = plane
        return plane


def _gauss_replicate(m, sigma):
    """Separable Gaussian, taps k=-r..r with r=ceil(3 sigma), normalised, edges replicated."""
    r = int(math.ceil(3 * sigma))
    k = np.exp(-(np.arange(-r, r + 1) ** 2) / (2 * sigma * sigma))
    k = (k / k.sum()).astype(np.float64)
    return cv2.sepFilter2D(np.ascontiguousarray(m), -1, k, k, borderType=cv2.BORDER_REPLICATE)


def mask_component(comp, src, w, h, mctx):
    """Value plane of one component, or None when a brush/subject has no pixels (skipped, not folded)."""
    kind = comp["kind"]
    px = np.arange(w, dtype=np.float64)[None, :] + 0.5
    py = np.arange(h, dtype=np.float64)[:, None] + 0.5
    if kind == "gradient":  # in pixel space; smoothstep(-f, 1+f, t)
        fx, fy = comp["from"]["x"] * w, comp["from"]["y"] * h
        dx, dy = comp["to"]["x"] * w - fx, comp["to"]["y"] * h - fy
        len2 = dx * dx + dy * dy
        if len2 < 1e-12:
            return np.zeros((h, w))
        t = ((px - fx) * dx + (py - fy) * dy) / len2
        f = comp["feather"]
        return smoothstep(-f, 1 + f, t)
    if kind == "radial":  # rotate clockwise on screen (y down); rx scales with width, ry with height
        cdx, cdy = px - comp["center"]["x"] * w, py - comp["center"]["y"] * h
        c, s = snapped_trig(comp["rotate"])
        u = cdx * c + cdy * s
        v = -cdx * s + cdy * c
        rx, ry = max(comp["rx"] * w, 1e-6), max(comp["ry"] * h, 1e-6)
        d = np.sqrt((u / rx) ** 2 + (v / ry) ** 2)
        return 1 - smoothstep(1 - comp["feather"], 1, d)
    if kind == "luminosity":
        L = luma(src)
        f = comp["feather"]
        return smoothstep(comp["lo"] - f, comp["lo"], L) * (1 - smoothstep(comp["hi"], comp["hi"] + f, L))
    if kind == "colorRange":  # R = fuzziness/100*60; dE76; max over targets; none = 0
        L, A, B = rgb_to_lab(src)
        R = comp["fuzziness"] / 100 * 60
        best = np.zeros((h, w))
        for t in comp["colors"]:
            tl, ta, tb = rgb_to_lab(np.array([t["r"], t["g"], t["b"]], dtype=np.float64) / 255)
            de = np.sqrt((L - tl) ** 2 + (A - ta) ** 2 + (B - tb) ** 2)
            best = np.maximum(best, 1 - smoothstep(0.5 * R, R, de))
        return best
    if kind == "brush":  # coverage = the PNG's alpha channel
        try:
            raw = base64.b64decode(comp["png"].split(",", 1)[1])
            im = cv2.imdecode(np.frombuffer(raw, dtype=np.uint8), cv2.IMREAD_UNCHANGED)
        except (ValueError, IndexError):
            im = None
        plane = _alpha_plane(im, True)
        return None if plane is None else sample_alpha(plane, w, h)
    if kind == "subject":  # invert applies to the sampled value BEFORE the fold
        plane = mctx.subject_alpha(comp["maskRef"])
        if plane is None:
            return None
        v = sample_alpha(plane, w, h)
        return 1 - v if comp.get("invert") else v
    return None


def mask_value(mask, src, mctx):
    """fold (add=max, subtract=min(m,1-v), intersect=min) -> feather -> invert -> opacity."""
    h, w = src.shape[:2]
    m = np.zeros((h, w))
    for comp in mask["components"]:
        v = mask_component(comp, src, w, h, mctx)
        if v is None:
            continue
        mode = comp.get("mode", "add")
        if mode == "add":
            m = np.maximum(m, v)
        elif mode == "subtract":
            m = np.minimum(m, 1 - v)
        else:
            m = np.minimum(m, v)
    if mask["feather"] > 0:
        sigma = mask["feather"] / 100 * 0.05 * max(w, h)
        if sigma >= 1e-3:
            m = _gauss_replicate(m, sigma)
    return clamp01((1 - m if mask["invert"] else m) * mask["opacity"])


# --------------------------------------------------------------------------------------
# Layer stack
# --------------------------------------------------------------------------------------

def apply_type(c, layer, cache):
    t, p = layer["type"], layer["params"]
    if t == "exposure": return ap_exposure(c, p)
    if t == "brightnessContrast": return ap_brightness_contrast(c, p)
    if t == "tone": return ap_tone(c, p)
    if t == "levels": return ap_levels(c, p)
    if t == "curves": return ap_curves(c, p, cache)
    if t == "whiteBalance": return ap_white_balance(c, p)
    if t == "vibranceSaturation": return ap_vibrance_saturation(c, p)
    if t == "hsl": return ap_hsl(c, p)
    if t == "colorBalance": return ap_color_balance(c, p)
    if t == "blackWhite": return ap_black_white(c, p)
    if t == "channelMixer": return ap_channel_mixer(c, p)
    if t == "photoFilter": return ap_photo_filter(c, p)
    if t == "splitTone": return ap_split_tone(c, p)
    if t == "lut": return ap_lut(c, p)
    if t == "invert": return 1 - c
    if t == "posterize": return ap_posterize(c, p)
    if t == "threshold": return ap_threshold(c, p)
    if t == "gamma": return ap_gamma(c, p)
    if t == "redEye": return ap_red_eye(c)
    if t == "sharpen": return ap_sharpen(c, p)
    if t == "blur": return ap_blur(c, p)
    if t == "noiseReduction": return ap_noise_reduction(c, p)
    if t == "clarity": return ap_clarity(c, p)
    if t == "texture": return ap_texture(c, p)
    if t == "dehaze": return ap_dehaze(c, p)
    if t == "vignette": return ap_vignette(c, p)
    if t == "grain": return ap_grain(c, p)
    return c


def blend_layer(mode, inp, adj):
    """normal replaces; luminosity keeps input chroma with the adjusted luma (shift);
    color keeps the input luma with the adjusted chroma (shift). Section 3.2 / 5."""
    if mode == "normal":
        return adj
    if mode == "luminosity":
        d, base = luma(adj) - luma(inp), inp
    else:
        d, base = luma(inp) - luma(adj), adj
    return clamp01(base + d[..., None])


def resolve_mask(layer, doc):
    m = layer.get("mask")
    if not m:
        return None
    if "ref" in m and "components" not in m:  # D3: {ref} names doc.masks[ref]
        return (doc.get("masks") or {}).get(m["ref"])
    return m


def run_layers(doc, c, mctx):
    src = c  # masks read the geometry-corrected SOURCE, never an earlier layer's output
    layers = [l for l in doc["layers"] if l.get("enabled", True) and l.get("opacity", 1) > 0]
    n = max(1, len(layers))
    cache_by_id = {}
    raster = {}  # one raster per distinct mask object, shared by layers (named masks)
    for i, layer in enumerate(layers):
        cache = cache_by_id.setdefault(layer["id"], {})
        mask = resolve_mask(layer, doc)
        mval = None
        if mask:
            key = layer["mask"]["ref"] if "ref" in layer["mask"] and "components" not in layer["mask"] else id(mask)
            if key not in raster:
                raster[key] = mask_value(mask, src, mctx)
            mval = raster[key]
        adjusted = blend_layer(layer.get("blend", "normal"), c, apply_type(c, layer, cache))
        a = layer.get("opacity", 1.0)
        if mval is not None:
            a = clamp01(a * mval)[..., None]
        if np.isscalar(a) and a >= 1:
            c = adjusted
        else:
            c = c + (adjusted - c) * a
        c = np.ascontiguousarray(c)
        progress(0.2 + 0.7 * (i + 1) / n)
    return c


# --------------------------------------------------------------------------------------
# Geometry (5.3), aligned to MM98 (UI planGeometry / applyGeometryCPU): the forward order is
# rotate -> flip -> perspective -> crop, evaluated as an inverse mapping of every OUTPUT pixel
# centre into the source, bilinear with texel centres.
# --------------------------------------------------------------------------------------

class Img:
    """float RGB (H,W,3) + optional alpha (H,W) in [0,1]."""

    def __init__(self, rgb, alpha):
        self.rgb = rgb
        self.alpha = alpha


def geometry_is_identity(g):
    return (g["rotate"] == 0 and not g["flipH"] and not g["flipV"] and not g.get("perspective") and not g.get("crop"))


def _js_round(v):
    return int(math.floor(v + 0.5))


def unit_square_to_quad(q):
    """Heckbert's unit square -> quad map; returns (a..h) with the affine special case."""
    (x0, y0), (x1, y1), (x2, y2), (x3, y3) = q
    sx = x0 - x1 + x2 - x3
    sy = y0 - y1 + y2 - y3
    if abs(sx) < 1e-12 and abs(sy) < 1e-12:
        return (x1 - x0, x3 - x0, x0, y1 - y0, y3 - y0, y0, 0.0, 0.0)
    dx1, dx2, dy1, dy2 = x1 - x2, x3 - x2, y1 - y2, y3 - y2
    den = dx1 * dy2 - dx2 * dy1
    g = (sx * dy2 - dx2 * sy) / den
    h = (dx1 * sy - sx * dy1) / den
    return (x1 - x0 + g * x1, x3 - x0 + h * x3, x0, y1 - y0 + g * y1, y3 - y0 + h * y3, y0, g, h)


def _bilinear(plane, sx, sy):
    """Bilinear sample with texel centres (u = sx - 0.5), neighbours clamped to the edge."""
    hh, ww = plane.shape[:2]
    u, v = sx - 0.5, sy - 0.5
    if plane.dtype == np.float32 and plane.size > F64_MAX_PIXELS:
        out = cv2.remap(plane, u.astype(np.float32), v.astype(np.float32), cv2.INTER_LINEAR, borderMode=cv2.BORDER_REPLICATE)
        return out.astype(plane.dtype)
    x0 = np.floor(u).astype(np.int64)
    y0 = np.floor(v).astype(np.int64)
    tx, ty = (u - x0), (v - y0)
    xa, xb = np.clip(x0, 0, ww - 1), np.clip(x0 + 1, 0, ww - 1)
    ya, yb = np.clip(y0, 0, hh - 1), np.clip(y0 + 1, 0, hh - 1)
    if plane.ndim == 3:
        tx, ty = tx[..., None], ty[..., None]
    p00, p01 = plane[ya, xa], plane[ya, xb]
    p10, p11 = plane[yb, xa], plane[yb, xb]
    top = p00 * (1 - tx) + p01 * tx
    bot = p10 * (1 - tx) + p11 * tx
    return top * (1 - ty) + bot * ty


def apply_geometry(img, g):
    h0, w0 = img.rgb.shape[:2]
    c, s = snapped_trig(g["rotate"])
    wr = max(1, _js_round(abs(w0 * c) + abs(h0 * s) - 1e-9))
    hr = max(1, _js_round(abs(w0 * s) + abs(h0 * c) - 1e-9))
    cr = g.get("crop")
    if cr:
        ox = min(wr - 1, _js_round(cr["x"] * wr))
        oy = min(hr - 1, _js_round(cr["y"] * hr))
        wo = max(1, min(wr - ox, _js_round(cr["width"] * wr)))
        ho = max(1, min(hr - oy, _js_round(cr["height"] * hr)))
    else:
        ox = oy = 0
        wo, ho = wr, hr
    qx = ox + np.arange(wo, dtype=np.float64)[None, :] + 0.5 + np.zeros((ho, 1))
    qy = oy + np.arange(ho, dtype=np.float64)[:, None] + 0.5 + np.zeros((1, wo))
    p = g.get("perspective")
    if p:
        quad = [(p["tl"]["x"], p["tl"]["y"]), (1 + p["tr"]["x"], p["tr"]["y"]),
                (1 + p["br"]["x"], 1 + p["br"]["y"]), (p["bl"]["x"], 1 + p["bl"]["y"])]
        a_, b_, c_, d_, e_, f_, g_, h_ = unit_square_to_quad(quad)
        u, v = qx / wr, qy / hr
        den = g_ * u + h_ * v + 1
        qx, qy = (a_ * u + b_ * v + c_) / den * wr, (d_ * u + e_ * v + f_) / den * hr
    if g["flipH"]:
        qx = wr - qx
    if g["flipV"]:
        qy = hr - qy
    dx, dy = qx - wr / 2, qy - hr / 2
    sx = w0 / 2 + dx * c + dy * s
    sy = h0 / 2 - dx * s + dy * c
    inside = (sx >= 0) & (sx <= w0) & (sy >= 0) & (sy <= h0)
    rgb = _bilinear(img.rgb, sx, sy)
    alpha = _bilinear(img.alpha, sx, sy) if img.alpha is not None else None
    fill = g["fill"]
    if fill != "edge":
        out = ~inside
        if fill == "transparent":
            rgb = np.where(out[..., None], 0.0, rgb)
            base = alpha if alpha is not None else np.ones((ho, wo), dtype=rgb.dtype)
            alpha = np.where(out, 0.0, base)
        else:
            col = hex_to_rgb(fill)
            rgb = np.where(out[..., None], col, rgb)
            if alpha is not None:
                alpha = np.where(out, 1.0, alpha)
    dt = img.rgb.dtype
    return Img(np.ascontiguousarray(rgb, dtype=dt), None if alpha is None else np.ascontiguousarray(alpha, dtype=dt))


# --------------------------------------------------------------------------------------
# I/O and pipeline
# --------------------------------------------------------------------------------------

def read_image(path):
    im = cv2.imread(path, cv2.IMREAD_UNCHANGED)
    if im is None:
        raise SystemExit("cannot decode input image %s" % path)
    scale = 65535.0 if im.dtype == np.uint16 else 255.0
    if im.ndim == 2:
        im = cv2.cvtColor(im, cv2.COLOR_GRAY2BGR)
    alpha = None
    if im.shape[2] == 4:
        alpha = im[..., 3].astype(np.float64) / scale
        im = im[..., :3]
    rgb = cv2.cvtColor(im, cv2.COLOR_BGR2RGB).astype(np.float64) / scale
    return rgb, alpha


def to_working(rgb, alpha):
    big = rgb.shape[0] * rgb.shape[1] > F64_MAX_PIXELS
    dt = np.float32 if big else np.float64
    return Img(np.ascontiguousarray(rgb, dtype=dt), None if alpha is None else np.ascontiguousarray(alpha, dtype=dt))


def downscale(img, max_mp):
    h, w = img.rgb.shape[:2]
    if not max_mp or max_mp <= 0 or h * w <= max_mp * 1e6:
        return img
    s = math.sqrt(max_mp * 1e6 / (h * w))
    nw, nh = max(1, int(math.floor(w * s))), max(1, int(math.floor(h * s)))
    interp = cv2.INTER_AREA if (1 / s) > 2 else cv2.INTER_LANCZOS4  # design 6.3
    rgb = clamp01(cv2.resize(np.ascontiguousarray(img.rgb, dtype=np.float32), (nw, nh), interpolation=interp))
    al = None if img.alpha is None else clamp01(cv2.resize(np.ascontiguousarray(img.alpha, dtype=np.float32), (nw, nh), interpolation=interp))
    return Img(rgb, al)


def render(doc, rgb, alpha, masks_dir, max_mp=0):
    img = to_working(rgb, alpha)
    progress(0.05)
    if not geometry_is_identity(doc["geometry"]):
        img = apply_geometry(img, doc["geometry"])
    progress(0.2)
    mctx = MaskCtx(masks_dir)
    out_rgb = run_layers(doc, img.rgb, mctx)
    out = downscale(Img(out_rgb, img.alpha), max_mp)
    progress(0.95)
    return out


def encode16(img):
    rgb = np.floor(clamp01(img.rgb.astype(np.float64)) * 65535 + 0.5).astype(np.uint16)
    bgr = cv2.cvtColor(rgb, cv2.COLOR_RGB2BGR)
    if img.alpha is not None:
        a = np.floor(clamp01(img.alpha.astype(np.float64)) * 65535 + 0.5).astype(np.uint16)
        bgr = np.dstack([bgr, a])
    return bgr


def to_bytes(img):
    rgb = np.floor(clamp01(img.rgb.astype(np.float64)) * 255 + 0.5).astype(np.uint8)
    a = np.full(rgb.shape[:2], 255, np.uint8) if img.alpha is None else np.floor(clamp01(img.alpha.astype(np.float64)) * 255 + 0.5).astype(np.uint8)
    return rgb, a


def load_doc(path):
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def run_cli(args):
    doc = load_doc(args.doc)
    rgb, alpha = read_image(args.input)
    out = render(doc, rgb, alpha, args.masks, args.max_megapixels)
    if not cv2.imwrite(args.out, encode16(out)):
        raise SystemExit("cannot write %s" % args.out)
    progress(1.0)
    return 0


# --------------------------------------------------------------------------------------
# Selftest against the UI-generated goldens (design 3.3)
# --------------------------------------------------------------------------------------

DETAIL_TYPES = {"sharpen", "blur", "clarity", "texture", "dehaze", "vignette"}


def tolerance(doc, manifest_border=None):
    """(mean, p99, max, border) for a doc, the loosest of its categories (design 3.3).
    The golden manifest's borderPx (UI-generated) wins over the built-in 2 px for geometry."""
    mean, p99, mx, border = 1.0, 2.0, 4.0, 0
    types = {l["type"] for l in doc["layers"]}
    if not geometry_is_identity(doc["geometry"]):
        mx, border = max(mx, 2.0), 2
    if manifest_border is not None:
        border = int(manifest_border)
    if types & DETAIL_TYPES:
        mean, p99, mx = max(mean, 1.5), 99.0, max(mx, 8.0)
    if "noiseReduction" in types:
        mean, p99, mx = max(mean, 3.0), 99.0, max(mx, 12.0)
    if types == {"grain"}:
        mean, p99, mx = 0.0, 0.0, 0.0
    return mean, p99, mx, border


def selftest(goldens_dir):
    goldens_dir = os.path.abspath(goldens_dir)
    root = os.path.dirname(goldens_dir)
    images_dir = os.path.join(root, "images")
    fails = 0
    count = 0
    manifest = {}
    mpath = os.path.join(root, "golden-manifest.json")
    if os.path.isfile(mpath):
        with open(mpath, "r", encoding="utf-8") as fh:
            manifest = json.load(fh)
    for name in sorted(os.listdir(goldens_dir)):
        if not name.endswith(".png"):
            continue
        stem = name[:-4]
        doc_name, image_name = stem.rsplit(".", 1)
        doc_path = None
        for sub in ("golden-docs", "docs"):
            cand = os.path.join(root, sub, doc_name + ".json")
            if os.path.isfile(cand):
                doc_path = cand
                break
        if doc_path is None:
            print("FAIL %-48s no doc json found" % stem)
            fails += 1
            continue
        count += 1
        doc = load_doc(doc_path)
        rgb, alpha = read_image(os.path.join(images_dir, image_name + ".png"))
        out = render(doc, rgb, alpha, os.path.join(root, "masks"), 0)
        got_rgb, got_a = to_bytes(out)
        gold = cv2.imread(os.path.join(goldens_dir, name), cv2.IMREAD_UNCHANGED)
        gold_a = gold[..., 3] if gold.ndim == 3 and gold.shape[2] == 4 else np.full(gold.shape[:2], 255, np.uint8)
        gold_rgb = cv2.cvtColor(gold[..., :3], cv2.COLOR_BGR2RGB)
        entry = manifest.get(stem)
        if entry and (entry["height"], entry["width"]) != tuple(got_rgb.shape[:2]):
            print("FAIL %-48s size %dx%d != manifest %dx%d" % (stem, got_rgb.shape[1], got_rgb.shape[0], entry["width"], entry["height"]))
            fails += 1
            continue
        if gold_rgb.shape != got_rgb.shape:
            print("FAIL %-48s size %s != golden %s" % (stem, got_rgb.shape, gold_rgb.shape))
            fails += 1
            continue
        mean_t, p99_t, max_t, border = tolerance(doc, entry["borderPx"] if entry else None)
        diff = np.abs(got_rgb.astype(np.int16) - gold_rgb.astype(np.int16))
        a_diff = np.abs(got_a.astype(np.int16) - gold_a.astype(np.int16))
        if border:
            diff = diff[border:-border, border:-border]
            a_diff = a_diff[border:-border, border:-border]
        mean, p99, mx = float(diff.mean()), float(np.percentile(diff, 99)), float(diff.max())
        amax = float(a_diff.max()) if a_diff.size else 0.0
        ok = mean <= mean_t and p99 <= p99_t and mx <= max_t and amax <= max(1.0, max_t if border else 1.0)
        if not ok:
            fails += 1
        print("%s %-48s mean %.3f p99 %.1f max %.0f alphaMax %.0f (tol mean %.1f p99 %.0f max %.0f)" % (
            "PASS" if ok else "FAIL", stem, mean, p99, mx, amax, mean_t, p99_t, max_t))
    print("selftest: %d goldens, %d failed" % (count, fails))
    return 1 if fails or count == 0 else 0


def main(argv=None):
    global _progress_on
    ap = argparse.ArgumentParser(description="Aperture photo renderer")
    ap.add_argument("--input")
    ap.add_argument("--doc")
    ap.add_argument("--masks", default="")
    ap.add_argument("--out")
    ap.add_argument("--max-megapixels", type=float, default=0)
    ap.add_argument("--progress", action="store_true")
    ap.add_argument("--selftest", metavar="GOLDENS_DIR")
    args = ap.parse_args(argv)
    _progress_on = args.progress
    if args.selftest:
        return selftest(args.selftest)
    if not (args.input and args.doc and args.out):
        ap.error("--input, --doc and --out are required")
    return run_cli(args)


if __name__ == "__main__":
    sys.exit(main())
