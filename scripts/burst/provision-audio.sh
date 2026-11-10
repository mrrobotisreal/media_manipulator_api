#!/usr/bin/env bash
# MM126: provision the 'audio' worker image on a builder instance.
# Venvs audio-clean, audio-separate, whisper-ct2 + the faster-whisper medium/large-v3 caches. ctranslate2 and torch are multi-arch wheels.
#   provision-audio.sh [--dry-run] [--smoke-only] [--date YYYY-MM-DD]
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MM_FAMILY=audio
# shellcheck source=provision-common.sh
. "$HERE/provision-common.sh"
parse_provision_args "$@"

provision_main true
