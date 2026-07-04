# AI Document Scan — server install & configuration

This is the operator runbook for the **AI Document Scan** feature: scanned
**printed documents** and **handwritten field notes** → a searchable, multi-page
**PDF** and an optional structured/transcribed **Word (DOCX)** document, with an
in-app PDF viewer, SSE progress, page reordering, and an optional separate AI
summary.

It is the document sibling of AI Image Restoration. The Go API shells out to a
single Python wrapper (`document_ocr.py`) once per pipeline stage; the wrapper
drives OCRmyPDF/Tesseract, PaddleOCR-VL, qwen3-vl (via Ollama), Docling, TrOCR,
and Pandoc. Nothing leaves your GPU host — there is no third-party AI call.

> **Nothing below is required to *build* the code.** `go build ./...` and the
> Next.js build work with zero models installed; the feature simply reports
> itself unavailable in `GET /api/document-scan/capabilities` until the engines
> are present. Install only what you want enabled — each engine degrades
> independently (printed-only works without Ollama; handwriting works without
> PaddleOCR-VL; DOCX falls back from PaddleOCR-VL to Docling, etc.).

---

## 0. Hardware & layout assumed

- **Ubuntu 24.04**, two GPUs (the dual-GPU split is the whole point):
  - **RTX 5080 16 GB at CUDA index 1** (primary) — hosts a **dedicated** Ollama
    instance for this feature (qwen3-vl + the optional text-summary model). Your
    existing shared Ollama daemon is left alone (see §3).
  - **RTX 5060 Ti 8 GB at CUDA index 0** (secondary) — hosts the served/torch
    document engines (PaddleOCR-VL, TrOCR, Docling).
- Keep `CUDA_DEVICE_ORDER=PCI_BUS_ID` everywhere so indices are stable.
- AI assets live under `/opt/media-manipulator-ai/` (venvs, scripts, models),
  matching the other AI tools on this host.

The net effect: during a handwriting job, qwen3-vl reads on the 5080 while
PaddleOCR-VL corroborates on the 5060 Ti **concurrently** — no model-reload
thrash. Only the optional summary stage serializes against the VLM (both are
5080-only).

---

## 1. System packages (printed-path OCR + conversion)

```bash
sudo apt-get update
sudo apt-get install -y tesseract-ocr tesseract-ocr-eng ghostscript pandoc poppler-utils unpaper
# extra Tesseract language packs as needed (must match DOCUMENT_SCAN_LANGUAGES):
# sudo apt-get install -y tesseract-ocr-fra tesseract-ocr-spa tesseract-ocr-deu
```

| Package | Used for |
| --- | --- |
| `tesseract-ocr` (+ language packs) | the faithful searchable-PDF text layer (printed pages) |
| `ghostscript` | OCRmyPDF PDF/A output |
| `pandoc` | Markdown → DOCX (structured reconstruction + summary) |
| `poppler-utils` | `pdfunite` / PDF utilities |
| `unpaper` | OCRmyPDF `--clean` (speckle removal) |

Verify:

```bash
tesseract --version && gs --version && pandoc --version && which pdfunite
tesseract --list-langs   # the codes here are what you may pass in DOCUMENT_SCAN_LANGUAGES
```

---

## 2. The `document-ocr` Python venv

torch must be the **cu128** wheels (Blackwell sm_120). Install torch **first**.

```bash
python3 -m venv /opt/media-manipulator-ai/venvs/document-ocr
source /opt/media-manipulator-ai/venvs/document-ocr/bin/activate
pip install --upgrade pip
# Install torch AND torchvision together from the cu128 index so their compiled
# ops match. docling + transformers both pull in torchvision; if torchvision
# comes from the default PyPI index instead, it won't match this torch and you'll
# hit "RuntimeError: operator torchvision::nms does not exist". Always install
# both from cu128 — never let pip resolve torchvision from PyPI.
pip install torch torchvision --index-url https://download.pytorch.org/whl/cu128   # Blackwell-correct, first
pip install ocrmypdf docling img2pdf pikepdf reportlab pillow
# OPTIONAL — handwriting second-opinion via TrOCR (runs on the 5060 Ti):
pip install transformers          # downloads microsoft/trocr-large-handwritten on first use
deactivate
```

