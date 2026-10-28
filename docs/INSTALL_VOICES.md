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
`127.0.0.1:8791`. Nothing in the API needs it at build time: the API
feature-detects it through `GET /health` and `/api/voices/capabilities`
reports `enabled: false` until it answers, so installing it later is safe.
Design: `docs/design/VOICES.md`; decision: ADR-0015.

> **Blackwell warning:** both cards are sm_120. The venv MUST use torch ≥ 2.7
> with cu128 wheels. Older cu121/cu124 wheels import fine and then fail at the
> first kernel launch with `no kernel image is available for execution on the
> device`. If an engine's own requirements pull an older torch, reinstall the
> cu128 one afterwards (S2).

## Layout

```
/opt/media-manipulator-ai/
├── venvs/voices/                     # Python 3.12 (3.11 fallback), torch cu128, engines
├── models/hf-cache/                  # HF_HOME: Chatterbox + Qwen3-TTS weights (~8 GB)
├── models/voices/presets/            # VOICES_BUILTIN_PRESETS_DIR (optional, see below)
├── scripts/voices_service.py         # deployed from scripts/server/voices_service.py
└── tmp/voices/                       # VOICES_SCRATCH_DIR, owned by mwintrow
/etc/systemd/system/mm-voices.service # deployed from deploy/mm-voices.service
```

Host facts this guide relies on (verified 2026-10-28): GPU 0 =
`GPU-8c5d035f-59ba-2501-42eb-ec4d97a71527` hosts only Whisper; GPU 1 hosts both
Ollama daemons and every PyTorch tool; port 8790 is `mitchaelina-api`, 8791 is
free; Python 3.12.3; ffmpeg 8.1.1 with `librubberband`, `libopus`,
`libmp3lame` (the API's shifter and mp3 output use it; the service does not).
Every `sudo` step is typed by Mitch; `sudo` asks for a password.

## S0. Pre-flight (5 min)

```bash
ssh mwintrow@100.74.11.69
nvidia-smi --query-gpu=index,uuid,memory.used --format=csv     # expect both cards, ~2 MiB used when idle
ss -ltn | grep ':8791 ' && echo "8791 BUSY, pick another port and tell MM77" || echo "8791 free"
df -h / | tail -1                                                # ≥ 50 GB free
python3 --version                                                # 3.12.x
```

If 8791 is busy, change the port in `deploy/mm-voices.service`
(`VOICES_PORT`), in the API's `VOICES_URL`, and in `docs/design/VOICES.md` §3.

## S1. Get the service script and unit from the repo

```bash
cd /home/mwintrow/media-manipulator/api && git pull
ls scripts/server/voices_service.py deploy/mm-voices.service docs/INSTALL_VOICES.md
```

If `git pull` is not how this checkout is updated on the server, copy the
three files over with `scp` from the MacBook instead, for example from the API
repo root on the MacBook:

```bash
scp scripts/server/voices_service.py deploy/mm-voices.service docs/INSTALL_VOICES.md \
  mwintrow@100.74.11.69:/tmp/
```

(then use `/tmp/...` in place of the repo paths in S4 and S5).

## S2. Create the venv and install the engines (20 to 40 min, mostly downloads)

```bash
sudo mkdir -p /opt/media-manipulator-ai/venvs /opt/media-manipulator-ai/tmp/voices /opt/media-manipulator-ai/models/hf-cache
sudo chown mwintrow:mwintrow /opt/media-manipulator-ai/tmp/voices
cd /opt/media-manipulator-ai/venvs
sudo python3 -m venv voices
sudo ./voices/bin/pip install --upgrade pip wheel setuptools

# Blackwell (sm_120): torch >= 2.7 with cu128 wheels, nothing older.
sudo ./voices/bin/pip install torch torchaudio --index-url https://download.pytorch.org/whl/cu128

# Engines + the service's own deps.
sudo ./voices/bin/pip install chatterbox-tts qwen-tts fastapi "uvicorn[standard]" soundfile numpy pydantic

# Record the pins (paste the output into the MM76 session log).
./voices/bin/pip freeze | grep -iE "^(torch|torchaudio|chatterbox|qwen-tts|transformers|fastapi|uvicorn)=="
```

If the engines' requirements replaced torch with a non-cu128 build (the
`pip freeze` line shows `torch==X` without `+cu128`), put the cu128 one back:

```bash
sudo ./voices/bin/pip install --force-reinstall torch torchaudio --index-url https://download.pytorch.org/whl/cu128
```

**Python 3.11 fallback.** If `chatterbox-tts` fails to resolve on 3.12 (it has
historically pinned 3.11):

```bash
sudo add-apt-repository ppa:deadsnakes/ppa && sudo apt install python3.11 python3.11-venv
sudo rm -rf /opt/media-manipulator-ai/venvs/voices
cd /opt/media-manipulator-ai/venvs && sudo python3.11 -m venv voices
# then repeat the pip lines above
```

Note the choice in the log. The unit's `ExecStart` uses the venv's own
`bin/python`, so it needs no change either way.

Check the install sees the right card:

```bash
CUDA_VISIBLE_DEVICES=GPU-8c5d035f-59ba-2501-42eb-ec4d97a71527 ./voices/bin/python -c "import torch;print(torch.__version__, torch.cuda.is_available(), torch.cuda.get_device_name(0), torch.cuda.get_device_capability(0))"
# expect: 2.x.x+cu128 True NVIDIA GeForce RTX 5080 (12, 0)
```

## S3. Download the model weights into the shared HF cache (10 to 20 min, ~8 GB)

```bash
export HF_HOME=/opt/media-manipulator-ai/models/hf-cache
sudo -E ./voices/bin/python - <<'PY'
from huggingface_hub import snapshot_download
for repo in ["ResembleAI/chatterbox", "Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign", "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice"]:
    print(repo, snapshot_download(repo))
PY
sudo chown -R mwintrow:mwintrow /opt/media-manipulator-ai/models/hf-cache
du -sh /opt/media-manipulator-ai/models/hf-cache
```

If a repo name has changed upstream (model cards move), the Resemble AI and
QwenLM GitHub READMEs name the current one; write the name actually used into
the log and into `deploy/mm-voices.service` as `VOICES_CHATTERBOX_REPO` /
`VOICES_QWEN_DESIGN_REPO` / `VOICES_QWEN_CUSTOM_REPO`. The service reads the
ids only from those variables (the defaults are fallbacks), so a rename never
needs a code change.

## S4. Deploy the script and run the self-test (5 min + first-load time)

```bash
sudo cp /home/mwintrow/media-manipulator/api/scripts/server/voices_service.py /opt/media-manipulator-ai/scripts/voices_service.py
sudo chown mwintrow:mwintrow /opt/media-manipulator-ai/scripts/voices_service.py
cd /opt/media-manipulator-ai
CUDA_VISIBLE_DEVICES=GPU-8c5d035f-59ba-2501-42eb-ec4d97a71527 HF_HOME=/opt/media-manipulator-ai/models/hf-cache \
  ./venvs/voices/bin/python scripts/voices_service.py --self-test --out /opt/media-manipulator-ai/tmp/voices/selftest
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
Play the files (`scp` them to the MacBook) and listen: the clone must sound
like its reference, the designed voice must match its description ("a warm,
calm, middle-aged male narrator with a low, slightly raspy voice"), and the VC
output must be intelligible and in the Chatterbox voice. **This is the model
acceptance test**; if VC quality is poor, say so in the log (OpenVoice v2 is the
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
| `no kernel image is available` at first request | non-cu128 torch in the venv | `pip install --force-reinstall torch torchaudio --index-url .../cu128` (S2) |
| `chatterbox-tts` will not resolve | its Python pin | the Python 3.11 fallback in S2 |
| self-test `"error: engine_load_failed"` | weights missing or repo renamed | re-run S3; set the `VOICES_*_REPO` ids in the unit |
| `/health` refuses the connection | unit not running or crashed on start | `journalctl -u mm-voices -n 100 --no-pager` |
| capabilities `enabled: false` with the unit running | API env missing or wrong URL | S6 values; `VOICES_ENABLED=true`; restart the API |
| 403 `path_outside_scratch` | API and unit disagree on `VOICES_SCRATCH_DIR` | make them the same directory |
| CUDA OOM on GPU 0 | Whisper + both engines busy at once | `POST /unload`; lower `VOICES_IDLE_UNLOAD_SECONDS`; the API's `TaskVoices` lease (`VOICES_VRAM_MIB`) serialises with Whisper |
| memory on GPU 1 | unit not pinned | check `CUDA_VISIBLE_DEVICES` in the unit is GPU 0's UUID |
