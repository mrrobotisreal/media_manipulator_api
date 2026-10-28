# Installing the voices service (`mm-voices`)

Operator guide for the resident voices service on the GPU host
(`ssh mwintrow@100.74.11.69`; Ubuntu 24.04, 2 × RTX 5080 16 GB). The steps are
the same, in the same order, as `docs/tickets/MM76-voices-server-install.md`
S0 to S7 in the workspace docs; that ticket is the verified source, and if the
two ever disagree the ticket wins and this file is corrected.

`mm-voices` is a small FastAPI process (`scripts/server/voices_service.py`)
that keeps **Chatterbox Multilingual** (MIT; clone + speak, voice conversion)
and **Qwen3-TTS 1.7B VoiceDesign + CustomVoice** (Apache 2.0; voices from a
description, presets) resident on **GPU 0**, unloads them after
`VOICES_IDLE_UNLOAD_SECONDS` of idleness, and answers the Go API on
`127.0.0.1:8791`. It runs from the existing `~/ai/qwen3-tts/.venv` (no new venv).
Nothing in the API needs it at build time: the API
feature-detects it through `GET /health` and `/api/voices/capabilities`
reports `enabled: false` until it answers, so installing it later is safe.
Design: `docs/design/VOICES.md`; decision: ADR-0015.

> **Blackwell warning:** both cards are sm_120. The venv MUST use torch ≥ 2.7
> with cu128 wheels. Older cu121/cu124 wheels import fine and then fail at the
> first kernel launch with `no kernel image is available for execution on the
> device`. If an engine's own requirements pull an older torch, reinstall the
> cu128 one afterwards (S2).

## Layout (reuses what is already on the host)

```
/home/mwintrow/ai/qwen3-tts/.venv/        # EXISTING venv (russian-local-assistant TTS): Python 3.12.3,
                                          # torch 2.10.0+cu128, qwen-tts 0.1.1, flash_attn, fastapi, uvicorn,
                                          # soundfile, numpy 2.4, transformers 4.57.3. We add chatterbox-tts to it.
/home/mwintrow/models/qwen3-tts/Qwen3-TTS-12Hz-1.7B-CustomVoice/   # EXISTING complete CustomVoice weights
/var/lib/creatv/hf/                       # HF_HOME: EXISTING shared HF cache; Chatterbox + VoiceDesign land here
/opt/media-manipulator-ai/models/voices/presets/   # VOICES_BUILTIN_PRESETS_DIR (optional, see below)
/opt/media-manipulator-ai/scripts/voices_service.py   # deployed from scripts/server/voices_service.py
/opt/media-manipulator-ai/tmp/voices/     # VOICES_SCRATCH_DIR, owned by mwintrow
/etc/systemd/system/mm-voices.service     # deployed from deploy/mm-voices.service
```

**Why no new venv (verified over SSH 2026-10-28).** `~/ai/qwen3-tts/.venv` already
has a working Blackwell torch (`2.10.0+cu128`, `cuda.is_available() True`,
capability `(12, 0)` on GPU 0) and every service dependency. `chatterbox-tts`
0.1.7 hard-pins `torch==2.6.0`, `transformers==5.2.0`, `numpy<2`: a normal
`pip install` would downgrade torch to a cu124 build that cannot run on sm_120
and break Qwen3-TTS (which pins `transformers==4.57.3`). So it is installed with
`--no-deps` plus its 18 small pure-Python/manylinux extras; a dry run on the
host showed the set **only adds packages, replaces none**, and a replica of the
venv imports `chatterbox.mtl_tts`, `chatterbox.vc` and `qwen_tts` side by side
(Chatterbox's code uses only `LlamaModel`/`AutoTokenizer`/generation APIs that
4.57.3 has). The only other user of this venv is
`winapps-ru-tutor-tts.service` (inactive, disabled), and nothing it imports is
changed.

