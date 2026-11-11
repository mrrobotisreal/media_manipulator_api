#!/usr/bin/env bash
# MM126: export one worker-image family's runtime from THIS host to S3.
#
#   scripts/burst/export-runtime.sh <image|audio|video|restore> [--date YYYY-MM-DD] [--dry-run] [--yes] [--keep-local]
#
# Runs on the GPU host as the normal user (never sudo). Every source is read-only.
# Writes only /var/tmp/mm-burst-export/<family>.* (deleted at the end) and
# s3://<bucket>/burst-images/<date>/<family>.{tar.zst,manifest.json}.
# Uses the burst controller's AWS key from the API .env (read inside this script,
# never printed). It never touches .env contents, HF tokens, /home or tmp: the
# path lists in internal/burst/manifest/families/*.paths are the whole truth,
# and a Go test enforces that.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

FAMILY="" DATE="$(date -u +%Y-%m-%d)" DRY=0 KEEP=0
API_DIR="${MM_API_DIR:-$(cd "$HERE/../.." && pwd)}"
STAGE="${MM_EXPORT_STAGE:-/var/tmp/mm-burst-export}"
while [ $# -gt 0 ]; do
  case "$1" in
    --date) DATE="$2"; shift ;;
    --dry-run) DRY=1 ;;
    --yes) ASSUME_YES=1 ;;
    --keep-local) KEEP=1 ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    -*) die "unknown flag $1" ;;
    *) [ -z "$FAMILY" ] || die "one family at a time"; FAMILY="$1" ;;
  esac
  shift
done
is_fresh_family "$FAMILY" && die "$FAMILY is installed fresh on the builder, there is nothing to export from this host: run scripts/burst/build-image.sh $FAMILY <date>"
valid_family "$FAMILY" || die "family must be one of: $MM_FAMILIES"
valid_date "$DATE" || die "--date must be YYYY-MM-DD"
[ "$(id -u)" -ne 0 ] || die "do not run as root; this export is read-only on every source"

# 1. burst-ctl: the one implementation of the path list and the manifest.
CTL="${BURST_CTL:-}"
if [ -z "$CTL" ]; then
  CTL="$STAGE/burst-ctl"
  mkdir -p "$STAGE"
  command -v go >/dev/null 2>&1 || export PATH="$PATH:/usr/local/go/bin"
  log "building burst-ctl (go build, output in $STAGE)"
  (cd "$API_DIR" && go build -o "$CTL" ./cmd/burst-ctl)
fi

PATHS="$("$CTL" manifest "$FAMILY" --paths)"
EXCLUDES="$("$CTL" manifest --excludes)"

KEY_PREFIX="burst-images/$DATE"
echo "== export plan: family=$FAMILY date=$DATE =="
echo "S3 target:   s3://$MM_BUCKET/$KEY_PREFIX/$FAMILY.tar.zst (+ $FAMILY.manifest.json)"
echo "Local stage: $STAGE (cleaned at the end)"
echo "Paths (source -> place in the image):"
while IFS="$(printf '\t')" read -r src dest; do
  [ -n "$src" ] || continue
  if [ "$src" = "$dest" ]; then echo "  $src"; else echo "  $src  ->  $dest"; fi
done <<EOF
$PATHS
EOF
echo "Skipped everywhere: $(echo "$EXCLUDES" | tr '\n' ' ')"
if [ "$DRY" = 1 ]; then
  echo "(dry run: nothing read, nothing written)"
  exit 0
fi

# 2. Size and upload-time estimate.
RAW=0
while IFS="$(printf '\t')" read -r src _dest; do
  [ -n "$src" ] || continue
  [ -e "$src" ] || die "missing on this host: $src"
  b="$(du -sb "$src" | cut -f1)"
  RAW=$((RAW + b))
done <<EOF
$PATHS
EOF
EST="$(awk -v r="$RAW" 'BEGIN{ printf "%d", r*0.6 }')" # venvs ~0.45, models ~0.9 (measured with zstd -3)
echo "Raw size:            $(human_bytes "$RAW")"
echo "Compressed estimate: about $(human_bytes "$EST")"
echo "Upload time at your home upstream (the upload is the slow part):"
for mbps in 10 20 50 100 300; do
  secs="$(awk -v e="$EST" -v m="$mbps" 'BEGIN{ printf "%d", e*8/(m*1000000) }')"
  printf '  at %3d Mbit/s up: %s\n' "$mbps" "$(fmt_duration "$secs")"
done
echo "A big family can take hours. Run this inside tmux (tmux new -s export) so a dropped SSH cannot stop it."
confirm "Start the export of '$FAMILY'?" || die "cancelled"

# 3. Controller credentials from the API .env: read two keys, never print them.
ENV_FILE="${MM_ENV_FILE:-$API_DIR/.env}"
[ -r "$ENV_FILE" ] || die "cannot read $ENV_FILE (the API .env)"
getenvkey() { grep -E "^$1=" "$ENV_FILE" | tail -1 | cut -d= -f2- | sed -e 's/^"//' -e 's/"$//'; }
AWS_ACCESS_KEY_ID="$(getenvkey BURST_AWS_ACCESS_KEY_ID)"
AWS_SECRET_ACCESS_KEY="$(getenvkey BURST_AWS_SECRET_ACCESS_KEY)"
[ -n "$AWS_ACCESS_KEY_ID" ] && [ -n "$AWS_SECRET_ACCESS_KEY" ] || die "BURST_AWS_ACCESS_KEY_ID/SECRET not set in $ENV_FILE"
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_DEFAULT_REGION="$MM_REGION"
unset AWS_PROFILE AWS_SESSION_TOKEN

