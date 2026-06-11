# Installing the AI Video Restoration toolchain

Operator guide for provisioning the six-model restoration pipeline on the
homelab GPU host ("creatv": Ryzen 9 9950X3D, 128GB DDR5, RTX 5080 16GB +
RTX 5060 Ti 8GB, Ubuntu 24.04). The Go API drives everything through two
Python venvs, five repo clones, one binary (already installed for image
upscaling), and two wrapper scripts. Nothing here is needed at build time —
the API feature-detects each model at request time (`stat` on the configured
paths) and reports availability through `GET /api/video-restore/capabilities`.

> **Blackwell warning:** both cards are sm_120 (Blackwell). Every PyTorch
> install below MUST be torch ≥ 2.7 with cu128 wheels, and mmcv MUST be built
> from source with `TORCH_CUDA_ARCH_LIST="12.0"`. Older cu121/cu124 wheels
> will import fine and then fail at kernel launch with
> `no kernel image is available for execution on the device`.

## 0. Layout

```
/opt/media-manipulator-ai/
├── bin/realesrgan-ncnn-vulkan/realesrgan-ncnn-vulkan   # already installed
├── venvs/
│   ├── restore-sr/        # torch cu128: SwinIR, HAT, RVRT, VRT
│   └── restore-vsr-mm/    # torch cu128 + mmcv/mmagic: BasicVSR++
├── repos/
│   ├── SwinIR/  ├── HAT/  ├── RVRT/  ├── VRT/  └── mmagic/
├── models/restore/
│   ├── swinir/003_realSR_BSRGAN_DFOWMFC_s64w8_SwinIR-L_x4_GAN.pth
│   ├── hat/Real_HAT_GAN_SRx4.pth
│   └── basicvsrpp/basicvsr_plusplus_c64n7_8x1_600k_reds4_20210217-db622b2f.pth
├── scripts/
│   ├── restore_frames.py  # deployed from scripts/server/restore_frames.py
│   └── restore_video.py   # deployed from scripts/server/restore_video.py
└── tmp/                   # scratch root the scripts may write under
```

Disk: keep ≥ 100GB free on the volume holding the API's `OUTPUT_DIR` — one
restoration job peaks at 10–30GB of PNGs before its work tree is reclaimed.

## 1. venv `restore-sr` (SwinIR, HAT, RVRT, VRT)

```bash
sudo mkdir -p /opt/media-manipulator-ai/{venvs,repos,tmp}
sudo mkdir -p /opt/media-manipulator-ai/models/restore/{swinir,hat,basicvsrpp}
cd /opt/media-manipulator-ai/venvs
sudo python3 -m venv restore-sr
sudo ./restore-sr/bin/pip install --upgrade pip wheel

# Blackwell (sm_120) → torch ≥ 2.7 cu128 wheels, nothing older.
sudo ./restore-sr/bin/pip install torch torchvision \
  --index-url https://download.pytorch.org/whl/cu128

sudo ./restore-sr/bin/pip install opencv-python-headless numpy einops timm \
  basicsr requests scipy
```

**The basicsr `functional_tensor` patch.** basicsr 1.4.2 imports a module
torchvision removed; HAT imports basicsr, so patch it once per venv:

```bash
sudo sed -i \
  's/from torchvision.transforms.functional_tensor import rgb_to_grayscale/from torchvision.transforms.functional import rgb_to_grayscale/' \
  /opt/media-manipulator-ai/venvs/restore-sr/lib/python3*/site-packages/basicsr/data/degradations.py
```

### Repo clones + weights

```bash
cd /opt/media-manipulator-ai/repos

# SwinIR — network definition is imported directly by restore_frames.py
sudo git clone https://github.com/JingyunLiang/SwinIR.git
sudo wget -O /opt/media-manipulator-ai/models/restore/swinir/003_realSR_BSRGAN_DFOWMFC_s64w8_SwinIR-L_x4_GAN.pth \
  https://github.com/JingyunLiang/SwinIR/releases/download/v0.0/003_realSR_BSRGAN_DFOWMFC_s64w8_SwinIR-L_x4_GAN.pth

# HAT — installed editable so `import hat` works in the venv
sudo git clone https://github.com/XPixelGroup/HAT.git
sudo /opt/media-manipulator-ai/venvs/restore-sr/bin/pip install -e ./HAT --no-build-isolation
# Real_HAT_GAN_SRx4.pth: download from the official links in the HAT README
# (Google Drive / OneDrive — no direct stable URL), then:
#   sudo mv Real_HAT_GAN_SRx4.pth /opt/media-manipulator-ai/models/restore/hat/

# RVRT + VRT — driven via their main_test_*.py by restore_video.py
sudo git clone https://github.com/JingyunLiang/RVRT.git
sudo git clone https://github.com/JingyunLiang/VRT.git
sudo /opt/media-manipulator-ai/venvs/restore-sr/bin/pip install -r RVRT/requirements.txt 2>/dev/null || true
# Pre-place weights so first-run never downloads (the test scripts otherwise
# auto-download into model_zoo/):
sudo mkdir -p RVRT/model_zoo VRT/model_zoo
sudo wget -P RVRT/model_zoo \
  https://github.com/JingyunLiang/RVRT/releases/download/v0.0/001_RVRT_videosr_bi_REDS_30frames.pth
sudo wget -P VRT/model_zoo \
  https://github.com/JingyunLiang/VRT/releases/download/v0.0/001_VRT_videosr_bi_REDS_6frames.pth
# NOTE: check the default --model_path inside main_test_rvrt.py /
# main_test_vrt.py for your clone — some revisions expect a model_zoo/rvrt/
# or model_zoo/vrt/ subfolder; place the .pth wherever that default points.
```

