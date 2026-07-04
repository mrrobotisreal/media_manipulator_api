#!/usr/bin/env python3
"""AI Document Scan orchestration helper for Media Manipulator.

Turns one or more normalized page images (printed documents OR handwritten field
notes) into a searchable PDF and/or a structured/transcribed DOCX. The Go API
shells out to this script once per pipeline stage (one process per --mode); the
script loops over the pages, reading and writing per-page JSON sidecars in
--work-dir so later stages see earlier results.

Engine roles (do NOT blur):
    printed faithful searchable PDF layer  : OCRmyPDF + Tesseract (CPU, deterministic)
    printed -> structured DOCX/Markdown    : PaddleOCR-VL (preferred) | Docling (fallback) [5060 Ti]
    handwriting primary read               : qwen3-vl via Ollama [5080]
    handwriting second opinion (optional)  : PaddleOCR-VL (preferred) | TrOCR (fallback) [5060 Ti]
    optional AI summary                    : text model via Ollama [5080]

The Go API shells out to the deployed copy at
/opt/media-manipulator-ai/scripts/document_ocr.py; this repo file is the source
of truth — deploy with:

    sudo cp scripts/server/document_ocr.py /opt/media-manipulator-ai/scripts/document_ocr.py
    sudo chmod +x /opt/media-manipulator-ai/scripts/document_ocr.py

Stdlib-only orchestration; ocrmypdf / docling / torch / transformers / reportlab
/ img2pdf / pikepdf are imported lazily inside the branch that needs them (the
document-ocr venv python, AI_DOCUMENT_OCR_PYTHON). Conventions match
preclean_image.py: one "PROGRESS <done>/<total>" line per processed page on
stdout, a final "PROGRESS total/total"; fatal errors as one "ERROR: <safe msg>"
line on stderr + a non-zero exit. --gpu addresses the physical primary CUDA
device; --secondary-gpu the 5060 Ti for TrOCR. NEVER prints page contents to
logs (only counts/markers).

Forensic honesty: handwriting transcription is verbatim with explicit
[illegible] / [?: best guess] markers. The verify pass may resolve a flagged
token from the image but MUST NOT alter confident text or add unseen content.
Engine disagreements are flagged ([?: vlm | other]), never silently merged.
"""
import argparse
import base64
import json
import re
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

# --- forensic prompt contracts ------------------------------------------------

HTR_PROMPT = (
    "You are transcribing a scanned handwritten page. Transcribe the handwriting "
    "VERBATIM. Preserve line breaks and layout. Wrap anything you genuinely cannot "
    "read as [illegible]. Wrap an uncertain word as [?: your-single-best-guess]. "
    "Do NOT invent, correct, complete, or paraphrase any text. Note non-text marks "
    "briefly in (parentheses), e.g. (arrow), (circled), (underlined). "
    "Return ONLY a JSON object: "
    '{"text": "<full transcription with line breaks>", '
    '"lines": [{"bbox": [x0, y0, x1, y1], "text": "<one line>"}]}. '
    "bbox is the pixel bounding box of each text line (top-left origin)."
)

VERIFY_PROMPT = (
    "Here is a scanned handwritten page and a draft transcription of it. Resolve "
    "ONLY the tokens marked [illegible] or [?: ...] using visual evidence from the "
    "image. You MAY leave a token unresolved if it is still unreadable. Do NOT "
    "change any other text. Do NOT add content that is not visibly written. "
    "Return ONLY a JSON object: "
    '{"text": "<the corrected transcription>"}. Draft transcription follows:\n\n'
)

CLASSIFY_PROMPT = (
    "Is this scanned page primarily printed/typed text or handwritten? "
    "Answer with exactly one word: printed or handwritten."
)

PADDLE_HTR_PROMPT = (
    "Transcribe all the text on this page verbatim, preserving line breaks. "
    "Return only the transcription text."
)

PADDLE_STRUCTURE_PROMPT = (
    "Convert this document page to GitHub-flavored Markdown. Preserve headings, "
    "lists, and tables. Return only the Markdown."
)

SUMMARY_PROMPT = (
    "You are summarizing a transcribed document. Summarize and organize ONLY what "
    "is present in the text below. Mark any gaps or illegible passages explicitly. "
    "Do NOT invent facts, names, dates, or details that are not in the text. "
    "Return GitHub-flavored Markdown.\n\nDocument transcription:\n\n"
)

