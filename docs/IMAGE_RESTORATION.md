# AI Image Restoration & Upscaling — API contract & ops notes

The still-image sibling of [AI Video Restoration](VIDEO_RESTORATION.md). One
uploaded image runs through a configurable pipeline of up to eight models and
returns a single results archive plus an inline comparison grid. Jobs reuse the
shared `JobManager` (mode `image_restore`) and the `/api/job/:jobId` (+ SSE)
machinery.

Install guides: [`INSTALL_FACE_RESTORATION.md`](INSTALL_FACE_RESTORATION.md)
(GFPGAN/CodeFormer) and [`INSTALL_PRECLEAN_MODELS.md`](INSTALL_PRECLEAN_MODELS.md)
(FBCNN/SCUNet/NAFNet). Env reference: `RUNBOOK.md` §4.7.

## Models

| Kind | IDs | Notes |
| --- | --- | --- |
| `preclean` | `fbcnn`, `scunet`, `nafnet` | Fidelity-preserving cleanup, **1x** (never changes dimensions). Apache-2.0/MIT — no license gate. |
| `general` | `realesrgan`, `swinir`, `hat` | Upscalers. **Reuse the video feature's binaries/venvs/`restore_frames.py` — zero new installs.** Native x4; x2 = Lanczos downscale of the x4 output. |
| `face` | `gfpgan`, `codeformer` | Generative face priors. CodeFormer is **license-gated** (`IMAGE_RESTORE_CODEFORMER_ENABLED`, default false — S-Lab non-commercial). |

## Endpoints (Firebase-gated `restoreGroup`)

All four sit inside the same group as `/api/video-restore/*`, so
`RESTORE_REQUIRE_FIREBASE_AUTH=true` returns 401 without a valid token.

### `GET /api/image-restore/capabilities`

```jsonc
{
  "enabled": true,
  "maxSourceWidth": 12000, "maxSourceHeight": 12000,
  "maxUploadSizeBytes": 10485760000,
  "maxOutputPixels": 67108864, "maxOutputs": 12,
  "chainSupported": true,
  "models": [
    { "id": "fbcnn", "kind": "preclean", "displayName": "FBCNN (JPEG artifact removal)",
      "scales": [1], "available": true, "estSecondsPerMegapixel": 3 },
    /* … */
    { "id": "codeformer", "kind": "face", "displayName": "CodeFormer",
      "scales": [2,4], "available": false, "reason": "CodeFormer is currently disabled on this server",
      "estSecondsPerMegapixel": 8 }
  ]
}
```

Availability is a per-request stat of the script/venv/weights/repo paths (plus
the CodeFormer flag). The three kinds are independent: a missing pre-clean venv
never disables the general models, and vice versa.

### `POST /api/image-restore/start` — multipart

- `image`: the file (PNG/JPEG/WebP/TIFF/BMP; HEIC only if ImageMagick decodes it).
- `options`: JSON string matching:

```jsonc
{
  "crop": { "x": 0.1, "y": 0.1, "width": 0.5, "height": 0.5 }, // optional; nil = whole image
  "preclean": ["fbcnn", "scunet"],     // normalized to fixed run order fbcnn→scunet→nafnet
  "models": ["realesrgan", "hat"],     // general
  "faceModels": ["gfpgan"],            // face
  "chain": true,                        // requires ≥1 face AND ≥1 general
  "scale": 0,                           // 0 auto (≤540px crop → 4, else 2), or 2 / 4
  "codeformerFidelity": 0.7,            // 0..1, default 0.7
  "fbcnnQualityFactor": 0,              // 0 = blind/auto, else 1..100
  "sessionId": "…"
}
```

At least one model across `preclean + models + faceModels` is required. Returns
`202 {"jobId": "…"}`. Validation (all user-safe messages): allowlist + dedupe
per kind, reject cross-kind contamination, chain requires both bands, FBCNN QF
∈ {0,1..100}, crop in bounds, output-unit count ≤ `maxOutputs`, and the image is
verified to be an image via `MediaInspector`. Crop pixel conversion (≥64×64),
scale resolution, and the output pixel budget are re-checked server-side at the
prepare stage.

### `GET /api/image-restore/:jobId/results`

Completed `image_restore` jobs only. Manifest-derived; **no filesystem paths**.

```jsonc
{
  "jobId": "…",
  "original": { "id": "original", "label": "Original (prepared)", "width": 800, "height": 600,
                "fileName": "image_restoration_results/original.png", "sizeBytes": 1234, "status": "completed" },
  "results": [
    { "id": "preclean_fbcnn", "label": "After FBCNN artifact removal", "kind": "preclean",
      "width": 800, "height": 600, "fileName": "image_restoration_results/preclean_fbcnn.png",
      "sizeBytes": 2222, "status": "completed",
      "fidelityNote": "Pre-clean models remove degradation without generating new content — output is a filtered version of the source signal." },
    { "id": "gfpgan_on_realesrgan", "label": "GFPGAN on Real-ESRGAN result", "kind": "face",
      "baseModel": "realesrgan", "status": "failed", "error": "No faces were detected in this image",
      "generativeNote": "Face enhancement is generative reconstruction — …" }
  ]
}
```

