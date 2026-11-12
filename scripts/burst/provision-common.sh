#!/usr/bin/env bash
# MM126 shared provisioning steps. Sourced by provision-<family>.sh, which run
# ON THE BUILDER INSTANCE (a DLAMI Ubuntu 24.04 box launched by build-image.sh)
# as root. With --dry-run (or MM_DRY=1) every step only prints its plan, so the
# whole thing can be read on a laptop.
#
# Inputs (environment, set by the builder's user-data):
#   MM_FAMILY  image|audio|video|restore|lab-voice (lab-voice is a fresh install, MM139)
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

STATUS_HISTORY="${MM_STATUS_HISTORY:-$LOGFILE.status}"
# The export manifest left in the image ({runtimeManifest: ...}); image.json reads it.
EXPORT_MANIFEST="${MM_EXPORT_MANIFEST:-/etc/mm-burst-export-manifest.json}"

dry() { [ "$MM_DRY" = "1" ]; }

# output_has "pattern" cmd args...: true when cmd's output contains pattern.
# Never pipe a command into `grep -q` under `set -o pipefail`: grep exits at
# the first match, the producer gets SIGPIPE (exit 141) and the pipeline
# reports failure even though the pattern is there. On 2026-11-10 that made
# `ffmpeg -encoders | grep -q h264_nvenc` fail on a build that has NVENC.
output_has() {
  local pat="$1" out
  shift
  out="$("$@" 2>/dev/null)" || true
  grep -q -- "$pat" <<<"$out"
}

# net SECONDS cmd...: run ONE network-bound command under a hard time limit.
# Nothing that downloads may run without it. On 2026-11-11 a builder sat in
# `add-apt-repository` for 170 minutes (about $7 on g7.2xlarge) because Ubuntu's
# Launchpad and key server were unreachable and nothing had a time limit.
# On a timeout the reason goes to the status object and the caller gets 124.
# MM_NET_LIMIT_CAP (seconds) caps every limit; tests use it.
net() {
  local limit="$1" rc=0 t0=$SECONDS
  shift
  if [ -n "${MM_NET_LIMIT_CAP:-}" ] && [ "$limit" -gt "$MM_NET_LIMIT_CAP" ]; then limit="$MM_NET_LIMIT_CAP"; fi
  timeout --kill-after=20 "$limit" "$@" || rc=$?
  # 124 = timeout ended it; 137 = it had to be killed (only counted as a
  # timeout when the limit really passed: an out-of-memory kill is also 137).
  if [ "$rc" -eq 124 ] || { [ "$rc" -eq 137 ] && [ $((SECONDS - t0)) -ge "$limit" ]; }; then
    status "TIMED OUT after ${limit}s: $(printf '%s ' "$@" | cut -c1-140)"
    return 124
  fi
  return "$rc"
}

# apt_get args...: apt-get with connection timeouts, retries, a wait for the
# dpkg lock (cloud-init may still hold it at boot) and the hard limit of net.
APT_OPTS=(-o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 -o Acquire::Retries=3 -o DPkg::Lock::Timeout=300)
apt_get() { net "${MM_APT_LIMIT:-1200}" apt-get "${APT_OPTS[@]}" "$@"; }

# with_heartbeat "label" cmd...: run a long LOCAL step (a compile; nothing that
# downloads) and report "still running" every 10 minutes, so the builder script
# does not take a long quiet compile for a stuck builder.
with_heartbeat() {
  local label="$1" hb rc=0 t0=$SECONDS
  shift
  (
    while sleep "${MM_HEARTBEAT_SECONDS:-600}"; do
      status "still running: $label ($(( (SECONDS - t0) / 60 )) min)"
    done
  ) &
  hb=$!
  "$@" || rc=$?
  kill "$hb" 2>/dev/null || true
  wait "$hb" 2>/dev/null || true
  return "$rc"
}

