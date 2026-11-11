#!/usr/bin/env bash
# MM126: keep the newest N worker images per family; deregister the rest and
# delete their snapshots. DRY RUN by default.
#
#   KEEP_AMIS="ami-aaa,ami-bbb" scripts/burst/prune-images.sh [--keep 2] [--family restore] [--yes]
#
# Needs the mm-burst-builder profile. Only touches images/snapshots tagged
# mm-burst-image=1 (the policy denies anything else). KEEP_AMIS is the list of
# AMI ids currently in BURST_AMIS: they are never removed, whatever their age.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

KEEP=2 ONLY="" YES=0
while [ $# -gt 0 ]; do
  case "$1" in
    --keep) KEEP="$2"; shift ;;
    --family) ONLY="$2"; shift ;;
    --yes) YES=1 ;;
    *) die "unknown flag $1" ;;
  esac
  shift
done
[[ "$KEEP" =~ ^[0-9]+$ ]] && [ "$KEEP" -ge 1 ] || die "--keep must be 1 or more"
if [ -n "$ONLY" ]; then valid_family "$ONLY" || die "unknown family"; fi
export AWS_PROFILE="${AWS_PROFILE:-mm-burst-builder}" AWS_DEFAULT_REGION="$MM_REGION" AWS_PAGER=""
KEEP_AMIS="${KEEP_AMIS:-}"

[ "$YES" = 1 ] || echo "DRY RUN (add --yes to act)"
total_gib=0
for fam in ${ONLY:-$MM_FAMILIES $MM_FRESH_FAMILIES}; do
  rows=()
  while IFS= read -r line; do rows+=("$line"); done < <(aws ec2 describe-images --owners self \
    --filters "Name=tag:mm-burst-image,Values=1" "Name=tag:family,Values=$fam" \
    --query 'reverse(sort_by(Images,&CreationDate))[].[ImageId,CreationDate,Name]' --output text)
  echo "== $fam: ${#rows[@]} image(s), keeping the newest $KEEP =="
  i=0
  for row in ${rows[@]+"${rows[@]}"}; do
    [ -n "$row" ] || continue
    i=$((i + 1))
    ami="$(echo "$row" | cut -f1)"
    if [ "$i" -le "$KEEP" ]; then echo "  keep   $row"; continue; fi
    case ",$KEEP_AMIS," in *",$ami,"*) echo "  KEEP   $row  (in KEEP_AMIS)"; continue ;; esac
    snaps="$(aws ec2 describe-images --image-ids "$ami" --query 'Images[0].BlockDeviceMappings[].Ebs.SnapshotId' --output text)"
    gib=0
    for sn in $snaps; do
      g="$(aws ec2 describe-snapshots --snapshot-ids "$sn" --query 'Snapshots[0].VolumeSize' --output text)"
      gib=$((gib + g))
    done
    total_gib=$((total_gib + gib))
    echo "  remove $row  (snapshots: $snaps, ${gib} GiB)"
    if [ "$YES" = 1 ]; then
      aws ec2 deregister-image --image-id "$ami"
      for sn in $snaps; do aws ec2 delete-snapshot --snapshot-id "$sn"; done
    fi
  done
done
echo "frees up to ${total_gib} GiB of snapshot (about \$$(awk -v g="$total_gib" 'BEGIN{printf "%.2f", g*0.05}')/month at most)"
