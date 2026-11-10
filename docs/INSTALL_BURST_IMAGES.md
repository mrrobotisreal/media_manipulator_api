# Burst worker images (MM126)

The GPU burst workers (`docs/design/GPU_BURST_EC2.md`, RUNBOOK 4.19) run the same commands as the host, at the same absolute paths. Each worker family is one AMI built from a copy of the host's own runtime, so a remote run is the same argv as a local one.

| Family | Tools | Host paths (see `internal/burst/manifest/families/<family>.paths`) | Raw size |
|---|---|---|---|
| `image` | rembg, Real-ESRGAN, EasyOCR redact, LaMa | venvs `bg-remove`, `vision-privacy`, `inpaint`; `bin/realesrgan-ncnn-vulkan`; `models/rembg`, `models/yunet`; the three env scripts; Vulkan ICD; 3 scripts; `~/.EasyOCR/model`, `big-lama.pt` | 17 GB |
| `audio` | DeepFilterNet, Demucs, faster-whisper | venvs `audio-clean`, `audio-separate`; `/opt/creatv/whisper-ct2`; HF cache for faster-whisper `medium` and `large-v3`; DeepFilterNet3 checkpoint; Demucs `htdemucs` weights | 19 GB |
| `video` | RIFE | `bin/rife-ncnn-vulkan` (all models, 0.45 GB); `frame_interpolate_rife.py` | 0.5 GB |
| `restore` | Real-ESRGAN, SwinIR, HAT, BasicVSR++, RVRT, VRT, GFPGAN, CodeFormer, FBCNN, SCUNet, NAFNet | venvs `restore-sr`, `restore-vsr-mm`, `face-restore`, `preclean`; `repos/{CodeFormer,FBCNN,HAT,NAFNet,RVRT,SCUNet,SwinIR,VRT}`; `models/restore` (3.2 GB); 4 scripts | 35 GB |

`ollama` and `voices` are MM130/MM131. The generative experiments (`gen-diffbir`, `gen-supir`, `repos/DiffBIR`), `document-ocr`, `paddleocr-vl`, `photo` and `voices` venvs are not in any family.

## When to rebuild

After **any** change to a path named in a family's `.paths` list: a pip upgrade in one of its venvs, a new model file, a changed wrapper script in `/opt/media-manipulator-ai/scripts`. `burst-ctl status` shows the family as `stale: true`, and the API logs `burst: image <family> is stale vs host (rebuild with scripts/burst/build-image.sh <family>)` once a day. A stale image still runs unless `BURST_REFUSE_STALE_IMAGES=true`. Also rebuild when the DLAMI has a security update you want. If you add a new path, add it to the family's `.paths` file first, run `go test ./internal/burst/...` (the exclusion test), and rebuild.

## What runs where

Everything below runs **on the server** (`ssh mwintrow@100.74.11.69`) in `~/media-manipulator/api`. Nothing runs on the MacBook.

1. **Export** (`scripts/burst/export-runtime.sh <family> --date YYYY-MM-DD`). Run inside `tmux`. Read-only on every source. Builds `burst-ctl`, prints the paths, the size and the upload time at several upstream speeds, asks `y/N`, then `tar | zstd -3` to `/var/tmp/mm-burst-export/` and uploads to `s3://media-manipulator/burst-images/<date>/<family>.tar.zst` plus `<family>.manifest.json` with the controller key read from `.env` (never printed). The local files are deleted at the end. Use `--dry-run` to see the plan only.
2. **Build** (`scripts/burst/build-image.sh <family> <date> [--instance-type g7.2xlarge]`). Needs the `mm-burst-builder` AWS profile (`~/.aws/credentials`), not the controller key. It resolves the newest Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) from the public SSM parameter `/aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-24.04/latest/ami-id`, prints a cost estimate, asks `y/N`, uploads a small bundle (these scripts + a `burst-ctl` build) to `s3://media-manipulator/burst/build/<date>/`, launches one on-demand g6.xlarge builder (tags `mm-burst=1`, `mm-burst-build=<family>`, terminate on shutdown, `shutdown -h +180`), and polls the status files the builder writes under `burst/build/<date>/`. On an all-pass smoke result it creates the AMI (`--no-reboot`; the builder cleaned itself first), tags the AMI and its snapshots, terminates the builder (always, also on failure) and prints `BURST_AMIS entry: <family>:x86_64=ami-...`.
3. **Prune** (`KEEP_AMIS="ami-a,ami-b" scripts/burst/prune-images.sh [--keep 2] [--yes]`). Dry run by default; keeps the newest 2 per family and anything in `KEEP_AMIS`.

The builder writes its status and smoke JSON under `burst/build/<date>/` because the `mm-burst-worker` role it runs as may write only under `burst/`. The 1-day lifecycle rule on `burst/` removes them; `build-image.sh` reads them immediately.

## What the builder does (`provision-<family>.sh`, as root on the builder)