HANDWRITING_DOCX_NOTE = (
    "**Machine transcription of handwriting — verify against the original scan.**"
)


def fail(message: str) -> None:
    raise SystemExit(f"ERROR: {message}")


def emit_progress(done: int, total: int) -> None:
    print(f"PROGRESS {done}/{max(total, 1)}", flush=True)


# --- sidecar helpers ----------------------------------------------------------

def sidecar_path(work_dir: Path, idx: int) -> Path:
    return work_dir / f"page-{idx:03d}.json"


def page_image(pages_dir: Path, idx: int) -> Path:
    return pages_dir / f"page-{idx:03d}.png"


def load_sidecar(work_dir: Path, idx: int) -> dict:
    p = sidecar_path(work_dir, idx)
    if p.is_file():
        try:
            return json.loads(p.read_text())
        except Exception:  # noqa: BLE001
            return {}
    return {}


def save_sidecar(work_dir: Path, idx: int, data: dict) -> None:
    data.setdefault("index", idx)
    sidecar_path(work_dir, idx).write_text(json.dumps(data))


def count_markers(text: str) -> int:
    return text.count("[illegible]") + text.count("[?:")


def confidence_for(text: str, line_count: int) -> str:
    markers = count_markers(text)
    if line_count <= 0:
        return "low" if markers else "medium"
    ratio = markers / max(1, line_count)
    if markers == 0:
        return "high"
    if ratio < 0.15:
        return "medium"
    return "low"


# --- HTTP model calls (stdlib only) ------------------------------------------

def _http_post_json(url: str, payload: dict, timeout: int) -> dict:
    body = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310 — trusted local endpoint
        return json.loads(resp.read().decode("utf-8"))


def _http_get_ok(url: str, timeout: int = 3) -> bool:
    try:
        req = urllib.request.Request(url, method="GET")
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
            return 200 <= resp.status < 500
    except Exception:  # noqa: BLE001
        return False


def b64_image(path: Path) -> str:
    return base64.b64encode(path.read_bytes()).decode("ascii")


def ollama_chat(ollama_url: str, model: str, prompt: str, image_path: Path | None,
                want_json: bool, timeout: int = 600) -> str:
    """Single non-streaming Ollama /api/chat call. Raises urllib errors so a dead
    backend fails the whole stage; content-level issues are handled by callers."""
    message: dict = {"role": "user", "content": prompt}
    if image_path is not None:
        message["images"] = [b64_image(image_path)]
    payload: dict = {
        "model": model,
        "stream": False,
        "options": {"temperature": 0.1},
        "messages": [message],
    }
    if want_json:
        payload["format"] = "json"
    url = ollama_url.rstrip("/") + "/api/chat"
    data = _http_post_json(url, payload, timeout)
    return (data.get("message", {}) or {}).get("content", "") or ""


def paddle_chat(endpoint: str, model: str, prompt: str, image_path: Path, timeout: int = 600) -> str:
    """OpenAI-compatible chat completion against the PaddleOCR-VL vLLM server."""
    data_uri = "data:image/png;base64," + b64_image(image_path)
    payload = {
        "model": model,
        "temperature": 0.0,
        "messages": [{
            "role": "user",
            "content": [
                {"type": "text", "text": prompt},
                {"type": "image_url", "image_url": {"url": data_uri}},
            ],
        }],
    }
    url = endpoint.rstrip("/") + "/chat/completions"
    data = _http_post_json(url, payload, timeout)
    choices = data.get("choices") or []
    if not choices:
        return ""
    return (choices[0].get("message", {}) or {}).get("content", "") or ""


def parse_htr_json(content: str) -> tuple[str, list]:
    """Lenient parse of the VLM HTR JSON. Returns (text, lines)."""
    content = content.strip()
    try:
        obj = json.loads(content)
    except Exception:  # noqa: BLE001
        # Strip code fences / surrounding prose and retry on the first {...} block.
        m = re.search(r"\{.*\}", content, re.DOTALL)
        if not m:
            return content, []
        try:
            obj = json.loads(m.group(0))
        except Exception:  # noqa: BLE001
            return content, []
    if isinstance(obj, dict):
        text = str(obj.get("text", "")).rstrip()
        lines = obj.get("lines") or []
        if not isinstance(lines, list):
            lines = []
        return text, lines
    return str(obj), []


