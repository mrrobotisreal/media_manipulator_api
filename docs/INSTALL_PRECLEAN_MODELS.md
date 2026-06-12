# INSTALL: Pre-clean Models (FBCNN + SCUNet + NAFNet)

> Server installation guide for the **Pre-clean** stage of the AI Image
> Restoration & Upscaling feature. Companion to `INSTALL_FACE_RESTORATION.md`
> and `INSTALL_VIDEO_RESTORATION.md` — everything those guides installed stays
> exactly where it is. This guide adds only the three fidelity-preserving
> cleanup models.
>
> Target: `creatv` — Ubuntu Server 24.04, Ryzen 9 9950X3D, 128GB DDR5,
> RTX 5080 16GB (CUDA index 1, primary AI GPU), RTX 5060 Ti 8GB (CUDA index 0),
> driver 590.x, CUDA toolkit 12.8 at `/usr/local/cuda-12.8`.
> Blackwell (sm_120) → **every torch install must come from the cu128 wheel index.**

---

> ## ✅ License status — all three are commercially safe
>
> **FBCNN: Apache-2.0. SCUNet: Apache-2.0. NAFNet: MIT.**
> Unlike CodeFormer, none of these needs a feature-flag license gate. They can
> ship enabled in the ad-monetized product and the DR deployment without
> additional permission. (Keep the upstream LICENSE files in the cloned repos —
> Apache-2.0/MIT both require preserving the notices.)

---

## 0. Layout

New pieces only:

```
/opt/media-manipulator-ai/
├── venvs/
│   └── preclean/              # NEW — torch cu128 + opencv/numpy/scipy/einops
├── repos/
│   ├── FBCNN/                 # NEW — network definitions (models/network_fbcnn.py)
│   ├── SCUNet/                # NEW — network definitions (models/network_scunet.py)
│   └── NAFNet/                # NEW — vendored basicsr fork with NAFNet_arch
├── models/restore/
│   ├── fbcnn/fbcnn_color.pth                          # NEW (~290MB)
│   ├── scunet/scunet_color_real_psnr.pth              # NEW (~70MB)
│   └── nafnet/NAFNet-GoPro-width64.pth                # NEW (~270MB)
└── scripts/
    └── preclean_image.py      # NEW — deployed from scripts/server/
```

Disk: weights total well under 1GB; the venv (another torch) is ~7GB. Image
jobs use trivial scratch. A dedicated venv keeps the working `restore-sr` /
`restore-vsr-mm` / `face-restore` environments untouched — same isolation
principle as everything else in this stack.

## 1. venv `preclean`

```bash
cd /opt/media-manipulator-ai/venvs
sudo python3 -m venv preclean
sudo ./preclean/bin/pip install --upgrade pip wheel

# Blackwell (sm_120) → torch ≥ 2.7 cu128 wheels, nothing older.
sudo ./preclean/bin/pip install torch torchvision \
  --index-url https://download.pytorch.org/whl/cu128

# Shared runtime deps. einops is required by SCUNet's Swin-Conv blocks.
sudo ./preclean/bin/pip install opencv-python-headless numpy scipy einops
```

No basicsr from pip here — NAFNet vendors its own basicsr fork in-repo, and
the wrapper constructs the network classes directly (see §4), so the venv
stays small and the `functional_tensor` patch saga doesn't apply.

## 2. Repo clones

The wrapper imports network *definitions* from these repos via `sys.path` —
they are code dependencies, not just references:

```bash
cd /opt/media-manipulator-ai/repos
sudo git clone https://github.com/jiaxi-jiang/FBCNN.git
sudo git clone https://github.com/cszn/SCUNet.git
sudo git clone https://github.com/megvii-research/NAFNet.git
```

## 3. Model weights

