#!/usr/bin/env bash
# MM126 smoke: video family (RIFE via the real frame_interpolate_rife.py).
# shellcheck source=smoke-lib.sh disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/smoke-lib.sh"

# The script is stdlib-only python; the host runs it with the system python3.
case_run rife-v4.6 1 "$(vulkan_env)" "[ \"\$(ffprobe -v error -select_streams v:0 -show_entries stream=avg_frame_rate -of csv=p=0 '$WORK/rife.mp4')\" = 48/1 ] && [ \"\$(ffprobe -v error -show_entries format=duration -of csv=p=0 '$WORK/rife.mp4' | cut -d. -f1)\" -ge 1 ]" -- \
  python3 "$MM_ROOT/scripts/frame_interpolate_rife.py" --input "$FIX/clip.mp4" --output "$WORK/rife.mp4" --target-fps 48 \
  --rife-bin "$MM_ROOT/bin/rife-ncnn-vulkan/rife-ncnn-vulkan" --rife-model "$MM_ROOT/bin/rife-ncnn-vulkan/rife-ncnn-vulkan-20221029-ubuntu/rife-v4.6" \
  --gpu "$SMOKE_CARD" --threads 1:2:2 --quality medium --max-height 720 --max-duration-seconds 120 --temp-root "$WORK"
smoke_finish video