# --- per-page read stages -----------------------------------------------------

def ordered_indices(order: str, pages_dir: Path) -> list[int]:
    if order.strip():
        return [int(x) for x in order.split(",") if x.strip()]
    found = sorted(pages_dir.glob("page-*.png"))
    return [int(p.stem.split("-")[1]) for p in found]


def mode_classify(args, indices: list[int]) -> None:
    work_dir = Path(args.work_dir)
    pages_dir = Path(args.pages_dir)
    total = len(indices)
    failures = 0
    for done, idx in enumerate(indices, start=1):
        sc = load_sidecar(work_dir, idx)
        if sc.get("kind"):  # already routed (forced mode) — leave it
            emit_progress(done, total)
            continue
        try:
            answer = ollama_chat(args.ollama_url, args.vlm_model, CLASSIFY_PROMPT,
                                 page_image(pages_dir, idx), want_json=False, timeout=180)
            kind = "handwriting" if "hand" in answer.strip().lower() else "printed"
        except (urllib.error.URLError, ConnectionError):
            failures += 1
            kind = "printed"  # safe default: faithful Tesseract path
        sc["kind"] = kind
        sc["engine"] = "qwen3-vl" if kind == "handwriting" else "tesseract"
        save_sidecar(work_dir, idx, sc)
        emit_progress(done, total)
    if failures and failures == total:
        fail("Could not reach the classification model")


def mode_htr(args, indices: list[int]) -> None:
    work_dir = Path(args.work_dir)
    pages_dir = Path(args.pages_dir)
    hw = [i for i in indices if load_sidecar(work_dir, i).get("kind") == "handwriting"]
    total = len(hw) or 1
    if not hw:
        emit_progress(1, 1)
        return
    ok = 0
    conn_fail = 0
    for done, idx in enumerate(hw, start=1):
        sc = load_sidecar(work_dir, idx)
        try:
            content = ollama_chat(args.ollama_url, args.vlm_model, HTR_PROMPT,
                                  page_image(pages_dir, idx), want_json=True, timeout=900)
            text, lines = parse_htr_json(content)
            ok += 1
        except (urllib.error.URLError, ConnectionError):
            conn_fail += 1
            text, lines = "[illegible]", []
        sc["text"] = text
        sc["lines"] = lines
        sc["engine"] = "qwen3-vl"
        sc["illegibleCount"] = count_markers(text)
        sc["confidence"] = confidence_for(text, len(lines))
        save_sidecar(work_dir, idx, sc)
        emit_progress(done, total)
    if ok == 0 and conn_fail:
        fail("Could not reach the handwriting model")


def mode_verify(args, indices: list[int]) -> None:
    work_dir = Path(args.work_dir)
    pages_dir = Path(args.pages_dir)
    hw = [i for i in indices if load_sidecar(work_dir, i).get("kind") == "handwriting"]
    total = len(hw) or 1
    if not hw:
        emit_progress(1, 1)
        return
    for done, idx in enumerate(hw, start=1):
        sc = load_sidecar(work_dir, idx)
        draft = sc.get("text", "")
        if count_markers(draft) == 0:
            emit_progress(done, total)
            continue  # nothing flagged to resolve
        try:
            content = ollama_chat(args.ollama_url, args.vlm_model, VERIFY_PROMPT + draft,
                                  page_image(pages_dir, idx), want_json=True, timeout=900)
            obj = json.loads(re.search(r"\{.*\}", content, re.DOTALL).group(0)) if "{" in content else {}
            revised = str(obj.get("text", "")).rstrip()
            if revised:
                sc["text"] = revised
                sc["illegibleCount"] = count_markers(revised)
                sc["confidence"] = confidence_for(revised, len(sc.get("lines") or []))
                save_sidecar(work_dir, idx, sc)
        except Exception:  # noqa: BLE001 — verify is best-effort; keep the draft
            pass
        emit_progress(done, total)


