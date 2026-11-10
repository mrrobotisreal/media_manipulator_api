#!/usr/bin/env bash
# MM126: build one worker-image family into an AMI.
#
#   scripts/burst/build-image.sh <image|audio|video|restore> <YYYY-MM-DD export date>
#        [--instance-type g6.xlarge|g7.2xlarge] [--yes] [--dry-run]
#        [--smoke-ami ami-... ]     test an ALREADY BUILT AMI (e.g. on g7.2xlarge); no image is created
#
# Runs ON THE SERVER (or anywhere with Go, AWS CLI v2 and the profile below).
# Needs the AWS profile `mm-burst-builder` (policy scripts/burst/iam/mm-burst-builder-policy.json),
# NOT the controller key and NOT .env. Override with AWS_PROFILE.
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
ITYPE="g6.xlarge" DRY=0 SMOKE_AMI=""
API_DIR="${MM_API_DIR:-$(cd "$HERE/../.." && pwd)}"
while [ $# -gt 0 ]; do
  case "$1" in
    --instance-type) ITYPE="$2"; shift ;;
    --smoke-ami) SMOKE_AMI="$2"; shift ;;
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

aws_() { if [ "$DRY" = 1 ]; then printf 'AWS   aws %s\n' "$*" >&2; return 0; fi; aws "$@"; }

echo "== build plan: family=$FAMILY export=$DATE builder=$ITYPE mode=$([ -n "$SMOKE_AMI" ] && echo "smoke-only on $SMOKE_AMI" || echo build) =="
echo "AWS profile:   $AWS_PROFILE   region: $MM_REGION"
echo "Root volume:   ${ROOT_GIB} GiB gp3, encrypted by default (same size the burst profile launches with)"
echo "Estimated cost: about \$$EST (about $HOURS h on $ITYPE at \$$PRICE/h, plus disk), then snapshot storage ~\$0.05/GB-month"
echo "Self-shutdown: 3 h after launch (instance-initiated shutdown = terminate)"

# 1. The export must exist.
if [ -z "$SMOKE_AMI" ]; then
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
ROOT_DEV="/dev/xvda"
if [ "$DRY" != 1 ]; then
  read -r IMG_NAME ROOT_DEV < <(aws ec2 describe-images --image-ids "$BASE_AMI" --query 'Images[0].[Name,RootDeviceName]' --output text | tr '\t' ' ')
  echo "Base image:    $BASE_AMI  ($IMG_NAME)  root device $ROOT_DEV"
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
  echo "BUILD go build ./cmd/burst-ctl (linux/amd64) + tar scripts/burst -> s3://$MM_BUCKET/$BUILD_PREFIX/bundle.tgz"
else
  command -v go >/dev/null 2>&1 || export PATH="$PATH:/usr/local/go/bin"
  (cd "$API_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$BUNDLE_DIR/burst-ctl" ./cmd/burst-ctl)
  mkdir -p "$BUNDLE_DIR/burst"
  cp -R "$HERE"/. "$BUNDLE_DIR/burst/"
  mv "$BUNDLE_DIR/burst-ctl" "$BUNDLE_DIR/burst/burst-ctl"
  tar -C "$BUNDLE_DIR" -czf "$BUNDLE_DIR/bundle.tgz" burst
  aws s3 cp "$BUNDLE_DIR/bundle.tgz" "s3://$MM_BUCKET/$BUILD_PREFIX/bundle.tgz" --only-show-errors
  # Stale results from an earlier attempt would be mistaken for this one's.
  for ext in status failed ready smoke.json verified log; do
    aws s3 rm "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.$ext" >/dev/null 2>&1 || true
  done
fi

# 4. User-data (no secrets: the worker role's S3 access is the only credential).
MODE_FLAG=""
[ -n "$SMOKE_AMI" ] && MODE_FLAG="--smoke-only"
USERDATA="#!/bin/bash
# MM126 image builder bootstrap (no secrets)
shutdown -h +180
export MM_FAMILY=$FAMILY MM_DATE=$DATE MM_BUCKET=$MM_BUCKET MM_REGION=$MM_REGION AWS_DEFAULT_REGION=$MM_REGION MM_BUILD_PREFIX=$BUILD_PREFIX MM_CTL=/opt/mm-burst-build/burst/burst-ctl
mkdir -p /opt/mm-burst-build
aws s3 cp s3://$MM_BUCKET/$BUILD_PREFIX/bundle.tgz /opt/mm-burst-build/bundle.tgz --only-show-errors
tar -xzf /opt/mm-burst-build/bundle.tgz -C /opt/mm-burst-build
bash /opt/mm-burst-build/burst/provision-$FAMILY.sh --date $DATE $MODE_FLAG >> /var/log/mm-burst-provision.log 2>&1 \\
  || { tail -n 200 /var/log/mm-burst-provision.log | aws s3 cp - s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.log --only-show-errors; echo failed | aws s3 cp - s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.failed --only-show-errors; shutdown -h now; }
"

