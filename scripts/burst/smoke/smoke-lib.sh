#!/usr/bin/env bash
# MM126 smoke-test helpers. Sourced by smoke/<family>.sh.
#
# Each case runs one workload's REAL command (same argv as the Go code) on a
# committed tiny fixture and records, into $SMOKE_OUT:
#   exit code, a validity check on the output (identify / ffprobe / grep),
#   seconds, and the peak extra GPU memory seen by nvidia-smi while it ran
#   (a silent CPU fallback shows ~0 and FAILS).
# The JSON's per-item seconds are MM128's first data points.
#
# Env: SMOKE_CARD (default 0) = physical GPU index to pin and to sample;
#      SMOKE_OUT  = result json path; SMOKE_MIN_GPU_MIB (default 40);
#      SMOKE_KEEP=1 keeps the work dir.
# shellcheck disable=SC2034
set -uo pipefail

SMOKE_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIX="$SMOKE_HERE/fixtures"
MM_ROOT="/opt/media-manipulator-ai"
SMOKE_CARD="${SMOKE_CARD:-0}"
SMOKE_OUT="${SMOKE_OUT:-/tmp/mm-smoke.json}"
SMOKE_MIN_GPU_MIB="${SMOKE_MIN_GPU_MIB:-40}"
WORK="$(mktemp -d /tmp/mm-smoke.XXXXXX)"
CASES_FILE="$WORK/cases.jsonl"
: > "$CASES_FILE"
FAILS=0

smoke_cleanup() { if [ "${SMOKE_KEEP:-0}" != "1" ]; then rm -rf "${WORK:?}"; fi; }
trap smoke_cleanup EXIT

gpu_used() { nvidia-smi -i "$SMOKE_CARD" --query-gpu=memory.used --format=csv,noheader,nounits 2>/dev/null | sed -n '1p' | tr -d ' '; }

# gpu_env prints the env the Go callers set for a CUDA tool.
cuda_env() { echo "CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES=$SMOKE_CARD PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True"; }
vulkan_env() { echo "VK_ICD_FILENAMES=$MM_ROOT/vulkan/nvidia_icd_egl.json VK_DRIVER_FILES=$MM_ROOT/vulkan/nvidia_icd_egl.json VK_LOADER_LAYERS_DISABLE=* CUDA_DEVICE_ORDER=PCI_BUS_ID"; }

# case_run NAME GPU(1|0) "ENV..." CHECK_CMD -- COMMAND...
#   CHECK_CMD is evaluated after the command with $OUT-style paths already
#   baked in by the caller (an `sh -c` string); exit 0 = valid output.
case_run() {
  local name="$1" needgpu="$2" envs="$3" check="$4"
  shift 5
  local log="$WORK/$name.log" base peak cur pid rc t0 t1 ok=true why=""
  base="$(gpu_used || echo 0)"; peak="$base"
  t0="$(date +%s.%N)"
  # shellcheck disable=SC2086
  env $envs "$@" >"$log" 2>&1 &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    cur="$(gpu_used || echo 0)"
    if [ -n "$cur" ] && [ "$cur" -gt "$peak" ] 2>/dev/null; then peak="$cur"; fi
    sleep 0.1
  done
  wait "$pid"; rc=$?
  t1="$(date +%s.%N)"
  local extra=$(( ${peak:-0} - ${base:-0} ))
  if [ "$rc" -ne 0 ]; then ok=false; why="exit $rc"; fi
  if $ok && ! sh -c "$check" >>"$log" 2>&1; then ok=false; why="output check failed"; fi
  if $ok && [ "$needgpu" = "1" ] && [ "$extra" -lt "$SMOKE_MIN_GPU_MIB" ]; then
    ok=false; why="no GPU use (peak +${extra} MiB < ${SMOKE_MIN_GPU_MIB}): silent CPU fallback?"
  fi
  $ok || FAILS=$((FAILS + 1))
  jq -cn --arg name "$name" --argjson ok "$ok" --arg why "$why" --argjson rc "$rc" \
    --argjson sec "$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f", b-a}')" --argjson gpu "$extra" \
    --arg tail "$(tail -n 8 "$log" | tr -d '\000' | cut -c1-300)" \
    '{name: $name, ok: $ok, seconds: $sec, gpuPeakMiB: $gpu, exit: $rc} + (if $ok then {} else {why: $why, logTail: $tail} end)' >> "$CASES_FILE"
  printf '%-22s %s  %6.2fs  gpu+%s MiB  %s\n' "$name" "$($ok && echo PASS || echo FAIL)" \
    "$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f", b-a}')" "$extra" "$why" >&2
}

smoke_finish() {
  local family="$1"
  local drv cuda ff
  drv="$(nvidia-smi --query-gpu=driver_version,name --format=csv,noheader | sed -n '1p')"
  cuda="$(nvidia-smi 2>/dev/null | sed -n 's/.*CUDA Version: \([0-9.]*\).*/\1/p' | sed -n '1p')"
  ff="$(ffmpeg -version 2>/dev/null | sed -n '1p' | cut -d' ' -f3)"
  jq -s --arg family "$family" --arg drv "$drv" --arg cuda "$cuda" --arg ff "$ff" --arg date "$(date -u +%FT%TZ)" \
    --arg itype "$(curl -fsS -m 2 http://169.254.169.254/latest/meta-data/instance-type 2>/dev/null || echo host)" --argjson fails "$FAILS" \
    '{family: $family, passed: ($fails == 0), failures: $fails, instanceType: $itype, driverAndGpu: $drv, cudaDriver: $cuda, ffmpeg: $ff, at: $date, cases: .}' \
    "$CASES_FILE" > "$SMOKE_OUT"
  echo "smoke: $family $([ "$FAILS" -eq 0 ] && echo ALL-PASS || echo "$FAILS FAILED") -> $SMOKE_OUT" >&2
  [ "$FAILS" -eq 0 ]
}

# Small helpers for the check strings.
is_png() { echo "identify -format %w x '$1' >/dev/null 2>&1 && [ \"\$(identify -format %w '$1' | sed -n '1p')\" -gt 0 ]"; }