Host facts this guide relies on (verified 2026-10-28): GPU 0 =
`GPU-8c5d035f-59ba-2501-42eb-ec4d97a71527` hosts only Whisper; GPU 1 hosts both
Ollama daemons and every PyTorch tool; port 8790 is `mitchaelina-api`, 8791 is
free; Python 3.12.3; ffmpeg 8.1.1 with `librubberband`, `libopus`,
`libmp3lame` (the API's shifter and mp3 output use it; the service does not).
Every `sudo` step is typed by Mitch; `sudo` asks for a password. Most steps
below need **no** sudo because the venv, the HF cache and `/opt/media-manipulator-ai`
are owned by `mwintrow`.

## S0. Pre-flight (done 2026-10-28)

Both cards idle at 2 MiB, 8791 free, 2.5 TB free on `/`, Python 3.12.3.

## S1. Get the service script and unit from the repo

```bash
cd /home/mwintrow/media-manipulator/api && git pull
ls scripts/server/voices_service.py deploy/mm-voices.service docs/INSTALL_VOICES.md
```

## S2. Add Chatterbox to the existing venv (2 to 5 min, no sudo)

```bash
V=/home/mwintrow/ai/qwen3-tts/.venv

# 1. Snapshot the current package set so it can be put back exactly.
$V/bin/pip freeze > ~/ai/qwen3-tts/pip-freeze-before-chatterbox.txt

# 2. Chatterbox and its extras, WITHOUT dependency resolution (keeps torch 2.10 cu128,
#    transformers 4.57.3 and numpy 2.4 that Qwen3-TTS needs).
$V/bin/pip install --no-deps \
  chatterbox-tts==0.1.7 s3tokenizer==0.3.0 resemble-perth==1.0.1 conformer==0.3.2 \
  diffusers==0.29.0 omegaconf==2.4.0 antlr4-python3-runtime==4.9.3 pyloudnorm==0.2.0 \
  pykakasi==2.3.0 jaconv==0.5.0 Deprecated==3.0.0 wrapt==2.5.1 \
  spacy-pkuseg==1.0.2 srsly==2.5.5 catalogue==2.0.10 \
  onnx==1.23.2 ml_dtypes==0.5.4 importlib_metadata==10.1.0 zipp==4.1.2

# 3. Prove nothing that was there changed (expect only "+" lines, no "-" lines).
$V/bin/pip freeze | diff ~/ai/qwen3-tts/pip-freeze-before-chatterbox.txt - | grep '^[<>]'

# 4. Both engines import, on GPU 0, with the Blackwell torch.
CUDA_VISIBLE_DEVICES=GPU-8c5d035f-59ba-2501-42eb-ec4d97a71527 $V/bin/python -c "
import torch, chatterbox.mtl_tts, chatterbox.vc, qwen_tts, perth
print(torch.__version__, torch.cuda.is_available(), torch.cuda.get_device_name(0), torch.cuda.get_device_capability(0))
print('chatterbox + qwen_tts import OK')"
# expect: 2.10.0+cu128 True NVIDIA GeForce RTX 5080 (12, 0)
#         chatterbox + qwen_tts import OK
```

`pip check` will now report that `chatterbox-tts` "requires torch==2.6.0 /
transformers==5.2.0 / numpy<2": that is expected and harmless (the pins are the
upstream author's test environment, not hard requirements). **Undo** (if
anything misbehaves): `$V/bin/pip uninstall -y chatterbox-tts s3tokenizer
resemble-perth conformer diffusers omegaconf antlr4-python3-runtime pyloudnorm
pykakasi jaconv Deprecated wrapt spacy-pkuseg srsly catalogue onnx ml_dtypes
importlib_metadata zipp`, then the `diff` in step 3 is empty again.

## S3. Download only the missing weights (5 to 10 min, ~8.8 GB, no sudo)

