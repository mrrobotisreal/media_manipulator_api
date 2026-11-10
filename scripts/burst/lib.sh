#!/usr/bin/env bash
# MM126 shared helpers for the burst image scripts. Sourced, never executed.
# Kept to bash 3.2-compatible syntax so --dry-run also works on a Mac.
# shellcheck disable=SC2034  # constants are used by the sourcing scripts

MM_REGION="${MM_REGION:-us-west-2}"
MM_BUCKET="${MM_BUCKET:-media-manipulator}"
# Resource ids from RUNBOOK 4.19 (MM123). None of them is a secret.
MM_SUBNETS="${MM_SUBNETS:-subnet-09c4144b464a3413e,subnet-0a5cdaee4febb78d9,subnet-0ffa06d407397f990,subnet-0d76e4ea060032607}"
MM_SG="${MM_SG:-sg-005b8dbe0db36040e}"
MM_PROFILE="${MM_PROFILE:-mm-burst-worker}"
MM_SSM_PARAM="${MM_SSM_PARAM:-/aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-24.04/latest/ami-id}"
MM_FAMILIES="image audio video restore"
MM_ROOT="/opt/media-manipulator-ai"
MM_HOME="/opt/mm-burst-home"

# Root volume size (GiB) a family's burst profile launches with
# (internal/burst/profiles.go). An AMI cannot be launched with a smaller root
# than its snapshot, so the builder never exceeds this. A Go test keeps the two in sync.
profile_root_gib() {
  case "$1" in
    image) echo 100 ;;
    audio) echo 80 ;;
    video) echo 100 ;;
    restore) echo 150 ;;
    *) return 1 ;;
  esac
}

# Rough builder wall-clock hours per family (provision + smoke + snapshot), for the cost estimate.
build_hours() {
  case "$1" in
    image) echo 1.0 ;;
    audio) echo 1.0 ;;
    video) echo 0.6 ;;
    restore) echo 2.5 ;;
  esac
}

# On-demand USD/hour (design 5, third-party figures until MM128).
price_per_hour() {
  case "$1" in
    g6.xlarge) echo 0.80 ;;
    g7.2xlarge) echo 2.52 ;;
    *) echo 0 ;;
  esac
}

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

valid_family() {
  local f
  for f in $MM_FAMILIES; do [ "$f" = "$1" ] && return 0; done
  return 1
}

valid_date() { [[ "$1" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; }

confirm() {
  # confirm "question"; honours ASSUME_YES=1
  [ "${ASSUME_YES:-0}" = "1" ] && return 0
  local a
  read -r -p "$1 [y/N] " a
  [ "$a" = "y" ] || [ "$a" = "Y" ]
}

human_bytes() {
  awk -v b="$1" 'BEGIN{ split("B KiB MiB GiB TiB", u, " "); i=1; while (b>=1024 && i<5) { b/=1024; i++ } printf "%.1f %s", b, u[i] }'
}

# fmt_duration SECONDS -> "3 h 12 min"
fmt_duration() {
  awk -v s="$1" 'BEGIN{ h=int(s/3600); m=int((s-h*3600)/60); if (h>0) printf "%d h %02d min", h, m; else printf "%d min", (m<1?1:m) }'
}
