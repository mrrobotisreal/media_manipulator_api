#!/usr/bin/env bash
# MM126 shared provisioning steps. Sourced by provision-<family>.sh, which run
# ON THE BUILDER INSTANCE (a DLAMI Ubuntu 24.04 box launched by build-image.sh)
# as root. With --dry-run (or MM_DRY=1) every step only prints its plan, so the
# whole thing can be read on a laptop.
#
# Inputs (environment, set by the builder's user-data):
#   MM_FAMILY  image|audio|video|restore
#   MM_DATE    the export date (s3://$MM_BUCKET/burst-images/$MM_DATE/$MM_FAMILY.tar.zst)
#   MM_BUCKET, MM_REGION
#   MM_BUILD_PREFIX   burst/build/$MM_DATE   (status + smoke json are written here;
#                     the worker role may write only under burst/)
#   MM_MODE    build (default) | smoke   (smoke = an already-built AMI, test only)
# shellcheck disable=SC2034,SC2030,SC2031
set -euo pipefail

PHERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$PHERE/lib.sh"

MM_DRY="${MM_DRY:-0}"
MM_MODE="${MM_MODE:-build}"
MM_DATE="${MM_DATE:-1970-01-01}"
MM_BUILD_PREFIX="${MM_BUILD_PREFIX:-burst/build/$MM_DATE}"
LOGFILE="${MM_PROVISION_LOG:-/var/log/mm-burst-provision.log}"
# The torch cu128 build used by every venv. nvcc for source builds comes from here.
CUDA_HOME_DIR="${CUDA_HOME_DIR:-/usr/local/cuda-12.8}"
# Both target architectures: Ada (L4, g6/g6e) and Blackwell (g7). 8.6 is added
# only if MM128 keeps a g5/A10G candidate.
CUDA_ARCHES="${CUDA_ARCHES:-8.9;12.0}"
# mmcv: the exact tag the host runs (pip list says 2.1.0; INSTALL_VIDEO_RESTORATION.md says 2.2.0 and is stale).
MMCV_TAG="${MMCV_TAG:-v2.1.0}"
MMCV_COMMIT="${MMCV_COMMIT:-57c4e25e06e2d4f8a9357c84bcd24089a284dc88}"

dry() { [ "$MM_DRY" = "1" ]; }

# plan "text": prints the step; returns 0 (true) when dry so callers can `plan x && return 0`.
plan() {
  printf 'PLAN  %s\n' "$*"
  dry
}

status() {
  # status "message": local log + a tiny S3 object the builder script polls.
  local line
  line="$(date -u +%H:%M:%S) $*"
  if dry; then printf 'STATUS %s\n' "$line"; return 0; fi
  printf '%s\n' "$line" | tee -a "$LOGFILE" >&2
  printf '%s\n' "$line" | aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.status" --only-show-errors || true
}

fail_build() {
  status "FAILED: $*"
  if ! dry; then
    printf '%s\n' "$*" | aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.failed" --only-show-errors || true
    tail -n 200 "$LOGFILE" 2>/dev/null | aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.log" --only-show-errors || true
  fi
  exit 1
}

step_base_packages() {
  status "apt: base packages"
  plan "apt-get install: jq ca-certificates curl zstd unzip imagemagick (6.x: convert/identify, never magick) libimage-exiftool-perl vulkan-tools libvulkan1 libgomp1 libgl1 libglib2.0-0t64 python3-venv" && return 0
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq jq ca-certificates curl zstd unzip imagemagick libimage-exiftool-perl \
    vulkan-tools libvulkan1 libgomp1 libgl1 libglib2.0-0t64 python3-venv software-properties-common gnupg
  if ! command -v aws >/dev/null 2>&1; then
    curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscli.zip
    unzip -q /tmp/awscli.zip -d /tmp/awscli && /tmp/awscli/aws/install && rm -rf /tmp/awscli /tmp/awscli.zip
  fi
}