# 4. Tar + zstd to the local stage (lowest CPU/IO priority; the API keeps serving).
mkdir -p "$STAGE"
TARBALL="$STAGE/$FAMILY.tar.zst"
cleanup() {
  if [ "$KEEP" = 0 ]; then rm -f "${STAGE:?}/${FAMILY:?}.tar.zst" "${STAGE:?}/${FAMILY:?}.host.json" "${STAGE:?}/${FAMILY:?}.manifest.core.json"; fi
}
trap cleanup EXIT
TAR_ARGS=(--numeric-owner --sort=name --warning=no-file-changed)
while IFS= read -r p; do
  if [ -n "$p" ]; then TAR_ARGS+=("--exclude=$p"); fi
done <<EOF
$EXCLUDES
EOF
SRCS=()
while IFS="$(printf '\t')" read -r src dest; do
  [ -n "$src" ] || continue
  SRCS+=("${src#/}")
  if [ "$src" != "$dest" ]; then
    # GNU tar: rewrite the member name so it extracts at the image path.
    esc_s="$(printf '%s' "${src#/}" | sed 's/[.[\*^$|]/\\&/g')"
    TAR_ARGS+=("--transform=s|^${esc_s}|${dest#/}|")
  fi
done <<EOF
$PATHS
EOF
log "tar | zstd -> $TARBALL"
START="$(date +%s)"
# tar exit 1 = "file changed as we read it" (a live venv); a warning only.
set +e
nice -n 19 ionice -c3 tar -C / -cf - "${TAR_ARGS[@]}" "${SRCS[@]}" | nice -n 19 zstd -T4 -3 -q -f -o "$TARBALL"
rc=("${PIPESTATUS[@]}")
set -e
[ "${rc[1]}" -eq 0 ] || die "zstd failed"
[ "${rc[0]}" -le 1 ] || die "tar failed (exit ${rc[0]})"
SIZE="$(stat -c %s "$TARBALL")"
log "compressed to $(human_bytes "$SIZE") in $(fmt_duration $(( $(date +%s) - START )))"
SHA="$(sha256sum "$TARBALL" | cut -d' ' -f1)"

# 5. Manifest + host facts, written beside the tarball.
HOSTJSON="$STAGE/$FAMILY.host.json"
"$CTL" manifest "$FAMILY" --json > "$STAGE/$FAMILY.manifest.core.json"
FFCONF="$(ffmpeg -hide_banner -buildconf 2>/dev/null | tr -s ' \n' ' ' | sed 's/^ *//')"
DRIVER="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | sed -n '1p' || true)"
jq -n --arg date "$DATE" --arg key "$KEY_PREFIX/$FAMILY.tar.zst" --argjson size "$SIZE" --arg sha "$SHA" \
  --arg ffver "$(ffmpeg -version 2>/dev/null | sed -n '1p' | cut -d' ' -f3)" --arg ffconf "$FFCONF" --arg drv "$DRIVER" \
  --slurpfile m "$STAGE/$FAMILY.manifest.core.json" \
  '{family: $m[0].family, date: $date, runtimeManifest: $m[0].sha256, manifest: $m[0],
    tarball: {key: $key, bytes: $size, sha256: $sha}, host: {ffmpeg: $ffver, ffmpegConfiguration: $ffconf, nvidiaDriver: $drv}}' > "$HOSTJSON"

# 6. Upload. The tarball first, the manifest last: a manifest means a complete upload.
log "uploading to s3://$MM_BUCKET/$KEY_PREFIX/ (this is the slow step)"
aws s3 cp "$TARBALL" "s3://$MM_BUCKET/$KEY_PREFIX/$FAMILY.tar.zst" --only-show-errors
aws s3 cp "$HOSTJSON" "s3://$MM_BUCKET/$KEY_PREFIX/$FAMILY.manifest.json" --only-show-errors
REMOTE="$(aws s3api head-object --bucket "$MM_BUCKET" --key "$KEY_PREFIX/$FAMILY.tar.zst" --query ContentLength --output text)"
[ "$REMOTE" = "$SIZE" ] || die "uploaded size $REMOTE differs from local $SIZE; run again"

unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
echo
echo "================ EXPORT RESULT (paste this into chat) ================"
echo "family:           $FAMILY"
echo "date:             $DATE"
echo "files:            $(jq -r .manifest.files "$HOSTJSON")"
echo "raw size:         $(human_bytes "$RAW")"
echo "tarball:          $(human_bytes "$SIZE") ($SIZE bytes)"
echo "tarball sha256:   $SHA"
echo "runtime manifest: $(jq -r .runtimeManifest "$HOSTJSON")"
echo "elapsed:          $(fmt_duration $(( $(date +%s) - START )))"
echo "======================================================================="