# require_cuda_archs LABEL FILE [OTHER_PROOF]: FILE must carry CUDA code for
# every architecture in CUDA_ARCHES; the failure names what is there instead.
# OTHER_PROOF says what else vouches for the file (a sha256 checked against a
# build that was verified elsewhere, the compiler flags in build.ninja). With
# it, a cuobjdump that is missing or broken on this machine is only a note:
# a tool problem must never be reported as "the code is not there".
require_cuda_archs() {
  local label="$1" file="$2" proof="${3:-}" have="" rc=0 a missing="" errf
  errf="$(mktemp /tmp/mm-cuobjdump.XXXXXX)"
  have="$(cuda_archs "$file" 2>"$errf")" || rc=$?
  if [ "$rc" -ne 0 ]; then
    local why="cuobjdump is not on this machine"
    [ "$rc" -eq 3 ] || why="cuobjdump could not read it: $(tr '\n' ' ' < "$errf" | cut -c1-160)"
    rm -f "$errf"
    [ -n "$proof" ] || fail_build "cannot list the CUDA architectures in $label ($why)"
    status "note: the CUDA architectures in $label were not listed here ($why); $proof"
    return 0
  fi
  rm -f "$errf"
  for a in $(wanted_archs); do
    grep -qx "$a" <<<"$have" || missing="$missing $a"
  done
  if [ -n "$missing" ]; then
    fail_build "$label has no CUDA code for:$missing (it has: $(printf '%s' "$have" | tr '\n' ' '); wanted TORCH_CUDA_ARCH_LIST=\"$CUDA_ARCHES\")"
  fi
  status "$label: CUDA code for $(printf '%s' "$have" | tr '\n' ' ')"
}

# curl flags for every download: fail on HTTP errors, 20 s to connect, a total limit, 3 tries.
CURL_NET=(-fsSL --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5)

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
  # stderr only: the builder's user-data already appends stdout+stderr to $LOGFILE.
  printf '%s\n' "$line" >&2
  # The status object holds EVERY line so far, not only the newest: the builder
  # script polls every 20 s and used to miss lines written between two polls
  # (on 2026-11-11 the line saying where ffmpeg came from was never shown).
  printf '%s\n' "$line" >> "$STATUS_HISTORY" 2>/dev/null || true
  { cat "$STATUS_HISTORY" 2>/dev/null || printf '%s\n' "$line"; } | timeout 60 aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.status" --only-show-errors || true
}

# Set by fail_build (a file, because fail_build also runs inside subshells).
FAILED_MARK="${MM_FAILED_MARK:-/tmp/mm-provision-failed.$$}"

# on_provision_exit: the EXIT trap of provision_main. A script error that is not
# a fail_build (an unset variable under `set -u`, a command failing under
# `set -e`) used to end the build with no FAILED line at all: on 2026-11-12 the
# only trace was "HOME: unbound variable" in the middle of 40 log lines. Now it
# is reported like any other failure, with the script's own last message.
on_provision_exit() {
  local rc=$?
  # "Did it finish" is PROVISION_OK, not the exit code: after an unset-variable
  # error some bash versions (3.2 on a Mac) hand the trap the PREVIOUS command's
  # status, 0, and would then leave with 0, which reads as success.
  if [ "${PROVISION_OK:-0}" != 1 ] && ! dry; then
    [ "$rc" -ne 0 ] || rc=1
    if [ ! -e "$FAILED_MARK" ]; then
      local last
      last="$(grep -v -e '^PLAN ' -e '^$' "$LOGFILE" 2>/dev/null | tail -n 1 | cut -c1-240)"
      status "FAILED: the build script itself stopped; this is a bug in the script, not a failed check. Its last message: ${last:-none}"
    fi
  fi
  rm -f "$FAILED_MARK"
  exit "$rc"
}

fail_build() {
  : > "$FAILED_MARK" 2>/dev/null || true
  status "FAILED: $*"
  if ! dry; then
    printf '%s\n' "$*" | timeout 60 aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.failed" --only-show-errors || true
    tail -n 200 "$LOGFILE" 2>/dev/null | timeout 60 aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.log" --only-show-errors || true
  fi
  exit 1
}