def flag_disagreements_line(vlm_line: str, other_line: str, other_name: str) -> tuple[str, int]:
    """Word-level diff within one line: VLM words that the other engine disagrees
    with are wrapped [?: word | other]. Verbatim VLM text is the source — we never
    inject the other engine's words, only flag ours."""
    import difflib  # noqa: PLC0415 — stdlib, lazy only for symmetry
    vlm_words = vlm_line.split()
    other_words = other_line.split()
    sm = difflib.SequenceMatcher(a=vlm_words, b=other_words)
    out: list[str] = []
    flags = 0
    for tag, i1, i2, _j1, _j2 in sm.get_opcodes():
        if tag == "equal":
            out.extend(vlm_words[i1:i2])
        elif tag in ("replace", "delete"):
            for w in vlm_words[i1:i2]:
                if w.startswith("[") or w.startswith("("):
                    out.append(w)  # already a marker / annotation
                    continue
                out.append(f"[?: {w} | {other_name}]")
                flags += 1
        # insert: other engine has extra words — ignored (verbatim VLM is source)
    return " ".join(out), flags


def apply_second_opinion(sc: dict, other_lines: list[str], other_name: str) -> None:
    """Align the VLM per-line text to the other engine's lines and flag
    disagreements in place. other_lines is the corroborating engine's text split
    into lines (paddle: full-page text; trocr: per-line crops)."""
    import difflib  # noqa: PLC0415
    vlm_text = sc.get("text", "")
    vlm_lines = vlm_text.split("\n")
    other_pool = [ln for ln in other_lines if ln.strip()]
    rebuilt: list[str] = []
    added_flags = 0
    for ln in vlm_lines:
        if not ln.strip():
            rebuilt.append(ln)
            continue
        match = difflib.get_close_matches(ln, other_pool, n=1, cutoff=0.3)
        if not match:
            rebuilt.append(ln)  # no corroboration available — not a disagreement
            continue
        flagged, n = flag_disagreements_line(ln, match[0], other_name)
        rebuilt.append(flagged)
        added_flags += n
    sc["text"] = "\n".join(rebuilt)
    sc["illegibleCount"] = count_markers(sc["text"])
    sc["engine"] = (sc.get("engine", "qwen3-vl") + "+" + other_name)


def mode_htr_paddle(args, indices: list[int]) -> None:
    work_dir = Path(args.work_dir)
    pages_dir = Path(args.pages_dir)
    hw = [i for i in indices if load_sidecar(work_dir, i).get("kind") == "handwriting"]
    total = len(hw) or 1
    if not hw:
        emit_progress(1, 1)
        return
    ok = 0
    for done, idx in enumerate(hw, start=1):
        sc = load_sidecar(work_dir, idx)
        try:
            paddle_text = paddle_chat(args.paddle_endpoint, args.paddle_model,
                                      PADDLE_HTR_PROMPT, page_image(pages_dir, idx))
            apply_second_opinion(sc, paddle_text.split("\n"), "paddle")
            save_sidecar(work_dir, idx, sc)
            ok += 1
        except Exception:  # noqa: BLE001 — corroboration is optional
            pass
        emit_progress(done, total)
    if ok == 0:
        fail("Could not reach the PaddleOCR-VL second-opinion server")


def mode_htr_trocr(args, indices: list[int]) -> None:
    work_dir = Path(args.work_dir)
    pages_dir = Path(args.pages_dir)
    hw = [i for i in indices if load_sidecar(work_dir, i).get("kind") == "handwriting"]
    total = len(hw) or 1
    if not hw:
        emit_progress(1, 1)
        return
    try:
        import torch  # noqa: PLC0415
        from PIL import Image  # noqa: PLC0415
        from transformers import TrOCRProcessor, VisionEncoderDecoderModel  # noqa: PLC0415
    except Exception as exc:  # noqa: BLE001
        fail(f"transformers/TrOCR not importable in this venv: {exc}")
        raise
    device = f"cuda:{args.secondary_gpu}" if torch.cuda.is_available() else "cpu"
    processor = TrOCRProcessor.from_pretrained(args.trocr_model)
    model = VisionEncoderDecoderModel.from_pretrained(args.trocr_model).to(device).eval()
    for done, idx in enumerate(hw, start=1):
        sc = load_sidecar(work_dir, idx)
        lines = sc.get("lines") or []
        if not lines:
            emit_progress(done, total)
            continue
        img = Image.open(page_image(pages_dir, idx)).convert("RGB")
        w, h = img.size
        trocr_lines: list[str] = []
        for ln in lines:
            bbox = ln.get("bbox") or []
            if len(bbox) != 4:
                trocr_lines.append("")
                continue
            x0, y0, x1, y1 = _scale_bbox(bbox, w, h)
            crop = img.crop((x0, y0, x1, y1))
            pixel_values = processor(images=crop, return_tensors="pt").pixel_values.to(device)
            with torch.no_grad():
                ids = model.generate(pixel_values)
            trocr_lines.append(processor.batch_decode(ids, skip_special_tokens=True)[0])
        apply_second_opinion(sc, trocr_lines, "trocr")
        save_sidecar(work_dir, idx, sc)
        emit_progress(done, total)


