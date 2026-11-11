#!/usr/bin/env bash
# MM126: build one worker-image family into an AMI.
#
#   scripts/burst/build-image.sh <image|audio|video|restore|lab-voice> <YYYY-MM-DD export date>
#        [--instance-type g6.xlarge|g7.2xlarge] [--yes] [--dry-run]
#        [--smoke-ami ami-... ]     test an ALREADY BUILT AMI (e.g. on g7.2xlarge); no image is created
#        [--result ami-... ]        print the BUILD RESULT box for an image that finished after the script stopped
#
# Runs ON THE SERVER (or anywhere with Go, AWS CLI v2 and the profile below).
# Needs the AWS profile `mm-burst-builder` (policy scripts/burst/iam/mm-burst-builder-policy.json),
# NOT the controller key and NOT .env. Override with AWS_PROFILE.
#
# lab-voice (MM139) is a FRESH family: nothing is copied from the host. The builder installs
# every model itself (venvs + weights), step 1 is skipped, the runtime manifest tag is computed
# on the builder from the recorded versions, and the default builder is g7.2xlarge.
#
# What it does:
#   1. checks the export exists in s3://<bucket>/burst-images/<date>/
#   2. resolves the newest Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04)
#      through its public SSM parameter and prints the cost estimate; asks y/N
#   3. uploads the provisioning scripts + a burst-ctl build to s3://<bucket>/burst/build/<date>/
#   4. launches ONE on-demand builder (tags mm-burst=1, mm-burst-build=<family>;
#      terminate on shutdown; shuts itself down after 3 h) whose user-data runs provision-<family>.sh
#   5. waits for <family>.smoke.json, then <family>.ready (or .failed)
#   6. on all-pass: create-image --no-reboot, tags the AMI and snapshots
#      (mm-burst-image=1, family, arch=ada+blackwell, runtime-manifest=<sha>)
#   7. ALWAYS terminates the builder, then prints BURST_AMIS=<family>:x86_64=ami-...
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

FAMILY="${1:-}" DATE="${2:-}"
[ $# -ge 2 ] || { sed -n 2,6p "$0"; exit 2; }
shift 2
ITYPE="g6.xlarge" DRY=0 SMOKE_AMI="" RESULT_AMI="" FRESH=0
# A fresh family is smoked on the bigger Blackwell card (Fish S2 Pro wants 24 GB).
if is_fresh_family "$FAMILY"; then FRESH=1; ITYPE="g7.2xlarge"; fi
API_DIR="${MM_API_DIR:-$(cd "$HERE/../.." && pwd)}"
while [ $# -gt 0 ]; do
  case "$1" in
    --instance-type) ITYPE="$2"; shift ;;
    --smoke-ami) SMOKE_AMI="$2"; shift ;;
    --result) RESULT_AMI="$2"; shift ;;
    --yes) ASSUME_YES=1 ;;
    --dry-run) DRY=1 ;;
    *) die "unknown flag $1" ;;
  esac
  shift
done
valid_family "$FAMILY" || die "family must be one of: $MM_FAMILIES"
valid_date "$DATE" || die "date must be YYYY-MM-DD"
case "$ITYPE" in g6.xlarge|g7.2xlarge) ;; *) die "instance type must be g6.xlarge or g7.2xlarge (the builder policy allows only those)" ;; esac

export AWS_PROFILE="${AWS_PROFILE:-mm-burst-builder}"
export AWS_DEFAULT_REGION="$MM_REGION" AWS_PAGER=""
BUILD_PREFIX="burst/build/$DATE"
ROOT_GIB="$(profile_root_gib "$FAMILY")"
HOURS="$(build_hours "$FAMILY")"
PRICE="$(price_per_hour "$ITYPE")"
EST="$(awk -v h="$HOURS" -v p="$PRICE" -v g="$ROOT_GIB" 'BEGIN{ printf "%.2f", h*p + g*0.08*h/730 }')"

