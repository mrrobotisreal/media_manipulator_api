# Fixtures and field-by-field spec for `PhotoDoc` v1 (Aperture)

This directory is the **contract** between the Go sanitizer (`internal/photo`, MM95) and the UI Zod model (`media-manipulator-ui/lib/photo/photoTypes.ts`, MM97) and the phone port (MA50). Design: `docs/design/PHOTO_EDITOR.md` §4 and §5.

## Files and copy rule

- `docs/*.json` are **byte-identical copies** of the UI's `lib/photo/fixtures/docs/*.json` (and later `mobile-app/src/photo/fixtures/`). Authored here first (MM95, API commit recorded in `.scratch/mm95-mm97/fixtures-ready`); the UI copies them byte for byte. Never regenerate on one side only: any change lands in both repos in one change, or neither.
- **Valid canonical fixtures** (15; `heal-base-refs` and `masks-named` were added by MM96 for D2/D3): `blank`, `defaults`, `clamped`, `rung1-all-layers`, `hsl-grey-except-red`, `curves-s`, `levels-channels`, `masks-all-kinds`, `geometry-rotate-crop`, `geometry-perspective`, `heal-two-bases`, `heal-base-refs`, `masks-named`, `detail-all`, `unknown-fields`. `sanitize(file) == file` (one compact line + exactly one trailing `\n`), and sanitizing twice gives the same bytes.
- **Invalid fixtures** (4, in `docs/`, pretty-printed, never canonical): `invalid-version` (version 2 -> `unsupported_version`), `invalid-layer-type` (`sparkle` -> `unknown_layer_type`), `invalid-curve-points` (duplicate x -> `invalid_curve_points`), `oversize-brush` (2000x2000 brush -> `brush_too_large`). The UI Zod schema must reject all four.
- `inputs/*.json` are **non-canonical inputs** that must produce a canonical fixture: `minimal` -> `blank`, `defaults` -> `defaults` (one layer of each of the 27 types with only `id`+`type`: every default filled), `clamp` -> `clamped` (out-of-range numbers). The Zod side must produce the same canonical bytes for these (schema `clamp` + defaults; Zod refuses out-of-range in the *editor*, so for this parity test use a clamping parse).
- `PHOTO_FIXTURES_UPDATE=1 go test ./internal/photo -run Generate` rewrites the canonical files from `inputs/` and `docs/` (maintainers only).

## Canonical JSON (what `Sanitize` emits and `canonicalPhotoDoc` must emit)

1. Compact (no whitespace outside strings); object keys **sorted by code point at every depth** (known and unknown keys together; JS: `Object.keys(o).sort()` compares UTF-16 code units, which agrees for BMP keys; use a code-point comparator). Array order is preserved.
2. Strings escaped exactly as `JSON.stringify` does: `"` `\\` `\b \f \n \r \t`, other controls `\u00xx` lowercase hex; **no HTML escaping**; U+2028/U+2029 are written **raw** (the sanitizer emits them raw, like JS).
3. Numbers as `Number.prototype.toString` (plain for 1e-6 <= |x| < 1e21, else `1e-7`, `1e+21`); `-0` is `0`; integers print without `.0`.
4. **Defaults are filled**: every known field is present after sanitizing. **Nullable** fields are present as `null` when unset: `geometry.perspective`, `geometry.crop`, `layer.mask`, `blackWhite.tint`. **Optional** fields are **absent** when unset (never `null`): `output.format`, `output.quality`, `output.metadataMode`, `layer.name`, `hsl.ranges[].name`. A present `null` for an optional field is refused.
5. **Unknown fields round-trip** at every object depth (doc, source, geometry, layer, params, mask, mask component, hsl range, ...), verbatim, sorted with the known keys. Unknown `version`, unknown layer `type`, unknown mask `kind` are refused.
6. Out-of-range numbers are **clamped** (never refused); integer fields are rounded with `Math.round` semantics (half up). A non-number where a number is required, a bad enum, a bad colour, a bad id are refused (`invalid_doc` / `bad_id`).
7. `version` is always `1`; `source.orientation` is always `1` (whatever was sent); `source.width`/`height` are **required** integers 1..100000.
8. Colour strings (`#rrggbb`) are lower-cased on output (`geometry.fill`, `photoFilter.color`).
9. Ids: `^[A-Za-z0-9_-]{1,40}$`; layer ids unique in the doc, heal ids unique, hsl range ids unique within the layer.

Errors: Go `*ValidationError{Code, Path, Message}` wrapping `photo.ErrInvalidDoc`. Codes: `doc_too_large`, `invalid_json` (also duplicate keys, trailing data, invalid UTF-8, nesting > 100), `unsupported_version`, `invalid_doc`, `unknown_layer_type`, `unknown_mask_kind`, `bad_id`, `duplicate_id`, `invalid_curve_points`, `brush_too_large`, `too_many_layers`, `too_many_mask_components`, `lut_too_large`.

