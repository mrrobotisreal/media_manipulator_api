# Server reference scripts

These files are reference copies of helper scripts that live on the GPU host at
`/opt/media-manipulator-ai/scripts/`. The Go API does **not** load them from
this repo — it shells out to whatever path is configured per script. They are
checked in so the runtime version is reviewable and so updates can be diffed.

## `face_privacy.py`

Runtime path is configured by `AI_FACE_PRIVACY_SCRIPT` and defaults to
`/opt/media-manipulator-ai/scripts/face_privacy.py`. The Go API invokes it via
`AI_VISION_PYTHON` (default
`/opt/media-manipulator-ai/venvs/vision-privacy/bin/python`).

Two entry points are used:

- **Detect-only preview** (`POST /api/ai/faces/detect`):
  `python face_privacy.py --input <img> --detect-only --json-out <path>`
- **Final conversion with selection** (`POST /api/upload`):
  `python face_privacy.py --input <img> --output <out> --mode blur \
     --selection-json <selection.json>` where the JSON contains the stored
  face boxes plus the user's `selectionMode` / `selectedFaceIds`.

To deploy on the server:

```bash
sudo cp scripts/server/face_privacy.py \
  /opt/media-manipulator-ai/scripts/face_privacy.py
```

## `frame_interpolate_rife.py`

Runtime path is configured by `AI_FRAME_INTERPOLATION_SCRIPT` and defaults to
`/opt/media-manipulator-ai/scripts/frame_interpolate_rife.py`. The Go API
shells out to the script directly (its shebang plus executable bit make a
dedicated venv unnecessary — it only uses the Python stdlib). The script in
turn invokes `ffmpeg`, `ffprobe`, and `rife-ncnn-vulkan`.

Single entry point — `POST /api/upload` and `POST /api/video-upload/complete`
with video options that contain:

```jsonc
"ai": {
  "enabled": true,
  "operation": "frame_interpolation",
  "frameInterpolation": {
    "targetFps": 60,
    "model": "rife-v4.6",
    "quality": "medium",
    "maxHeight": 720,
    "preserveAudio": true
  }
}
```

The Go side resolves the model directory based on the `model` field, validates
target FPS / quality / max-height, and runs the script with the Vulkan ICD
env vars from `/opt/media-manipulator-ai/env/vulkan-nvidia.sh`.

No Python packages are required — the script uses only the Python standard
library. Make sure ffmpeg, ffprobe, and rife-ncnn-vulkan are installed and
that `AI_RIFE_BIN`, `AI_RIFE_MODEL`, and `AI_RIFE_GPU` point at the right
places.

To deploy on the server:

```bash
sudo cp scripts/server/frame_interpolate_rife.py \
  /opt/media-manipulator-ai/scripts/frame_interpolate_rife.py
sudo chmod +x /opt/media-manipulator-ai/scripts/frame_interpolate_rife.py
```

See `docs/server-ai-frame-interpolation.md` for the full RIFE install flow.

## `document_ocr.py`

Runtime path is configured by `AI_DOCUMENT_OCR_SCRIPT` and defaults to
`/opt/media-manipulator-ai/scripts/document_ocr.py`. The Go API invokes it via
`AI_DOCUMENT_OCR_PYTHON` (default
`/opt/media-manipulator-ai/venvs/document-ocr/bin/python`), once per pipeline
stage (`--mode {classify,htr,htr-verify,htr-paddle,htr-trocr,structure,build-pdf,build-docx,summary}`).

It powers the **AI Document Scan** tool — scanned printed documents AND
handwritten field notes → searchable PDF + optional structured/transcribed DOCX.
The script orchestrates several engines (it imports the heavy ones lazily, only
in the stage that needs them):

- **OCRmyPDF + Tesseract** (CPU) — faithful searchable text layer for printed pages.
- **PaddleOCR-VL-1.6** via its vLLM OpenAI-compatible HTTP server on the 5060 Ti —
  printed→Markdown structure (DOCX) and the preferred handwriting second opinion.
- **qwen3-vl** via Ollama on the 5080 — handwriting primary read + verification.
- **Docling** (fallback structured engine) and **TrOCR** (fallback second opinion).
- **Pandoc** — Markdown → DOCX. **reportlab / img2pdf / pikepdf** — PDF assembly.

The Go side seeds each `work-dir/page-NNN.json` sidecar with `{index, kind}`; the
script read-modify-writes the read results back (`kind, engine, text, lines,
confidence, illegibleCount`). Page transcription text never leaves the sidecars /
artifacts — only `PROGRESS <done>/<total>` lines and `ERROR: <safe msg>` reach
stdout/stderr. See `internal/services/document_scan_pipeline.go` for the stage
orchestration and `README-document-scan.md` (repo root) for the full install flow.

To deploy on the server:

```bash
sudo cp scripts/server/document_ocr.py \
  /opt/media-manipulator-ai/scripts/document_ocr.py
sudo chmod +x /opt/media-manipulator-ai/scripts/document_ocr.py
```

## `voices_service.py`

The resident **voices** service (`mm-voices`, MM77, ADR-0015). Unlike the
scripts above it is not shelled out per job: it runs as its own systemd unit
(`deploy/mm-voices.service`) from
`/opt/media-manipulator-ai/scripts/voices_service.py` with the
`/opt/media-manipulator-ai/venvs/voices` Python, bound to `127.0.0.1:8791`
and pinned to GPU 0 by UUID. It keeps Chatterbox Multilingual (TTS from a
reference clip, voice conversion) and Qwen3-TTS VoiceDesign + CustomVoice
(voices from a description, presets) loaded, unloading each after
`VOICES_IDLE_UNLOAD_SECONDS` (600) of idleness.

The Go API (`internal/services/voices_client.go`) calls it over HTTP JSON:
`GET /health`, `POST /tts`, `/design`, `/presets`, `/tts-preset`, `/vc`,
`/unload`. Audio travels by path under `VOICES_SCRATCH_DIR`
(`/opt/media-manipulator-ai/tmp/voices`), which the API owns; the service
refuses any path outside it. Hugging Face repo ids come from
`VOICES_CHATTERBOX_REPO`, `VOICES_QWEN_DESIGN_REPO`,
`VOICES_QWEN_CUSTOM_REPO` (set in the unit). Clips dropped into
`VOICES_BUILTIN_PRESETS_DIR` become built-in presets spoken by Chatterbox.

Module-level imports are stdlib only (torch and the engines load lazily), so
`python3 -m py_compile scripts/server/voices_service.py`, `--help` and the
helper tests (`python3 -m unittest scripts/server/test_voices_service.py`)
run on a machine without a GPU. `--self-test --out <dir>` is the model
acceptance check.

To deploy on the server:

```bash
sudo cp scripts/server/voices_service.py \
  /opt/media-manipulator-ai/scripts/voices_service.py
sudo chown mwintrow:mwintrow /opt/media-manipulator-ai/scripts/voices_service.py
sudo systemctl restart mm-voices
```

See `docs/INSTALL_VOICES.md` for the full install flow (MM76).
