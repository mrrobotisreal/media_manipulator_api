#!/usr/bin/env bash
# MM126: keep a signed copy of the ffmpeg 8 packages in our own S3 bucket, so an
# image build does not depend on Ubuntu's Launchpad being up.
#
#   scripts/burst/mirror-ffmpeg.sh [--no-upload] [--out FILE]
#
# Why: the host's ffmpeg (NVENC build) comes from ppa:ubuntuhandbook1/ffmpeg8. On
# 2026-11-11 Launchpad and Ubuntu's key server were unreachable and a builder sat
# for 170 minutes waiting for them. Every worker image installs ffmpeg, so every
# build had that dependency.
#
# What it does (runs ON THE SERVER as the normal user, never sudo; costs nothing):
#   1. downloads the PPA's signed index (InRelease) and checks the signature with
#      the PPA's own public key, committed in keys/ppa-ubuntuhandbook1-ffmpeg8.asc
#   2. downloads the package lists named in that index and checks their sha256
#   3. downloads every amd64 package in the list (about 34 MiB) and checks each sha256
#   4. proves apt accepts the copy, with the same source line the builder uses
#   5. uploads it as ONE tar to s3://<bucket>/burst-images/apt/ffmpeg8-noble-amd64.tar
#      with the burst controller's key from the API .env (read here, never printed)
#
# The copy is a plain apt repository: the index is still signed by the PPA's key
# and the builder's apt checks that signature again, so nothing in S3 is trusted
# by itself. Run it again whenever you want the builds to pick up a newer ffmpeg.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

UPLOAD=1 OUT=""
API_DIR="${MM_API_DIR:-$(cd "$HERE/../.." && pwd)}"
while [ $# -gt 0 ]; do
  case "$1" in
    --no-upload) UPLOAD=0 ;;
    --out) OUT="$2"; shift ;;
    -h|--help) sed -n '2,23p' "$0"; exit 0 ;;
    *) die "unknown flag $1" ;;
  esac
  shift
done
KEY_FILE="${MM_FFMPEG_KEY_FILE:-$HERE/keys/ppa-ubuntuhandbook1-ffmpeg8.asc}"
[ -r "$KEY_FILE" ] || die "the PPA signing key is missing: $KEY_FILE"
for tool in curl gpg gpgv sha256sum gunzip tar awk; do
  command -v "$tool" >/dev/null 2>&1 || die "this script needs '$tool' on PATH"
done

WORK="$(mktemp -d /tmp/mm-ffmpeg-mirror.XXXXXX)"
trap 'rm -rf "${WORK:?}"' EXIT
BASE="$MM_FFMPEG_PPA_URL"
REPO="$WORK/mirror/ubuntu"
DIST="$REPO/dists/noble"
mkdir -p "$DIST" "$WORK/gnupg"
chmod 700 "$WORK/gnupg"
CURL=(curl -fsSL --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5)

# 1. The signed index.
log "1/5 the signed package index from $BASE"
"${CURL[@]}" "$BASE/dists/noble/InRelease" -o "$DIST/InRelease" \
  || die "cannot download from $BASE (Launchpad is down or slow). Nothing was changed. Try again later; image builds still work without this copy as long as Launchpad answers."
gpg --homedir "$WORK/gnupg" --batch --yes --quiet --dearmor -o "$WORK/key.gpg" "$KEY_FILE"
KEY_FPR="$(gpg --homedir "$WORK/gnupg" --batch --show-keys --with-colons "$KEY_FILE" 2>/dev/null | awk -F: '$1=="fpr"{print $10; exit}')"
[[ "$KEY_FPR" =~ ^[0-9A-F]{40}$ ]] || die "cannot read a key fingerprint from $KEY_FILE"
# gpg writes the text the signature covers to Release.verified, and ONLY that
# file is read below, so nothing outside the signed part of InRelease is used.
# Three things must all hold: gpg exits 0, it reports a VALID signature, and
# that signature is by the committed key (a file with no signature at all
# would otherwise pass the first check).
bad_index() { die "the index is NOT signed by the PPA's key ($KEY_FILE). Do not use it. gpg said: $(tr '\n' ' ' < "$WORK/gpg.log" | cut -c1-300)"; }
gpg --homedir "$WORK/gnupg" --batch --no-default-keyring --keyring "$WORK/key.gpg" --trust-model always \
  --status-file "$WORK/gpg.status" --output "$WORK/Release.verified" --decrypt "$DIST/InRelease" 2>"$WORK/gpg.log" || bad_index
SIGNER="$(awk '$1=="[GNUPG:]" && $2=="VALIDSIG" {print $3 " " $NF}' "$WORK/gpg.status" | sed -n '1p')"
case " $SIGNER " in *" $KEY_FPR "*) ;; *) bad_index ;; esac
[ -s "$WORK/Release.verified" ] || bad_index

