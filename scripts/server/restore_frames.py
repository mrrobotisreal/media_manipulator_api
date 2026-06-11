#!/usr/bin/env python3
"""Per-frame super-resolution helper for Media Manipulator's AI Video Restoration.

Enhances every PNG in --frames-dir with SwinIR-L (real-world SR GAN) or HAT
(Real_HAT_GAN) and writes the enhanced PNGs — same filenames — into
<--out-dir>/frames. The Go API shells out to the deployed copy at
/opt/media-manipulator-ai/scripts/restore_frames.py; this file in the repo is
the source of truth — deploy with:

    sudo cp scripts/server/restore_frames.py \
      /opt/media-manipulator-ai/scripts/restore_frames.py
    sudo chmod +x /opt/media-manipulator-ai/scripts/restore_frames.py

Stdlib-only orchestration; torch/cv2/model imports happen lazily inside the
restore-sr venv python (AI_RESTORE_PYTHON). Real-ESRGAN is NOT handled here —
the Go API drives realesrgan-ncnn-vulkan directly.

Progress protocol: one "PROGRESS <done>/<total>" line per enhanced frame on
stdout. Fatal errors: one-line "ERROR: <safe message>" and a non-zero exit.
Honors CUDA_VISIBLE_DEVICES (set by the Go caller); --gpu addresses the
visible devices, so it is normally 0. Never writes outside --out-dir and
--temp-root.
"""
import argparse
import shutil
import subprocess
import sys
import tempfile
import threading
from pathlib import Path
from typing import List, Optional

SWINIR_WEIGHTS = "003_realSR_BSRGAN_DFOWMFC_s64w8_SwinIR-L_x4_GAN.pth"
HAT_WEIGHTS = "Real_HAT_GAN_SRx4.pth"


def fail(message: str) -> None:
    raise SystemExit(f"ERROR: {message}")


def emit_progress(done: int, total: int) -> None:
    print(f"PROGRESS {done}/{total}", flush=True)


def run(cmd: List[str], *, cwd: Optional[Path] = None) -> None:
    print("+ " + " ".join(cmd), flush=True)
    proc = subprocess.run(cmd, cwd=str(cwd) if cwd else None)
    if proc.returncode != 0:
        fail(f"subprocess exited with code {proc.returncode}")


def list_frames(frames_dir: Path) -> List[Path]:
    frames = sorted(p for p in frames_dir.glob("*.png") if p.is_file())
    if not frames:
        fail("no input frames found")
    return frames


def resolve_weights(models_dir: Path, model: str, preferred: str) -> Path:
    exact = models_dir / model / preferred
    if exact.exists():
        return exact
    candidates = sorted((models_dir / model).glob("*.pth")) if (models_dir / model).is_dir() else []
    if candidates:
        return candidates[0]
    fail(f"weights for {model} not found under {models_dir / model} (expected {preferred})")
    raise AssertionError  # unreachable


# ---------------------------------------------------------------------------
# Network construction. Definitions match the official release configs for
# 003_realSR_BSRGAN SwinIR-L x4 GAN and Real_HAT_GAN_SRx4.
# ---------------------------------------------------------------------------

def build_swinir(repos_dir: Path, weights: Path, device: str):
    import torch  # noqa: PLC0415 — venv import by design

    repo = repos_dir / "SwinIR"
    if not repo.is_dir():
        fail(f"SwinIR repo not cloned at {repo}")
    sys.path.insert(0, str(repo))
    try:
        from models.network_swinir import SwinIR  # type: ignore
    except Exception as exc:  # noqa: BLE001
        fail(f"could not import SwinIR network definition: {exc}")
        raise
    model = SwinIR(
        upscale=4,
        in_chans=3,
        img_size=64,
        window_size=8,
        img_range=1.0,
        depths=[6, 6, 6, 6, 6, 6, 6, 6, 6],
        embed_dim=240,
        num_heads=[8, 8, 8, 8, 8, 8, 8, 8, 8],
        mlp_ratio=2,
        upsampler="nearest+conv",
        resi_connection="3conv",
    )
    state = torch.load(str(weights), map_location="cpu")
    if isinstance(state, dict) and "params_ema" in state:
        state = state["params_ema"]
    elif isinstance(state, dict) and "params" in state:
        state = state["params"]
    model.load_state_dict(state, strict=True)
    return model.eval().to(device), 8


def build_hat(weights: Path, device: str):
    import torch  # noqa: PLC0415

    try:
        from hat.archs.hat_arch import HAT  # type: ignore
    except Exception as exc:  # noqa: BLE001
        raise RuntimeError(f"hat package not importable: {exc}") from exc
    model = HAT(
        upscale=4,
        in_chans=3,
        img_size=64,
        window_size=16,
        compress_ratio=3,
        squeeze_factor=30,
        conv_scale=0.01,
        overlap_ratio=0.5,
        img_range=1.0,
        depths=[6, 6, 6, 6, 6, 6],
        embed_dim=180,
        num_heads=[6, 6, 6, 6, 6, 6],
        mlp_ratio=2,
        upsampler="pixelshuffle",
        resi_connection="1conv",
    )
    state = torch.load(str(weights), map_location="cpu")
    if isinstance(state, dict) and "params_ema" in state:
        state = state["params_ema"]
    elif isinstance(state, dict) and "params" in state:
        state = state["params"]
    model.load_state_dict(state, strict=True)
    return model.eval().to(device), 16