```bash
sudo mkdir -p /opt/media-manipulator-ai/models/restore/{fbcnn,scunet,nafnet}

# FBCNN — color model, blind QF prediction with manual override.
# Hosted on the FBCNN repo's GitHub release; if the URL 404s, check the
# repo's Releases page / main_download_pretrained_models.py for the
# current home:
sudo wget -O /opt/media-manipulator-ai/models/restore/fbcnn/fbcnn_color.pth \
  https://github.com/jiaxi-jiang/FBCNN/releases/download/v1.0/fbcnn_color.pth

# SCUNet — PSNR-trained real-denoise weights. This is the forensic default:
# the GAN variant hallucinates texture; the PSNR variant is a pure filter.
# SCUNet's own download script pulls from the KAIR release assets:
sudo wget -O /opt/media-manipulator-ai/models/restore/scunet/scunet_color_real_psnr.pth \
  https://github.com/cszn/KAIR/releases/download/v1.0/scunet_color_real_psnr.pth
# (Optional, NOT used by the product: scunet_color_real_gan.pth from the same
#  release, if you want to eyeball the difference during evaluation.)

# NAFNet — GoPro motion-deblur weights (width64). megvii publishes these on
# Google Drive/Baidu (linked from the README's results table), so wget won't
# work. Two options:
#   a) On a machine with a browser: download NAFNet-GoPro-width64.pth from the
#      README link, then scp it to the server path below.
#   b) On the server with gdown:
sudo /opt/media-manipulator-ai/venvs/preclean/bin/pip install gdown
# copy the Drive link for "NAFNet-GoPro-width64" from
# https://github.com/megvii-research/NAFNet#results — then:
sudo /opt/media-manipulator-ai/venvs/preclean/bin/gdown --fuzzy '<drive-share-link>' \
  -O /opt/media-manipulator-ai/models/restore/nafnet/NAFNet-GoPro-width64.pth

# Verify all three landed with sane sizes:
ls -lh /opt/media-manipulator-ai/models/restore/{fbcnn,scunet,nafnet}/
```

## 4. Deploy the wrapper script

The repo copy under `scripts/server/` is the source of truth (created by the
AI Image Restoration implementation in `media_manipulator_api`). It prepends
the relevant repo to `sys.path` and constructs the network classes directly
(`models.network_fbcnn`, `models.network_scunet`,
`basicsr.models.archs.NAFNet_arch`) with tiled inference — it does not use the
repos' training/option machinery.

```bash
cd ~/media_manipulator_api   # wherever the API repo is checked out
sudo cp scripts/server/preclean_image.py /opt/media-manipulator-ai/scripts/preclean_image.py
sudo chmod +x /opt/media-manipulator-ai/scripts/preclean_image.py
```

Re-run this copy step any time the repo script changes.

## 5. systemd environment

Add to the API unit's `EnvironmentFile` (e.g. `/etc/media-manipulator/api.env`).
Defaults shown; only set what differs. Models dir, repos dir, and GPU indices
are shared with the other restoration features and are already set.

```ini
# --- Pre-clean stage (AI Image Restoration) ---
AI_PRECLEAN_PYTHON=/opt/media-manipulator-ai/venvs/preclean/bin/python
AI_PRECLEAN_SCRIPT=/opt/media-manipulator-ai/scripts/preclean_image.py
IMAGE_RESTORE_EST_SPM_FBCNN=3
IMAGE_RESTORE_EST_SPM_SCUNET=8
IMAGE_RESTORE_EST_SPM_NAFNET=6
IMAGE_RESTORE_VRAM_MIB_FBCNN=3000
IMAGE_RESTORE_VRAM_MIB_SCUNET=4000
IMAGE_RESTORE_VRAM_MIB_NAFNET=4000
```

Then:

```bash
sudo systemctl restart media-manipulator-api
journalctl -u media-manipulator-api -n 50 --no-pager   # confirm clean boot
```

## 6. Smoke tests

`--gpu 1` targets the RTX 5080 (the configured `AI_CUDA_GPU`); substitute
`--gpu 0` to exercise the 5060 Ti.