Pre-fetch the Docling models (the fallback structured-DOCX engine):

```bash
source /opt/media-manipulator-ai/venvs/document-ocr/bin/activate
docling-tools models download
deactivate
```

What each package powers:

| Package | Stage |
| --- | --- |
| `ocrmypdf` | printed searchable PDF (imports Tesseract + Ghostscript) |
| `img2pdf`, `pikepdf` | per-page PDF + merge into `document.pdf` |
| `reportlab`, `pillow` | handwriting page: scan image + invisible transcription layer |
| `docling` | fallback printed → Markdown for DOCX |
| `transformers` (optional) | TrOCR fallback second opinion |

---

## 3. Ollama + the handwriting VLM (primary read)

> **Do NOT repin your existing Ollama service.** Setting `CUDA_VISIBLE_DEVICES`
> on the shared `ollama.service` systemd unit forces **every** model that daemon
> serves onto one card — that would break any other API on this box that relies
> on Ollama using the 5060 Ti. Instead we run a **second, dedicated Ollama
> instance** for document-scan, pinned to the 5080 on its own port, and point
> only this feature at it. Your existing daemon is left completely untouched.

### 3.1 Pull the models (into the shared model store — no daemon change)

(This assumes Ollama is already installed — you run it for other services. If a
box somehow doesn't have it: `curl -fsSL https://ollama.com/install.sh | sh`.)

`ollama pull` only downloads blobs to disk; it does not load anything onto a GPU
or restart the daemon, so this is safe to run against your existing instance:

```bash
ollama pull qwen3-vl:8b-instruct-q8_0
# (a plain qwen3-vl:8b already present is a fine fallback; q8 instruct is recommended)
```

Optional text model for the (off-by-default) AI summary:

```bash
ollama pull qwen3.5:9b-q8_0    # recommended default (~10 GB, fits the 5080)
# qwen3.6:27b (~17 GB) also works for max-quality summaries but must SPAN both
# GPUs via Ollama multi-GPU (slower); set OLLAMA_TEXT_MODEL=qwen3.6:27b to use it.
```

### 3.2 Run a second Ollama instance pinned to the 5080

First note where your existing daemon stores models and which user it runs as
(so the new instance reuses the same store — no re-download):

```bash
which ollama                 # e.g. /usr/local/bin/ollama
systemctl cat ollama         # note User=, Group=, and OLLAMA_MODELS (if set)
```

The default Ollama install runs as user `ollama` with models at
`/usr/share/ollama/.ollama/models`. Substitute your real values below:

```ini
# /etc/systemd/system/ollama-mm-doc.service
[Unit]
Description=Ollama (Media Manipulator document-scan — pinned to RTX 5080)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/ollama serve
User=ollama
Group=ollama
# Listen on a DIFFERENT port so it never collides with your shared daemon (:11434).
Environment="OLLAMA_HOST=127.0.0.1:11435"
# Pin THIS instance only to the 5080.
Environment="CUDA_VISIBLE_DEVICES=1"
Environment="CUDA_DEVICE_ORDER=PCI_BUS_ID"
# Reuse the existing model store so qwen3-vl isn't downloaded twice.
Environment="OLLAMA_MODELS=/usr/share/ollama/.ollama/models"
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ollama-mm-doc
curl -s http://127.0.0.1:11435/api/tags | head     # the 5080 instance; should list qwen3-vl
curl -s http://localhost:11434/api/tags | head     # your shared daemon — unchanged
```

This starts a brand-new process; it does **not** touch, restart, or reconfigure
`ollama.service`. Your other APIs keep hitting `:11434` on the 5060 Ti exactly as
before. The only resource the new instance uses is the 5080.

### 3.3 Point document-scan at the dedicated instance

Set this in the API's environment (`.env`) — it is a **document-scan-only** knob,
separate from the global `OLLAMA_URL` the caption translator uses, so nothing
else is rerouted:

```bash
DOCUMENT_SCAN_OLLAMA_URL=http://127.0.0.1:11435
```

If you leave `DOCUMENT_SCAN_OLLAMA_URL` unset it falls back to `OLLAMA_URL`
(default `http://localhost:11434`), i.e. your shared daemon — handy for a
single-GPU dev box, but on this host set it to `:11435` to keep the VLM on the
5080.

> **Lighter alternative (no second instance):** because `qwen3-vl:8b-instruct-q8_0`
> needs ~9–10 GB it physically cannot load on the 8 GB 5060 Ti, so a shared
> Ollama that can see both cards will place it on the 5080 on its own. If you're
> comfortable letting Ollama's scheduler decide, you can skip 3.2 and just leave
> `DOCUMENT_SCAN_OLLAMA_URL` pointing at your existing daemon. The dedicated
> instance in 3.2 is the deterministic, isolated option and is recommended if you
> want guaranteed placement and no contention with other models the shared daemon
> may load.

---

## 4. PaddleOCR-VL (preferred DOCX structure + handwriting second opinion)

PaddleOCR-VL is the preferred printed→Markdown (DOCX) parser and the preferred
handwriting second-opinion engine. We serve it as an **OpenAI-compatible HTTP
server on the 5060 Ti** so the wrapper talks to it like Ollama, and so it stays
off the 5080 (where the qwen3-vl VLM lives).

> **Why its own venv (don't reuse `document-ocr`):** the serving stack pins a
> specific `transformers` version, and PaddleOCR's docs state the versions
> required by vLLM / SGLang / the Transformers engine are mutually incompatible —
> they can't share an environment. The `document-ocr` venv already has
> `transformers` (for TrOCR), so PaddleOCR-VL gets a **separate** venv.

> **Blackwell (sm_120) caveat — read this:** PaddleOCR's convenience installer
> `paddleocr install_genai_server_deps vllm` pulls a **CUDA 12.6** vLLM build,
> which does **not** ship sm_120 kernels for your 5080/5060 Ti — it will fail with
> "no kernel image is available" on Blackwell. So we install a **cu128/cu129**
> vLLM directly instead. This is why the spec said "run PaddleOCR-VL via vLLM, not
> in-process PaddlePaddle."

### 4.1 Create the venv + install a Blackwell-correct vLLM

```bash
python3 -m venv /opt/media-manipulator-ai/venvs/paddleocr-vl
source /opt/media-manipulator-ai/venvs/paddleocr-vl/bin/activate
pip install --upgrade pip

# A recent stable vLLM includes Blackwell (sm_120) support — try this first:
pip install -U vllm

# If `vllm serve` later errors with sm_120 / "no kernel image is available",
# install the nightly cu129 wheels per the official vLLM PaddleOCR-VL recipe
# instead (uv resolves the mixed indices cleanly; `pip install uv` if needed):
#   uv pip install -U vllm --pre \
#     --extra-index-url https://wheels.vllm.ai/nightly \
#     --extra-index-url https://download.pytorch.org/whl/cu129 \
#     --index-strategy unsafe-best-match

deactivate
```

> The PaddleOCR-VL weights download automatically from Hugging Face on first
> server start (the `PaddlePaddle/PaddleOCR-VL-1.6` repo), so the box needs
> outbound network / HF access the first time. To pre-stage them you can run the
> serve command once and let it download, then Ctrl-C.

### 4.2 Smoke-test the server (foreground, on the 5060 Ti)

Run it once by hand to confirm it comes up before daemonizing. `--served-model-name`
makes the OpenAI model id match the API's `PADDLEOCR_VL_MODEL` default, and
`--gpu-memory-utilization` is kept low so it **coexists** with your existing
Ollama usage on the same 8 GB card (tune to taste — 0.35 ≈ ~2.8 GB, plenty for
this 0.9B model):

```bash
source /opt/media-manipulator-ai/venvs/paddleocr-vl/bin/activate
CUDA_VISIBLE_DEVICES=0 CUDA_DEVICE_ORDER=PCI_BUS_ID \
  vllm serve PaddlePaddle/PaddleOCR-VL-1.6 \
    --served-model-name PaddleOCR-VL-1.6-0.9B \
    --trust-remote-code \
    --host 127.0.0.1 --port 8080 \
    --gpu-memory-utilization 0.35 \
    --max-num-batched-tokens 16384 \
    --no-enable-prefix-caching \
    --mm-processor-cache-gb 0
```

In another shell, confirm the OpenAI-compatible endpoint is live (this is exactly
what the API's capabilities probe hits):

```bash
curl -s http://127.0.0.1:8080/v1/models | jq    # should list "PaddleOCR-VL-1.6-0.9B"
```

Then Ctrl-C the foreground server and make it permanent.

### 4.3 Make it a systemd unit

```ini
# /etc/systemd/system/paddleocr-vl.service
[Unit]
Description=PaddleOCR-VL OpenAI-compatible server (5060 Ti)
After=network-online.target
Wants=network-online.target

[Service]
User=mwintrow
Environment="CUDA_VISIBLE_DEVICES=0"
Environment="CUDA_DEVICE_ORDER=PCI_BUS_ID"
ExecStart=/opt/media-manipulator-ai/venvs/paddleocr-vl/bin/vllm serve PaddlePaddle/PaddleOCR-VL-1.6 \
  --served-model-name PaddleOCR-VL-1.6-0.9B \
  --trust-remote-code \
  --host 127.0.0.1 --port 8080 \
  --gpu-memory-utilization 0.35 \
  --max-num-batched-tokens 16384 \
  --no-enable-prefix-caching \
  --mm-processor-cache-gb 0
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now paddleocr-vl
journalctl -u paddleocr-vl -f                 # watch it load the model (first start downloads weights)
curl -s http://127.0.0.1:8080/v1/models | jq  # the wrapper probes /v1/models
```

### 4.4 Wire it into the API config

The defaults already match the commands above, but confirm in the API `.env`:

```bash
PADDLEOCR_VL_ENDPOINT=http://127.0.0.1:8080/v1
PADDLEOCR_VL_MODEL=PaddleOCR-VL-1.6-0.9B   # MUST equal the id shown by /v1/models
```

If you used a different `--served-model-name` (or `--port`), set `PADDLEOCR_VL_MODEL`
/ `PADDLEOCR_VL_ENDPOINT` to match — the wrapper sends that exact model id and a
mismatch makes vLLM reject the request.

### 4.5 Alternative: PaddleOCR's own `genai_server` wrapper

If you'd rather use PaddleOCR's built-in launcher, install it in the **same
dedicated venv** and override its bundled cu126 vLLM with the cu128/cu129 one from
4.1:

```bash
source /opt/media-manipulator-ai/venvs/paddleocr-vl/bin/activate
pip install -U "paddleocr[doc-parser]"
paddleocr install_genai_server_deps vllm        # NOTE: pulls a cu126 vLLM…
pip install -U vllm                              # …so reinstall the Blackwell-correct one over it
paddleocr genai_server --model_name PaddleOCR-VL-1.6-0.9B \
  --backend vllm --host 127.0.0.1 --port 8080
deactivate
```

Both paths expose the same OpenAI-compatible `/v1` interface, so the API config in
4.4 is identical either way.

### 4.6 Degradation + preclean

If PaddleOCR-VL is **not** running, the feature degrades cleanly: DOCX falls back
to Docling (§2), and the handwriting second opinion falls back to TrOCR (or
`none`). Set `DOCUMENT_SCAN_STRUCTURE_ENGINE=docling` /
`DOCUMENT_SCAN_SECOND_OPINION_ENGINE=trocr` if you decide to skip PaddleOCR-VL
entirely.

> **Preclean (optional, on by default for handwriting/auto):** low-resolution
> phone scans of notes are upscaled with the existing `realesrgan-ncnn-vulkan`
> binary (the same one AI Image/Video Restoration use, under
> `/opt/media-manipulator-ai/bin/`) on the Vulkan card — **no new install**. If
> that binary isn't present, preclean just reports unavailable and is skipped.

---

## 5. Deploy the wrapper

The repo file is the source of truth; the API shells out to the deployed copy:

```bash
sudo cp scripts/server/document_ocr.py /opt/media-manipulator-ai/scripts/document_ocr.py
sudo chmod +x /opt/media-manipulator-ai/scripts/document_ocr.py
```

Re-run this after every change to `scripts/server/document_ocr.py`.

---

## 6. Configuration (environment variables)

All have sensible defaults (see `internal/config/config.go`); override in the
API's `.env`/environment as needed.

| Env | Default | Meaning |
| --- | --- | --- |
| `DOCUMENT_SCAN_ENABLED` | `true` | master switch |
| `DOCUMENT_SCAN_DOCX_ENABLED` | `true` | allow DOCX output |
| `DOCUMENT_SCAN_HANDWRITING_ENABLED` | `true` | allow the handwriting (VLM) path |
| `DOCUMENT_SCAN_SECOND_OPINION_ENABLED` | `false` | allow the second-opinion engine |
| `DOCUMENT_SCAN_SUMMARY_ENABLED` | `false` | allow the separate AI summary artifact |
| `AI_DOCUMENT_OCR_PYTHON` | `/opt/media-manipulator-ai/venvs/document-ocr/bin/python` | wrapper interpreter |
| `AI_DOCUMENT_OCR_SCRIPT` | `/opt/media-manipulator-ai/scripts/document_ocr.py` | wrapper path |
| `AI_DOCUMENT_OCR_GPU` | `1` | primary card (5080) for Ollama-side work |
| `AI_DOCUMENT_OCR_SECONDARY_GPU` | `0` | secondary card (5060 Ti) for PaddleOCR-VL/TrOCR |
| `PANDOC_BIN` | `pandoc` | Pandoc binary |
| `DOCUMENT_SCAN_OLLAMA_URL` | falls back to `OLLAMA_URL` (`http://localhost:11434`) | **document-scan-only** Ollama endpoint — point at the dedicated 5080 instance (`http://127.0.0.1:11435`) without rerouting the shared `OLLAMA_URL` |
| `OLLAMA_URL` | `http://localhost:11434` | shared Ollama base URL (used by the caption translator etc.) — **not** repinned by this feature |
| `OLLAMA_VLM_MODEL` | `qwen3-vl:8b-instruct-q8_0` | handwriting primary model |
| `OLLAMA_TEXT_MODEL` | `qwen3.5:9b-q8_0` | summary model |
| `PADDLEOCR_VL_ENDPOINT` | `http://127.0.0.1:8080/v1` | PaddleOCR-VL OpenAI-compatible endpoint |
| `PADDLEOCR_VL_MODEL` | `PaddleOCR-VL-1.6-0.9B` | PaddleOCR-VL model name |
| `DOCUMENT_SCAN_STRUCTURE_ENGINE` | `paddleocr-vl` | printed→DOCX engine (`paddleocr-vl`/`docling`) |
| `DOCUMENT_SCAN_SECOND_OPINION_ENGINE` | `paddleocr-vl` | second opinion (`paddleocr-vl`/`trocr`/`none`) |
| `TROCR_MODEL` | `microsoft/trocr-large-handwritten` | TrOCR weights |
| `DOCUMENT_SCAN_LANGUAGES` | `eng` | comma-split allowlist of Tesseract codes |
| `DOCUMENT_SCAN_MAX_IMAGES` | `50` | max images per job |
| `DOCUMENT_SCAN_MAX_PAGES` | `100` | max pages per job |
| `DOCUMENT_SCAN_MAX_IMAGE_BYTES` | `26214400` | per-image size cap (25 MB) |
| `DOCUMENT_SCAN_MODEL_TIMEOUT_SECONDS` | `2400` | per-stage timeout |
| `DOCUMENT_SCAN_MAX_CONCURRENT_JOBS` | `1` | process-wide job cap |
| `DOCUMENT_SCAN_RATE_LIMIT_PER_SESSION_PER_HOUR` | `20` | per-session rate limit |
| `DOCUMENT_SCAN_RATE_LIMIT_PER_IP_PER_HOUR` | `40` | per-IP rate limit |

> If you add a Tesseract language pack, add its code to `DOCUMENT_SCAN_LANGUAGES`
> (e.g. `eng,fra,spa`) — only allowlisted codes can reach the `tesseract -l`
> argument.

---

## 7. Verify it's wired up

1. **Build & test the API** (no models needed):
   ```bash
   go build ./... && go test ./internal/services/ -run DocumentScan
   ```
2. **Restart the API**, then check capabilities:
   ```bash
   curl -s http://localhost:59997/api/document-scan/capabilities | jq
   ```
   Each flag should reflect what you installed (`printedAvailable`,
   `handwritingAvailable`, `docxAvailable`, `paddleOcrAvailable`,
   `secondOpinionAvailable`, `precleanAvailable`, `summaryAvailable`).
3. **Frontend:** open `/tools/ai-document-scan`, upload a scan, and confirm the
   PDF viewer modal opens on completion. The home page exposes the same flow:
   select an image and choose **“Scan to PDF / Word (AI)”**.
4. **Dual-GPU check** (with second opinion on, during a handwriting job):
   ```bash
   watch -n1 nvidia-smi   # the 5080 (qwen3-vl) and 5060 Ti (PaddleOCR-VL) are both busy
   ```

---

## 8. What gets produced (and the honesty rules)

- **Printed PDF** — original scan + an invisible, searchable **Tesseract** text
  layer (PDF/A). Faithful: the pixels are never altered.
- **Handwriting PDF** — original scan as the visible page + an **invisible
  machine-transcription** layer, stamped “Machine transcription — verify against
  original.”
- **DOCX** — a structured/transcribed **reconstruction**; each handwriting
  section is prefixed “Machine transcription of handwriting — verify against the
  original scan.”
- **AI summary** (optional) — a **separate** `document.summary.docx` headed
  “AI-generated summary — not a verbatim transcription.” It never modifies the
  verbatim outputs.
- Handwriting is **verbatim**: `[illegible]` and `[?: best guess]` markers are
  preserved end-to-end and surfaced as a per-page confidence note. The verify
  pass may *resolve* a flagged token from the image but must not alter confident
  text or add unseen content. Engine disagreements are flagged `[?: vlm | other]`,
  never silently merged.

---

## 9. Troubleshooting

| Symptom | Likely cause / fix |
| --- | --- |
| `handwritingAvailable: false` | Ollama not running / `qwen3-vl` not pulled / `DOCUMENT_SCAN_HANDWRITING_ENABLED=false`. |
| `docxAvailable: false` | Pandoc missing, or neither PaddleOCR-VL nor the docling venv is reachable. |
| `paddleOcrAvailable: false` | The vLLM server isn't up on `PADDLEOCR_VL_ENDPOINT` — `curl …/v1/models`. |
| `printedAvailable: false` | `tesseract`/`gs` not on PATH, or the document-ocr venv/script is missing. |
| `precleanAvailable: false` | `realesrgan-ncnn-vulkan` binary not installed (preclean is optional). |
| Handwriting job is slow / VLM reloads mid-job | The dedicated 5080 Ollama instance isn't running or `DOCUMENT_SCAN_OLLAMA_URL` doesn't point at it — `curl :11435/api/tags`, check `ollama-mm-doc.service`, and keep `CUDA_DEVICE_ORDER=PCI_BUS_ID`. |
| `handwritingAvailable: false` but the shared Ollama is up | `DOCUMENT_SCAN_OLLAMA_URL` points at an instance that doesn't have the model loaded/pulled, or that instance is down. The capabilities probe checks `DOCUMENT_SCAN_OLLAMA_URL`, not the shared daemon. |
| `summaryAvailable: false` | `DOCUMENT_SCAN_SUMMARY_ENABLED=false` or the text model isn't pulled. |
| PaddlePaddle wheel/Blackwell errors | Don't run PaddleOCR-VL in-process — use the vLLM server over HTTP (Section 4). |
| `RuntimeError: operator torchvision::nms does not exist` (during `docling-tools models download`, or any docling/TrOCR import) | torch/torchvision mismatch — torchvision was installed from PyPI instead of cu128. Reinstall both as a matched pair: `pip install --force-reinstall torch torchvision --index-url https://download.pytorch.org/whl/cu128`, then `python -c "import torchvision; from torchvision.io import decode_image; print('ok')"`. |

See `scripts/server/README.md` for the wrapper deploy entry and
`internal/services/document_scan_pipeline.go` for the stage orchestration.