def _scale_bbox(bbox, w: int, h: int) -> tuple[int, int, int, int]:
    x0, y0, x1, y1 = (float(v) for v in bbox)
    if max(x0, y0, x1, y1) <= 1.0:  # normalized coordinates
        x0, x1 = x0 * w, x1 * w
        y0, y1 = y0 * h, y1 * h
    x0, x1 = sorted((max(0, int(x0)), min(w, int(x1))))
    y0, y1 = sorted((max(0, int(y0)), min(h, int(y1))))
    if x1 <= x0:
        x1 = min(w, x0 + 1)
    if y1 <= y0:
        y1 = min(h, y0 + 1)
    return x0, y0, x1, y1


# --- structured markdown (printed -> DOCX) -----------------------------------

def structure_markdown(args, idx: int) -> str:
    pages_dir = Path(args.pages_dir)
    img = page_image(pages_dir, idx)
    if args.structure_engine == "docling":
        try:
            from docling.document_converter import DocumentConverter  # noqa: PLC0415
        except Exception as exc:  # noqa: BLE001
            fail(f"docling not importable in this venv: {exc}")
            raise
        conv = DocumentConverter()
        res = conv.convert(str(img))
        return res.document.export_to_markdown()
    # default: paddleocr-vl over HTTP
    return paddle_chat(args.paddle_endpoint, args.paddle_model, PADDLE_STRUCTURE_PROMPT, img)


def mode_structure(args, indices: list[int]) -> None:
    work_dir = Path(args.work_dir)
    printed = [i for i in indices if load_sidecar(work_dir, i).get("kind", "printed") != "handwriting"]
    total = len(printed) or 1
    for done, idx in enumerate(printed, start=1):
        try:
            md = structure_markdown(args, idx)
            (work_dir / f"page-{idx:03d}.md").write_text(md)
        except Exception:  # noqa: BLE001 — fall back to empty section
            (work_dir / f"page-{idx:03d}.md").write_text("")
        emit_progress(done, total)


# --- build-pdf ----------------------------------------------------------------

def mode_build_pdf(args, indices: list[int]) -> None:
    work_dir = Path(args.work_dir)
    pages_dir = Path(args.pages_dir)
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    tmp_dir = work_dir / "pdf_pages"
    tmp_dir.mkdir(parents=True, exist_ok=True)

    try:
        import img2pdf  # noqa: PLC0415
        import pikepdf  # noqa: PLC0415
    except Exception as exc:  # noqa: BLE001
        fail(f"img2pdf/pikepdf not importable in this venv: {exc}")
        raise

    total = len(indices)
    page_pdfs: list[Path] = []
    for done, idx in enumerate(indices, start=1):
        sc = load_sidecar(work_dir, idx)
        kind = sc.get("kind", "printed")
        img = page_image(pages_dir, idx)
        out_pdf = tmp_dir / f"page-{idx:03d}.pdf"
        if kind == "handwriting":
            _handwriting_page_pdf(img, sc, out_pdf)
        else:
            _printed_page_pdf(args, img, out_pdf, tmp_dir, idx, img2pdf)
        if out_pdf.is_file():
            page_pdfs.append(out_pdf)
        emit_progress(done, total)

    if not page_pdfs:
        fail("no pages could be rendered to PDF")

    merged = pikepdf.Pdf.new()
    for p in page_pdfs:
        try:
            src = pikepdf.Pdf.open(str(p))
            merged.pages.extend(src.pages)
        except Exception:  # noqa: BLE001
            continue
    if len(merged.pages) == 0:
        fail("could not assemble the output PDF")
    merged.save(str(out_dir / "document.pdf"))


