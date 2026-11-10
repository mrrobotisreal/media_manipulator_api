#!/usr/bin/env bash
# MM126: provision the 'image' worker image on a builder instance.
# Venvs bg-remove, vision-privacy, inpaint; ncnn Vulkan Real-ESRGAN; models. No source-compiled CUDA code (torch, onnxruntime-gpu are multi-arch wheels).
#   provision-image.sh [--dry-run] [--smoke-only] [--date YYYY-MM-DD]
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MM_FAMILY=image
# shellcheck source=provision-common.sh
. "$HERE/provision-common.sh"
parse_provision_args "$@"

provision_main true