# --result: an image whose build finished but whose wait timed out (or that
# the script never saw become available). Read everything back from AWS and
# S3 and print the same BUILD RESULT box. Launches nothing.
if [ -n "$RESULT_AMI" ]; then
  case "$RESULT_AMI" in ami-*) ;; *) die "--result needs an ami-... id" ;; esac
  read_img() { aws ec2 describe-images --image-ids "$RESULT_AMI" --query "$1" --output text; }
  st="$(read_img 'Images[0].State')"
  [ "$st" = "available" ] || die "image $RESULT_AMI is $st, not available yet; try again later"
  [ "$(read_img "Images[0].Tags[?Key=='mm-burst-image'].Value | [0]")" = "1" ] || die "$RESULT_AMI is not an mm-burst image"
  [ "$(read_img "Images[0].Tags[?Key=='family'].Value | [0]")" = "$FAMILY" ] || die "$RESULT_AMI is not a $FAMILY image"
  ROOT_DEV="$(read_img 'Images[0].RootDeviceName')"
  BASE_AMI="$(read_img "Images[0].Tags[?Key=='base-ami'].Value | [0]")"
  RUNTIME_MANIFEST="$(read_img "Images[0].Tags[?Key=='runtime-manifest'].Value | [0]")"
  IMG_NAME="$(aws ec2 describe-images --image-ids "$BASE_AMI" --query 'Images[0].Name' --output text 2>/dev/null || echo unknown)"
  SNAP_GIB=0
  for sn in $(read_img 'Images[0].BlockDeviceMappings[].Ebs.SnapshotId'); do
    [ "$sn" = "None" ] && continue
    g="$(aws ec2 describe-snapshots --snapshot-ids "$sn" --query 'Snapshots[0].VolumeSize' --output text)"
    SNAP_GIB=$((SNAP_GIB + g))
  done
  SMOKE="$(aws s3 cp "s3://$MM_BUCKET/burst/build/$DATE/$FAMILY.smoke.json" - 2>/dev/null | jq -c . 2>/dev/null || echo '{}')"
  SMOKE_LINE="$(echo "$SMOKE" | jq -c 'if .cases then {passed, driverAndGpu, cases: [.cases[] | {name, ok, seconds, gpuPeakMiB}]} else "not found (the burst/ staging copy expires after a day)" end')"
  echo
  echo "================ BUILD RESULT (paste this into chat) ================"
  echo "family:           $FAMILY"
  echo "AMI:              $RESULT_AMI"
  echo "root device:      $ROOT_DEV"
  echo "base image:       $BASE_AMI ($IMG_NAME)"
  echo "created:          $(read_img 'Images[0].CreationDate')"
  echo "snapshot size:    ${SNAP_GIB} GiB provisioned (billed on used blocks; about \$$(awk -v g="$SNAP_GIB" 'BEGIN{printf "%.2f", g*0.05}')/month at most)"
  echo "runtime manifest: $RUNTIME_MANIFEST"
  echo "smoke:            $SMOKE_LINE"
  echo "BURST_AMIS entry: $FAMILY:x86_64=$RESULT_AMI"
  [ "$ROOT_DEV" = "/dev/sda1" ] || echo "NOTE: set BURST_ROOT_DEVICE=$ROOT_DEV in the API .env (this image's root device is not the default /dev/sda1)"
  echo "====================================================================="
  exit 0
fi

aws_() { if [ "$DRY" = 1 ]; then printf 'AWS   aws %s\n' "$*" >&2; return 0; fi; aws "$@"; }

echo "== build plan: family=$FAMILY export=$DATE builder=$ITYPE mode=$([ -n "$SMOKE_AMI" ] && echo "smoke-only on $SMOKE_AMI" || echo build) =="
echo "AWS profile:   $AWS_PROFILE   region: $MM_REGION"
echo "Root volume:   ${ROOT_GIB} GiB gp3, encrypted by default (same size the burst profile launches with)"
echo "Estimated cost: about \$$EST (about $HOURS h on $ITYPE at \$$PRICE/h, plus disk), then snapshot storage ~\$0.05/GB-month"
echo "Self-shutdown: 3 h after launch (instance-initiated shutdown = terminate)"