def _printed_page_pdf(args, img: Path, out_pdf: Path, tmp_dir: Path, idx: int, img2pdf) -> None:
    """img2pdf the scan, then OCRmyPDF a faithful Tesseract text layer underneath."""
    base_pdf = tmp_dir / f"page-{idx:03d}.base.pdf"
    base_pdf.write_bytes(img2pdf.convert(str(img)))
    try:
        import ocrmypdf  # noqa: PLC0415
        ocrmypdf.ocr(
            str(base_pdf), str(out_pdf),
            language=args.language or "eng",
            deskew=bool(args.deskew),
            rotate_pages=bool(args.rotate),
            clean=bool(args.clean),
            output_type="pdfa",
            progress_bar=False,
            force_ocr=True,
        )
    except Exception:  # noqa: BLE001 — fall back to the image-only page (still valid)
        if base_pdf.is_file():
            base_pdf.replace(out_pdf)


def _handwriting_page_pdf(img: Path, sc: dict, out_pdf: Path) -> None:
    """Scan image as the visible page + an invisible (render mode 3) machine
    transcription layer, positioned per line via grounding boxes (full-page block
    fallback), with a visible 'machine transcription' footer stamp."""
    from PIL import Image  # noqa: PLC0415
    from reportlab.lib.utils import ImageReader  # noqa: PLC0415
    from reportlab.pdfgen import canvas  # noqa: PLC0415

    image = Image.open(img).convert("RGB")
    w, h = image.size
    c = canvas.Canvas(str(out_pdf), pagesize=(w, h))
    c.drawImage(ImageReader(str(img)), 0, 0, width=w, height=h)

    lines = sc.get("lines") or []
    placed = False
    for ln in lines:
        bbox = ln.get("bbox") or []
        text = (ln.get("text") or "").strip()
        if len(bbox) != 4 or not text:
            continue
        x0, y0, x1, y1 = _scale_bbox(bbox, w, h)
        font_size = max(6, min(48, y1 - y0))
        t = c.beginText(x0, h - y1)  # PDF origin is bottom-left
        t.setTextRenderMode(3)       # invisible: searchable but not drawn
        t.setFont("Helvetica", font_size)
        t.textLine(text)
        c.drawText(t)
        placed = True

    if not placed:
        # Full-page invisible block from the whole transcription.
        full = (sc.get("text") or "").split("\n")
        t = c.beginText(10, h - 16)
        t.setTextRenderMode(3)
        t.setFont("Helvetica", 10)
        for line in full:
            t.textLine(line)
        c.drawText(t)

    c.setFont("Helvetica", 8)
    c.setFillGray(0.4)
    c.drawString(8, 6, "Machine transcription — verify against original.")
    c.showPage()
    c.save()


# --- build-docx ---------------------------------------------------------------

def mode_build_docx(args, indices: list[int]) -> None:
    work_dir = Path(args.work_dir)
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    total = len(indices)
    sections: list[str] = []
    for done, idx in enumerate(indices, start=1):
        sc = load_sidecar(work_dir, idx)
        kind = sc.get("kind", "printed")
        sections.append(f"\n\n<!-- page {idx} -->\n\n")
        if kind == "handwriting":
            sections.append(HANDWRITING_DOCX_NOTE + "\n\n")
            body = sc.get("text", "") or "_[no transcription]_"
            # Preserve handwritten line breaks in Markdown (two trailing spaces).
            sections.append("  \n".join(body.split("\n")))
        else:
            md_file = work_dir / f"page-{idx:03d}.md"
            if not md_file.is_file():
                try:
                    md_file.write_text(structure_markdown(args, idx))
                except Exception:  # noqa: BLE001
                    md_file.write_text("")
            sections.append(md_file.read_text())
        emit_progress(done, total)

    combined = work_dir / "combined.md"
    header = (
        "% Document Scan — structured reconstruction\n\n"
        "> This document is a machine reconstruction/transcription. "
        "Verify against the original scan.\n\n"
    )
    combined.write_text(header + "".join(sections))
    out_docx = out_dir / "document.docx"
    _pandoc(args.pandoc_bin, combined, out_docx)
    if not out_docx.is_file():
        fail("pandoc did not produce a DOCX")