# index_entry PATH -> "sha256 size" from the verified index ("" when not listed).
index_entry() {
  awk -v want="$1" '/^SHA256:/{on=1; next} /^[^ ]/{on=0} on && $3==want {print $1, $2}' "$WORK/Release.verified" | sed -n '1p'
}
sha_of() { sha256sum "$1" | cut -d' ' -f1; }
size_of() { wc -c < "$1" | tr -d ' '; }

# fetch_indexed PATH REQUIRED(1|0): download a file named in the index and check it.
fetch_indexed() {
  local rel="$1" req="$2" ent sha size
  ent="$(index_entry "$rel")"
  if [ -z "$ent" ]; then
    if [ "$req" = 1 ]; then die "the index does not list $rel"; fi
    return 0
  fi
  sha="${ent%% *}"
  size="${ent##* }"
  mkdir -p "$(dirname "$DIST/$rel")"
  # Launchpad lists some forms it does not serve (404): quiet, and not an error for an optional file.
  if ! "${CURL[@]}" "$BASE/dists/noble/$rel" -o "$DIST/$rel" 2>/dev/null; then
    rm -f "$DIST/$rel"
    if [ "$req" = 1 ]; then die "could not download $rel from $BASE"; fi
    return 0
  fi
  [ "$(sha_of "$DIST/$rel")" = "$sha" ] && [ "$(size_of "$DIST/$rel")" = "$size" ] \
    || die "$rel does not match the signed index (sha256 or size differs). Do not use it."
}

# 2. The package lists.
log "2/5 the package lists"
fetch_indexed main/binary-amd64/Packages.gz 1
for f in main/binary-amd64/Packages.xz main/binary-amd64/Packages main/binary-amd64/Release \
  main/i18n/Translation-en main/i18n/Translation-en.gz main/i18n/Translation-en.xz; do
  fetch_indexed "$f" 0
done
# Launchpad lists the uncompressed files in the index but does not serve them.
# apt may ask for any listed form, so make the uncompressed one from the .gz
# and hold it to the index's own sha256.
unpacked_from_gz() {
  local rel="$1" ent
  ent="$(index_entry "$rel")"
  if [ -z "$ent" ] || [ -f "$DIST/$rel" ] || [ ! -f "$DIST/$rel.gz" ]; then return 0; fi
  gunzip -c "$DIST/$rel.gz" > "$DIST/$rel"
  [ "$(sha_of "$DIST/$rel")" = "${ent%% *}" ] || die "$rel (unpacked) does not match the signed index. Do not use it."
}
unpacked_from_gz main/binary-amd64/Packages
unpacked_from_gz main/i18n/Translation-en
gunzip -c "$DIST/main/binary-amd64/Packages.gz" > "$WORK/Packages"

# One line per package: filename, sha256, size, name, version (tab separated).
awk 'BEGIN{RS=""; FS="\n"} {
  f=""; h=""; s=""; p=""; v="";
  for (i=1; i<=NF; i++) {
    if ($i ~ /^Filename: /) f=substr($i, 11);
    else if ($i ~ /^SHA256: /) h=substr($i, 9);
    else if ($i ~ /^Size: /) s=substr($i, 7);
    else if ($i ~ /^Package: /) p=substr($i, 10);
    else if ($i ~ /^Version: /) v=substr($i, 10);
  }
  if (f != "") print f "\t" h "\t" s "\t" p "\t" v
}' "$WORK/Packages" > "$WORK/packages.tsv"
[ -s "$WORK/packages.tsv" ] || die "the package list is empty"

FFVER="$(awk -F'\t' '$4=="ffmpeg"{print $5}' "$WORK/packages.tsv" | sed -n '1p')"
case "${FFVER#*:}" in
  8.1.*) ;;
  *) die "the PPA now carries ffmpeg '${FFVER:-none}', not an 8.1.x build. Nothing was uploaded. Paste this line into chat: the worker images are built for the host's ffmpeg 8.1." ;;
esac