# 1. The export must exist (a fresh family has none: the builder installs it).
if [ "$FRESH" = 1 ]; then
  RUNTIME_MANIFEST="<computed on the builder from its recorded versions>"
  echo "Fresh install: no host export; the builder installs every model itself."
elif [ -z "$SMOKE_AMI" ]; then
  if [ "$DRY" = 1 ]; then
    echo "AWS   aws s3api head-object s3://$MM_BUCKET/burst-images/$DATE/$FAMILY.manifest.json (dry run: not called)"
    RUNTIME_MANIFEST="<runtime manifest from $FAMILY.manifest.json>"
  else
    aws s3api head-object --bucket "$MM_BUCKET" --key "burst-images/$DATE/$FAMILY.manifest.json" >/dev/null 2>&1 \
      || die "no export for '$FAMILY' on $DATE. On the host run: scripts/burst/export-runtime.sh $FAMILY --date $DATE"
    RUNTIME_MANIFEST="$(aws s3 cp "s3://$MM_BUCKET/burst-images/$DATE/$FAMILY.manifest.json" - | jq -r .runtimeManifest)"
  fi
fi

# 2. Base image.
if [ -n "$SMOKE_AMI" ]; then
  BASE_AMI="$SMOKE_AMI"
elif [ "$DRY" = 1 ]; then
  BASE_AMI="<ami-id from $MM_SSM_PARAM>"
  echo "AWS   aws ssm get-parameter --name $MM_SSM_PARAM"
else
  BASE_AMI="$(aws ssm get-parameter --name "$MM_SSM_PARAM" --query Parameter.Value --output text)"
fi
ROOT_DEV="/dev/sda1"
if [ "$DRY" != 1 ]; then
  # One value per call: the image name contains spaces ("Deep Learning Base ..."),
  # so reading name and device from one line split the name into the device.
  IMG_NAME="$(aws ec2 describe-images --image-ids "$BASE_AMI" --query 'Images[0].Name' --output text)"
  ROOT_DEV="$(aws ec2 describe-images --image-ids "$BASE_AMI" --query 'Images[0].RootDeviceName' --output text)"
  case "$ROOT_DEV" in
    /dev/[a-z]*) ;;
    *) die "could not read the root device name of $BASE_AMI (got '$ROOT_DEV')" ;;
  esac
  echo "Base image:    $BASE_AMI  ($IMG_NAME)  root device $ROOT_DEV"
  BASE_GIB="$(aws ec2 describe-images --image-ids "$BASE_AMI" --query 'Images[0].BlockDeviceMappings[?DeviceName==`'"$ROOT_DEV"'`].Ebs.VolumeSize | [0]' --output text)"
  echo "Base root disk: ${BASE_GIB} GiB (an image cannot launch with a smaller root than this; the profile root for $FAMILY is $ROOT_GIB GiB)"
  if [ "$BASE_GIB" != "None" ] && [ "$BASE_GIB" -gt "$ROOT_GIB" ]; then
    die "the base image's root disk (${BASE_GIB} GiB) is larger than the $FAMILY profile root ($ROOT_GIB GiB): raise RootGiB in internal/burst/profiles.go and lib.sh first"
  fi
  [ "$(aws ec2 describe-images --image-ids "$BASE_AMI" --query 'Images[0].Architecture' --output text)" = "x86_64" ] || die "$BASE_AMI is not x86_64"
else
  echo "Base image:    $BASE_AMI"
fi

if [ "$DRY" = 1 ]; then
  echo "(dry run: nothing launched, nothing uploaded)"
fi
confirm "Launch a $ITYPE builder (about \$$EST)?" || die "cancelled"

