#!/usr/bin/env bash
# MM126 smoke: restore family. Runs every model the app can pick, through the
# same wrapper scripts and argv as internal/services/restore_models.go and
# image_restore_models.go, on a 4 -> 32 frame PNG fixture (64x64).
# shellcheck source=smoke-lib.sh disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/smoke-lib.sh"
export HOME="${HOME_OVERRIDE:-$([ -d /opt/mm-burst-home ] && echo /opt/mm-burst-home || echo "$HOME")}"

R="$MM_ROOT"
SR="$R/venvs/restore-sr/bin/python"
n_png() { echo "[ \"\$(ls '$1'/*.png 2>/dev/null | wc -l)\" -ge $2 ]"; }
mkdir -p "$WORK/f4" "$WORK/f32"
cp "$FIX"/frames/f0[1-4].png "$WORK/f4/"
cp "$FIX"/frames/f*.png "$WORK/f32/"
# Real-ESRGAN, then SwinIR/HAT on one tile-sized frame.
case_run realesrgan 1 "$(vulkan_env)" "$(n_png "$WORK/o-esr" 4)" -- sh -c \
  "mkdir -p $WORK/o-esr && $R/bin/realesrgan-ncnn-vulkan/realesrgan-ncnn-vulkan -i $WORK/f4 -o $WORK/o-esr -n realesrgan-x4plus -s 4 -g $SMOKE_CARD -t 256 -f png"
for m in swinir hat; do
  case_run "$m" 1 "$(cuda_env)" "$(n_png "$WORK/o-$m/frames" 4)" -- \
    "$SR" "$R/scripts/restore_frames.py" --model "$m" --frames-dir "$WORK/f4" --out-dir "$WORK/o-$m" --scale 4 --tile 320 --tile-overlap 32 --gpu 0 --models-dir "$R/models/restore" --repos-dir "$R/repos"
done
case_run basicvsrpp 1 "$(cuda_env)" "$(n_png "$WORK/o-bvsr/frames" 4)" -- \
  "$R/venvs/restore-vsr-mm/bin/python" "$R/scripts/restore_video.py" --model basicvsrpp --frames-dir "$WORK/f4" --out-dir "$WORK/o-bvsr" --gpu 0 --models-dir "$R/models/restore" --repos-dir "$R/repos" --max-seq-len 16
# RVRT and VRT take fixed temporal windows; give them the 32-frame fixture.
case_run rvrt 1 "$(cuda_env)" "$(n_png "$WORK/o-rvrt/frames" 32)" -- \
  "$SR" "$R/scripts/restore_video.py" --model rvrt --frames-dir "$WORK/f32" --out-dir "$WORK/o-rvrt" --gpu 0 --models-dir "$R/models/restore" --repos-dir "$R/repos" --tile 30,128,128 --tile-overlap 2,20,20 --temp-root "$WORK"
case_run vrt 1 "$(cuda_env)" "$(n_png "$WORK/o-vrt/frames" 32)" -- \
  "$SR" "$R/scripts/restore_video.py" --model vrt --frames-dir "$WORK/f32" --out-dir "$WORK/o-vrt" --gpu 0 --models-dir "$R/models/restore" --repos-dir "$R/repos" --tile 12,128,128 --tile-overlap 2,20,20 --temp-root "$WORK"
# Faces: the fixture has no face, so the model loads and the script's own
# "No faces were detected" error is the expected, GPU-initialised outcome.
# We assert that exact message instead of an output file.
for m in gfpgan codeformer; do
  case_run "face-$m" 1 "CUDA_DEVICE_ORDER=PCI_BUS_ID" "grep -q 'No faces were detected' \"\$CASE_LOG\"" -- sh -c \
    "$R/venvs/face-restore/bin/python $R/scripts/restore_image_faces.py --model $m --input $FIX/photo.png --out-dir $WORK/o-$m --upscale 2 --models-dir $R/models/restore --repos-dir $R/repos --gpu $SMOKE_CARD; [ \$? -ne 0 ]"
done
for m in fbcnn scunet nafnet; do
  case_run "preclean-$m" 1 "CUDA_DEVICE_ORDER=PCI_BUS_ID PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True" "[ -s '$WORK/o-$m/cleaned.png' ]" -- \
    "$R/venvs/preclean/bin/python" "$R/scripts/preclean_image.py" --model "$m" --input "$FIX/photo.png" --out-dir "$WORK/o-$m" --models-dir "$R/models/restore" --repos-dir "$R/repos" --gpu "$SMOKE_CARD"
done
smoke_finish restore