# 3. The packages.
COUNT="$(wc -l < "$WORK/packages.tsv" | tr -d ' ')"
log "3/5 $COUNT packages (ffmpeg $FFVER)"
TOTAL=0
while IFS="$(printf '\t')" read -r file sha size _name _ver; do
  case "$file" in
    *..*) die "refusing a package path with '..' in it: $file" ;;
    pool/*.deb) ;;
    *) die "refusing an unexpected package path: $file" ;;
  esac
  [[ "$file" =~ ^[A-Za-z0-9._+~/-]+$ ]] || die "refusing a package path with unusual characters: $file"
  [[ "$sha" =~ ^[0-9a-f]{64}$ ]] || die "no sha256 for $file in the package list"
  mkdir -p "$(dirname "$REPO/$file")"
  "${CURL[@]}" "$BASE/$file" -o "$REPO/$file" || die "could not download $file from $BASE (Launchpad is down or slow). Nothing was uploaded. Try again later."
  [ "$(sha_of "$REPO/$file")" = "$sha" ] && [ "$(size_of "$REPO/$file")" = "$size" ] \
    || die "$file does not match the signed package list (sha256 or size differs). Do not use it."
  TOTAL=$((TOTAL + size))
done < "$WORK/packages.tsv"

# 4. Prove apt accepts the copy (Linux with apt only; the builder uses the same source line).
SELFTEST="skipped (no apt-get on this machine)"
if [ "${MM_MIRROR_APT_SELFTEST:-1}" = 0 ]; then
  SELFTEST="skipped (MM_MIRROR_APT_SELFTEST=0)"
elif command -v apt-get >/dev/null 2>&1 && command -v apt-cache >/dev/null 2>&1; then
  log "4/5 checking that apt accepts the copy"
  T="$WORK/apt"
  mkdir -p "$T/lists/partial" "$T/cache/archives/partial" "$T/etc"
  ffmpeg_apt_line "$KEY_FILE" "file:$REPO" "by-hash=no" > "$T/etc/sources.list"
  APT=(-o "Dir::Etc::sourcelist=$T/etc/sources.list" -o Dir::Etc::sourceparts=/dev/null -o "Dir::State::lists=$T/lists"
    -o "Dir::Cache=$T/cache" -o Debug::NoLocking=1 -o APT::Get::List-Cleanup=0)
  if ! apt-get "${APT[@]}" update > "$T/update.log" 2>&1; then
    die "apt refused the copy. Nothing was uploaded. Paste this into chat: $(tr '\n' ' ' < "$T/update.log" | cut -c1-600)"
  fi
  CAND="$(apt-cache "${APT[@]}" policy ffmpeg 2>/dev/null | sed -n 's/^ *Candidate: *//p' | sed -n '1p')"
  [ "$CAND" = "$FFVER" ] || die "apt read the copy but offers ffmpeg '${CAND:-none}', not $FFVER. Nothing was uploaded. Paste this into chat: $(tr '\n' ' ' < "$T/update.log" | cut -c1-400)"
  SELFTEST="passed (apt offers ffmpeg $CAND from the copy)"
else
  log "4/5 skipped: no apt-get here (run this on the server)"
fi

# 5. One tar, then S3.
jq_date="$(sed -n 's/^Date: //p' "$WORK/Release.verified" | sed -n '1p')"
printf '{"source":"%s","ffmpeg":"%s","indexDate":"%s","packages":%s,"mirroredAt":"%s"}\n' \
  "$BASE" "$FFVER" "$jq_date" "$COUNT" "$(date -u +%FT%TZ)" > "$REPO/MIRROR.json"
TARFILE="${OUT:-$WORK/ffmpeg8-noble-amd64.tar}"
tar -C "$WORK/mirror" -cf "$TARFILE" ubuntu
TARSHA="$(sha_of "$TARFILE")"
TARSIZE="$(size_of "$TARFILE")"
DEST="s3://$MM_BUCKET/$MM_FFMPEG_MIRROR_KEY"
if [ "$UPLOAD" = 1 ]; then
  log "5/5 uploading to $DEST"
  controller_key_env "${MM_ENV_FILE:-$API_DIR/.env}"
  export AWS_PAGER=""
  aws s3 cp "$TARFILE" "$DEST" --only-show-errors
  REMOTE="$(aws s3api head-object --bucket "$MM_BUCKET" --key "$MM_FFMPEG_MIRROR_KEY" --query ContentLength --output text)"
  unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
  [ "$REMOTE" = "$TARSIZE" ] || die "uploaded size $REMOTE differs from local $TARSIZE; run again"
  WHERE="$DEST"
else
  log "5/5 not uploaded (--no-upload)"
  WHERE="not uploaded${OUT:+ (kept at $OUT)}"
fi

echo
echo "================ MIRROR RESULT (paste this into chat) ================"
echo "ffmpeg version:  $FFVER"
echo "index date:      $jq_date"
echo "packages:        $COUNT ($(human_bytes "$TOTAL"))"
echo "signature:       good (PPA key $(basename "$KEY_FILE"))"
echo "apt self-test:   $SELFTEST"
echo "tar:             $(human_bytes "$TARSIZE"), sha256 $TARSHA"
echo "stored at:       $WHERE"
echo "======================================================================"