## Limits (`photo.DefaultLimits()` == UI `PHOTO_MAX_*`)

`MaxDocBytes` 8 MiB (raw input length) · `MaxLayers` 64 · `MaxMaskComponents` 16 (per mask) · `MaxBrushPNGBytes` 524288 (decoded) · `MaxBrushPx` 1024 (per side, from the PNG IHDR **and** the declared width/height) · `MaxLutBytes` 1 MiB · `MaxLutSize` 65 (`LUT_3D_SIZE`) · `MaxCurvePoints` 16 · fixed: heals <= 256, hsl ranges <= 32, colorRange colours <= 16, names <= 80 UTF-16 units.

## Document shape (top level; all keys sorted in the canonical form)

| key | type | default / rule |
|---|---|---|
| `version` | `1` | required, else `unsupported_version` |
| `source` | `{width, height, orientation}` | `width`,`height` int 1..100000 **required**; `orientation` always 1 |
| `heals` | `[{id, maskRef?, baseRef?}]` | default `[]`; at least one of the two per entry (else `invalid_doc`); `maskRef` matches `^masks/[A-Za-z0-9_-]{1,40}\.png$`; `baseRef` matches `^bases/[A-Za-z0-9_-]{1,64}\.png$` (server base) or `^local:[A-Za-z0-9_-]{1,64}$` (a guest's browser-local base); each **absent** when unset (MM96 D2) |
| `masks` | `{<maskId>: Mask}` | optional, **absent when unset**; an empty `{}` canonicalises to absent; <= 64 entries; ids per the id regex (`bad_id`); a `layer.mask` of `{ref}` must name a key (else `invalid_doc`) (MM96 D3) |
| `geometry` | object | below; default object |
| `layers` | `[Layer]` | default `[]`, <= 64, applied in order |
| `output` | `{format?, quality?, metadataMode?}` | default `{}`; `format` in `jpg\|png\|webp\|avif\|tiff`; `quality` int 1..100; `metadataMode` in `keep\|strip`; each **absent** when unset |

`geometry`: `rotate` -360..360 (0), `flipH` false, `flipV` false, `perspective` `null` or `{tl,tr,br,bl}` each `{x,y}` -1..1 (0) (corner offsets normalized to the source size), `crop` `null` or `{x 0..0.999 (0), y 0..0.999 (0), width 0.001..1 (1), height 0.001..1 (1)}` (then `x+width` and `y+height` are clamped to <= 1 by shrinking width/height), `fill` `transparent` (default) `| edge | #rrggbb`.

## Layer

`{id, type, name?, enabled, opacity, blend, params, mask}`: `id` required id; `type` one of the 27 below (else `unknown_layer_type`); `name?` string <= 80 (absent when unset; additive field decided by the coordinator); `enabled` true; `opacity` 0..1 (1); `blend` `normal | luminosity | color` (`normal`); `params` per type (all params default, so `params` may be omitted); `mask` `null` (default), a Mask, or `{ref: "<maskId>"}` naming a key of `doc.masks` (D3).

### Layer types and params (min..max, default)

Rung 1 (design §5.1):

- `exposure`: `ev` -5..5 (0), `offset` -0.5..0.5 (0), `gamma` 0.1..10 (1)
- `brightnessContrast`: `brightness` -100..100 (0), `contrast` -100..100 (0)
- `tone`: `highlights`, `shadows`, `whites`, `blacks`, each -100..100 (0)
- `levels`: `master`, `r`, `g`, `b` (channel keys, all four always present), each `{inBlack 0..1 (0), inWhite 0..1 (1), gamma 0.1..10 (1), outBlack 0..1 (0), outWhite 0..1 (1)}`
- `curves`: `master`, `r`, `g`, `b` (all four always present), each `{points: [{x 0..1, y 0..1}]}`; 2..16 points; **x strictly increasing** (else `invalid_curve_points`, also for fewer than 2 or more than 16 points); default points `[{x:0,y:0},{x:1,y:1}]`
- `whiteBalance`: `temperature` -100..100 (0), `tint` -100..100 (0)
- `vibranceSaturation`: `vibrance` -100..100 (0), `saturation` -100..100 (0)
- `hsl`: `master` `{hue -180..180 (0), saturation -100..100 (0), lightness -100..100 (0)}`; `ranges` `[{id, name?, center 0..360 (0), width 10..120 (30), feather 0..90 (30), hue -180..180 (0), saturation -100..100 (0), lightness -100..100 (0)}]` (<= 32). **If `ranges` is absent** it defaults to the six Photoshop ranges with ids `reds, yellows, greens, cyans, blues, magentas`, `center` 0/60/120/180/240/300, width 30, feather 30, hue/saturation/lightness 0, no `name`. A present `ranges` (even `[]`) is kept as sent.
- `colorBalance`: `shadows`, `midtones`, `highlights` each `{r,g,b}` -100..100 (0); `preserveLuminosity` true
- `blackWhite`: `reds` 40, `yellows` 60, `greens` 40, `cyans` 60, `blues` 20, `magentas` 80 (each -200..300); `tint` **nullable**: `null` (default) or `{hue 0..360 (0), saturation 0..100 (0)}`
- `channelMixer`: `r`, `g`, `b` each `{r,g,b,constant}` -200..200; defaults identity (`r`: 100,0,0,0; `g`: 0,100,0,0; `b`: 0,0,100,0); `monochrome` false
- `photoFilter`: `color` `#rrggbb` (`#ec8a00`), `density` 0..100 (25), `preserveLuminosity` true
- `splitTone`: `shadows`, `highlights` each `{hue 0..360 (0), saturation 0..100 (0)}`; `balance` -100..100 (0)
- `lut`: `cube` string (default `""`; when non-empty it must contain a `LUT_3D_SIZE n` line with 2 <= n <= 65 else `lut_too_large`/`invalid_doc`; <= 1 MiB), `intensity` 0..1 (1)
- `invert`: no params. `redEye`: no params (additive type; colour op inside the mask: `r = min(r, (g+b)/2)`)
- `posterize`: `levels` int 2..255 (4). `threshold`: `level` 0..1 (0.5). `gamma`: `gamma` 0.1..10 (1)

Rung 4 (design §5.4):

- `sharpen`: `amount` 0..500 (100), `radius` 0.1..50 (1), `threshold` 0..255 (0)
- `blur`: `radius` 0..100 (0)
- `noiseReduction`: `luminance` 0..100 (0), `color` 0..100 (0)
- `clarity`, `texture`, `dehaze`: `amount` -100..100 (0)
- `vignette`: `amount` -100..100 (0), `midpoint` 0..100 (50), `roundness` -100..100 (0), `feather` 0..100 (50), `highlights` 0..100 (0)
- `grain`: `amount` 0..100 (0), `size` 1..3 (1), `roughness` 0..100 (50), `seed` int 0..4294967295 (0)

## Mask

`{components, feather, invert, opacity}`: `components` `[Component]` (default `[]`, <= 16, folded left to right), `feather` 0..100 (0), `invert` false, `opacity` 0..1 (1). Coordinates are normalized to the geometry-corrected image.

Every component has `kind` and `mode` (`add | subtract | intersect`, default `add`). Kinds (else `unknown_mask_kind`):

- `gradient`: `from` `{x,y}` (0.5, 0.3), `to` `{x,y}` (0.5, 0.7), `feather` 0..1 (0); point coordinates -1..2
- `radial`: `center` `{x,y}` (0.5, 0.5), `rx` 0..2 (0.3), `ry` 0..2 (0.3), `rotate` -180..180 (0), `feather` 0..1 (0.5)
- `brush`: `png` **required** `data:image/png;base64,...` (decoded <= 512 KB, real PNG signature+IHDR, each side <= 1024 else `brush_too_large`), `width`, `height` **required** ints 1..1024 (declared size; a value above 1024 -> `brush_too_large`)
- `luminosity`: `lo` 0..1 (0), `hi` 0..1 (1), `feather` 0..1 (0.1)
- `colorRange`: `colors` `[{r,g,b}]` ints 0..255 (default `[]`, <= 16), `fuzziness` 0..100 (40)
- `subject`: `maskRef` **required** `masks/<id>.png`, `invert` false

## Notes for the UI port

- The Go sanitizer is the *server* normalizer (clamps). The Zod schema used by the editor may refuse instead of clamp; the parity requirement is only: for every `docs/` valid fixture and every `inputs/` file, canonical output == the fixture bytes, and the four invalid fixtures are rejected.
- `canonicalPhotoDoc` must parse then re-emit with the rules above; `JSON.parse` + a recursive sorted-key stringifier is enough because defaults are already in the fixtures.

## Goldens (copied from the UI, MM97, 2026-11-03)

`images/{photo,chart,alpha}.png`, `golden-docs/*.json` and `goldens/<doc>.<image>.png`
are byte-identical copies of `media-manipulator-ui/lib/photo/fixtures/` (UI `main`
merge of MM97). **The UI is now the source**: regenerate there with
`npm run photo:goldens`, then copy all three directories here in the same change.
The goldens are rendered by `lib/photo/colorMath.ts` (8-bit RGBA, rounding
`floor(v*255+0.5)`) and are what MM96's `photo_render.py --selftest` must match
within the design §3.3 tolerances. They cover the shared docs `blank`, `defaults`,
`curves-s`, `hsl-grey-except-red`, `levels-channels`, `rung1-all-layers` and every
`golden-docs/*.json`; geometry, mask and detail goldens arrive with MM98/MM99.
`TestPhotoGoldenDocsCanonical` checks every golden doc is Go-canonical;
`TestPhotoGoldensComplete` checks one golden per doc × image.