### `GET /api/image-restore/:jobId/result/:resultId`

Streams one result PNG inline (`Content-Type: image/png`, no attachment). The
`resultId` is matched against the manifest — the file path is resolved from the
manifest only, never from client input. `original` is valid; failed/unknown ids
return 404. This powers the inline comparison grid.

> On the Firebase-gated deployment these requests come from `<img>` tags (no
> auth header), so the UI grid falls back to "included in download" if a preview
> 403s rather than showing a broken image. There is no token-in-query auth.

### Download

Unlike video restoration, the results archive is **not** uploaded to S3 (it's
tens–hundreds of MB, not multi-GB). The pipeline leaves
`image_restoration_results.tar.gz` in the job output dir and sets the job result
to `/api/download/:jobId`, so the existing conversion download endpoint serves
it (the `getOutputFilename`/`outputPath` helpers special-case
`mode == "image_restore"`). Result PNGs stay alongside the tarball so the
results/preview endpoints can stream them until the 24h cleanup sweep reclaims
the directory.

## Pipeline & semantics

Stages: `queued → prepare → <one stage per output unit> → package → completed`.
Unit stage keys: `preclean_<id>`, `model_<id>`, `face_<id>_original`,
`face_<id>_on_<generalId>`.

- **prepare** — decode + `-auto-orient` to a normalized PNG, apply the crop
  server-side (`-crop WxH+X+Y +repage`, never client-side), resolve the
  effective scale, re-check the pixel budget against the real crop.
- **pre-clean** — selected models run **sequentially in the fixed semantic order
  fbcnn → scunet → nafnet** (normalized regardless of request order), each on
  the previous step's output. The final cleaned image is the **working source**
  for every general and face unit. Each unit also produces its own result
  (`preclean_<id>.png`, the cumulative image after that step). A failed pre-clean
  unit leaves the working source unchanged (continue from the last good
  intermediate); every downstream outcome records `precleanApplied` = the ids
  that actually ran.
- **general** — each runs on the working source (one-image frames dir + the
  video `restore_frames.py` / `realesrgan-ncnn-vulkan`). x4 native; x2 downscales.
- **face** — on the working source with upscale = effective scale and
  `bg_upsampler` disabled; or, when chained, on a general result with upscale 1.
  A chained unit whose base general model failed is marked failed **without
  execution**.
- **package** — write `manifest.json` (request echo, source `{width,height,
  cropAppliedPx}`, per-unit outcomes incl. `generativeNote`/`fidelityNote`/
  `precleanApplied`), tar `image_restoration_results/` (manifest, `original.png`,
  one `<resultId>.png` per success, `<resultId>.FAILED.txt` per failure).

**Output-unit ordering** (`orderImageRestoreOutputs`): pre-clean (run order) →
general (run order) → face band grouped per face model (gfpgan before
codeformer; each model's on-original run, then — with chaining — its run on each
general result in run order). For `G={realesrgan,hat}`, `F={gfpgan,codeformer}`,
chain on: `realesrgan, hat, gfpgan_on_original, gfpgan_on_realesrgan,
gfpgan_on_hat, codeformer_on_original, codeformer_on_realesrgan,
codeformer_on_hat`. (This groups each face model's outputs together — a
documented refinement of the per-band wording in the spec, and exactly the
product-owner worked example.)

**Partial failure:** a single failed unit never fails the job — it gets a
`FAILED.txt` + a `status:"failed"` outcome. The job fails only if **all** units
fail.

## Forensic-honesty (non-negotiable)

- Every **face** outcome carries `generativeNote`: GFPGAN/CodeFormer synthesize
  plausible detail; the output is for clarity/leads, **not identification
  evidence**. The UI shows a persistent warning whenever a face model is selected
  and a per-result badge.
- Every **pre-clean** outcome carries `fidelityNote`: these models filter the
  existing signal without generating new content. The UI shows a matching
  non-generative badge.

## CodeFormer license gate

CodeFormer is **S-Lab License 1.0 (non-commercial)**. It is feature-flagged via
`IMAGE_RESTORE_CODEFORMER_ENABLED` (default **false**). When off, capabilities
reports it `available:false` with reason "CodeFormer is currently disabled on
this server" and `/start` rejects it. Do **not** flip the flag in the
ad-monetized product or the DR deployment until CreaTV Ltd. has written
permission from S-Lab/NTU or counsel sign-off. GFPGAN (Apache-2.0) and the
pre-clean models (Apache-2.0/MIT) carry no such restriction.

## Privacy / telemetry

Derived metadata only — model counts, chain flag, scale, size bucket, output
count, timings, success/failure, and a `crop_used` boolean. Never filenames,
paths, crop coordinates, or content.
