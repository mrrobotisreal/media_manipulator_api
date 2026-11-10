#!/usr/bin/env bash
# MM126 smoke: image family (rembg, upscale, redact, LaMa).
# shellcheck source=smoke-lib.sh disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/smoke-lib.sh"
export HOME="${HOME_OVERRIDE:-$([ -d /opt/mm-burst-home ] && echo /opt/mm-burst-home || echo "$HOME")}"

# rembg: the exact bash -lc script shape the Go code uses.
case_run rembg 1 "" "$(is_png "$WORK/rembg.png")" -- bash -lc \
  "set -e; source $MM_ROOT/env/onnxruntime-cuda-bg-remove.sh; export CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES=$SMOKE_CARD; U2NET_HOME=$MM_ROOT/models/rembg $MM_ROOT/venvs/bg-remove/bin/rembg i -m isnet-general-use $FIX/photo.png $WORK/rembg.png"
# onnxruntime must list the CUDA provider (a CPU-only wheel would pass the case above slowly).
case_run ort-providers 0 "$(cuda_env)" "true" -- sh -c \
  "$MM_ROOT/venvs/bg-remove/bin/python -c \"import onnxruntime as o,sys; p=o.get_available_providers(); print(p); sys.exit(0 if 'CUDAExecutionProvider' in p else 1)\""
case_run upscale-x4 1 "$(vulkan_env)" "$(is_png "$WORK/up.png")" -- \
  "$MM_ROOT/bin/realesrgan-ncnn-vulkan/realesrgan-ncnn-vulkan" -i "$FIX/photo.png" -o "$WORK/up.png" -n realesrgan-x4plus -s 4 -t 0 -g "$SMOKE_CARD" -f png
case_run redact-text 1 "$(cuda_env)" "$(is_png "$WORK/redact.png") && grep -q 'redactions=[1-9]' '$WORK/redact.log'" -- \
  "$MM_ROOT/venvs/vision-privacy/bin/python" "$MM_ROOT/scripts/redact_text_pii.py" --input "$FIX/text.png" --output "$WORK/redact.png" --detect pii --redaction blackbox --gpu
case_run lama 1 "$(cuda_env)" "$(is_png "$WORK/lama.png")" -- \
  "$MM_ROOT/venvs/inpaint/bin/python" "$MM_ROOT/scripts/remove_object_lama.py" --input "$FIX/photo.png" --mask "$FIX/mask.png" --output "$WORK/lama.png"
smoke_finish image