```bash
AI=/opt/media-manipulator-ai

# 1. torch sees CUDA with sm_120 support in the new venv:
$AI/venvs/preclean/bin/python - <<'PY'
import torch
print(torch.__version__, torch.version.cuda, torch.cuda.is_available())
PY
# Expect 2.7+ / 12.8 / True. "no kernel image" warnings → non-cu128 wheel, §7.

# 2. Build a realistically degraded test input — take any clean photo and
#    wreck it CCTV-style (downscale + heavy JPEG + noise):
magick /tmp/clean-source.jpg -resize 640x \
  -attenuate 0.4 +noise Gaussian -quality 12 /tmp/degraded.jpg

# 3. FBCNN — blind artifact removal (auto QF):
$AI/venvs/preclean/bin/python $AI/scripts/preclean_image.py \
  --model fbcnn --input /tmp/degraded.jpg --out-dir /tmp/pc-fbcnn \
  --models-dir $AI/models/restore --repos-dir $AI/repos --gpu 1
# Expect PROGRESS tile lines, then cleaned.png — blocking artifacts visibly gone.

# 4. FBCNN with a manual QF override (lower = assumes heavier compression):
$AI/venvs/preclean/bin/python $AI/scripts/preclean_image.py \
  --model fbcnn --input /tmp/degraded.jpg --out-dir /tmp/pc-fbcnn-qf10 \
  --fbcnn-qf 10 \
  --models-dir $AI/models/restore --repos-dir $AI/repos --gpu 1

# 5. SCUNet — denoise the FBCNN output (this is exactly the pipeline order):
$AI/venvs/preclean/bin/python $AI/scripts/preclean_image.py \
  --model scunet --input /tmp/pc-fbcnn/cleaned.png --out-dir /tmp/pc-scunet \
  --models-dir $AI/models/restore --repos-dir $AI/repos --gpu 1

# 6. NAFNet — deblur pass on the denoised result:
$AI/venvs/preclean/bin/python $AI/scripts/preclean_image.py \
  --model nafnet --input /tmp/pc-scunet/cleaned.png --out-dir /tmp/pc-nafnet \
  --models-dir $AI/models/restore --repos-dir $AI/repos --gpu 1

# 7. Dimensions must be unchanged at every step (pre-clean is 1x):
magick identify /tmp/degraded.jpg /tmp/pc-*/cleaned.png

# 8. API reports availability:
curl -s http://127.0.0.1:59997/api/image-restore/capabilities | python3 -m json.tool
# → fbcnn/scunet/nafnet all "available": true, no license-flag reasons.
```

## 7. Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `no kernel image is available` at model start | non-cu128 torch | reinstall torch/torchvision from the cu128 index in `preclean` |
| `ModuleNotFoundError: einops` | missing dep for SCUNet's Swin blocks | `pip install einops` in the `preclean` venv (§1) |
| `ModuleNotFoundError: models.network_fbcnn` (or scunet/NAFNet arch) | repo not cloned or not on `sys.path` | confirm the clone exists under `repos/` (§2); redeploy the script (§4) |
| state-dict key mismatch on load | wrong weights variant for the arch | re-download the exact files in §3 (fbcnn_color / scunet_color_real_psnr / NAFNet-GoPro-width64) |
| CUDA OOM on very large crops | tiles too big for the VRAM lease | the wrapper tiles at 512px/32px overlap — if a custom build changed that, restore it; or raise `IMAGE_RESTORE_VRAM_MIB_*` so the lease blocks instead of OOMing |
| output looks softer than input after SCUNet | normal — denoising trades micro-texture for noise removal | run a general upscaler after (that's the pipeline design); for evaluation only, try the optional GAN weights to compare |
| NAFNet "deblur" does nothing visible | input blur isn't motion blur (it's defocus/compression) | expected — GoPro weights target motion blur; lean on FBCNN/SCUNet + upscalers for the rest |
| capabilities shows a pre-clean model `available: false` | path missing | compare `AI_PRECLEAN_*` env values against §0; check weights file + repo dir exist |
