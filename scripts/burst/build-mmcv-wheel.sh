#!/usr/bin/env bash
# MM126: build mmcv for BOTH GPU generations once, on the server, and keep the
# wheel in S3. The restore image build installs it instead of compiling.
#
#   scripts/burst/build-mmcv-wheel.sh [--no-upload] [--jobs N] [--rebuild]
#
# Why: video restoration (BasicVSR++) needs mmcv's compiled CUDA code. The host's
# mmcv is compiled for its own RTX 5080s only (sm_120). A burst worker may be an
# L4 (sm_89), so the image needs both. Compiling that on the rented builder took
# 40 minutes on 2026-11-11 and then failed a check, with nothing to show for it.
#
# Runs ON THE SERVER as the normal user (never sudo). Costs nothing: no machine
# is launched. It only READS the restore venv (its python, torch headers and
# setuptools); it never installs into it and never touches the running API.
#   1. clones mmcv at the pinned tag into /var/tmp and checks the commit
#   2. compiles a wheel with TORCH_CUDA_ARCH_LIST="8.9;12.0" at the lowest CPU
#      priority (10 to 20 minutes at 8 jobs)
#   3. lists the architectures inside the wheel with cuobjdump: both must be there
#   4. uploads the wheel and a small record (sha256, commit, torch, python) to
#      s3://<bucket>/burst-images/wheels/ with the burst controller's key from
#      the API .env (read here, never printed)
# The S3 key holds the torch and python version, so after a torch upgrade in the
# restore venv the image build asks for a new wheel by name: run this again.
# A finished build is kept in /var/tmp/mm-mmcv-wheel and reused (--rebuild forces a new one).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

UPLOAD=1 FORCE=0 JOBS="${MM_MMCV_JOBS:-8}"
API_DIR="${MM_API_DIR:-$(cd "$HERE/../.." && pwd)}"
while [ $# -gt 0 ]; do
  case "$1" in
    --no-upload) UPLOAD=0 ;;
    --rebuild) FORCE=1 ;;
    --jobs) JOBS="$2"; shift ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) die "unknown flag $1" ;;
  esac
  shift
done
[[ "$JOBS" =~ ^[0-9]+$ ]] && [ "$JOBS" -ge 1 ] || die "--jobs must be a number"
[ "$(id -u)" -ne 0 ] || die "do not run as root"
for tool in git unzip sha256sum jq nice; do
  command -v "$tool" >/dev/null 2>&1 || die "this script needs '$tool' on PATH"
done
VENV="${MM_MMCV_VENV:-$MM_ROOT/venvs/restore-vsr-mm}"
PY="$VENV/bin/python"
[ -x "$PY" ] || die "the restore venv is not on this machine ($PY)"
[ -x "$CUDA_HOME_DIR/bin/nvcc" ] || die "no nvcc at $CUDA_HOME_DIR/bin (the CUDA 12.8 toolkit is needed to compile)"

# Nothing may be written into the venv, not even bytecode caches.
export PYTHONDONTWRITEBYTECODE=1
TORCHV="$("$PY" -c 'import torch; print(torch.__version__)')" || die "torch does not import in $VENV"
PYTAG="$("$PY" -c 'import sys; print("cp%d%d" % sys.version_info[:2])')"
KEY="$(mmcv_wheel_key "$TORCHV" "$PYTAG")"
STAGE="${MM_MMCV_STAGE:-/var/tmp/mm-mmcv-wheel}/$(basename "$KEY")"
RECORD="$STAGE/wheel.json"
sha_of() { sha256sum "$1" | cut -d' ' -f1; }

echo "== mmcv $MMCV_TAG for torch $TORCHV, $PYTAG, architectures: $(wanted_archs | tr '\n' ' ')=="
echo "S3 target: s3://$MM_BUCKET/$KEY.whl (+ .json)"

# A finished build of the same commit is reused.
WHEEL=""
if [ "$FORCE" = 0 ] && [ -f "$RECORD" ] && [ "$(jq -r '.commit // empty' "$RECORD")" = "$MMCV_COMMIT" ]; then
  w="$STAGE/out/$(jq -r '.wheel // empty' "$RECORD")"
  if [ -f "$w" ] && [ "$(sha_of "$w")" = "$(jq -r '.sha256 // empty' "$RECORD")" ]; then
    WHEEL="$w"
    log "reusing the finished build in $STAGE (use --rebuild for a new one)"
  fi
fi