# 3. Provisioning bundle: scripts + a linux burst-ctl built from this checkout.
BUNDLE_DIR="$(mktemp -d /tmp/mm-bundle.XXXXXX)"
INSTANCE_ID=""
cleanup() {
  local rc=$?
  if [ -n "$INSTANCE_ID" ] && [ "$DRY" != 1 ]; then
    log "terminating builder $INSTANCE_ID"
    aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" >/dev/null 2>&1 || log "WARNING: could not terminate $INSTANCE_ID; run: aws ec2 terminate-instances --instance-ids $INSTANCE_ID"
  fi
  rm -rf "${BUNDLE_DIR:?}"
  return $rc
}
trap cleanup EXIT
if [ "$DRY" = 1 ]; then
  echo "BUILD go build ./cmd/burst-ctl (linux/amd64) + tar scripts/burst$([ "$FRESH" = 1 ] && echo ' + scripts/lab') -> s3://$MM_BUCKET/$BUILD_PREFIX/bundle.tgz"
else
  command -v go >/dev/null 2>&1 || export PATH="$PATH:/usr/local/go/bin"
  (cd "$API_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$BUNDLE_DIR/burst-ctl" ./cmd/burst-ctl)
  mkdir -p "$BUNDLE_DIR/burst"
  cp -R "$HERE"/. "$BUNDLE_DIR/burst/"
  mv "$BUNDLE_DIR/burst-ctl" "$BUNDLE_DIR/burst/burst-ctl"
  BUNDLE_ITEMS="burst"
  if [ "$FRESH" = 1 ]; then
    # the model wrappers (scripts/lab) are installed into the image by provision-<family>.sh
    cp -R "$HERE/../lab" "$BUNDLE_DIR/lab"
    BUNDLE_ITEMS="burst lab"
  fi
  # shellcheck disable=SC2086  # BUNDLE_ITEMS is a short word list
  tar -C "$BUNDLE_DIR" -czf "$BUNDLE_DIR/bundle.tgz" $BUNDLE_ITEMS
  aws s3 cp "$BUNDLE_DIR/bundle.tgz" "s3://$MM_BUCKET/$BUILD_PREFIX/bundle.tgz" --only-show-errors
  # Stale results from an earlier attempt would be mistaken for this one's.
  for ext in status failed ready smoke.json verified log; do
    aws s3 rm "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.$ext" >/dev/null 2>&1 || true
  done
  if [ "$FRESH" = 1 ]; then aws s3 rm "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.runtime-manifest" >/dev/null 2>&1 || true; fi
fi

# 4. User-data (no secrets: the worker role's S3 access is the only credential).
MODE_FLAG=""
[ -n "$SMOKE_AMI" ] && MODE_FLAG="--smoke-only"
USERDATA="#!/bin/bash
# MM126 image builder bootstrap (no secrets)
shutdown -h +180
export MM_FAMILY=$FAMILY MM_DATE=$DATE MM_BUCKET=$MM_BUCKET MM_REGION=$MM_REGION AWS_DEFAULT_REGION=$MM_REGION MM_BUILD_PREFIX=$BUILD_PREFIX MM_CTL=/opt/mm-burst-build/burst/burst-ctl
mkdir -p /opt/mm-burst-build
timeout 600 aws s3 cp s3://$MM_BUCKET/$BUILD_PREFIX/bundle.tgz /opt/mm-burst-build/bundle.tgz --only-show-errors
tar -xzf /opt/mm-burst-build/bundle.tgz -C /opt/mm-burst-build
bash /opt/mm-burst-build/burst/provision-$FAMILY.sh --date $DATE $MODE_FLAG >> /var/log/mm-burst-provision.log 2>&1 \\
  || { tail -n 200 /var/log/mm-burst-provision.log | timeout 120 aws s3 cp - s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.log --only-show-errors; echo failed | timeout 120 aws s3 cp - s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.failed --only-show-errors; shutdown -h now; }
"