step_ffmpeg() {
  # Host: 10:8.1.1-0build1~ubuntu24.04 from ppa:ubuntuhandbook1/ffmpeg8, NVENC build (nonfree, fdk-aac).
  status "apt: ffmpeg 8.1.1 (host build) from ppa:ubuntuhandbook1/ffmpeg8"
  plan "add-apt-repository ppa:ubuntuhandbook1/ffmpeg8; apt-get install ffmpeg=10:8.1.1-0build1~ubuntu24.04 (falls back to the PPA's newest, recorded in image.json)" && return 0
  add-apt-repository -y ppa:ubuntuhandbook1/ffmpeg8 >/dev/null
  apt-get update -qq
  if ! apt-get install -y -qq "ffmpeg=10:8.1.1-0build1~ubuntu24.04"; then
    status "WARNING: ffmpeg 8.1.1 is no longer in the PPA; installing the newest"
    apt-get install -y -qq ffmpeg
  fi
  ffmpeg -hide_banner -encoders 2>/dev/null | grep -q h264_nvenc || fail_build "ffmpeg has no h264_nvenc"
}

step_tailscale() {
  status "apt: tailscale"
  plan "install tailscale from pkgs.tailscale.com (noble); do NOT run 'tailscale up' (the run's user-data does)" && return 0
  if ! command -v tailscale >/dev/null 2>&1; then
    curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg -o /usr/share/keyrings/tailscale-archive-keyring.gpg
    curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list -o /etc/apt/sources.list.d/tailscale.list
    apt-get update -qq
    apt-get install -y -qq tailscale
  fi
  systemctl enable tailscaled >/dev/null 2>&1 || true
}

step_vulkan_driver_bits() {
  # The Go code pins VK_ICD_FILENAMES to /opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json, which
  # loads libEGL_nvidia.so.0. The OSS-driver DLAMI may ship only the compute half of the driver.
  status "vulkan: NVIDIA GL/EGL userspace libraries"
  plan "if libEGL_nvidia.so.0 is not in ldconfig: apt-get install libnvidia-gl-<driver major>-server (fallback libnvidia-gl-<major>); copy api_version from the system nvidia_icd.json into $MM_ROOT/vulkan/nvidia_icd_egl.json if that file exists (library_path stays libEGL_nvidia.so.0)" && return 0
  if ! ldconfig -p | grep -q 'libEGL_nvidia.so.0'; then
    local major
    major="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1 | cut -d. -f1)"
    apt-get install -y -qq "libnvidia-gl-${major}-server" || apt-get install -y -qq "libnvidia-gl-${major}" \
      || fail_build "no libnvidia-gl package for driver $major (Vulkan ICD would fail)"
    ldconfig
  fi
  local sys=/usr/share/vulkan/icd.d/nvidia_icd.json icd="$MM_ROOT/vulkan/nvidia_icd_egl.json"
  if [ -f "$sys" ] && [ -f "$icd" ]; then
    local v
    v="$(jq -r '.ICD.api_version // empty' "$sys")"
    if [ -n "$v" ]; then
      jq --arg v "$v" '.ICD.api_version = $v' "$icd" > "$icd.new" && mv "$icd.new" "$icd"
    fi
  fi
}