## 2. venv `restore-vsr-mm` (BasicVSR++ via mmagic)

mmcv has no cu128/sm_120 binary wheels — build it from source against this
venv's torch. This takes a while (~20–40 min) and needs CUDA toolkit headers
(`sudo apt install cuda-toolkit-12-8` or matching nvcc).

```bash
cd /opt/media-manipulator-ai/venvs
sudo python3 -m venv restore-vsr-mm
sudo ./restore-vsr-mm/bin/pip install --upgrade pip wheel
sudo ./restore-vsr-mm/bin/pip install torch torchvision \
  --index-url https://download.pytorch.org/whl/cu128
sudo ./restore-vsr-mm/bin/pip install opencv-python-headless numpy mmengine

# mmcv from source with Blackwell arch
cd /opt/media-manipulator-ai/repos
sudo git clone https://github.com/open-mmlab/mmcv.git -b v2.2.0
cd mmcv
sudo MMCV_WITH_OPS=1 TORCH_CUDA_ARCH_LIST="12.0" FORCE_CUDA=1 \
  /opt/media-manipulator-ai/venvs/restore-vsr-mm/bin/pip install -e . --no-build-isolation

# mmagic (provides the BasicVSR++ config + inference API)
cd /opt/media-manipulator-ai/repos
sudo git clone https://github.com/open-mmlab/mmagic.git
sudo /opt/media-manipulator-ai/venvs/restore-vsr-mm/bin/pip install -e ./mmagic --no-build-isolation
# basicsr patch again if mmagic's dep tree pulled basicsr in:
sudo sed -i \
  's/from torchvision.transforms.functional_tensor import rgb_to_grayscale/from torchvision.transforms.functional import rgb_to_grayscale/' \
  /opt/media-manipulator-ai/venvs/restore-vsr-mm/lib/python3*/site-packages/basicsr/data/degradations.py 2>/dev/null || true

# BasicVSR++ weights (official OpenMMLab mirror)
sudo wget -O /opt/media-manipulator-ai/models/restore/basicvsrpp/basicvsr_plusplus_c64n7_8x1_600k_reds4_20210217-db622b2f.pth \
  https://download.openmmlab.com/mmediting/restorers/basicvsr_plusplus/basicvsr_plusplus_c64n7_8x1_600k_reds4_20210217-db622b2f.pth
```

## 3. Deploy the wrapper scripts

The repo copies under `scripts/server/` are the source of truth:

```bash
cd ~/media_manipulator_api   # wherever the API repo is checked out
sudo cp scripts/server/restore_frames.py /opt/media-manipulator-ai/scripts/restore_frames.py
sudo cp scripts/server/restore_video.py  /opt/media-manipulator-ai/scripts/restore_video.py
sudo chmod +x /opt/media-manipulator-ai/scripts/restore_frames.py \
              /opt/media-manipulator-ai/scripts/restore_video.py
```

## 4. systemd environment

systemd does **not** inherit the repo's `.env` files. Add the restoration
block to the API unit's `EnvironmentFile` (e.g.
`/etc/media-manipulator/api.env`) — defaults shown; only set what differs:

```ini
# --- AI Video Restoration ------------------------------------------------
RESTORE_ENABLED=true
RESTORE_BASICVSRPP_ENABLED=true
RESTORE_MAX_CLIP_SECONDS=15
RESTORE_RECOMMENDED_CLIP_SECONDS=10
RESTORE_MAX_FRAMES=450
RESTORE_MAX_SOURCE_WIDTH=1920
RESTORE_MAX_SOURCE_HEIGHT=1080
RESTORE_MAX_CONCURRENT_JOBS=1
RESTORE_MODEL_TIMEOUT_SECONDS=4500
RESTORE_RESULT_PRESIGN_TTL_SECONDS=21600
RESTORE_RATE_LIMIT_PER_SESSION_PER_HOUR=2
RESTORE_RATE_LIMIT_PER_IP_PER_HOUR=4
# CUDA index of the RTX 5080 16GB (check `nvidia-smi -L`), Vulkan index for
# realesrgan-ncnn (check `realesrgan-ncnn-vulkan` startup output):
AI_RESTORE_CUDA_GPU=0
AI_RESTORE_VULKAN_GPU=1
AI_RESTORE_PYTHON=/opt/media-manipulator-ai/venvs/restore-sr/bin/python
AI_RESTORE_MM_PYTHON=/opt/media-manipulator-ai/venvs/restore-vsr-mm/bin/python
AI_RESTORE_FRAMES_SCRIPT=/opt/media-manipulator-ai/scripts/restore_frames.py
AI_RESTORE_VIDEO_SCRIPT=/opt/media-manipulator-ai/scripts/restore_video.py
AI_RESTORE_MODELS_DIR=/opt/media-manipulator-ai/models/restore
AI_RESTORE_REPOS_DIR=/opt/media-manipulator-ai/repos
# Optional per-model tuning after real runs:
#RESTORE_EST_SPF_REALESRGAN=0.8
#RESTORE_EST_SPF_SWINIR=3.5
#RESTORE_EST_SPF_HAT=5.0
#RESTORE_EST_SPF_BASICVSRPP=1.0
#RESTORE_EST_SPF_RVRT=3.0
#RESTORE_EST_SPF_VRT=7.0
#RESTORE_VRAM_MIB_REALESRGAN=3000
#RESTORE_VRAM_MIB_SWINIR=9000
#RESTORE_VRAM_MIB_HAT=10000
#RESTORE_VRAM_MIB_BASICVSRPP=11000
#RESTORE_VRAM_MIB_RVRT=12000
#RESTORE_VRAM_MIB_VRT=14000
```

Then `sudo systemctl restart media-manipulator-api`.

## 5. Smoke tests

Run each as the API's service user. Every one must pass before flipping
traffic to the feature.

```bash
AI=/opt/media-manipulator-ai

# 1. Torch sees the Blackwell card in both venvs
$AI/venvs/restore-sr/bin/python -c \
  "import torch; print(torch.__version__, torch.version.cuda, torch.cuda.get_device_name(0))"
$AI/venvs/restore-vsr-mm/bin/python -c \
  "import torch, mmcv, mmagic; print(torch.cuda.is_available(), mmcv.__version__)"

# 2. Build a 5-frame test clip
mkdir -p /tmp/restore-smoke/frames && cd /tmp/restore-smoke
ffmpeg -y -f lavfi -i testsrc=duration=1:size=320x240:rate=5 -vsync 0 frames/%06d.png

# 3. Per-frame models (expect PROGRESS 1/5 … 5/5, PNGs in out/<m>/frames)
for m in swinir hat; do
  $AI/venvs/restore-sr/bin/python $AI/scripts/restore_frames.py \
    --model $m --frames-dir frames --out-dir out/$m --scale 4 \
    --models-dir $AI/models/restore --repos-dir $AI/repos --gpu 0
done

# 4. Video models
$AI/venvs/restore-vsr-mm/bin/python $AI/scripts/restore_video.py \
  --model basicvsrpp --frames-dir frames --out-dir out/basicvsrpp \
  --models-dir $AI/models/restore --repos-dir $AI/repos --max-seq-len 5
for m in rvrt vrt; do
  $AI/venvs/restore-sr/bin/python $AI/scripts/restore_video.py \
    --model $m --frames-dir frames --out-dir out/$m \
    --models-dir $AI/models/restore --repos-dir $AI/repos
done

# 5. Real-ESRGAN binary (already installed for image upscaling)
$AI/bin/realesrgan-ncnn-vulkan/realesrgan-ncnn-vulkan \
  -i frames -o /tmp/restore-smoke/re-out -n realesrgan-x4plus -s 4 -f png

# 6. API reports availability
curl -s http://127.0.0.1:59997/api/video-restore/capabilities | python3 -m json.tool
# → every model should show "available": true
```

## 6. Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `no kernel image is available` at model start | non-cu128 torch or mmcv built without sm_120 | reinstall torch cu128; rebuild mmcv with `TORCH_CUDA_ARCH_LIST="12.0"` |
| `ImportError: functional_tensor` | unpatched basicsr | re-run the sed patch (section 1) |
| capabilities shows `available: false` | path missing for that model | compare the `AI_RESTORE_*` env values against the layout in section 0 |
| VRT/RVRT re-download weights every job | weights not where the clone's `--model_path` default points | check `main_test_*.py` defaults; move the `.pth` accordingly |
| CUDA OOM on VRT at 1080p | 16GB is tight for VRT's attention windows | lower the tile via the Go config (future knob) or restrict clips to 720p |
| jobs stuck in `queued` | another restore job holds the permit | expected — `RESTORE_MAX_CONCURRENT_JOBS=1`; raise only with VRAM headroom |