step_base_packages() {
  status "apt: base packages"
  plan "apt-get install: jq ca-certificates curl zstd unzip imagemagick (6.x: convert/identify, never magick) libimage-exiftool-perl vulkan-tools libvulkan1 libgomp1 libgl1 libglib2.0-0t64 python3-venv" && return 0
  export DEBIAN_FRONTEND=noninteractive
  apt_get update -qq
  apt_get install -y -qq jq ca-certificates curl zstd unzip imagemagick libimage-exiftool-perl \
    vulkan-tools libvulkan1 libgomp1 libgl1 libglib2.0-0t64 python3-venv software-properties-common gnupg
  if ! command -v aws >/dev/null 2>&1; then
    curl "${CURL_NET[@]}" https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscli.zip
    unzip -q /tmp/awscli.zip -d /tmp/awscli && /tmp/awscli/aws/install && rm -rf /tmp/awscli /tmp/awscli.zip
  fi
}

# The ffmpeg 8 package source. The host's build comes from ppa:ubuntuhandbook1/ffmpeg8
# (NVENC, nonfree, fdk-aac). Two ways to reach it, both verified by apt with the
# PPA's own signing key, which is committed in keys/ (a public key, fingerprint
# F4E48910A020E77056748B745738AE8480447DDF, the one Launchpad reports for the PPA):
#   1. the copy in our S3 bucket (scripts/burst/mirror-ffmpeg.sh), so a build
#      needs nothing from Launchpad;
#   2. the PPA itself, as a fallback.
# `add-apt-repository` is never used: it asks api.launchpad.net and
# keyserver.ubuntu.com for the key and waits for ever when they are down.
FFMPEG_KEYRING="${MM_KEYRING_DIR:-/usr/share/keyrings}/mm-ffmpeg8.asc"
FFMPEG_LIST="${MM_APT_LIST_DIR:-/etc/apt/sources.list.d}/mm-ffmpeg8.list"
FFMPEG_MIRROR_DIR="${MM_APT_MIRROR_DIR:-/opt/mm-apt}/ffmpeg8"

# ffmpeg_candidate_ok: apt's candidate for ffmpeg is an 8.1.x build (Ubuntu's own is 6.1).
ffmpeg_candidate_ok() {
  local cand
  cand="$(apt-cache policy ffmpeg 2>/dev/null | sed -n 's/^ *Candidate: *//p' | sed -n '1p')"
  case "$cand" in *:8.1.*|8.1.*) return 0 ;; esac
  FFMPEG_CANDIDATE="${cand:-none}"
  return 1
}

# ffmpeg_source_mirror: put the S3 copy in place as an apt source. Any failure
# returns non-zero and the caller falls back to the PPA.
ffmpeg_source_mirror() {
  local tarf
  tarf="$(mktemp /tmp/mm-ffmpeg8.XXXXXX)"
  net 300 aws s3 cp "s3://$MM_BUCKET/$MM_FFMPEG_MIRROR_KEY" "$tarf" --only-show-errors 2>/dev/null || { rm -f "$tarf"; return 1; }
  rm -rf "$FFMPEG_MIRROR_DIR"
  mkdir -p "$FFMPEG_MIRROR_DIR"
  tar -xf "$tarf" -C "$FFMPEG_MIRROR_DIR" || { rm -f "$tarf"; return 1; }
  rm -f "$tarf"
  [ -f "$FFMPEG_MIRROR_DIR/ubuntu/dists/noble/InRelease" ] || return 1
  chmod -R a+rX "$FFMPEG_MIRROR_DIR"
  ffmpeg_apt_line "$FFMPEG_KEYRING" "file:$FFMPEG_MIRROR_DIR/ubuntu" "by-hash=no" > "$FFMPEG_LIST"
  apt_get update -qq || return 1
  ffmpeg_candidate_ok
}