step_unpack() {
  status "s3: download and unpack $MM_FAMILY.tar.zst"
  plan "aws s3 cp s3://$MM_BUCKET/burst-images/$MM_DATE/$MM_FAMILY.tar.zst -> instance store; sha256 vs $MM_FAMILY.manifest.json; zstd -d | tar -x -C / --numeric-owner; then '${MM_CTL:-burst-ctl} manifest $MM_FAMILY --from-dest' must equal the exported runtime manifest (before any rebuild)" && return 0
  local dir=/opt/dlami/nvme
  mountpoint -q "$dir" || dir=/var/tmp
  mkdir -p "$dir/mm-import"
  aws s3 cp "s3://$MM_BUCKET/burst-images/$MM_DATE/$MM_FAMILY.manifest.json" "$dir/mm-import/manifest.json" --only-show-errors
  aws s3 cp "s3://$MM_BUCKET/burst-images/$MM_DATE/$MM_FAMILY.tar.zst" "$dir/mm-import/t.tar.zst" --only-show-errors
  local want got
  want="$(jq -r .tarball.sha256 "$dir/mm-import/manifest.json")"
  got="$(sha256sum "$dir/mm-import/t.tar.zst" | cut -d' ' -f1)"
  [ "$want" = "$got" ] || fail_build "tarball sha256 mismatch (want $want got $got)"
  zstd -d -q --stdout "$dir/mm-import/t.tar.zst" | tar -x -C / --numeric-owner --same-permissions
  rm -f "$dir/mm-import/t.tar.zst"
  local exp have
  exp="$(jq -r .runtimeManifest "$dir/mm-import/manifest.json")"
  have="$("${MM_CTL:-burst-ctl}" manifest "$MM_FAMILY" --from-dest)"
  [ "$exp" = "$have" ] || fail_build "unpacked runtime differs from the export (want $exp got $have)"
  cp "$dir/mm-import/manifest.json" /etc/mm-burst-export-manifest.json
  RUNTIME_MANIFEST="$exp"
  rm -rf "$dir/mm-import"
}

step_dirs_and_unit() {
  status "worker: directories, HOME, systemd unit"
  plan "mkdir $MM_ROOT/tmp (1777), $MM_HOME, /var/lib/creatv/hf, /etc/mm-burst; install /etc/systemd/system/burst-worker.service (HOME=$MM_HOME HF_HUB_OFFLINE=1 TORCH_CUDA_ARCH_LIST=\"$CUDA_ARCHES\" CUDA_HOME=$CUDA_HOME_DIR; NOT enabled: the run's user-data starts it)" && return 0
  mkdir -p "$MM_ROOT/tmp" "$MM_HOME" /var/lib/creatv/hf /etc/mm-burst
  chmod 1777 "$MM_ROOT/tmp"
  cat > /etc/systemd/system/burst-worker.service <<UNIT
[Unit]
Description=Media Manipulator burst worker (MM125)
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/burst-worker /etc/mm-burst/run.json
Restart=no
User=root
WorkingDirectory=/
Environment=HOME=$MM_HOME
Environment=HF_HOME=/var/lib/creatv/hf
Environment=HF_HUB_OFFLINE=1
Environment=TRANSFORMERS_OFFLINE=1
Environment=CUDA_DEVICE_ORDER=PCI_BUS_ID
Environment=CUDA_HOME=$CUDA_HOME_DIR
Environment=TORCH_CUDA_ARCH_LIST=$CUDA_ARCHES
Environment=PATH=$CUDA_HOME_DIR/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
}

step_image_json() {
  status "write /etc/mm-burst/image.json"
  plan "/etc/mm-burst/image.json: family, arch=ada+blackwell, base AMI (from IMDS), driver, CUDA, ffmpeg, runtime manifest sha, build date" && return 0
  local tok base drv ff
  tok="$(curl -fsS -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' || true)"
  base="$(curl -fsS -H "X-aws-ec2-metadata-token: $tok" http://169.254.169.254/latest/meta-data/ami-id || echo unknown)"
  drv="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1)"
  ff="$(ffmpeg -version | head -1 | cut -d' ' -f3)"
  jq -n --arg family "$MM_FAMILY" --arg base "$base" --arg drv "$drv" --arg cuda "$(basename "$(readlink -f "$CUDA_HOME_DIR")")" \
    --arg ff "$ff" --arg rm "${RUNTIME_MANIFEST:-$(jq -r .runtimeManifest /etc/mm-burst-export-manifest.json)}" --arg date "$(date -u +%F)" --arg exp "$MM_DATE" \
    '{family: $family, arch: "ada+blackwell", baseAmi: $base, driver: $drv, cuda: $cuda, ffmpeg: $ff, runtimeManifest: $rm, exportDate: $exp, builtAt: $date}' \
    > /etc/mm-burst/image.json
}

