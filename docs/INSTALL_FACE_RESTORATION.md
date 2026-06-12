# INSTALL: Face Restoration Models (GFPGAN + CodeFormer)

> Server installation guide for the **AI Image Restoration & Upscaling** feature.
> Companion to `INSTALL_VIDEO_RESTORATION.md` — everything that guide installed
> (CUDA 12.8, driver, `restore-sr` venv, Real-ESRGAN/SwinIR/HAT, models dir layout)
> stays exactly where it is. This guide adds only the two face models.
>
> Target: `creatv` — Ubuntu Server 24.04, Ryzen 9 9950X3D, 128GB DDR5,
> RTX 5080 16GB (CUDA index 1, primary AI GPU), RTX 5060 Ti 8GB (CUDA index 0),
> driver 590.x, CUDA toolkit 12.8 at `/usr/local/cuda-12.8`.
> Blackwell (sm_120) → **every torch install must come from the cu128 wheel index.**

---

> ## ⚠️ License warning — read before enabling CodeFormer
>
> **GFPGAN is Apache-2.0** and **Real-ESRGAN is BSD-3** — no restrictions for your
> deployment.
>
> **CodeFormer is under the S-Lab License 1.0, which is non-commercial.** Media
> Manipulator is ad-monetized and the DR deployment is a commercial contract —
> do **not** flip `IMAGE_RESTORE_CODEFORMER_ENABLED=true` in production until
> CreaTV Ltd. has either obtained written permission from S-Lab/NTU or had
> counsel sign off. Installing CodeFormer for internal evaluation is fine; the flag
> is what keeps it out of the product.

---

## 0. Layout

New pieces only — everything else from `INSTALL_VIDEO_RESTORATION.md` stays
where it is:

```
/opt/media-manipulator-ai/
├── venvs/
│   └── face-restore/          # NEW — torch cu128 + gfpgan/facexlib/basicsr
├── repos/
│   └── CodeFormer/            # NEW — vendored basicsr + inference modules
├── models/restore/
│   ├── gfpgan/GFPGANv1.4.pth                  # NEW
│   └── codeformer/codeformer.pth              # NEW
├── scripts/
│   └── restore_image_faces.py # NEW — deployed from scripts/server/
└── (facexlib detection/parsing weights — see §3; they live inside the venv's
     site-packages/facexlib/weights so the libraries find them without
     attempting first-run downloads)
```

Disk: trivial compared to video restoration — weights total ~1.2GB and image
jobs peak well under 1GB of scratch. The existing `OUTPUT_DIR` headroom is
more than enough.

## 1. venv `face-restore`

```bash
cd /opt/media-manipulator-ai/venvs
sudo python3 -m venv face-restore
sudo ./face-restore/bin/pip install --upgrade pip wheel

# Blackwell (sm_120) → torch ≥ 2.7 cu128 wheels, nothing older.
sudo ./face-restore/bin/pip install torch torchvision \
  --index-url https://download.pytorch.org/whl/cu128

# GFPGAN + the face plumbing both models share. Pin basicsr 1.4.2 (the
# version both GFPGAN and CodeFormer were built against).
sudo ./face-restore/bin/pip install opencv-python-headless numpy \
  basicsr==1.4.2 facexlib gfpgan

# CodeFormer's extra deps (its vendored basicsr is used at runtime via
# sys.path, but these supporting packages must exist in the venv):
sudo ./face-restore/bin/pip install lpips scipy
```

**The basicsr `functional_tensor` patch** — same issue as the video venvs:
basicsr 1.4.2 imports a module torchvision removed. Patch once:

```bash
sudo sed -i \
  's/from torchvision.transforms.functional_tensor import rgb_to_grayscale/from torchvision.transforms.functional import rgb_to_grayscale/' \
  /opt/media-manipulator-ai/venvs/face-restore/lib/python3*/site-packages/basicsr/data/degradations.py
```

## 2. Repo clone + model weights