# --- summary ------------------------------------------------------------------

def mode_summary(args, indices: list[int]) -> None:
    work_dir = Path(args.work_dir)
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    parts: list[str] = []
    for idx in indices:
        sc = load_sidecar(work_dir, idx)
        if sc.get("kind") == "handwriting":
            parts.append(sc.get("text", ""))
        else:
            md_file = work_dir / f"page-{idx:03d}.md"
            if md_file.is_file():
                parts.append(md_file.read_text())
    transcription = "\n\n".join(p for p in parts if p.strip())
    if not transcription.strip():
        fail("no transcription text available to summarize")

    emit_progress(1, 2)
    try:
        md = ollama_chat(args.ollama_url, args.text_model, SUMMARY_PROMPT + transcription,
                         image_path=None, want_json=False, timeout=900)
    except (urllib.error.URLError, ConnectionError):
        fail("could not reach the summary model")
        return
    summary_md = work_dir / "summary.md"
    header = (
        "% AI-generated summary — not a verbatim transcription\n\n"
        "> AI-generated summary — not a verbatim transcription. "
        "It may omit or rephrase details; the source scans remain authoritative.\n\n"
    )
    summary_md.write_text(header + (md or "_No summary produced._"))
    out_docx = out_dir / "document.summary.docx"
    _pandoc(args.pandoc_bin, summary_md, out_docx)
    if not out_docx.is_file():
        fail("pandoc did not produce the summary DOCX")
    emit_progress(2, 2)


def _pandoc(pandoc_bin: str, md_path: Path, out_path: Path) -> None:
    try:
        subprocess.run(
            [pandoc_bin or "pandoc", str(md_path), "-o", str(out_path)],
            check=True, capture_output=True, timeout=300,
        )
    except subprocess.CalledProcessError as exc:  # noqa: BLE001
        tail = (exc.stderr or b"").decode("utf-8", "replace")[-300:]
        fail(f"pandoc failed: {tail}")
    except FileNotFoundError:
        fail("pandoc is not installed on this server")


# --- main ---------------------------------------------------------------------

MODES = {
    "classify": mode_classify,
    "htr": mode_htr,
    "htr-verify": mode_verify,
    "htr-paddle": mode_htr_paddle,
    "htr-trocr": mode_htr_trocr,
    "structure": mode_structure,
    "build-pdf": mode_build_pdf,
    "build-docx": mode_build_docx,
    "summary": mode_summary,
}


def main() -> None:
    parser = argparse.ArgumentParser(description="AI Document Scan orchestration helper")
    parser.add_argument("--mode", required=True, choices=sorted(MODES.keys()))
    parser.add_argument("--pages-dir", required=True)
    parser.add_argument("--work-dir", required=True)
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--order", default="")
    parser.add_argument("--language", default="eng")
    parser.add_argument("--gpu", default="1")
    parser.add_argument("--secondary-gpu", default="0")
    parser.add_argument("--deskew", action="store_true")
    parser.add_argument("--rotate", action="store_true")
    parser.add_argument("--clean", action="store_true")
    parser.add_argument("--pandoc-bin", default="pandoc")
    parser.add_argument("--ollama-url", default="http://localhost:11434")
    parser.add_argument("--vlm-model", default="qwen3-vl:8b-instruct-q8_0")
    parser.add_argument("--text-model", default="qwen3.5:9b-q8_0")
    parser.add_argument("--paddle-endpoint", default="http://127.0.0.1:8080/v1")
    parser.add_argument("--paddle-model", default="PaddleOCR-VL-1.6-0.9B")
    parser.add_argument("--structure-engine", default="paddleocr-vl", choices=["paddleocr-vl", "docling"])
    parser.add_argument("--trocr-model", default="microsoft/trocr-large-handwritten")
    args = parser.parse_args()

    work_dir = Path(args.work_dir)
    work_dir.mkdir(parents=True, exist_ok=True)
    pages_dir = Path(args.pages_dir)
    if not pages_dir.is_dir():
        fail(f"pages directory does not exist: {pages_dir}")

    indices = ordered_indices(args.order, pages_dir)
    if not indices:
        fail("no pages to process")

    MODES[args.mode](args, indices)


if __name__ == "__main__":
    main()
