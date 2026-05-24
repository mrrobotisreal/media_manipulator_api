# Server setup: AI video frame interpolation (rife-ncnn-vulkan)

This document covers the server install + configuration required for Media
Manipulator's AI Frame Interpolation feature. The feature increases a video's
frame rate (e.g. 30fps → 60fps, 60fps → 120fps) using rife-ncnn-vulkan and a
small Python helper script.

The runtime model + binary live under `/opt/media-manipulator-ai/`. The Go API
shells out to a Python script that orchestrates ffmpeg → rife → ffmpeg. The
reference copy of the script is checked in at
`scripts/server/frame_interpolate_rife.py`.

## Install RIFE ncnn Vulkan

```bash
# ============================================================
# Install RIFE ncnn Vulkan for Media Manipulator
# ============================================================

sudo apt update
sudo apt install -y ffmpeg ffmpegthumbnailer wget unzip vulkan-tools jq python3 python3-venv

mkdir -p /opt/media-manipulator-ai/bin/rife-ncnn-vulkan
cd /opt/media-manipulator-ai/bin/rife-ncnn-vulkan

# Download the Linux release from:
# https://github.com/nihui/rife-ncnn-vulkan/releases
#
# The release URL below is the known public Linux release package.
# If this URL ever 404s, open the releases page and copy the Linux/Ubuntu zip URL.
wget -O rife-ncnn-vulkan-ubuntu.zip \
  https://github.com/nihui/rife-ncnn-vulkan/releases/download/20221029/rife-ncnn-vulkan-20221029-ubuntu.zip

unzip -o rife-ncnn-vulkan-ubuntu.zip

# If the zip extracts into a nested directory, keep it nested but locate the binary:
find /opt/media-manipulator-ai/bin/rife-ncnn-vulkan -type f -name 'rife-ncnn-vulkan' -exec chmod +x {} \;

# Print discovered binary path:
find /opt/media-manipulator-ai/bin/rife-ncnn-vulkan -type f -name 'rife-ncnn-vulkan' -print

# Recommended final symlink:
sudo ln -sf "$(find /opt/media-manipulator-ai/bin/rife-ncnn-vulkan -type f -name 'rife-ncnn-vulkan' | head -n 1)" \
  /opt/media-manipulator-ai/bin/rife-ncnn-vulkan/rife-ncnn-vulkan

# Verify:
source /opt/media-manipulator-ai/env/vulkan-nvidia.sh
/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/rife-ncnn-vulkan -h
```

## Create runtime script + temp directories

```bash
mkdir -p /opt/media-manipulator-ai/scripts
mkdir -p /opt/media-manipulator-ai/tmp
```

## Deploy the helper script after a repo merge

```bash
sudo cp scripts/server/frame_interpolate_rife.py \
  /opt/media-manipulator-ai/scripts/frame_interpolate_rife.py

sudo chmod +x /opt/media-manipulator-ai/scripts/frame_interpolate_rife.py
```

## Test the script manually

```bash
source /opt/media-manipulator-ai/env/vulkan-nvidia.sh

/opt/media-manipulator-ai/scripts/frame_interpolate_rife.py \
  --input /path/to/input.mp4 \
  --output /tmp/interpolated_60fps.mp4 \
  --target-fps 60 \
  --rife-bin /opt/media-manipulator-ai/bin/rife-ncnn-vulkan/rife-ncnn-vulkan \
  --rife-model /opt/media-manipulator-ai/bin/rife-ncnn-vulkan/models/rife-v4.6 \
  --gpu 1 \
  --quality medium \
  --max-height 720 \
  --max-duration-seconds 120

ffprobe -hide_banner /tmp/interpolated_60fps.mp4
```

## API .env

Add these to the API's `.env`:

```dotenv
# ============================================================
# AI Video Frame Interpolation
# ============================================================

AI_FRAME_INTERPOLATION_ENABLED=true
AI_FRAME_INTERPOLATION_SCRIPT=/opt/media-manipulator-ai/scripts/frame_interpolate_rife.py
AI_RIFE_BIN=/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/rife-ncnn-vulkan
AI_RIFE_MODEL=/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/models/rife-v4.6
AI_RIFE_GPU=1
AI_RIFE_THREADS=1:2:2
AI_FRAME_INTERPOLATION_MAX_DURATION_SECONDS=120
AI_FRAME_INTERPOLATION_MAX_HEIGHT=720
AI_FRAME_INTERPOLATION_TEMP_ROOT=/opt/media-manipulator-ai/tmp
```

If the model path differs after unzip, update `AI_RIFE_MODEL` to whichever
model directory exists, for example:

- `/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/models/rife-v4.6`
- `/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/models/rife-v4`
- `/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/models/rife-v2.3`

The Go side accepts `rife-v4.6`, `rife-v4`, and `rife-v2.3` as the
`frameInterpolation.model` field. Non-default models are looked up relative to
the directory containing `AI_RIFE_MODEL`, so all three model folders must be
siblings under the same parent.

## How the Go API calls the script

The converter routes video jobs to `AIService.InterpolateFrames` when
`options.ai.enabled = true && options.ai.operation = "frame_interpolation"`.
It exec's the script directly (the shebang + executable bit handle the
interpreter) with:

```
<AI_FRAME_INTERPOLATION_SCRIPT>
  --input <inputPath>
  --output <outputPath>           # always .mp4 for v1
  --target-fps <48|60|120>
  --rife-bin   <AI_RIFE_BIN>
  --rife-model <resolved model dir>
  --gpu        <AI_RIFE_GPU>
  --threads    <AI_RIFE_THREADS>
  --quality    <low|medium|high>
  --max-height <144..1080>
  --max-duration-seconds <AI_FRAME_INTERPOLATION_MAX_DURATION_SECONDS>
  --temp-root  <AI_FRAME_INTERPOLATION_TEMP_ROOT>
```

Vulkan ICD env vars from `vulkan-nvidia.sh` are forwarded explicitly:

```
VK_ICD_FILENAMES=/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json
VK_DRIVER_FILES=/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json
VK_LOADER_LAYERS_DISABLE=*
CUDA_DEVICE_ORDER=PCI_BUS_ID
```

## Known limitations (v1)

- Output is always MP4 (H.264 + AAC). GIF output is disallowed when AI frame
  interpolation is selected.
- Max duration and max processing height are bounded by the env vars above so
  a single long clip can't tie up the GPU indefinitely.
- AI interpolation owns the output FPS — the regular temporal frame rate
  override is rejected when AI frame interpolation is on.
- v1 does not combine pre-trim with AI interpolation; trim the source first.
