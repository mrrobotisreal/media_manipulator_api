#!/usr/bin/env bash
# MM126 smoke: audio family (DeepFilterNet, Demucs, faster-whisper small -> medium).
# shellcheck source=smoke-lib.sh disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/smoke-lib.sh"
export HOME="${HOME_OVERRIDE:-$([ -d /opt/mm-burst-home ] && echo /opt/mm-burst-home || echo "$HOME")}"

mkdir -p "$WORK/df-in" "$WORK/df" "$WORK/dm"
ffmpeg -v error -y -i "$FIX/speech.wav" -ac 1 -ar 48000 "$WORK/df-in/in48.wav"
# Same shape as runDeepFilter in ai_tools.go: the input in one directory, the
# enhanced file in another (named after the model version, so it is looked up).
# A deepFilter whose save_audio ignores --output-dir overwrites the input and
# writes nothing here; that was the host's state until 2026-11-10 and it
# fails this case. The fix is scripts/server/patch_deepfilter_io.py.
case_run deepfilter 1 "$(cuda_env)" "$(valid_wav_in "$WORK/df")" -- \
  "$MM_ROOT/venvs/audio-clean/bin/deepFilter" --output-dir "$WORK/df" "$WORK/df-in/in48.wav"
case_run demucs 1 "$(cuda_env)" "ls '$WORK'/dm/htdemucs/*/vocals.wav >/dev/null" -- \
  "$MM_ROOT/venvs/audio-separate/bin/demucs" -n htdemucs --two-stems=vocals --segment 7 -o "$WORK/dm" "$FIX/speech.wav"
# The host only has medium and large-v3 cached (no "small"); medium is the API default model.
mkdir -p "$WORK/wh"
case_run whisper-medium 1 "$(cuda_env) HF_HOME=/var/lib/creatv/hf HF_HUB_OFFLINE=1" "grep -q '\"text\"' '$WORK'/wh/*.json" -- \
  /opt/creatv/whisper-ct2/bin/whisper-ctranslate2 "$FIX/speech.wav" --model medium --device cuda --device_index 0 --compute_type float16 --vad_filter False --output_format json --output_dir "$WORK/wh"
smoke_finish audio