START="$(date +%s)"
if [ -z "$WHEEL" ]; then
  rm -rf "${STAGE:?}"
  mkdir -p "$STAGE/out"
  log "1/4 cloning mmcv $MMCV_TAG"
  timeout 600 git -c advice.detachedHead=false clone -q --depth 1 --branch "$MMCV_TAG" https://github.com/open-mmlab/mmcv.git "$STAGE/src" \
    || die "could not clone mmcv from github.com (unreachable or timed out). Nothing was changed."
  [ "$(git -C "$STAGE/src" rev-parse HEAD)" = "$MMCV_COMMIT" ] || die "mmcv $MMCV_TAG is not commit $MMCV_COMMIT"
  log "2/4 compiling for $(wanted_archs | tr '\n' ' ')with $JOBS jobs at the lowest CPU priority (10 to 20 minutes; the log is $STAGE/build.log)"
  if ! (
    cd "$STAGE/src"
    export CUDA_HOME="$CUDA_HOME_DIR" PATH="$VENV/bin:$CUDA_HOME_DIR/bin:$PATH" MMCV_WITH_OPS=1 FORCE_CUDA=1 \
      TORCH_CUDA_ARCH_LIST="$CUDA_ARCHES" MAX_JOBS="$JOBS" PYTHONDONTWRITEBYTECODE=1
    nice -n 19 "$PY" -m pip wheel --no-build-isolation --no-deps --no-cache-dir -q -w "$STAGE/out" .
  ) > "$STAGE/build.log" 2>&1; then
    tail -n 25 "$STAGE/build.log" >&2
    die "the mmcv build failed (last lines above; full log: $STAGE/build.log). Paste the lines above into chat."
  fi
  WHEEL="$(find "$STAGE/out" -maxdepth 1 -name 'mmcv-*.whl' -type f | sort | sed -n '1p')"
  [ -n "$WHEEL" ] || die "the build finished without a wheel (see $STAGE/build.log)"
  rm -rf "$STAGE/src"
else
  log "1/4 and 2/4 skipped (reused)"
fi

log "3/4 checking the architectures inside the wheel"
rm -rf "$STAGE/check"
mkdir -p "$STAGE/check"
unzip -q -o "$WHEEL" 'mmcv/_ext*' -d "$STAGE/check" || die "the wheel has no compiled mmcv/_ext (MMCV_WITH_OPS did not take effect)"
SO="$(find "$STAGE/check/mmcv" -maxdepth 1 -name '_ext*.so' -type f | sed -n '1p')"
[ -n "$SO" ] || die "the wheel has no compiled mmcv/_ext*.so"
rc=0
HAVE="$(cuda_archs "$SO")" || rc=$?
[ "$rc" -eq 0 ] || die "cuobjdump is missing or could not read the build (exit $rc): the wheel cannot be vouched for, so nothing was uploaded"
for a in $(wanted_archs); do
  grep -qx "$a" <<<"$HAVE" || die "the build has no CUDA code for $a (it has: $(printf '%s' "$HAVE" | tr '\n' ' ')). Nothing was uploaded. Paste this line into chat."
done
rm -rf "$STAGE/check"

SHA="$(sha_of "$WHEEL")"
SIZE="$(wc -c < "$WHEEL" | tr -d ' ')"
jq -n --arg wheel "$(basename "$WHEEL")" --arg sha "$SHA" --argjson bytes "$SIZE" --arg commit "$MMCV_COMMIT" --arg tag "$MMCV_TAG" \
  --arg torch "$TORCHV" --arg python "$PYTAG" --arg archs "$(printf '%s' "$HAVE" | tr '\n' ' ' | sed 's/ $//')" --arg at "$(date -u +%FT%TZ)" \
  '{wheel: $wheel, sha256: $sha, bytes: $bytes, commit: $commit, tag: $tag, torch: $torch, python: $python, archs: ($archs | split(" ")), builtAt: $at}' > "$RECORD"

if [ "$UPLOAD" = 1 ]; then
  log "4/4 uploading to s3://$MM_BUCKET/$KEY.whl"
  controller_key_env "${MM_ENV_FILE:-$API_DIR/.env}"
  export AWS_PAGER=""
  # The wheel first, the record last: a record means a complete upload.
  aws s3 cp "$WHEEL" "s3://$MM_BUCKET/$KEY.whl" --only-show-errors
  aws s3 cp "$RECORD" "s3://$MM_BUCKET/$KEY.json" --only-show-errors
  REMOTE="$(aws s3api head-object --bucket "$MM_BUCKET" --key "$KEY.whl" --query ContentLength --output text)"
  unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
  [ "$REMOTE" = "$SIZE" ] || die "uploaded size $REMOTE differs from local $SIZE; run again"
  WHERE="s3://$MM_BUCKET/$KEY.whl"
else
  log "4/4 not uploaded (--no-upload)"
  WHERE="not uploaded (kept at $WHEEL)"
fi

echo
echo "================ MMCV WHEEL RESULT (paste this into chat) ================"
echo "mmcv:            $MMCV_TAG (commit $MMCV_COMMIT)"
echo "built for:       torch $TORCHV, $PYTAG"
echo "architectures:   $(printf '%s' "$HAVE" | tr '\n' ' ')"
echo "wheel:           $(basename "$WHEEL"), $(human_bytes "$SIZE")"
echo "sha256:          $SHA"
echo "elapsed:         $(fmt_duration $(( $(date +%s) - START )))"
echo "stored at:       $WHERE"
echo "==========================================================================="