# 5. Launch: try every subnet (one per AZ) until one has capacity.
TAGS="ResourceType=instance,Tags=[{Key=mm-burst,Value=1},{Key=mm-burst-build,Value=$FAMILY},{Key=Name,Value=mm-burst-build-$FAMILY}]"
VTAGS="ResourceType=volume,Tags=[{Key=mm-burst,Value=1},{Key=mm-burst-build,Value=$FAMILY}]"
BDM="[{\"DeviceName\":\"$ROOT_DEV\",\"Ebs\":{\"VolumeSize\":$ROOT_GIB,\"VolumeType\":\"gp3\",\"DeleteOnTermination\":true,\"Encrypted\":true}}]"
if [ "$DRY" = 1 ]; then
  echo "AWS   aws ec2 run-instances --image-id $BASE_AMI --instance-type $ITYPE --subnet-id <each of $MM_SUBNETS in turn> --security-group-ids $MM_SG --iam-instance-profile Name=$MM_PROFILE --instance-initiated-shutdown-behavior terminate --metadata-options HttpTokens=required --block-device-mappings '$BDM' --tag-specifications '$TAGS' '$VTAGS' --user-data <$(printf '%s' "$USERDATA" | wc -c | tr -d ' ') bytes>"
  echo "WAIT  poll s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.{status,smoke.json,ready,failed} every 20 s, up to 170 min"
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
while :; do
  el=$(( $(date +%s) - START ))
  [ "$el" -lt 10200 ] || die "timed out after 170 min waiting for the builder"
  if aws s3api head-object --bucket "$MM_BUCKET" --key "$BUILD_PREFIX/$FAMILY.failed" >/dev/null 2>&1; then
    log "BUILD FAILED. Last log lines:"
    aws s3 cp "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.log" - 2>/dev/null | tail -n 40 >&2 || true
    aws s3 cp "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.smoke.json" - 2>/dev/null | jq -c '.cases[]? | select(.ok==false)' >&2 || true
    exit 1
  fi
  cur="$(aws s3 cp "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.status" - 2>/dev/null | tail -n 1 || true)"
  if [ -n "$cur" ] && [ "$cur" != "$last" ]; then log "builder: $cur"; last="$cur"; fi
  if [ -n "$SMOKE_AMI" ]; then
    aws s3api head-object --bucket "$MM_BUCKET" --key "$BUILD_PREFIX/$FAMILY.verified" >/dev/null 2>&1 && break
  else
    aws s3api head-object --bucket "$MM_BUCKET" --key "$BUILD_PREFIX/$FAMILY.ready" >/dev/null 2>&1 && break
  fi
  sleep 20
done
SMOKE="$(aws s3 cp "s3://$MM_BUCKET/$BUILD_PREFIX/$FAMILY.smoke.json" - | jq -c .)"
echo "$SMOKE" | jq .
if [ -n "$SMOKE_AMI" ]; then
  echo "smoke-only run on $ITYPE finished; passed=$(echo "$SMOKE" | jq .passed)"
  exit 0
fi
[ "$(echo "$SMOKE" | jq .passed)" = "true" ] || die "smoke tests did not all pass; no image created"

# 7. Image.
STAMP="$(date -u +%H%M)"
TSPEC="$(printf 'ResourceType=image,Tags=[%s] ResourceType=snapshot,Tags=[%s]' \
  "$(printf '{Key=%s,Value=%s},' mm-burst-image 1 family "$FAMILY" arch ada+blackwell runtime-manifest "$RUNTIME_MANIFEST" mm-burst-image-built "$(date -u +%F)" export-date "$DATE" base-ami "$BASE_AMI" | sed 's/,$//')" \
  "$(printf '{Key=%s,Value=%s},' mm-burst-image 1 family "$FAMILY" runtime-manifest "$RUNTIME_MANIFEST" | sed 's/,$//')")"
# shellcheck disable=SC2086  # TSPEC is two --tag-specifications words
AMI="$(aws ec2 create-image --instance-id "$INSTANCE_ID" --name "mm-burst-$FAMILY-$DATE-$STAMP" --no-reboot \
  --description "MM126 $FAMILY worker image, export $DATE, driver $(echo "$SMOKE" | jq -r .driverAndGpu | cut -d, -f1)" \
  --tag-specifications $TSPEC --query ImageId --output text)"
log "image $AMI is being created (snapshot of $ROOT_GIB GiB; a few minutes)"
for _ in $(seq 1 120); do
  st="$(aws ec2 describe-images --image-ids "$AMI" --query 'Images[0].State' --output text)"
  [ "$st" = "available" ] && break
  [ "$st" != "failed" ] || die "image $AMI failed"
  sleep 30
done
[ "$st" = "available" ] || die "image $AMI not available after 60 min"
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
[ "$ROOT_DEV" = "/dev/xvda" ] || echo "NOTE: set BURST_ROOT_DEVICE=$ROOT_DEV in the API .env (the DLAMI root device is not /dev/xvda)"
echo "====================================================================="
