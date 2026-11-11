#!/usr/bin/env bash
# MM139: provision the 'lab-voice' worker image on a builder instance, FRESH.
#
# Unlike the four product families nothing is copied from the host: these models
# are not installed there. This script installs system packages, one uv-managed
# venv per model (so dependencies never clash), the models' code at pinned
# commits, and the weights at pinned Hugging Face revisions, then records what it
# installed (versions.json per model) and fingerprints that as the runtime manifest.
#
#   provision-lab-voice.sh [--dry-run] [--smoke-only] [--date YYYY-MM-DD]
#
# Layout (only scripts/ venvs/ models/ repos/ are subtrees the burst executor lets a Spec name):
#   $MM_ROOT/scripts/lab/voice_<m>.py     the thin wrappers (scripts/lab/ in the API repo)
#   $MM_ROOT/venvs/lab-voice-<m>/         one venv per model
#   $MM_ROOT/repos/lab-voice/<m>/         the model's code at a pinned commit
#   $MM_ROOT/models/lab-voice/<m>/        weights + versions.json
#
# Nothing here needs a Hugging Face token: every checkpoint used is public and
# un-gated (verified 2026-11-11, see the MM139 ticket). The script still unsets every
# token variable, removes ~/.cache/huggingface/token and asserts it is gone.
# shellcheck disable=SC2030,SC2031  # HOME is set per command (VAR=x cmd), never exported globally
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MM_FAMILY=lab-voice
# shellcheck source=provision-common.sh
. "$HERE/provision-common.sh"
parse_provision_args "$@"

LAB_ROOT_DIR="$MM_ROOT/models/lab-voice"
LAB_REPOS_DIR="$MM_ROOT/repos/lab-voice"
LAB_VENVS_DIR="$MM_ROOT/venvs"
LAB_WRAPPERS_SRC="${LAB_WRAPPERS_SRC:-$PHERE/../lab}"
LAB_TOOLS=/opt/mm-build/tools
UV_VERSION=0.13.7
HUB_VERSION=0.35.3
export UV_PYTHON_INSTALL_DIR="$MM_ROOT/python" UV_CACHE_DIR=/opt/mm-build/uv-cache UV_LINK_MODE=copy

# Pins, all read from the primary sources on 2026-11-11 (MM139 ticket log).
FISH_REPO=https://github.com/fishaudio/fish-speech.git FISH_COMMIT=0f8a12e8a651062fdee9c33a894bdccd3a8513f1
INDEX_REPO=https://github.com/index-tts/index-tts.git INDEX_COMMIT=d9e41aac89fd00b3d71497fddb287b7f24613712
SEEDVC_REPO=https://github.com/Plachtaa/seed-vc.git SEEDVC_COMMIT=51383efd921027683c89e5348211d93ff12ac2a8
COSY_REPO=https://github.com/QwenAudio/CosyVoice.git COSY_COMMIT=074ca6dc9e80a2f424f1f74b48bdd7d3fea531cc
REV_FISH=1de9996b6be38b745688de084d87a5633f714e4e          # fishaudio/s2-pro
REV_INDEX2=740dcaff396282ffb241903d150ac011cd4b1ede        # IndexTeam/IndexTTS-2
REV_INDEX25=c39ce5ba981572cb187443877ff559dfb246ce63       # IndexTeam/IndexTTS-2.5
REV_W2VBERT=da985ba0987f70aaeb84a80f2851cfac8c697a7b       # facebook/w2v-bert-2.0
REV_MASKGCT=265c6cef07625665d0c28d2faafb1415562379dc       # amphion/MaskGCT (semantic codec only)
REV_CAMPPLUS=e4b6ede7ce16997aff4ae69fbca1f0175e2afede      # funasr/campplus
REV_BIGVGAN22=633ff708ed5b74903e86ff1298cf4a98e921c513     # nvidia/bigvgan_v2_22khz_80band_256x
REV_BIGVGAN44=95a9d1dcb12906c03edd938d77b9333d6ded7dfb     # nvidia/bigvgan_v2_44khz_128band_512x
REV_WHISPER=973afd24965f72e36ca33b3055d56a652f456b4d       # openai/whisper-small
REV_SEEDVC=257283f9f41585055e8f858fba4fd044e5caed6e        # Plachta/Seed-VC
REV_RMVPE=e6d0c1a17da07c33557852f9dfa2bd44cc75737d         # lj1995/VoiceConversionWebUI
REV_COSY=29e01c4e8d000f4bcd70751be16fa94bf3d85a18          # FunAudioLLM/Fun-CosyVoice3-0.5B-2512