Qwen3-TTS **CustomVoice is already on disk** (complete, 4.3 GB) at
`/home/mwintrow/models/qwen3-tts/Qwen3-TTS-12Hz-1.7B-CustomVoice` and the unit
points at it. Two things are missing: Qwen3-TTS **VoiceDesign** (4.5 GB) and
the Chatterbox multilingual + VC files (4.3 GB of the repo's 14 GB):

```bash
export HF_HOME=/var/lib/creatv/hf
V=/home/mwintrow/ai/qwen3-tts/.venv
$V/bin/hf download Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign
$V/bin/hf download ResembleAI/chatterbox ve.pt t3_mtl23ls_v2.safetensors s3gen.pt s3gen.safetensors \
  grapheme_mtl_merged_expanded_v1.json conds.pt Cangjie5_TC.json
du -sh $HF_HOME/hub/models--Qwen--Qwen3-TTS-12Hz-1.7B-VoiceDesign $HF_HOME/hub/models--ResembleAI--chatterbox
# expect ~4.2G and ~4.0G
```

If a repo name has changed upstream, write the name actually used into the log
and into `deploy/mm-voices.service` (`VOICES_CHATTERBOX_REPO` /
`VOICES_QWEN_DESIGN_REPO` / `VOICES_QWEN_CUSTOM_REPO`; a local directory works
too). The service fetches only these seven Chatterbox files
(`CHATTERBOX_FILES` in the script), never the whole repo.

## S4. Deploy the script and run the self-test (5 min + first-load time, no sudo)

```bash
mkdir -p /opt/media-manipulator-ai/tmp/voices /opt/media-manipulator-ai/models/voices/presets
cp /home/mwintrow/media-manipulator/api/scripts/server/voices_service.py /opt/media-manipulator-ai/scripts/voices_service.py
cd /opt/media-manipulator-ai
CUDA_VISIBLE_DEVICES=GPU-8c5d035f-59ba-2501-42eb-ec4d97a71527 HF_HOME=/var/lib/creatv/hf \
VOICES_QWEN_CUSTOM_REPO=/home/mwintrow/models/qwen3-tts/Qwen3-TTS-12Hz-1.7B-CustomVoice \
  /home/mwintrow/ai/qwen3-tts/.venv/bin/python scripts/voices_service.py --self-test --out /opt/media-manipulator-ai/tmp/voices/selftest
ls -la /opt/media-manipulator-ai/tmp/voices/selftest
```

The self-test synthesises one sentence with Chatterbox's built-in voice
(`chatterbox_tts.wav`), designs a voice from a fixed description with Qwen3-TTS
VoiceDesign (`qwen_design.wav`), and converts the Qwen sentence to the
Chatterbox voice (`chatterbox_vc.wav`). It prints one JSON line and exits 0
only when all three succeed:

```json
{"chatterbox": "ok", "qwen_tts": "ok", "vc": "ok", "seconds": 41.7}
```

A failing engine shows `"error: <reason>"` in its slot and the exit code is 1.
Copy the files to the MacBook and listen:

```bash
# on the MacBook
scp 'mwintrow@100.74.11.69:/opt/media-manipulator-ai/tmp/voices/selftest/*.wav' ~/Downloads/
```

The designed voice must match its description ("a warm, calm, middle-aged male
narrator with a low, slightly raspy voice"), and the VC output must be
intelligible and in the Chatterbox voice. **This is the model acceptance
test**; if VC quality is poor, say so in the log (OpenVoice v2 is the
designated fallback, swapped behind the same `/vc` endpoint).

## S5. Install the systemd unit (5 min)

```bash
sudo cp /home/mwintrow/media-manipulator/api/deploy/mm-voices.service /etc/systemd/system/mm-voices.service
sudo systemctl daemon-reload
sudo systemctl enable --now mm-voices
sleep 5; systemctl status mm-voices --no-pager | head -12
curl -s http://127.0.0.1:8791/health | python3 -m json.tool
```

Expected `health` (engines load on the first request):

```json
{
  "status": "ok",
  "service": "mm-voices",
  "engines": {
    "chatterbox": {"loaded": false, "version": "0.1.x", "repo": "ResembleAI/chatterbox", "models": []},
    "qwen_tts": {"loaded": false, "version": "0.x", "repo": {"design": "...", "custom": "..."}, "models": []}
  },
  "gpu": "NVIDIA GeForce RTX 5080",
  "port": 8791,
  "idleSeconds": 5,
  "idleUnloadSeconds": 600,
  "languages": ["ar", "da", "de", "..."]
}
```

Then one real request:

```bash
curl -s -X POST http://127.0.0.1:8791/tts -H 'Content-Type: application/json' \
  -d '{"text":"Media Manipulator voices are running.","language":"en","referenceWav":"/opt/media-manipulator-ai/tmp/voices/selftest/chatterbox_tts.wav","outWav":"/opt/media-manipulator-ai/tmp/voices/hello.wav"}'
curl -s http://127.0.0.1:8791/health | grep -o '"loaded": true' | wc -l     # 1
nvidia-smi --query-gpu=index,memory.used --format=csv                       # GPU 0 up by ~4 GB, GPU 1 unchanged
sleep 660; curl -s http://127.0.0.1:8791/health | grep -c '"loaded": true'  # 0 after idle-unload
```

The service refuses any `referenceWav` / `inputWav` / `outWav` outside
`VOICES_SCRATCH_DIR` (403 `path_outside_scratch`), symlinks included.

### Is it alive?

```bash
systemctl is-active mm-voices && curl -sf http://127.0.0.1:8791/health >/dev/null && echo "mm-voices OK"
journalctl -u mm-voices -n 50 --no-pager      # load / unload lines, never text or audio
```

To free GPU 0 immediately (for example before a heavy Whisper batch):
`curl -s -X POST http://127.0.0.1:8791/unload`.

## S6. Wire the API (2 min, then a restart)

Append to `/home/mwintrow/media-manipulator/api/.env` (never paste the file
anywhere):

```
VOICES_ENABLED=true
VOICES_URL=http://127.0.0.1:8791
VOICES_TIMEOUT_SECONDS=300
VOICES_SCRATCH_DIR=/opt/media-manipulator-ai/tmp/voices
GPU_SCHEDULER_DEFAULT_VOICES_DEVICE=cuda:0
VOICES_VRAM_MIB=6000
```

`VOICES_SCRATCH_DIR` must be the same directory in the API's env and the
unit: the API writes the inputs there and the service writes the outputs
there. Both run as `mwintrow`. Then, once the MM77 API build is deployed:

```bash
sudo systemctl restart media-manipulator-api
curl -s https://api.media-manipulator.com/api/voices/capabilities   # or the local port
```

It must return `enabled: true` with both engines and the language list. The
remaining optional keys (tier limits `TIER_<T>_VOICES_*`, the `voices_create`
rate-limit bucket) are listed in `RUNBOOK.md` §4 "Voices"; their defaults are
the decided values.

## S7. Record it

Session log row in the MM76 ticket with: the pinned versions (S2 output), the
HF repo ids actually used (S3), self-test timing and the listening verdict
(S4), and the `health` output (S5). Check the `mm-voices` row in `RUNBOOK.md`
§12 and the `VOICES_*` / `GPU_SCHEDULER_DEFAULT_VOICES_DEVICE` rows in §4
against reality and correct any value that differs.

## Built-in presets (later, a file drop)

Mitch's showcase voices become presets by dropping a clip, no code change:

```bash
sudo mkdir -p /opt/media-manipulator-ai/models/voices/presets
sudo cp mr_wintrow.wav /opt/media-manipulator-ai/models/voices/presets/   # 5 to 60 s of clean speech
echo '{"name": "Mr. Wintrow", "description": "", "language": "en"}' | \
  sudo tee /opt/media-manipulator-ai/models/voices/presets/mr_wintrow.json
```

The file stem (`[A-Za-z0-9_-]`, up to 64 chars) is the preset id; `/presets`
lists it ahead of the Qwen CustomVoice speakers and `/tts-preset` speaks it
with Chatterbox from that clip. No restart needed.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `no kernel image is available` at first request | a dependency replaced the cu128 torch | `pip freeze` diff against `~/ai/qwen3-tts/pip-freeze-before-chatterbox.txt`; reinstall `torch==2.10.0 torchaudio==2.10.0 --index-url https://download.pytorch.org/whl/cu128` |
| `pip check` complains about chatterbox pins | upstream test pins (torch 2.6, transformers 5.2, numpy<2) | expected; installed with `--no-deps` on purpose (S2) |
| self-test `"error: engine_load_failed"` | weights missing or repo renamed | re-run S3; set the `VOICES_*_REPO` ids in the unit |
| `/health` refuses the connection | unit not running or crashed on start | `journalctl -u mm-voices -n 100 --no-pager` |
| capabilities `enabled: false` with the unit running | API env missing or wrong URL | S6 values; `VOICES_ENABLED=true`; restart the API |
| 403 `path_outside_scratch` | API and unit disagree on `VOICES_SCRATCH_DIR` | make them the same directory |
| CUDA OOM on GPU 0 | Whisper + both engines busy at once | `POST /unload`; lower `VOICES_IDLE_UNLOAD_SECONDS`; the API's `TaskVoices` lease (`VOICES_VRAM_MIB`) serialises with Whisper |
| memory on GPU 1 | unit not pinned | check `CUDA_VISIBLE_DEVICES` in the unit is GPU 0's UUID |