step_ffmpeg() {
  # Host: 10:8.1.1-0build1~ubuntu24.04. The PPA keeps only its newest build
  # (8.1.2 since 2026-06-20), so a newer 8.1.x patch release is accepted; any
  # other series stops the build. The version used is recorded in /etc/mm-burst/image.json.
  status "apt: ffmpeg 8.1.x (the host's NVENC build of ppa:ubuntuhandbook1/ffmpeg8)"
  plan "ffmpeg 8.1.x, verified with the PPA's committed signing key (no add-apt-repository, no key server): first the copy in s3://$MM_BUCKET/$MM_FFMPEG_MIRROR_KEY (scripts/burst/mirror-ffmpeg.sh), else $MM_FFMPEG_PPA_URL; every download has a time limit; then check the version is 8.1.x and that h264_nvenc is there" && return 0
  mkdir -p "$(dirname "$FFMPEG_KEYRING")" "$(dirname "$FFMPEG_LIST")"
  install -m 644 "$PHERE/keys/ppa-ubuntuhandbook1-ffmpeg8.asc" "$FFMPEG_KEYRING"
  local src="the S3 copy" FFMPEG_CANDIDATE=""
  if ! ffmpeg_source_mirror; then
    src="the PPA"
    status "note: no usable ffmpeg copy in S3 (run scripts/burst/mirror-ffmpeg.sh once on the server); using $MM_FFMPEG_PPA_URL"
    rm -rf "$FFMPEG_MIRROR_DIR"
    ffmpeg_apt_line "$FFMPEG_KEYRING" "$MM_FFMPEG_PPA_URL" > "$FFMPEG_LIST"
    apt_get update -qq || fail_build "apt-get update failed or timed out after adding the ffmpeg 8 source ($MM_FFMPEG_PPA_URL)"
    ffmpeg_candidate_ok || fail_build "the ffmpeg 8 package source did not load (apt offers ffmpeg $FFMPEG_CANDIDATE, need 8.1.x): ppa.launchpadcontent.net is unreachable or slow. Nothing is wrong with the image. Retry later, or run scripts/burst/mirror-ffmpeg.sh on the server once Launchpad answers so builds stop depending on it."
  fi
  apt_get install -y -qq ffmpeg || fail_build "installing ffmpeg from $src failed or timed out"
  local ffv
  ffv="$(ffmpeg -version 2>/dev/null | sed -n '1p' | cut -d' ' -f3)"
  case "$ffv" in
    8.1|8.1.*) status "ffmpeg $ffv installed from $src (host has 8.1.1)" ;;
    *) fail_build "ffmpeg $ffv is not an 8.1.x release (host has 8.1.1)" ;;
  esac
  output_has h264_nvenc ffmpeg -hide_banner -encoders || fail_build "ffmpeg has no h264_nvenc"
  if [ "$src" = "the S3 copy" ]; then
    # The local copy was only an install source: the image keeps neither it nor a source entry pointing at it.
    rm -rf "$FFMPEG_MIRROR_DIR" "$FFMPEG_LIST"
  fi
}

step_tailscale() {
  status "apt: tailscale"
  plan "install tailscale from pkgs.tailscale.com (noble); do NOT run 'tailscale up' (the run's user-data does)" && return 0
  if ! command -v tailscale >/dev/null 2>&1; then
    curl "${CURL_NET[@]}" https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg -o /usr/share/keyrings/tailscale-archive-keyring.gpg
    curl "${CURL_NET[@]}" https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list -o /etc/apt/sources.list.d/tailscale.list
    apt_get update -qq
    apt_get install -y -qq tailscale
  fi
  systemctl enable tailscaled >/dev/null 2>&1 || true
}