# 5. Launch: try every subnet (one per AZ) until one has capacity.
TAGS="ResourceType=instance,Tags=[{Key=mm-burst,Value=1},{Key=mm-burst-build,Value=$FAMILY},{Key=Name,Value=mm-burst-build-$FAMILY}]"
VTAGS="ResourceType=volume,Tags=[{Key=mm-burst,Value=1},{Key=mm-burst-build,Value=$FAMILY}]"
BDM="[{\"DeviceName\":\"$ROOT_DEV\",\"Ebs\":{\"VolumeSize\":$ROOT_GIB,\"VolumeType\":\"gp3\",\"DeleteOnTermination\":true,\"Encrypted\":true}}]"
if [ "$DRY" = 1 ]; then
  echo "AWS   aws ec2 run-instances --image-id $BASE_AMI --instance-type $ITYPE --subnet-id <each of $MM_SUBNETS in turn> --security-group-ids $MM_SG --iam-instance-profile Name=$MM_PROFILE --instance-initiated-shutdown-behavior terminate --metadata-options HttpTokens=required --block-device-mappings '$BDM' --tag-specifications '$TAGS' '$VTAGS' --user-data <$(printf '%s' "$USERDATA" | wc -c | tr -d ' ') bytes>"
  echo "WAIT  poll s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.{status,smoke.json,ready,failed} every 20 s, up to 170 min; stop and terminate the builder when its status has not changed for $(( ${MM_STALL_SECONDS:-5400} / 60 )) min"
  echo "AWS   aws ec2 create-image --no-reboot --instance-id <builder> --name mm-burst-$FAMILY-$DATE-<hhmm> + tags mm-burst-image=1 family=$FAMILY arch=ada+blackwell runtime-manifest=$RUNTIME_MANIFEST"
  echo "AWS   aws ec2 terminate-instances --instance-ids <builder>"
  echo "OUT   BURST_AMIS=$FAMILY:x86_64=<new ami>"
  exit 0
fi
IFS=',' read -r -a SUBS <<<"$MM_SUBNETS"
for s in "${SUBS[@]}"; do
  if out="$(aws ec2 run-instances --image-id "$BASE_AMI" --instance-type "$ITYPE" --subnet-id "$s" --security-group-ids "$MM_SG" \
      --iam-instance-profile "Name=$MM_PROFILE" --instance-initiated-shutdown-behavior terminate \
      --metadata-options HttpTokens=required,HttpEndpoint=enabled --block-device-mappings "$BDM" \
      --tag-specifications "$TAGS" "$VTAGS" --user-data "$USERDATA" --count 1 --query 'Instances[0].InstanceId' --output text 2>&1)"; then
    INSTANCE_ID="$out"; log "builder $INSTANCE_ID launched in $s"; break
  fi
  log "launch in $s failed: $(printf '%s' "$out" | tail -n 1 | cut -c1-200)"
done
[ -n "$INSTANCE_ID" ] || die "no subnet had capacity for $ITYPE (or the policy denied the launch)"