step_smoke() {
  status "smoke: running $MM_FAMILY on the real GPU"
  plan "bash $PHERE/smoke/$MM_FAMILY.sh -> /tmp/mm-smoke-$MM_FAMILY.json (each workload's real command on a committed tiny fixture; asserts exit 0, a valid output, and GPU memory in use)" && return 0
  local out="/tmp/mm-smoke-$MM_FAMILY.json" rc=0
  SMOKE_OUT="$out" SMOKE_CARD=0 bash "$PHERE/smoke/$MM_FAMILY.sh" || rc=$?
  aws s3 cp "$out" "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.smoke.json" --only-show-errors
  if [ "$rc" -ne 0 ]; then
    fail_build "smoke test failed (see $MM_FAMILY.smoke.json)"
  fi
}

step_cleanup() {
  status "cleanup: cloud-init, machine-id, ssh host keys, history, tmp"
  plan "cloud-init clean --logs; truncate /etc/machine-id; rm ssh host keys, shell history, /var/lib/tailscale, journal, /tmp, /var/tmp, $MM_ROOT/tmp/*; sync" && return 0
  rm -rf /var/lib/tailscale/* 2>/dev/null || true
  rm -f /etc/ssh/ssh_host_* /root/.bash_history /home/*/.bash_history /root/.aws/credentials 2>/dev/null || true
  rm -rf "${MM_ROOT:?}/tmp"/* "${MM_HOME:?}/.cache/torch_extensions/"*/lock 2>/dev/null || true
  rm -rf /tmp/mm-* /var/tmp/mm-* /opt/mm-build 2>/dev/null || true
  truncate -s 0 /etc/machine-id
  journalctl --rotate >/dev/null 2>&1 || true
  journalctl --vacuum-time=1s >/dev/null 2>&1 || true
  cloud-init clean --logs
  sync
}

step_ready_and_wait() {
  status "READY: image can be created"
  plan "upload $MM_FAMILY.ready; sleep until the builder script creates the image and terminates this instance" && return 0
  printf 'ready\n' | aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.ready" --only-show-errors
  sleep 10800
}

# provision_main runs the standard sequence. A family script passes its own
# rebuild function (or "true") as $1; it runs after the unpack and its manifest check.
provision_main() {
  local rebuild="${1:-true}"
  if [ "$MM_MODE" = "smoke" ]; then
    status "smoke-only mode on an already-built image"
    step_smoke
    printf 'smoke-only\n' | { dry || aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.verified" --only-show-errors; }
    return 0
  fi
  step_base_packages
  step_ffmpeg
  step_tailscale
  step_unpack
  step_vulkan_driver_bits
  "$rebuild"
  step_dirs_and_unit
  step_image_json
  step_smoke
  step_cleanup
  step_ready_and_wait
}

parse_provision_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --dry-run) MM_DRY=1 ;;
      --smoke-only) MM_MODE=smoke ;;
      --date) MM_DATE="$2"; MM_BUILD_PREFIX="burst/build/$2"; shift ;;
      -h|--help) echo "usage: provision-<family>.sh [--dry-run] [--smoke-only] [--date YYYY-MM-DD]"; exit 0 ;;
      *) die "unknown flag $1" ;;
    esac
    shift
  done
  if [ "$MM_DRY" != "1" ] && [ "$(id -u)" -ne 0 ]; then die "run as root on the builder instance (or use --dry-run)"; fi
  if [ "$MM_DRY" = "1" ]; then
    echo "== provision-$MM_FAMILY.sh dry-run plan (nothing is executed) =="
    echo "export date: $MM_DATE   bucket: $MM_BUCKET   mode: $MM_MODE   arches: $CUDA_ARCHES"
  fi
}