step_vulkan_driver_bits() {
  # The Go code pins VK_ICD_FILENAMES to /opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json, which
  # loads libEGL_nvidia.so.0 (host: package libnvidia-gl-590). The OSS-driver DLAMI may ship only the
  # compute half of the driver, and may have installed it from a runfile rather than apt, so the
  # matching libnvidia-gl package is tried by exact driver version first, and the result is proven
  # with vulkaninfo through our own ICD file before anything else runs.
  status "vulkan: NVIDIA GL/EGL userspace libraries"
  plan "if libEGL_nvidia.so.0 is not in ldconfig: apt-get install libnvidia-gl-<major>-server=<exact driver> (then -server any version, then libnvidia-gl-<major>); copy api_version from the system nvidia_icd.json into $MM_ROOT/vulkan/nvidia_icd_egl.json if that file exists; prove it: vulkaninfo --summary with VK_ICD_FILENAMES=our ICD must list an NVIDIA GPU" && return 0
  local drv major
  drv="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | sed -n '1p' | tr -d ' ')"
  major="${drv%%.*}"
  status "vulkan: driver $drv; driver packages: $(dpkg -l 2>/dev/null | awk '/^ii/ && $2 ~ /nvidia/ {print $2"="$3}' | tr '\n' ' ' | cut -c1-400)"
  if ! output_has 'libEGL_nvidia.so.0' ldconfig -p; then
    apt_get install -y -qq "libnvidia-gl-${major}-server=${drv}-0ubuntu1" \
      || apt_get install -y -qq "libnvidia-gl-${major}-server" \
      || apt_get install -y -qq "libnvidia-gl-${major}" \
      || fail_build "no libnvidia-gl package for driver $drv (Vulkan ICD would fail); driver packages: $(dpkg -l | awk '/^ii/ && $2 ~ /nvidia/ {print $2}' | tr '\n' ' ')"
    ldconfig
    output_has 'libEGL_nvidia.so.0' ldconfig -p || fail_build "libEGL_nvidia.so.0 still missing after installing libnvidia-gl for driver $drv"
  fi
  local sys=/usr/share/vulkan/icd.d/nvidia_icd.json icd="$MM_ROOT/vulkan/nvidia_icd_egl.json"
  if [ -f "$sys" ] && [ -f "$icd" ]; then
    local v
    v="$(jq -r '.ICD.api_version // empty' "$sys")"
    if [ -n "$v" ]; then
      jq --arg v "$v" '.ICD.api_version = $v' "$icd" > "$icd.new" && mv "$icd.new" "$icd"
    fi
  fi
  if [ -f "$icd" ]; then
    local vk
    vk="$(VK_ICD_FILENAMES="$icd" VK_DRIVER_FILES="$icd" VK_LOADER_LAYERS_DISABLE='*' vulkaninfo --summary 2>&1 || true)"
    if ! grep -qi 'nvidia' <<<"$vk"; then
      printf '%s\n' "$vk" >> "$LOGFILE"
      fail_build "Vulkan does not see the NVIDIA GPU through $icd (vulkaninfo output in the log)"
    fi
    status "vulkan: OK, $(grep -m1 -i 'deviceName' <<<"$vk" | sed 's/^ *//' | cut -c1-120)"
  fi
}

step_unpack() {
  status "s3: download and unpack $MM_FAMILY.tar.zst"
  plan "aws s3 cp s3://$MM_BUCKET/burst-images/$MM_DATE/$MM_FAMILY.tar.zst -> instance store; sha256 vs $MM_FAMILY.manifest.json; zstd -d | tar -x -C / --numeric-owner; then '${MM_CTL:-burst-ctl} manifest $MM_FAMILY --from-dest' must equal the exported runtime manifest (before any rebuild)" && return 0
  local dir=/opt/dlami/nvme
  mountpoint -q "$dir" || dir=/var/tmp
  mkdir -p "$dir/mm-import"
  net 300 aws s3 cp "s3://$MM_BUCKET/burst-images/$MM_DATE/$MM_FAMILY.manifest.json" "$dir/mm-import/manifest.json" --only-show-errors
  net 3600 aws s3 cp "s3://$MM_BUCKET/burst-images/$MM_DATE/$MM_FAMILY.tar.zst" "$dir/mm-import/t.tar.zst" --only-show-errors
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
  cp "$dir/mm-import/manifest.json" "$EXPORT_MANIFEST"
  RUNTIME_MANIFEST="$exp"
  rm -rf "$dir/mm-import"
  status "disk after unpack: $(df -h / | awk 'NR==2{print $3" used of "$2" ("$5")"}')"
  local pct
  pct="$(df --output=pcent / | sed -n '2p' | tr -dc '0-9')"
  if [ "${pct:-0}" -gt 85 ]; then
    fail_build "root disk is ${pct}% full after unpacking $MM_FAMILY: raise its RootGiB in internal/burst/profiles.go and scripts/burst/lib.sh"
  fi
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
  tok="$(curl -fsS -m 5 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' || true)"
  base="$(curl -fsS -m 5 -H "X-aws-ec2-metadata-token: $tok" http://169.254.169.254/latest/meta-data/ami-id || echo unknown)"
  drv="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | sed -n '1p')"
  ff="$(ffmpeg -version | sed -n '1p' | cut -d' ' -f3)"
  jq -n --arg family "$MM_FAMILY" --arg base "$base" --arg drv "$drv" --arg cuda "$(basename "$(readlink -f "$CUDA_HOME_DIR")")" \
    --arg ff "$ff" --arg rm "${RUNTIME_MANIFEST:-$(jq -r .runtimeManifest "$EXPORT_MANIFEST")}" --arg date "$(date -u +%F)" --arg exp "$MM_DATE" \
    '{family: $family, arch: "ada+blackwell", baseAmi: $base, driver: $drv, cuda: $cuda, ffmpeg: $ff, runtimeManifest: $rm, exportDate: $exp, builtAt: $date}' \
    > /etc/mm-burst/image.json
}