scrub_hf_token() {
  unset HF_TOKEN HUGGING_FACE_HUB_TOKEN HUGGINGFACE_TOKEN HF_API_TOKEN HUGGINGFACE_HUB_TOKEN
  rm -f /root/.cache/huggingface/token /root/.huggingface/token "$MM_HOME/.cache/huggingface/token" 2>/dev/null || true
}

assert_no_hf_token() {
  local t
  for t in /root/.cache/huggingface/token /root/.huggingface/token "$MM_HOME/.cache/huggingface/token"; do
    [ ! -e "$t" ] || fail_build "a Hugging Face token file is present at $t (it must never be in the image)"
  done
  if [ -n "${HF_TOKEN:-}${HUGGING_FACE_HUB_TOKEN:-}${HUGGINGFACE_TOKEN:-}${HF_API_TOKEN:-}" ]; then
    fail_build "a Hugging Face token variable is set in the build environment"
  fi
}

# hf_get REPO REVISION DEST [include globs...]: pinned download into a plain directory.
hf_get() {
  local repo="$1" rev="$2" dest="$3"
  shift 3
  # --include takes a LIST after one flag (nargs="*"); repeating the flag keeps only the last, so pass it once.
  local inc=()
  [ $# -eq 0 ] || inc=(--include "$@")
  status "weights: $repo@${rev%"${rev#????????}"} -> $dest"
  "$LAB_TOOLS/bin/hf" download "$repo" --revision "$rev" --local-dir "$dest" ${inc[@]+"${inc[@]}"} --quiet >/dev/null
}

# hf_cache_get REPO REVISION CACHE_DIR [include globs...]: pinned download into an HF cache layout,
# plus refs/main so an offline `revision="main"` lookup resolves to the pinned snapshot (Seed-VC's
# own code asks for main with cache_dir=./checkpoints and HF_HUB_CACHE=./checkpoints/hf_cache).
hf_cache_get() {
  local repo="$1" rev="$2" cache="$3"
  shift 3
  local inc=() key
  [ $# -eq 0 ] || inc=(--include "$@")
  status "weights (cache): $repo@${rev%"${rev#????????}"} -> $cache"
  "$LAB_TOOLS/bin/hf" download "$repo" --revision "$rev" --cache-dir "$cache" ${inc[@]+"${inc[@]}"} --quiet >/dev/null
  key="models--${repo//\//--}"
  mkdir -p "$cache/$key/refs"
  printf '%s' "$rev" > "$cache/$key/refs/main"
}

git_pin() {
  # git_pin URL COMMIT DEST: clone and check out exactly COMMIT (the repo's own submodules, if any, are initialised).
  local url="$1" commit="$2" dest="$3"
  rm -rf "$dest"
  git clone -q "$url" "$dest"
  git -C "$dest" checkout -q "$commit"
  [ "$(git -C "$dest" rev-parse HEAD)" = "$commit" ] || fail_build "$url is not at $commit"
  if [ -f "$dest/.gitmodules" ]; then git -C "$dest" submodule update -q --init; fi
}

record_versions() {
  # record_versions MODEL REPO_URL COMMIT VENV PYTHON_VERSION "repo@rev ..." : versions.json for one model.
  local model="$1" url="$2" commit="$3" venv="$4" pyver="$5" weights="$6"
  local freeze="$LAB_ROOT_DIR/$model/pip-freeze.txt" fh torchv
  mkdir -p "$LAB_ROOT_DIR/$model"
  "$LAB_TOOLS/bin/uv" pip freeze --python "$venv/bin/python" > "$freeze"
  fh="$(sha256sum "$freeze" | cut -d' ' -f1)"
  torchv="$("$venv/bin/python" -c 'import torch; print(torch.__version__)')"
  jq -n --arg model "$model" --arg url "$url" --arg commit "$commit" --arg fh "$fh" --arg py "$pyver" --arg torch "$torchv" \
    --arg weights "$weights" --arg at "$(date -u +%FT%TZ)" \
    '{model: $model, repo: $url, repoCommit: $commit, pipFreezeSha256: $fh, python: $py, torch: $torchv, weights: ($weights | split(" ")), builtAt: $at}' \
    > "$LAB_ROOT_DIR/$model/versions.json"
  # versions.json must be deterministic input to the manifest: builtAt is informational, so it is dropped when hashing.
  jq 'del(.builtAt)' "$LAB_ROOT_DIR/$model/versions.json" > "$LAB_ROOT_DIR/$model/versions.json.tmp" && mv "$LAB_ROOT_DIR/$model/versions.json.tmp" "$LAB_ROOT_DIR/$model/versions.json"
}

step_lab_tools() {
  status "tools: uv $UV_VERSION + huggingface_hub $HUB_VERSION"
  plan "python3 -m venv $LAB_TOOLS; pip install uv==$UV_VERSION huggingface-hub[hf-xet]==$HUB_VERSION (the 'hf' downloader); apt: build-essential portaudio19-dev libsox-dev (Fish Speech's pyaudio/sox); UV_PYTHON_INSTALL_DIR=$UV_PYTHON_INSTALL_DIR UV_LINK_MODE=copy" && return 0
  scrub_hf_token
  apt-get install -y -qq build-essential portaudio19-dev libsox-dev libsndfile1 >/dev/null
  mkdir -p /opt/mm-build
  python3 -m venv "$LAB_TOOLS"
  "$LAB_TOOLS/bin/pip" install -q "uv==$UV_VERSION" "huggingface-hub[hf-xet]==$HUB_VERSION"
  mkdir -p "$LAB_ROOT_DIR" "$LAB_REPOS_DIR" "$MM_ROOT/scripts/lab"
}

step_install_fish_s2() {
  status "fish_s2: venv (python 3.12, torch 2.8.0 cu128 from the repo's uv.lock) + weights"
  plan "git clone $FISH_REPO @ $FISH_COMMIT -> $LAB_REPOS_DIR/fish_s2; UV_PROJECT_ENVIRONMENT=$LAB_VENVS_DIR/lab-voice-fish_s2 uv sync --frozen --python 3.12 --extra cu128; hf download fishaudio/s2-pro@$REV_FISH -> $LAB_ROOT_DIR/fish_s2/s2-pro (11 GB, un-gated); record versions.json" && return 0
  local repo="$LAB_REPOS_DIR/fish_s2" venv="$LAB_VENVS_DIR/lab-voice-fish_s2"
  git_pin "$FISH_REPO" "$FISH_COMMIT" "$repo"
  (cd "$repo" && UV_PROJECT_ENVIRONMENT="$venv" "$LAB_TOOLS/bin/uv" sync --frozen --no-dev --python 3.12 --extra cu128) || fail_build "fish_s2: uv sync failed (see $LOGFILE)"
  hf_get fishaudio/s2-pro "$REV_FISH" "$LAB_ROOT_DIR/fish_s2/s2-pro"
  [ -f "$LAB_ROOT_DIR/fish_s2/s2-pro/codec.pth" ] || fail_build "fish_s2: codec.pth missing after the download"
  record_versions fish_s2 "$FISH_REPO" "$FISH_COMMIT" "$venv" 3.12 "fishaudio/s2-pro@$REV_FISH"
}

step_install_indextts2() {
  status "indextts2: venv (python 3.11, torch 2.8.0 cu128 from the repo's uv.lock) + weights for v2 and v2.5"
  plan "git clone $INDEX_REPO @ $INDEX_COMMIT -> $LAB_REPOS_DIR/indextts2; UV_PROJECT_ENVIRONMENT=$LAB_VENVS_DIR/lab-voice-indextts2 uv sync --frozen --no-dev --python 3.11; hf download IndexTeam/IndexTTS-2@$REV_INDEX2 -> checkpoints_2, IndexTeam/IndexTTS-2.5@$REV_INDEX25 -> checkpoints_25; aux models pre-placed in checkpoints_2/hf_cache (w2v-bert-2.0@$REV_W2VBERT, MaskGCT semantic codec@$REV_MASKGCT, campplus@$REV_CAMPPLUS, bigvgan 22k@$REV_BIGVGAN22), checkpoints_25/hf_cache is a symlink to it; warm g2p_en (nltk data) with HOME=$MM_HOME; record versions.json" && return 0
  local repo="$LAB_REPOS_DIR/indextts2" venv="$LAB_VENVS_DIR/lab-voice-indextts2" w="$LAB_ROOT_DIR/indextts2"
  git_pin "$INDEX_REPO" "$INDEX_COMMIT" "$repo"
  (cd "$repo" && UV_PROJECT_ENVIRONMENT="$venv" "$LAB_TOOLS/bin/uv" sync --frozen --no-dev --python 3.11) || fail_build "indextts2: uv sync failed (see $LOGFILE)"
  hf_get IndexTeam/IndexTTS-2 "$REV_INDEX2" "$w/checkpoints_2"
  hf_get IndexTeam/IndexTTS-2.5 "$REV_INDEX25" "$w/checkpoints_25"
  local aux="$w/checkpoints_2/hf_cache"
  mkdir -p "$aux/bigvgan"
  hf_get facebook/w2v-bert-2.0 "$REV_W2VBERT" "$aux/w2v-bert-2.0" "config.json" "model.safetensors" "preprocessor_config.json"
  hf_get amphion/MaskGCT "$REV_MASKGCT" "$w/.maskgct" "semantic_codec/model.safetensors"
  cp "$w/.maskgct/semantic_codec/model.safetensors" "$aux/semantic_codec_model.safetensors"
  rm -rf "$w/.maskgct"
  hf_get funasr/campplus "$REV_CAMPPLUS" "$w/.campplus" "campplus_cn_common.bin"
  cp "$w/.campplus/campplus_cn_common.bin" "$aux/campplus_cn_common.bin"
  rm -rf "$w/.campplus"
  hf_get nvidia/bigvgan_v2_22khz_80band_256x "$REV_BIGVGAN22" "$aux/bigvgan" "config.json" "bigvgan_generator.pt"
  ln -sfn ../checkpoints_2/hf_cache "$w/checkpoints_25/hf_cache"
  [ -f "$w/checkpoints_2/gpt.pth" ] && [ -f "$w/checkpoints_25/gpt.pth" ] || fail_build "indextts2: gpt.pth missing after the downloads"
  HOME="$MM_HOME" "$venv/bin/python" -c "import g2p_en" || fail_build "indextts2: g2p_en does not import (nltk data)"
  record_versions indextts2 "$INDEX_REPO" "$INDEX_COMMIT" "$venv" 3.11 \
    "IndexTeam/IndexTTS-2@$REV_INDEX2 IndexTeam/IndexTTS-2.5@$REV_INDEX25 facebook/w2v-bert-2.0@$REV_W2VBERT amphion/MaskGCT@$REV_MASKGCT funasr/campplus@$REV_CAMPPLUS nvidia/bigvgan_v2_22khz_80band_256x@$REV_BIGVGAN22"
}

step_install_seedvc() {
  status "seedvc: venv (python 3.10, torch 2.8.0 cu128; the repo's requirements.txt pins torch 2.4 which has no sm_120) + weights"
  plan "git clone $SEEDVC_REPO @ $SEEDVC_COMMIT -> $LAB_REPOS_DIR/seedvc; uv venv --python 3.10; uv pip install torch==2.8.0 torchaudio==2.8.0 (cu128 index) then the packages inference.py imports (numpy 1.26.4, scipy 1.13.1, librosa 0.10.2, transformers 4.46.3, descript-audio-codec 1.0.0, munch, einops, matplotlib, pyyaml, soundfile, tqdm, huggingface-hub); weights into the repo's ./checkpoints HF cache layout with refs/main pinned: Plachta/Seed-VC@$REV_SEEDVC (small wavenet 22k + base f0 44k v2 + configs), lj1995/VoiceConversionWebUI@$REV_RMVPE rmvpe.pt, funasr/campplus@$REV_CAMPPLUS, bigvgan 22k@$REV_BIGVGAN22 + 44k@$REV_BIGVGAN44, openai/whisper-small@$REV_WHISPER; one online warm-up conversion fills anything the pins missed" && return 0
  local repo="$LAB_REPOS_DIR/seedvc" venv="$LAB_VENVS_DIR/lab-voice-seedvc" ck cc
  git_pin "$SEEDVC_REPO" "$SEEDVC_COMMIT" "$repo"
  "$LAB_TOOLS/bin/uv" venv --python 3.10 "$venv" -q
  "$LAB_TOOLS/bin/uv" pip install --python "$venv/bin/python" -q --index-url https://download.pytorch.org/whl/cu128 "torch==2.8.0" "torchaudio==2.8.0" \
    || fail_build "seedvc: torch cu128 install failed"
  "$LAB_TOOLS/bin/uv" pip install --python "$venv/bin/python" -q "numpy==1.26.4" "scipy==1.13.1" "librosa==0.10.2" "transformers==4.46.3" \
    "descript-audio-codec==1.0.0" "munch==4.0.0" "einops==0.8.0" matplotlib pyyaml "soundfile==0.12.1" tqdm "huggingface-hub==$HUB_VERSION" \
    || fail_build "seedvc: dependency install failed (see $LOGFILE)"
  ck="$repo/checkpoints"
  cc="$ck/hf_cache"
  mkdir -p "$ck" "$cc"
  hf_cache_get Plachta/Seed-VC "$REV_SEEDVC" "$ck" "DiT_seed_v2_uvit_whisper_small_wavenet_bigvgan_pruned.pth" "config_dit_mel_seed_uvit_whisper_small_wavenet.yml" \
    "DiT_seed_v2_uvit_whisper_base_f0_44k_bigvgan_pruned_ft_ema_v2.pth" "config_dit_mel_seed_uvit_whisper_base_f0_44k.yml"
  hf_cache_get lj1995/VoiceConversionWebUI "$REV_RMVPE" "$ck" "rmvpe.pt"
  hf_cache_get funasr/campplus "$REV_CAMPPLUS" "$ck" "campplus_cn_common.bin"
  hf_cache_get nvidia/bigvgan_v2_22khz_80band_256x "$REV_BIGVGAN22" "$cc" "config.json" "bigvgan_generator.pt"
  hf_cache_get nvidia/bigvgan_v2_44khz_128band_512x "$REV_BIGVGAN44" "$cc" "config.json" "bigvgan_generator.pt"
  hf_cache_get openai/whisper-small "$REV_WHISPER" "$cc" "config.json" "preprocessor_config.json" "model.safetensors" "generation_config.json"
  # One ONLINE warm-up conversion per mode (the smoke later runs offline): any file the pins missed is fetched now.
  local fx="$PHERE/smoke/fixtures" warm=/opt/mm-build/seedvc-warm
  mkdir -p "$warm"
  HOME="$MM_HOME" HF_HUB_OFFLINE=0 CUDA_VISIBLE_DEVICES=0 "$venv/bin/python" "$MM_ROOT/scripts/lab/voice_seedvc.py" --source "$fx/speech.wav" --ref "$fx/lab_ref.wav" \
    --out "$warm/a.wav" --diffusion-steps 4 || fail_build "seedvc: warm-up conversion failed"
  HOME="$MM_HOME" HF_HUB_OFFLINE=0 CUDA_VISIBLE_DEVICES=0 "$venv/bin/python" "$MM_ROOT/scripts/lab/voice_seedvc.py" --source "$fx/speech.wav" --ref "$fx/lab_ref.wav" \
    --out "$warm/b.wav" --diffusion-steps 4 --f0-condition true || fail_build "seedvc: warm-up f0 conversion failed"
  rm -rf "$warm"
  record_versions seedvc "$SEEDVC_REPO" "$SEEDVC_COMMIT" "$venv" 3.10 \
    "Plachta/Seed-VC@$REV_SEEDVC lj1995/VoiceConversionWebUI@$REV_RMVPE funasr/campplus@$REV_CAMPPLUS nvidia/bigvgan_v2_22khz_80band_256x@$REV_BIGVGAN22 nvidia/bigvgan_v2_44khz_128band_512x@$REV_BIGVGAN44 openai/whisper-small@$REV_WHISPER"
}

step_install_cosyvoice3() {
  status "cosyvoice3: venv (python 3.10, torch 2.8.0 cu128; the repo pins torch 2.3.1 cu121, no sm_120) + weights"
  plan "git clone $COSY_REPO @ $COSY_COMMIT (+ the Matcha-TTS submodule) -> $LAB_REPOS_DIR/cosyvoice3; uv venv --python 3.10; torch 2.8.0 cu128, then the packages cosyvoice/ and Matcha-TTS import (onnxruntime-gpu 1.22.0, conformer, diffusers, hyperpyyaml, inflect, librosa, lightning, modelscope, openai-whisper, transformers 4.51.3, wetext, x-transformers, ...); hf download FunAudioLLM/Fun-CosyVoice3-0.5B-2512@$REV_COSY without the TensorRT/batch onnx extras -> $LAB_ROOT_DIR/cosyvoice3/Fun-CosyVoice3-0.5B" && return 0
  local repo="$LAB_REPOS_DIR/cosyvoice3" venv="$LAB_VENVS_DIR/lab-voice-cosyvoice3"
  git_pin "$COSY_REPO" "$COSY_COMMIT" "$repo"
  "$LAB_TOOLS/bin/uv" venv --python 3.10 "$venv" -q
  "$LAB_TOOLS/bin/uv" pip install --python "$venv/bin/python" -q --index-url https://download.pytorch.org/whl/cu128 "torch==2.8.0" "torchaudio==2.8.0" \
    || fail_build "cosyvoice3: torch cu128 install failed"
  "$LAB_TOOLS/bin/uv" pip install --python "$venv/bin/python" -q "numpy==1.26.4" "scipy==1.13.1" "librosa==0.10.2" "transformers==4.51.3" \
    "onnxruntime-gpu==1.22.0" "conformer==0.3.2" "diffusers==0.29.0" "HyperPyYAML==1.2.3" "inflect==7.3.1" "lightning==2.2.4" "modelscope==1.20.0" \
    "omegaconf==2.3.0" "hydra-core==1.3.2" rich "openai-whisper==20231117" "x-transformers==2.11.24" "wetext==0.0.4" einops regex "soundfile==0.12.1" tqdm pyyaml "protobuf==4.25" \
    || fail_build "cosyvoice3: dependency install failed (see $LOGFILE)"
  hf_get FunAudioLLM/Fun-CosyVoice3-0.5B-2512 "$REV_COSY" "$LAB_ROOT_DIR/cosyvoice3/Fun-CosyVoice3-0.5B" \
    "cosyvoice3.yaml" "config.json" "llm.pt" "flow.pt" "hift.pt" "campplus.onnx" "speech_tokenizer_v3.onnx" "CosyVoice-BlankEN/*"
  [ -f "$LAB_ROOT_DIR/cosyvoice3/Fun-CosyVoice3-0.5B/llm.pt" ] || fail_build "cosyvoice3: llm.pt missing after the download"
  record_versions cosyvoice3 "$COSY_REPO" "$COSY_COMMIT" "$venv" 3.10 "FunAudioLLM/Fun-CosyVoice3-0.5B-2512@$REV_COSY"
}

step_install_wrappers() {
  status "wrappers: scripts/lab -> $MM_ROOT/scripts/lab"
  plan "copy voice_*.py + runtime_manifest.py from the bundle's lab/ directory into $MM_ROOT/scripts/lab (chmod 755)" && return 0
  [ -f "$LAB_WRAPPERS_SRC/voice_common.py" ] || fail_build "the wrappers are not in the bundle ($LAB_WRAPPERS_SRC)"
  mkdir -p "$MM_ROOT/scripts/lab"
  cp "$LAB_WRAPPERS_SRC"/voice_*.py "$LAB_WRAPPERS_SRC/runtime_manifest.py" "$MM_ROOT/scripts/lab/"
  chmod 755 "$MM_ROOT/scripts/lab/"*.py
}

step_record_runtime_manifest() {
  status "manifest: fingerprint of the recorded versions + wrappers"
  plan "python3 $MM_ROOT/scripts/lab/runtime_manifest.py --weights $LAB_ROOT_DIR --wrappers $MM_ROOT/scripts/lab -> RUNTIME_MANIFEST; upload $MM_FAMILY.runtime-manifest next to the status object; write /etc/mm-burst-export-manifest.json {runtimeManifest}" && return 0
  RUNTIME_MANIFEST="$(python3 "$MM_ROOT/scripts/lab/runtime_manifest.py" --weights "$LAB_ROOT_DIR" --wrappers "$MM_ROOT/scripts/lab")"
  [[ "$RUNTIME_MANIFEST" =~ ^[0-9a-f]{64}$ ]] || fail_build "runtime manifest is not a sha256 ('$RUNTIME_MANIFEST')"
  jq -n --arg rm "$RUNTIME_MANIFEST" '{runtimeManifest: $rm, fresh: true}' > /etc/mm-burst-export-manifest.json
  printf '%s\n' "$RUNTIME_MANIFEST" | aws s3 cp - "s3://$MM_BUCKET/$MM_BUILD_PREFIX/$MM_FAMILY.runtime-manifest" --only-show-errors
  status "runtime manifest $RUNTIME_MANIFEST"
}

step_lab_disk_check() {
  plan "fail when the root disk is over 85% full after the install (raise RootGiB in internal/lab/registry.go and lib.sh)" && return 0
  status "disk after the install: $(df -h / | awk 'NR==2{print $3" used of "$2" ("$5")"}'); weights $(du -sh "$LAB_ROOT_DIR" | cut -f1), venvs and python $(du -shc "$LAB_VENVS_DIR"/lab-voice-* "$MM_ROOT/python" | sed -n '$p' | cut -f1)"
  local pct
  pct="$(df --output=pcent / | sed -n '2p' | tr -dc '0-9')"
  if [ "${pct:-0}" -gt 85 ]; then
    fail_build "root disk is ${pct}% full after installing lab-voice: raise RootGiB in internal/lab/registry.go and scripts/burst/lib.sh"
  fi
}

step_lab_cleanup_extras() {
  status "cleanup: uv cache, tools, build leftovers, HF token files"
  plan "rm -rf /opt/mm-build (uv cache + tools) and every venv's __pycache__ is kept; scrub token variables and files; assert none remain" && return 0
  scrub_hf_token
  rm -rf /opt/mm-build "$HOME/.cache/uv" "$HOME/.cache/pip" 2>/dev/null || true
  assert_no_hf_token
}

# The lab-voice sequence: the standard provision_main with the fresh branch (no unpack, no Vulkan).
lab_voice_install() {
  step_lab_tools
  step_install_wrappers
  step_install_fish_s2
  step_install_indextts2
  step_install_seedvc
  step_install_cosyvoice3
  step_lab_disk_check
  step_record_runtime_manifest
  step_lab_cleanup_extras
}

provision_main lab_voice_install