# ---------------------------------------------------------------------------
# Tiled inference (the canonical SwinIR test-time tiling; works for any
# x4 image SR network with a window size to pad to).
# ---------------------------------------------------------------------------

def enhance_one(img_lq, model, scale: int, tile: int, tile_overlap: int, window_size: int):
    import torch  # noqa: PLC0415

    _, _, h_old, w_old = img_lq.size()
    h_pad = (h_old // window_size + 1) * window_size - h_old
    w_pad = (w_old // window_size + 1) * window_size - w_old
    img = torch.cat([img_lq, torch.flip(img_lq, [2])], 2)[:, :, : h_old + h_pad, :]
    img = torch.cat([img, torch.flip(img, [3])], 3)[:, :, :, : w_old + w_pad]

    if tile <= 0:
        output = model(img)
    else:
        b, c, h, w = img.size()
        tile_size = min(tile, h, w)
        if tile_size % window_size != 0:
            tile_size = (tile_size // window_size) * window_size
        stride = tile_size - tile_overlap
        h_idx_list = list(range(0, h - tile_size, stride)) + [h - tile_size]
        w_idx_list = list(range(0, w - tile_size, stride)) + [w - tile_size]
        E = torch.zeros(b, c, h * scale, w * scale, dtype=img.dtype, device=img.device)
        W = torch.zeros_like(E)
        for h_idx in h_idx_list:
            for w_idx in w_idx_list:
                in_patch = img[..., h_idx : h_idx + tile_size, w_idx : w_idx + tile_size]
                out_patch = model(in_patch)
                mask = torch.ones_like(out_patch)
                E[
                    ...,
                    h_idx * scale : (h_idx + tile_size) * scale,
                    w_idx * scale : (w_idx + tile_size) * scale,
                ].add_(out_patch)
                W[
                    ...,
                    h_idx * scale : (h_idx + tile_size) * scale,
                    w_idx * scale : (w_idx + tile_size) * scale,
                ].add_(mask)
        output = E.div_(W)
    return output[..., : h_old * scale, : w_old * scale]


def enhance_frames(model, window_size: int, frames: List[Path], out_frames: Path, scale: int, tile: int, tile_overlap: int, device: str) -> None:
    import cv2  # noqa: PLC0415
    import numpy as np  # noqa: PLC0415
    import torch  # noqa: PLC0415

    total = len(frames)
    with torch.no_grad():
        for i, frame in enumerate(frames, start=1):
            img = cv2.imread(str(frame), cv2.IMREAD_COLOR)
            if img is None:
                fail(f"could not read frame {frame.name}")
            img = img.astype(np.float32) / 255.0
            img = torch.from_numpy(np.transpose(img[:, :, [2, 1, 0]], (2, 0, 1))).unsqueeze(0).to(device)
            out = enhance_one(img, model, scale, tile, tile_overlap, window_size)
            out = out.data.squeeze().float().cpu().clamp_(0, 1).numpy()
            out = np.transpose(out[[2, 1, 0], :, :], (1, 2, 0))
            out = (out * 255.0).round().astype(np.uint8)
            if not cv2.imwrite(str(out_frames / frame.name), out):
                fail(f"could not write enhanced frame {frame.name}")
            emit_progress(i, total)
            if i % 10 == 0:
                torch.cuda.empty_cache()


# ---------------------------------------------------------------------------
# HAT fallback: when direct construction fights the installed hat/basicsr
# versions, generate a BasicSR test YAML and subprocess hat/test.py, then move
# the visualization outputs into place.
# ---------------------------------------------------------------------------

def hat_yaml(frames_dir: Path, weights: Path, tile: int, tile_overlap: int) -> str:
    return f"""# Generated by restore_frames.py — temp file, safe to delete.
name: mm_hat_restore
model_type: HATModel
scale: 4
num_gpu: 1
manual_seed: 0

tile:
  tile_size: {tile}
  tile_pad: {tile_overlap}

datasets:
  test_1:
    name: frames
    type: SingleImageDataset
    dataroot_lq: {frames_dir}
    io_backend:
      type: disk

network_g:
  type: HAT
  upscale: 4
  in_chans: 3
  img_size: 64
  window_size: 16
  compress_ratio: 3
  squeeze_factor: 30
  conv_scale: 0.01
  overlap_ratio: 0.5
  img_range: 1.
  depths: [6, 6, 6, 6, 6, 6]
  embed_dim: 180
  num_heads: [6, 6, 6, 6, 6, 6]
  mlp_ratio: 2
  upsampler: 'pixelshuffle'
  resi_connection: '1conv'

path:
  pretrain_network_g: {weights}
  strict_load_g: true
  param_key_g: 'params_ema'

val:
  save_img: true
  suffix: 'mm'
"""


def watch_count(directory: Path, total: int, stop: threading.Event) -> None:
    while not stop.wait(2.0):
        done = len(list(directory.rglob("*.png"))) if directory.exists() else 0
        emit_progress(min(done, total), total)


def run_hat_via_testpy(repos_dir: Path, frames: List[Path], frames_dir: Path, out_frames: Path, weights: Path, tile: int, tile_overlap: int, temp_root: Path) -> None:
    repo = repos_dir / "HAT"
    test_py = repo / "hat" / "test.py"
    if not test_py.exists():
        fail(f"HAT repo not cloned at {repo} (and direct model construction failed)")
    temp_root.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix="mm_hat_", dir=str(temp_root)))
    try:
        yml = work / "mm_hat_restore.yml"
        yml.write_text(hat_yaml(frames_dir, weights, tile, tile_overlap))
        results_root = work / "results"
        stop = threading.Event()
        watcher = threading.Thread(target=watch_count, args=(results_root, len(frames), stop), daemon=True)
        watcher.start()
        try:
            # cwd=work so BasicSR's relative results/ tree lands inside our
            # temp dir, never inside the repo clone.
            run([sys.executable, str(test_py), "-opt", str(yml)], cwd=work)
        finally:
            stop.set()
            watcher.join(timeout=5)
        produced = sorted(results_root.rglob("*.png"))
        if len(produced) != len(frames):
            fail(f"HAT produced {len(produced)} frames, expected {len(frames)}")
        # Outputs are named <stem>_mm.png — restore the original frame names.
        by_stem = {p.name.replace("_mm", ""): p for p in produced}
        for frame in frames:
            src = by_stem.get(frame.name)
            if src is None:
                fail(f"HAT output missing for frame {frame.name}")
            shutil.move(str(src), str(out_frames / frame.name))
        emit_progress(len(frames), len(frames))
    finally:
        shutil.rmtree(work, ignore_errors=True)


def main() -> None:
    parser = argparse.ArgumentParser(description="Per-frame SR (SwinIR-L / HAT) for AI Video Restoration")
    parser.add_argument("--model", required=True, choices=["swinir", "hat"])
    parser.add_argument("--frames-dir", required=True)
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--scale", type=int, default=4)
    parser.add_argument("--tile", type=int, default=320)
    parser.add_argument("--tile-overlap", type=int, default=32)
    parser.add_argument("--gpu", default="0")
    parser.add_argument("--models-dir", default="/opt/media-manipulator-ai/models/restore")
    parser.add_argument("--repos-dir", default="/opt/media-manipulator-ai/repos")
    parser.add_argument("--temp-root", default="/opt/media-manipulator-ai/tmp")
    args = parser.parse_args()

    if args.scale != 4:
        fail("only --scale 4 is supported (the Go side downscales for the x2 path)")

    frames_dir = Path(args.frames_dir).resolve()
    out_dir = Path(args.out_dir).resolve()
    models_dir = Path(args.models_dir).resolve()
    repos_dir = Path(args.repos_dir).resolve()
    if not frames_dir.is_dir():
        fail(f"frames dir does not exist: {frames_dir}")
    out_frames = out_dir / "frames"
    out_frames.mkdir(parents=True, exist_ok=True)

    frames = list_frames(frames_dir)

    try:
        import torch  # noqa: PLC0415
    except Exception as exc:  # noqa: BLE001
        fail(f"torch not importable in this venv: {exc}")
        raise
    if not torch.cuda.is_available():
        fail("CUDA is not available in this venv (check driver / CUDA_VISIBLE_DEVICES)")
    device = f"cuda:{args.gpu}"

    if args.model == "swinir":
        weights = resolve_weights(models_dir, "swinir", SWINIR_WEIGHTS)
        model, window_size = build_swinir(repos_dir, weights, device)
        enhance_frames(model, window_size, frames, out_frames, args.scale, args.tile, args.tile_overlap, device)
    else:
        weights = resolve_weights(models_dir, "hat", HAT_WEIGHTS)
        try:
            model, window_size = build_hat(weights, device)
        except Exception as exc:  # noqa: BLE001
            print(f"direct HAT construction failed ({exc}); falling back to hat/test.py", flush=True)
            run_hat_via_testpy(repos_dir, frames, frames_dir, out_frames, weights, args.tile, args.tile_overlap, Path(args.temp_root))
            return
        enhance_frames(model, window_size, frames, out_frames, args.scale, args.tile, args.tile_overlap, device)


if __name__ == "__main__":
    main()