step_smoke() {
  status "smoke: running $MM_FAMILY on the real GPU"
  plan "bash $PHERE/smoke/$MM_FAMILY.sh -> /tmp/mm-smoke-$MM_FAMILY.json (each workload's real command on a committed tiny fixture; asserts exit 0, a valid output, and GPU memory in use)" && return 0
  local out="/tmp/mm-smoke-$MM_FAMILY.json" rc=0
  # Each finished case prints one "name PASS|FAIL ..." line on stderr. Pass it on
  # as a status, so the build shows progress and a long smoke run is never
  # mistaken for a stuck builder.
  local errf="/tmp/mm-smoke-$MM_FAMILY.err" pid seen=0
  # report_cases: one status per case result not reported yet.
  report_cases() {
    local n line
    n="$(grep -c -E ' (PASS|FAIL) ' "$errf" 2>/dev/null || true)"
    while [ "$seen" -lt "${n:-0}" ]; do
      seen=$((seen + 1))
      line="$(grep -E ' (PASS|FAIL) ' "$errf" | sed -n "${seen}p" | tr -s ' ' | cut -c1-110)"
      status "smoke: $line"
    done
  }
  SMOKE_OUT="$out" SMOKE_CARD=0 bash "$PHERE/smoke/$MM_FAMILY.sh" 2>"$errf" &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    report_cases
    sleep "${MM_SMOKE_POLL_SECONDS:-5}"
  done
  wait "$pid" || rc=$?
  report_cases
  cat "$errf" >&2
  rm -f "$errf"
  net 300 aws s3 cp "$out" "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.smoke.json" --only-show-errors
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
  printf 'ready\n' | net 300 aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.ready" --only-show-errors
  sleep 10800
}

