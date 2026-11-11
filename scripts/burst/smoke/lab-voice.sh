#!/usr/bin/env bash
# MM139 smoke: lab-voice family. One short sentence through every Lab voice model, using the real
# wrapper (scripts/lab/voice_<m>.py) inside the model's own venv, offline, on committed fixtures.
# Flags and outputs of each tool were read from its source (see the wrappers' headers).
# shellcheck source=smoke-lib.sh disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/smoke-lib.sh"
export HOME="${HOME_OVERRIDE:-$([ -d /opt/mm-burst-home ] && echo /opt/mm-burst-home || echo "$HOME")}"

LAB="$MM_ROOT/scripts/lab"
REF="$FIX/lab_ref.wav"
REFTXT="$(cat "$FIX/lab_ref.txt")"
SENTENCE="The quick brown fox jumps over the lazy dog."
OFFLINE="HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 USE_MODELSCOPE=false HOME=$HOME"

# lab_wav FILE: the check string passes when FILE is a WAV >= 1 s and FILE.json has a sample rate and a duration.
lab_wav() {
  local side="${1%.wav}.json"
  echo "$(valid_wav "$1") && jq -e '.sampleRate > 0 and .durationSeconds >= 1 and (.model | length) > 0' '$side' >/dev/null"
}

# The image must carry no Hugging Face credentials, ever (MM139: the build needs none).
case_run hf-token-absent 0 "" \
  "[ ! -e /root/.cache/huggingface/token ] && [ ! -e '$HOME/.cache/huggingface/token' ] && [ ! -e /root/.huggingface/token ] && [ -z \"\${HF_TOKEN:-}\${HUGGING_FACE_HUB_TOKEN:-}\" ]" -- true

case_run fish-s2 1 "$(cuda_env) $OFFLINE" "$(lab_wav "$WORK/fish.wav")" -- \
  "$MM_ROOT/venvs/lab-voice-fish_s2/bin/python" "$LAB/voice_fish_s2.py" --ref "$REF" --ref-text "$REFTXT" --text "[excited] $SENTENCE" --out "$WORK/fish.wav"

case_run indextts2-v2 1 "$(cuda_env) $OFFLINE" "$(lab_wav "$WORK/idx2.wav")" -- \
  "$MM_ROOT/venvs/lab-voice-indextts2/bin/python" "$LAB/voice_indextts2.py" --version 2 --ref "$REF" --text "$SENTENCE" --emo-vector "0,0,0,0,0,0,0.5,0.3" --out "$WORK/idx2.wav"

case_run indextts2-v25 1 "$(cuda_env) $OFFLINE" "$(lab_wav "$WORK/idx25.wav")" -- \
  "$MM_ROOT/venvs/lab-voice-indextts2/bin/python" "$LAB/voice_indextts2.py" --version 2.5 --lang EN --duration-factor 1.0 --ref "$REF" --text "$SENTENCE" --out "$WORK/idx25.wav"

case_run seedvc 1 "$(cuda_env) $OFFLINE" "$(lab_wav "$WORK/svc.wav")" -- \
  "$MM_ROOT/venvs/lab-voice-seedvc/bin/python" "$LAB/voice_seedvc.py" --source "$FIX/speech.wav" --ref "$REF" --diffusion-steps 10 --out "$WORK/svc.wav"

case_run seedvc-f0 1 "$(cuda_env) $OFFLINE" "$(lab_wav "$WORK/svcf0.wav")" -- \
  "$MM_ROOT/venvs/lab-voice-seedvc/bin/python" "$LAB/voice_seedvc.py" --source "$FIX/speech.wav" --ref "$REF" --diffusion-steps 10 --f0-condition true --out "$WORK/svcf0.wav"

case_run cosyvoice3 1 "$(cuda_env) $OFFLINE" "$(lab_wav "$WORK/cosy.wav")" -- \
  "$MM_ROOT/venvs/lab-voice-cosyvoice3/bin/python" "$LAB/voice_cosyvoice3.py" --ref "$REF" --ref-text "$REFTXT" --text "$SENTENCE" --out "$WORK/cosy.wav"

smoke_finish lab-voice