```bash
sudo mkdir -p /opt/media-manipulator-ai/models/restore/{gfpgan,codeformer}
cd /opt/media-manipulator-ai/repos

# CodeFormer — cloned, NOT pip-installed: the wrapper script runs it with the
# repo prepended to sys.path so its vendored modules win over pip basicsr
# where they differ.
sudo git clone https://github.com/sczhou/CodeFormer.git

# GFPGAN v1.4 weights. The .pth historically lives under the v1.3.4 release
# tag of the TencentARC/GFPGAN repo — if the URL 404s, grab it from the
# repo's Releases page (the README's "Model Zoo" links to the current home):
sudo wget -O /opt/media-manipulator-ai/models/restore/gfpgan/GFPGANv1.4.pth \
  https://github.com/TencentARC/GFPGAN/releases/download/v1.3.4/GFPGANv1.4.pth

# CodeFormer weights (official release asset):
sudo wget -O /opt/media-manipulator-ai/models/restore/codeformer/codeformer.pth \
  https://github.com/sczhou/CodeFormer/releases/download/v0.1.0/codeformer.pth
```

## 3. Pre-place the facexlib helper weights

Both models route face detection/alignment through facexlib, which tries to
**download weights on first use** into its own package directory. The API
service user must never need outbound downloads mid-job, so pre-place them:

```bash
FXW=$(ls -d /opt/media-manipulator-ai/venvs/face-restore/lib/python3*/site-packages/facexlib)/weights
sudo mkdir -p "$FXW"

# RetinaFace detector (used by GFPGAN's and CodeFormer's FaceRestoreHelper):
sudo wget -O "$FXW/detection_Resnet50_Final.pth" \
  https://github.com/xinntao/facexlib/releases/download/v0.1.0/detection_Resnet50_Final.pth

# Face parsing net (used for the paste-back blend mask):
sudo wget -O "$FXW/parsing_parsenet.pth" \
  https://github.com/xinntao/facexlib/releases/download/v0.2.2/parsing_parsenet.pth
```

If a smoke test still tries to download something, watch its log line for the
exact filename + destination and pre-place that file the same way — facexlib
prints the URL before fetching.

## 4. Deploy the wrapper script

The repo copy under `scripts/server/` is the source of truth (it is created
by the AI Image Restoration implementation in `media_manipulator_api`):

```bash
cd ~/media_manipulator_api   # wherever the API repo is checked out
sudo cp scripts/server/restore_image_faces.py /opt/media-manipulator-ai/scripts/restore_image_faces.py
sudo chmod +x /opt/media-manipulator-ai/scripts/restore_image_faces.py
```

Re-run this copy step any time the repo script changes.

## 5. systemd environment

systemd does **not** inherit the repo's `.env` files — add the image
restoration block to the API unit's `EnvironmentFile`
(e.g. `/etc/media-manipulator/api.env`). Defaults shown; only set what
differs. The general-model paths (`AI_RESTORE_PYTHON`,
`AI_RESTORE_FRAMES_SCRIPT`, `AI_RESTORE_MODELS_DIR`, `AI_RESTORE_REPOS_DIR`,
GPU indices) are shared with video restoration and are already set.

```ini
# --- AI Image Restoration & Upscaling ---
IMAGE_RESTORE_ENABLED=true
IMAGE_RESTORE_CODEFORMER_ENABLED=false     # license gate — see the warning at the top
AI_FACE_RESTORE_PYTHON=/opt/media-manipulator-ai/venvs/face-restore/bin/python
AI_FACE_RESTORE_SCRIPT=/opt/media-manipulator-ai/scripts/restore_image_faces.py
IMAGE_RESTORE_MAX_OUTPUT_PIXELS=67108864   # ~64MP — past 8K headroom
IMAGE_RESTORE_MAX_OUTPUTS=12
IMAGE_RESTORE_MAX_CONCURRENT_JOBS=1
IMAGE_RESTORE_MODEL_TIMEOUT_SECONDS=1800
IMAGE_RESTORE_RATE_LIMIT_PER_SESSION_PER_HOUR=6
IMAGE_RESTORE_RATE_LIMIT_PER_IP_PER_HOUR=12
IMAGE_RESTORE_VRAM_MIB_GFPGAN=5000
IMAGE_RESTORE_VRAM_MIB_CODEFORMER=6000
```

Then:

```bash
sudo systemctl restart media-manipulator-api
journalctl -u media-manipulator-api -n 50 --no-pager   # confirm clean boot
```

## 6. Smoke tests