# provision_main runs the standard sequence. A family script passes its own
# rebuild function (or "true") as $1; it runs after the unpack and its manifest check.
provision_main() {
  local rebuild="${1:-true}"
  trap on_provision_exit EXIT
  if [ "$MM_MODE" = "smoke" ]; then
    status "smoke-only mode on an already-built image"
    step_smoke
    printf 'smoke-only\n' | { dry || net 300 aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.verified" --only-show-errors; }
    PROVISION_OK=1
    return 0
  fi
  step_base_packages
  step_ffmpeg
  step_tailscale
  if is_fresh_family "$MM_FAMILY"; then
    # MM139: installed fresh on the builder. No host export to unpack and no
    # Vulkan ICD (the Lab voice models are CUDA only); the family script's
    # install function does everything and sets RUNTIME_MANIFEST.
    status "fresh install: no host export for $MM_FAMILY"
  else
    step_unpack
    step_vulkan_driver_bits
  fi
  "$rebuild"
  step_dirs_and_unit
  step_image_json
  step_smoke
  step_cleanup
  step_ready_and_wait
  PROVISION_OK=1
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
  if [ "$MM_DRY" != "1" ] && [ "${MM_ALLOW_NONROOT:-0}" != 1 ] && [ "$(id -u)" -ne 0 ]; then die "run as root on the builder instance (or use --dry-run)"; fi
  if [ "$MM_DRY" = "1" ]; then
    echo "== provision-$MM_FAMILY.sh dry-run plan (nothing is executed) =="
    echo "export date: $MM_DATE   bucket: $MM_BUCKET   mode: $MM_MODE   arches: $CUDA_ARCHES"
  fi
}

# The restore family's mmcv. The host's mmcv is compiled for sm_120 only (its
# own cards); a worker image must also run on Ada (g6). Compiling mmcv on the
# builder took 40 minutes of rented GPU time on 2026-11-11 and then failed its
# check, so the two-architecture build is now made ONCE on the server
# (scripts/burst/build-mmcv-wheel.sh: free, checked there with cuobjdump) and
# the builder only installs that wheel, by sha256.
step_rebuild_mmcv() {
  status "mmcv: install the $MMCV_TAG build for TORCH_CUDA_ARCH_LIST=\"$CUDA_ARCHES\" made on the server (host venv: sm_120 only)"
  plan "restore-vsr-mm venv: download the mmcv wheel built by scripts/burst/build-mmcv-wheel.sh from s3://$MM_BUCKET/burst-images/wheels/ (key holds the venv's torch and python version); check commit $MMCV_COMMIT and the sha256; pip install --no-index --no-deps --force-reinstall; import mmcv.ops; list the architectures in _ext with cuobjdump when it is on the image (sm_89 and sm_120). Nothing is compiled on the builder" && return 0
  local py="$MM_ROOT/venvs/restore-vsr-mm/bin/python" torchv pytag key dir name want got commit
  torchv="$("$py" -c 'import torch; print(torch.__version__)')" || fail_build "torch does not import in the restore-vsr-mm venv"
  pytag="$("$py" -c 'import sys; print("cp%d%d" % sys.version_info[:2])')"
  key="$(mmcv_wheel_key "$torchv" "$pytag")"
  dir="$(mktemp -d /tmp/mm-mmcv.XXXXXX)"
  net 300 aws s3 cp "s3://$MM_BUCKET/$key.json" "$dir/wheel.json" --only-show-errors 2>/dev/null \
    || fail_build "there is no two-architecture mmcv build in S3 for torch $torchv / $pytag (s3://$MM_BUCKET/$key.json). On the server run: scripts/burst/build-mmcv-wheel.sh   (free, no machine is launched). Then start this build again."
  name="$(jq -r '.wheel // empty' "$dir/wheel.json")"
  want="$(jq -r '.sha256 // empty' "$dir/wheel.json")"
  commit="$(jq -r '.commit // empty' "$dir/wheel.json")"
  [[ "$name" =~ ^mmcv-[A-Za-z0-9._+-]+\.whl$ ]] || fail_build "the mmcv build record in S3 names no wheel ('$name')"
  [ "$commit" = "$MMCV_COMMIT" ] || fail_build "the mmcv build in S3 is commit '$commit', not $MMCV_COMMIT: run scripts/burst/build-mmcv-wheel.sh on the server again"
  net 600 aws s3 cp "s3://$MM_BUCKET/$key.whl" "$dir/$name" --only-show-errors || fail_build "could not download the mmcv wheel from S3"
  got="$(sha256sum "$dir/$name" | cut -d' ' -f1)"
  [ "$want" = "$got" ] || fail_build "mmcv wheel sha256 mismatch (want $want got $got)"
  net 600 "$py" -m pip install --no-index --no-deps --force-reinstall -q "$dir/$name" || fail_build "installing the mmcv wheel failed (see $LOGFILE)"
  rm -rf "$dir"
  "$py" -c "import mmcv, mmcv.ops; print('mmcv', mmcv.__version__)" || fail_build "mmcv.ops does not import"
  local so
  so="$("$py" -c "import mmcv,os;print(os.path.join(os.path.dirname(mmcv.__file__),[f for f in os.listdir(os.path.dirname(mmcv.__file__)) if f.startswith('_ext')][0]))")"
  require_cuda_archs "mmcv _ext" "$so" "the wheel's sha256 matches the build that was checked on the server"
}

# RVRT's deform_attn is a torch JIT extension. The host cache has it for sm_120
# only; compile it for both here, into the worker's HOME. The worker unit sets
# the same TORCH_CUDA_ARCH_LIST, so the build flags (and cache key) match at run time.
step_prebuild_rvrt_ext() {
  status "rvrt: pre-build deform_attn for TORCH_CUDA_ARCH_LIST=\"$CUDA_ARCHES\""
  plan "apt: ninja-build if no ninja is on PATH (torch needs it to compile an extension at import time)" || {
    # torch's cpp_extension.load() has no fallback without ninja; the restore-sr venv carries none.
    command -v ninja >/dev/null 2>&1 || apt_get install -y -qq ninja-build || fail_build "could not install ninja-build (RVRT's extension cannot be compiled without it)"
  }
  plan "restore-sr venv: import RVRT models.op.deform_attn with HOME=$MM_HOME TORCH_CUDA_ARCH_LIST=\"$CUDA_ARCHES\" CUDA_HOME=$CUDA_HOME_DIR so the JIT build lands in $MM_HOME/.cache/torch_extensions" && return 0
  (
    export HOME="$MM_HOME" CUDA_HOME="$CUDA_HOME_DIR" PATH="$CUDA_HOME_DIR/bin:$PATH" TORCH_CUDA_ARCH_LIST="$CUDA_ARCHES" MAX_JOBS=3
    cd "$MM_ROOT/repos/RVRT"
    with_heartbeat "rvrt deform_attn build" "$MM_ROOT/venvs/restore-sr/bin/python" -c "from models.op.deform_attn import deform_attn_ext; print('deform_attn ext ok')"
  ) || fail_build "RVRT deform_attn extension build failed"
  local so
  so="$(find "$MM_HOME/.cache/torch_extensions" -name 'deform_attn*.so' -type f 2>/dev/null | sort | sed -n '1p')"
  [ -n "$so" ] || fail_build "RVRT deform_attn was built but its .so is not under $MM_HOME/.cache/torch_extensions"
  # First proof, with no tool needed: the compiler flags torch wrote for this build.
  local ninja a
  ninja="$(dirname "$so")/build.ninja"
  [ -f "$ninja" ] || fail_build "RVRT deform_attn: no build.ninja beside $so"
  for a in $(wanted_archs); do
    grep -qE -- "code=${a}( |\$)" "$ninja" || fail_build "RVRT deform_attn was not compiled for $a (build.ninja has: $(grep -o 'code=sm_[0-9a-z]*' "$ninja" | sort -u | tr '\n' ' '))"
  done
  require_cuda_archs "RVRT deform_attn" "$so" "its build.ninja compiled for $(wanted_archs | tr '\n' ' ')"
}

# S0 finding: restore_video.py reads the BasicVSR++ config from
# $MM_ROOT/repos/mmagic/configs/..., but the host has mmagic only as a pip
# package (configs ship inside it at site-packages/mmagic/.mim/configs) and no
# repos/mmagic directory. The image links the packaged configs there so the
# script's path resolves. A symlink only; nothing else differs from the host.
step_link_mmagic_configs() {
  status "restore: link repos/mmagic -> packaged mmagic configs"
  plan "if $MM_ROOT/repos/mmagic is absent: ln -s <restore-vsr-mm site-packages>/mmagic/.mim $MM_ROOT/repos/mmagic (BasicVSR++ config path)" && return 0
  [ -e "$MM_ROOT/repos/mmagic" ] && return 0
  local mim
  mim="$("$MM_ROOT/venvs/restore-vsr-mm/bin/python" -c "import mmagic,os;print(os.path.join(os.path.dirname(mmagic.__file__),'.mim'))")"
  [ -d "$mim/configs" ] || fail_build "mmagic package has no .mim/configs"
  ln -s "$mim" "$MM_ROOT/repos/mmagic"
}