1. apt: ImageMagick 6 (`convert`/`identify`, never `magick`), exiftool, vulkan-tools, jq, zstd, AWS CLI v2 if missing.
2. ffmpeg 8.1.1 from `ppa:ubuntuhandbook1/ffmpeg8` (the host's own source; the build checks `h264_nvenc`). tailscale from `pkgs.tailscale.com`; `tailscale up` is left to each run's user-data.
3. Downloads the tarball, verifies the sha256, unpacks to `/` with `--numeric-owner`, then checks `burst-ctl manifest <family> --from-dest` equals the exported runtime manifest (so a bad copy fails before anything else).
4. Vulkan: if `libEGL_nvidia.so.0` is missing it installs `libnvidia-gl-<driver major>-server`, and copies the system ICD's `api_version` into `/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json` (the path is hard-coded in Go; `library_path` stays `libEGL_nvidia.so.0`).
5. `restore` only: rebuilds **mmcv v2.1.0** (commit `57c4e25e06e2d4f8a9357c84bcd24089a284dc88`, the version the host runs; `INSTALL_VIDEO_RESTORATION.md` still says v2.2.0) with `MMCV_WITH_OPS=1 FORCE_CUDA=1 TORCH_CUDA_ARCH_LIST="8.9;12.0"` against the venv's torch 2.11 cu128 and CUDA 12.8, then checks the `_ext` has `sm_89` and `sm_120`. Pre-builds RVRT's torch JIT extension `deform_attn` for both. Links `repos/mmagic` to the pip package's packaged configs (the host has no `repos/mmagic`; see below).
6. Installs `burst-worker.service` (HOME `/opt/mm-burst-home`, `HF_HUB_OFFLINE=1`, `TORCH_CUDA_ARCH_LIST="8.9;12.0"`, `CUDA_HOME=/usr/local/cuda-12.8`; not enabled), writes `/etc/mm-burst/image.json`.
7. Runs `smoke/<family>.sh` (below), uploads the JSON.
8. Cleans: `cloud-init clean --logs`, truncated machine id, removed ssh host keys, shell history, Tailscale state, `/tmp`, `/opt/media-manipulator-ai/tmp/*`.

### HOME model caches
Three tools keep models in the operator's home on the host (`~/.EasyOCR/model`, `~/.cache/torch/hub/checkpoints/{big-lama.pt,955717e8-8726e21a.th}`, `~/.cache/DeepFilterNet/DeepFilterNet3`). The export takes **only those files** and `tar --transform`s them to `/opt/mm-burst-home/...`; the worker unit sets `HOME=/opt/mm-burst-home`. Nothing else under `/home` is ever read (a test enforces it).

## Smoke tests (`scripts/burst/smoke/<family>.sh`)

Each runs the workload's real command (the same argv as the Go code) on a committed tiny fixture in `smoke/fixtures/` (256 px PNG, 3 s WAV, 2 s 360p MP4, a 32-frame 64 px PNG set), with the card pinned. Each case asserts exit 0, a valid output (`identify`/`ffprobe`/`grep`), and that `nvidia-smi` showed at least 40 MiB more GPU memory in use while it ran (a silent CPU fallback fails). `image` also asserts onnxruntime lists `CUDAExecutionProvider`. The JSON records seconds per case: MM128's first data points.
Run one on the host for comparison: `SMOKE_CARD=1 SMOKE_OUT=/tmp/s.json scripts/burst/smoke/image.sh` (it uses the GPU for a few seconds; do it when idle).

Deviations from the ticket's list, from the S0 inventory: the host has no faster-whisper `small` (only `medium`, the API default, and `large-v3`), so the audio test uses `medium`; the face-restore cases use a face-less fixture and assert the script's own `No faces were detected` error (the model still loads on the GPU); `basicvsrpp` runs 4 frames, `rvrt` and `vrt` run 32 (their temporal windows).

## Troubleshooting

- **`no export for '<family>'`**: run the export first, with the same `--date`.
- **Driver mismatch / `no kernel image is available`**: the DLAMI's driver must be at least what the cu128 wheels need (the host runs 590.48.01; the DLAMI ships 595.x). A `no kernel image` error in `restore` means the mmcv step did not run for both architectures; check `$MM_PROVISION_LOG` in the failure log.
- **Vulkan ICD** (`vkCreateInstance` failed, `realesrgan`/`rife` smoke fail): the builder log shows the `libnvidia-gl` step. Check `ldconfig -p | grep libEGL_nvidia` on a builder and `/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json`.
- **mmcv build failure**: `nvcc` must be CUDA 12.8 (`/usr/local/cuda-12.8`). If the DLAMI dropped 12.8, set `CUDA_HOME_DIR` in `provision-common.sh` and keep it equal to the torch wheel's CUDA minor version.
- **Root device**: the DLAMI uses `/dev/sda1`, the default. Set `BURST_ROOT_DEVICE` only if a build result box says to.
- **Root disk larger than the profile root**: the build refuses; raise `RootGiB` in `internal/burst/profiles.go` and in `scripts/burst/lib.sh` (a test keeps the two equal).
- **A builder left running**: it terminates itself after 3 h; the API reaper spares `mm-burst-build` instances for 4 h and terminates older ones. Console: EC2 → Instances → tag `mm-burst-build`.
- **Stale after a rebuild**: the AMI id in `BURST_AMIS` is still the old one; update `.env` and restart the API.

## Cost

A build is about $0.50 (video) to $2 (restore) of g6.xlarge time. Snapshot storage is about $0.05 per GB-month on used blocks (an upper bound is the provisioned root size: 100 + 80 + 100 + 150 GiB = $21.50 a month). Prune keeps the newest two per family.

## Open decisions logged by MM126

- One AMI per family serves both Ada (L4) and Blackwell. The G7 smoke run of `restore` decides whether a second AMI is needed.
- `INSTALL_VIDEO_RESTORATION.md` says mmcv v2.2.0; the host runs 2.1.0 and the image rebuilds 2.1.0.
- Whisper `small` and `large-v3-turbo` (`/opt/creatv/models`, owned by `syncd`) are not in the audio family.