Run these as the same user/permissions context the service runs under
(or `sudo -u <service-user>` equivalents) so path/permission problems show up
now, not mid-job. `--gpu 1` targets the RTX 5080 (the configured `AI_CUDA_GPU`);
substitute `--gpu 0` if you want to exercise the 5060 Ti instead.

```bash
AI=/opt/media-manipulator-ai

# 1. GPU + driver sanity (both cards healthy, no ERR! states):
nvidia-smi

# 2. torch sees CUDA with sm_120 support in the new venv:
$AI/venvs/face-restore/bin/python - <<'PY'
import torch
print(torch.__version__, torch.version.cuda)
print(torch.cuda.is_available(), torch.cuda.device_count())
print([torch.cuda.get_device_name(i) for i in range(torch.cuda.device_count())])
PY
# Expect: 2.7+ / 12.8, True, 2, and both card names. Any "no kernel image"
# warning here means a non-cu128 wheel slipped in — see §7.

# 3. Make a face smoke-test input (any small JPEG containing a clear face;
#    a downscaled selfie works — copy one to /tmp/face-smoke.jpg). To stress
#    the realistic case, downscale it hard first:
magick /tmp/face-source.jpg -resize 200x /tmp/face-smoke.jpg

# 4. GFPGAN end to end through the wrapper:
$AI/venvs/face-restore/bin/python $AI/scripts/restore_image_faces.py \
  --model gfpgan --input /tmp/face-smoke.jpg --out-dir /tmp/face-smoke-gfp \
  --upscale 2 \
  --models-dir $AI/models/restore --repos-dir $AI/repos --gpu 1
# Expect PROGRESS lines per face, then restored.png in /tmp/face-smoke-gfp.

# 5. CodeFormer end to end (internal evaluation; the API flag stays false):
$AI/venvs/face-restore/bin/python $AI/scripts/restore_image_faces.py \
  --model codeformer --input /tmp/face-smoke.jpg --out-dir /tmp/face-smoke-cf \
  --upscale 2 --fidelity 0.7 \
  --models-dir $AI/models/restore --repos-dir $AI/repos --gpu 1

# 6. The negative case: a faceless input must fail with the safe message:
ffmpeg -y -f lavfi -i testsrc=duration=1:size=320x240:rate=1 -frames:v 1 /tmp/no-face.png
$AI/venvs/face-restore/bin/python $AI/scripts/restore_image_faces.py \
  --model gfpgan --input /tmp/no-face.png --out-dir /tmp/no-face-out \
  --upscale 2 --models-dir $AI/models/restore --repos-dir $AI/repos --gpu 1
# → "ERROR: No faces were detected in this image", non-zero exit

# 7. API reports availability:
curl -s http://127.0.0.1:59997/api/image-restore/capabilities | python3 -m json.tool
# → gfpgan "available": true; codeformer "available": false with the
#   "currently disabled" reason while the license flag is off.
```

## 7. Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `no kernel image is available` at model start | non-cu128 torch | reinstall torch/torchvision from the cu128 index in `face-restore` |
| `ImportError: functional_tensor` | unpatched basicsr | re-run the sed patch (§1) |
| wrapper hangs ~30s then errors mid-job | facexlib trying to download weights without egress | pre-place the weight it names (§3) |
| `No faces were detected` on images that clearly have faces | face too small/blurred for RetinaFace at native res | upscale first via chaining (run a general model, chain the face model on its result) — that is exactly what the Chain toggle is for |
| CodeFormer import errors mentioning `basicsr.archs` registry | repo not first on `sys.path` / wrong cwd | the wrapper must prepend `repos/CodeFormer` and run with it as cwd; redeploy the script (§4) |
| capabilities shows gfpgan `available: false` | path missing | compare `AI_FACE_RESTORE_*` env values against §0 layout; check weights file exists |
| capabilities shows codeformer `available: false` with "disabled" | license flag off | intentional default — see the license warning before enabling |
| identity looks "off" in CodeFormer output | fidelity weight too low | raise `w` toward 1.0 (the UI slider); remember all face output is generative reconstruction regardless |
| CUDA OOM with both GPUs busy | scheduler VRAM hints too low for a huge crop | raise `IMAGE_RESTORE_VRAM_MIB_*` so the lease blocks instead of OOMing, or lower the crop/scale |
