#!/usr/bin/env bash
# MM126: provision the 'video' worker image on a builder instance.
# RIFE ncnn Vulkan binary and models. No Python venv, no CUDA code.
#   provision-video.sh [--dry-run] [--smoke-only] [--date YYYY-MM-DD]
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MM_FAMILY=video
# shellcheck source=provision-common.sh
. "$HERE/provision-common.sh"
parse_provision_args "$@"

provision_main true