# Shared by the families that carry torch-based CUDA extensions: a source build
# for both architectures, so no first-run JIT on the worker.
step_rebuild_mmcv() {
  status "mmcv: rebuild $MMCV_TAG for TORCH_CUDA_ARCH_LIST=\"$CUDA_ARCHES\" (host: sm_120 only)"
  plan "restore-vsr-mm venv: pip uninstall mmcv; git clone open-mmlab/mmcv $MMCV_TAG (commit $MMCV_COMMIT, verified); MMCV_WITH_OPS=1 FORCE_CUDA=1 TORCH_CUDA_ARCH_LIST=\"$CUDA_ARCHES\" MAX_JOBS=3 pip install --no-build-isolation --no-deps . ; import mmcv.ops; cuobjdump/cuda-ELF check that _ext carries sm_89 and sm_120" && return 0
  local py="$MM_ROOT/venvs/restore-vsr-mm/bin/python" src=/opt/dlami/nvme/mmcv-src
  [ -d "$CUDA_HOME_DIR" ] || fail_build "$CUDA_HOME_DIR missing on this DLAMI (need CUDA 12.8 toolkit for the cu128 torch)"
  mkdir -p /opt/dlami/nvme
  rm -rf "$src"
  git clone -q --depth 1 --branch "$MMCV_TAG" https://github.com/open-mmlab/mmcv.git "$src"
  [ "$(git -C "$src" rev-parse HEAD)" = "$MMCV_COMMIT" ] || fail_build "mmcv $MMCV_TAG is not commit $MMCV_COMMIT"
  "$py" -m pip uninstall -y -q mmcv
  (
    cd "$src"
    export CUDA_HOME="$CUDA_HOME_DIR" PATH="$CUDA_HOME_DIR/bin:$PATH" MMCV_WITH_OPS=1 FORCE_CUDA=1 \
      TORCH_CUDA_ARCH_LIST="$CUDA_ARCHES" MAX_JOBS=3
    "$py" -m pip install --no-build-isolation --no-deps -q .
  ) || fail_build "mmcv build failed (see $LOGFILE)"
  rm -rf "$src"
  "$py" -c "import mmcv, mmcv.ops; print('mmcv', mmcv.__version__)" || fail_build "mmcv.ops does not import"
  local so
  so="$("$py" -c "import mmcv,os;print(os.path.join(os.path.dirname(mmcv.__file__),[f for f in os.listdir(os.path.dirname(mmcv.__file__)) if f.startswith('_ext')][0]))")"
  "$CUDA_HOME_DIR/bin/cuobjdump" --list-elf "$so" | grep -q 'sm_89' || fail_build "mmcv _ext has no sm_89 code"
  "$CUDA_HOME_DIR/bin/cuobjdump" --list-elf "$so" | grep -q 'sm_120' || fail_build "mmcv _ext has no sm_120 code"
}

# RVRT's deform_attn is a torch JIT extension. The host cache has it for sm_120
# only; compile it for both here, into the worker's HOME. The worker unit sets
# the same TORCH_CUDA_ARCH_LIST, so the build flags (and cache key) match at run time.
step_prebuild_rvrt_ext() {
  status "rvrt: pre-build deform_attn for TORCH_CUDA_ARCH_LIST=\"$CUDA_ARCHES\""
  plan "restore-sr venv: import RVRT models.op.deform_attn with HOME=$MM_HOME TORCH_CUDA_ARCH_LIST=\"$CUDA_ARCHES\" CUDA_HOME=$CUDA_HOME_DIR so the JIT build lands in $MM_HOME/.cache/torch_extensions" && return 0
  (
    export HOME="$MM_HOME" CUDA_HOME="$CUDA_HOME_DIR" PATH="$CUDA_HOME_DIR/bin:$PATH" TORCH_CUDA_ARCH_LIST="$CUDA_ARCHES" MAX_JOBS=3
    cd "$MM_ROOT/repos/RVRT"
    "$MM_ROOT/venvs/restore-sr/bin/python" -c "from models.op.deform_attn import deform_attn_ext; print('deform_attn ext ok')"
  ) || fail_build "RVRT deform_attn extension build failed"
}