# 6. Wait.
START="$(date +%s)"
last=""
# A builder that stops reporting is stuck, not busy: every step reports when it
# starts, and every download in provisioning has its own time limit. On
# 2026-11-11 one sat at "apt: ffmpeg" for 170 minutes (about $7) because
# Launchpad was unreachable and this loop only knew the 170-minute limit.
# 90 min: well past the longest single time limit in provisioning (60 min) and
# the longest quiet step (the mmcv source build); smoke reports after each case.
STALL="${MM_STALL_SECONDS:-5400}"
last_change="$START"
while :; do
  now="$(date +%s)"
  el=$(( now - START ))
  [ "$el" -lt 10200 ] || die "timed out after 170 min waiting for the builder"
  if [ $(( now - last_change )) -ge "$STALL" ]; then
    die "the builder has reported nothing new for $(( STALL / 60 )) min (last status: ${last:-none}); it is stuck, so it is being terminated. Nothing is wrong on your side. Paste this line into chat."
  fi
  if aws s3api head-object --bucket "$MM_BUCKET" --key "$BUILD_PREFIX/$FAMILY.failed" >/dev/null 2>&1; then
    log "BUILD FAILED. Last log lines:"
    aws s3 cp "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.log" - 2>/dev/null | tail -n 40 >&2 || true
    aws s3 cp "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.smoke.json" - 2>/dev/null | jq -c '.cases[]? | select(.ok==false)' >&2 || true
    exit 1
  fi
  cur="$(aws s3 cp "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.status" - 2>/dev/null | tail -n 1 || true)"
  if [ -n "$cur" ] && [ "$cur" != "$last" ]; then log "builder: $cur"; last="$cur"; last_change="$now"; fi
  # The builder can vanish without writing .failed: terminated by someone else
  # (on 2026-11-10 an API reaper older than MM126 killed one a minute after
  # launch), its 3 h self-shutdown, or an AWS problem. Stop at once with the
  # reason instead of waiting out the timeout.
  ist="$(aws ec2 describe-instances --instance-ids "$INSTANCE_ID" --query 'Reservations[0].Instances[0].[State.Name,StateReason.Message]' --output text 2>/dev/null || true)"
  case "$ist" in
    shutting-down*|terminated*|stopping*|stopped*)
      why="$(printf '%s' "$ist" | cut -f2-)"
      INSTANCE_ID=""
      die "builder is gone before it finished (state: $(printf '%s' "$ist" | cut -f1); reason: ${why:-unknown}; last status: ${last:-none}). If the reason is a user-initiated termination, check that the API on the server runs MM126 or later (its reaper spares builders tagged mm-burst-build) and run 'go run ./cmd/burst-ctl status'."
      ;;
    "")
      # Not found at all: AWS forgets terminated instances after about an hour.
      if [ "$el" -gt 120 ]; then
        INSTANCE_ID=""
        die "builder is gone before it finished (AWS no longer lists it; last status: ${last:-none}). Check that the API on the server runs MM126 or later, then retry."
      fi
      ;;
  esac
  if [ -n "$SMOKE_AMI" ]; then
    aws s3api head-object --bucket "$MM_BUCKET" --key "$BUILD_PREFIX/$FAMILY.verified" >/dev/null 2>&1 && break
  else
    aws s3api head-object --bucket "$MM_BUCKET" --key "$BUILD_PREFIX/$FAMILY.ready" >/dev/null 2>&1 && break
  fi
  sleep "${MM_POLL_SECONDS:-20}"
done
SMOKE="$(aws s3 cp "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.smoke.json" - | jq -c .)"
echo "$SMOKE" | jq .
if [ -n "$SMOKE_AMI" ]; then
  echo "smoke-only run on $ITYPE finished; passed=$(echo "$SMOKE" | jq .passed)"
  exit 0
fi
[ "$(echo "$SMOKE" | jq .passed)" = "true" ] || die "smoke tests did not all pass; no image created"
if [ "$FRESH" = 1 ]; then
  RUNTIME_MANIFEST="$(aws s3 cp "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.runtime-manifest" - | tr -d '[:space:]')"
  [[ "$RUNTIME_MANIFEST" =~ ^[0-9a-f]{64}$ ]] || die "the builder did not record a runtime manifest for $FAMILY (got '$RUNTIME_MANIFEST')"
  log "runtime manifest (from the recorded versions): $RUNTIME_MANIFEST"
fi

# 7. Image.
STAMP="$(date -u +%H%M)"
TSPEC="$(printf 'ResourceType=image,Tags=[%s] ResourceType=snapshot,Tags=[%s]' \
  "$(printf '{Key=%s,Value=%s},' mm-burst-image 1 family "$FAMILY" arch ada+blackwell runtime-manifest "$RUNTIME_MANIFEST" mm-burst-image-built "$(date -u +%F)" export-date "$DATE" base-ami "$BASE_AMI" | sed 's/,$//')" \
  "$(printf '{Key=%s,Value=%s},' mm-burst-image 1 family "$FAMILY" runtime-manifest "$RUNTIME_MANIFEST" | sed 's/,$//')")"
# shellcheck disable=SC2086  # TSPEC is two --tag-specifications words
AMI="$(aws ec2 create-image --instance-id "$INSTANCE_ID" --name "mm-burst-$FAMILY-$DATE-$STAMP" --no-reboot \
  --description "MM126 $FAMILY worker image, $([ "$FRESH" = 1 ] && echo "fresh install" || echo "export") $DATE, driver $(echo "$SMOKE" | jq -r .driverAndGpu | cut -d, -f1)" \
  --tag-specifications $TSPEC --query ImageId --output text)"
