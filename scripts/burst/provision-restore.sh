#!/usr/bin/env bash
# MM126: provision the 'restore' worker image on a builder instance.
# Venvs restore-sr, restore-vsr-mm, face-restore, preclean. Rebuilds mmcv (v2.1.0, the host version) for both architectures and pre-builds the RVRT JIT extension.
#   provision-restore.sh [--dry-run] [--smoke-only] [--date YYYY-MM-DD]
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MM_FAMILY=restore
# shellcheck source=provision-common.sh
. "$HERE/provision-common.sh"
parse_provision_args "$@"
restore_rebuild() { step_rebuild_mmcv; step_prebuild_rvrt_ext; }
provision_main restore_rebuild