log "image $AMI is being created (snapshot of $ROOT_GIB GiB)"
# The snapshot is point-in-time: once AWS has taken it (snapshot state
# "pending"), the builder is no longer needed, so terminate it now instead of
# paying for it while the snapshot copies to S3. On 2026-11-10 the builder
# idled for an hour (about $2.50 on g7.2xlarge) waiting for "available".
SNAP_SEEN=""
for _ in $(seq 1 60); do
  SNAP_SEEN="$(aws ec2 describe-images --image-ids "$AMI" --query 'Images[0].BlockDeviceMappings[0].Ebs.SnapshotId' --output text 2>/dev/null || true)"
  case "$SNAP_SEEN" in snap-*) break ;; esac
  sleep "${MM_POLL_SECONDS:-10}"
done
case "$SNAP_SEEN" in
  snap-*)
    log "snapshot $SNAP_SEEN started; terminating the builder now (the image finishes without it)"
    if aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" >/dev/null 2>&1; then
      INSTANCE_ID=""
    else
      log "WARNING: could not terminate the builder yet; the exit handler will retry"
    fi
    ;;
  *) log "no snapshot id after 10 min; keeping the builder until the image is available" ;;
esac
# AWS: AMI creation takes "several minutes ... (sometimes up to 24 hours)";
# the 100 GiB video image took over an hour on 2026-11-10. Wait up to 4 h, and
# if it is still pending, print how to finish by hand rather than calling it a
# failure.
st="pending"
for _ in $(seq 1 480); do
  st="$(aws ec2 describe-images --image-ids "$AMI" --query 'Images[0].State' --output text)"
  [ "$st" = "available" ] && break
  [ "$st" != "failed" ] || die "image $AMI failed: $(aws ec2 describe-images --image-ids "$AMI" --query 'Images[0].StateReason.Message' --output text)"
  sleep "${MM_POLL_SECONDS:-30}"
done
if [ "$st" != "available" ]; then
  log "image $AMI is still $st after 4 h; nothing is running and nothing is lost."
  log "When it shows available, finish with: scripts/burst/build-image.sh $FAMILY $DATE --result $AMI"
  exit 3
fi
SNAPS="$(aws ec2 describe-images --image-ids "$AMI" --query 'Images[0].BlockDeviceMappings[].Ebs.SnapshotId' --output text)"
SNAP_GIB=0
for sn in $SNAPS; do
  g="$(aws ec2 describe-snapshots --snapshot-ids "$sn" --query 'Snapshots[0].VolumeSize' --output text)"
  SNAP_GIB=$((SNAP_GIB + g))
done

echo
echo "================ BUILD RESULT (paste this into chat) ================"
echo "family:           $FAMILY"
echo "AMI:              $AMI"
echo "root device:      $ROOT_DEV"
echo "base image:       $BASE_AMI ($IMG_NAME)"
echo "builder:          $ITYPE, $(fmt_duration $(( $(date +%s) - START ))) wall clock"
echo "snapshot size:    ${SNAP_GIB} GiB provisioned (billed on used blocks; about \$$(awk -v g="$SNAP_GIB" 'BEGIN{printf "%.2f", g*0.05}')/month at most)"
echo "runtime manifest: $RUNTIME_MANIFEST"
echo "smoke:            $(echo "$SMOKE" | jq -c '{passed, instanceType, driverAndGpu, cases: [.cases[] | {name, ok, seconds, gpuPeakMiB}]}')"
echo "BURST_AMIS entry: $FAMILY:x86_64=$AMI"
[ "$ROOT_DEV" = "/dev/sda1" ] || echo "NOTE: set BURST_ROOT_DEVICE=$ROOT_DEV in the API .env (this image's root device is not the default /dev/sda1)"
echo "====================================================================="
