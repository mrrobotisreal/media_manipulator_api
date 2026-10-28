#!/usr/bin/env python3
"""mm-voices: the resident voices service for Media Manipulator (MM77, ADR-0015).

A small FastAPI process that keeps the voice engines resident on GPU 0 of the
GPU host and answers the Go API over 127.0.0.1:8791 (8790 is mitchaelina-api).
Loading a model takes 10-20 s and a request produces 5-30 s of audio, so a
per-job subprocess (the pattern every other AI tool uses) would spend most of
its time loading weights. Design: docs/design/VOICES.md section 3.

Engines (all commercially licensed, all zero-shot, nothing is ever trained):
    chatterbox  Chatterbox Multilingual (Resemble AI, MIT): text-to-speech from a
                5-60 s reference clip (/tts) and voice conversion (/vc).
    qwen_tts    Qwen3-TTS 1.7B VoiceDesign (/design: a voice from a text
                description) and CustomVoice (/presets, /tts-preset), Apache 2.0.

Every output carries the inaudible PerTh watermark (Chatterbox applies it
itself; Qwen output is watermarked here with the same `perth` package).

Files travel by path, never by body: the Go API writes inputs under the shared
scratch dir it owns (VOICES_SCRATCH_DIR) and names the output path; this
service refuses any path that resolves outside that dir (symlinks included).

Engines load lazily on first use, one threading.Lock per engine, and an
idle thread unloads an engine after VOICES_IDLE_UNLOAD_SECONDS (default 600)
and frees its CUDA memory so the card is free for other work between bursts.

Environment (all optional; defaults in brackets):
    VOICES_HOST [127.0.0.1]   VOICES_PORT [8791]
    VOICES_SCRATCH_DIR [/opt/media-manipulator-ai/tmp/voices]
    VOICES_IDLE_UNLOAD_SECONDS [600]
    VOICES_DEVICE [cuda]      (CUDA_VISIBLE_DEVICES pins the card)
    VOICES_CHATTERBOX_REPO [ResembleAI/chatterbox]
    VOICES_QWEN_DESIGN_REPO [Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign]
    VOICES_QWEN_CUSTOM_REPO [Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice]
    VOICES_QWEN_ATTN [unset]  (e.g. flash_attention_2 when it is installed)
    VOICES_BUILTIN_PRESETS_DIR [/opt/media-manipulator-ai/models/voices/presets]
        Drop <id>.wav (5-60 s of clean speech) and an optional <id>.json
        ({"name": "...", "description": "...", "language": "en"}) here and the
        clip becomes a built-in preset spoken by Chatterbox. No code change.
    HF_HOME (the Hugging Face cache the weights were downloaded into;
        /var/lib/creatv/hf on the host, shared with the existing Qwen3-TTS service)

Deploy (the repo copy is the source of truth):
    sudo cp scripts/server/voices_service.py /opt/media-manipulator-ai/scripts/voices_service.py
    sudo systemctl restart mm-voices

Self-test (the model acceptance check, MM76 S4):
    python voices_service.py --self-test --out /opt/media-manipulator-ai/tmp/voices/selftest

Module-level imports are stdlib only: torch, the engines, numpy, soundfile,
FastAPI and uvicorn are imported lazily, so `python -m py_compile` and
`--help` work on a machine without any of them. Never logs text, audio
contents or reference paths at info level (voices are biometric-adjacent).
"""
import argparse
import gc
import json
import logging
import os
import re
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Tuple

SERVICE_VERSION = "1.0.0"

DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8791
DEFAULT_SCRATCH_DIR = "/opt/media-manipulator-ai/tmp/voices"
DEFAULT_PRESETS_DIR = "/opt/media-manipulator-ai/models/voices/presets"
DEFAULT_IDLE_UNLOAD_SECONDS = 600
DEFAULT_CHATTERBOX_REPO = "ResembleAI/chatterbox"
DEFAULT_QWEN_DESIGN_REPO = "Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign"
DEFAULT_QWEN_CUSTOM_REPO = "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice"
# The files ChatterboxMultilingualTTS.from_local and ChatterboxVC.from_local read.
CHATTERBOX_FILES = ("ve.pt", "t3_mtl23ls_v2.safetensors", "s3gen.pt", "s3gen.safetensors",
                    "grapheme_mtl_merged_expanded_v1.json", "conds.pt", "Cangjie5_TC.json")

# Chatterbox Multilingual's language ids (ISO 639-1). The API validates against
# the same list (internal/services/voices_client.go VoiceLanguages).
CHATTERBOX_LANGUAGES = [
    "ar", "da", "de", "el", "en", "es", "fi", "fr", "he", "hi", "it", "ja",
    "ko", "ms", "nl", "no", "pl", "pt", "ru", "sv", "sw", "tr", "zh",
]

# Qwen3-TTS takes language names; the API speaks ISO codes.
QWEN_LANGUAGES = {
    "zh": "Chinese", "en": "English", "ja": "Japanese", "ko": "Korean",
    "de": "German", "fr": "French", "ru": "Russian", "pt": "Portuguese",
    "es": "Spanish", "it": "Italian",
}

# CustomVoice speakers, used for /presets when the model is not loaded (the
# loaded model's own list wins). Model cards move: MM76 pins the real list.
QWEN_DEFAULT_SPEAKERS = [
    "Vivian", "Serena", "Uncle_Fu", "Dylan", "Eric", "Ryan", "Aiden",
    "Ono_Anna", "Sohee",
]

MAX_TEXT_CHARS = 20000          # 900 s x 18 chars/s is 16200; a backstop only
CHUNK_MAX_CHARS = 280           # one engine call; Chatterbox tops out near 40 s
CHUNK_SILENCE_SECONDS = 0.15
VC_CHUNK_SECONDS = 30.0
VC_SEARCH_SECONDS = 5.0
IDLE_CHECK_INTERVAL_SECONDS = 15

SELF_TEST_SENTENCE = "Media Manipulator voices are running, and this sentence proves it."
SELF_TEST_DESCRIPTION = (
    "A warm, calm, middle-aged male narrator with a low, slightly raspy voice "
    "and an unhurried pace."
)

log = logging.getLogger("mm-voices")


def env_str(name: str, default: str) -> str:
    value = os.environ.get(name, "").strip()
    return value or default


def env_int(name: str, default: int) -> int:
    try:
        return int(os.environ.get(name, "").strip())
    except ValueError:
        return default


# --- errors -------------------------------------------------------------------


class ServiceError(Exception):
    """An error with an HTTP status and a stable machine code for the Go API."""

    def __init__(self, status: int, code: str, message: str = "") -> None:
        super().__init__(message or code)
        self.status = status
        self.code = code
        self.message = message or code


# --- paths --------------------------------------------------------------------


def resolve_in_scratch(scratch_dir: str, raw: Any, field: str, must_exist: bool) -> Path:
    """Resolve a request path and refuse anything outside the scratch dir.

    Symlinks and `..` are resolved before the containment check, so a link
    inside the scratch dir that points out of it is refused too.
    """
    if not isinstance(raw, str) or not raw.strip():
        raise ServiceError(400, "invalid_path", f"{field} is required")
    if "\x00" in raw:
        raise ServiceError(400, "invalid_path", f"{field} is not a valid path")
    root = Path(os.path.realpath(scratch_dir))
    candidate = Path(raw)
    if not candidate.is_absolute():
        raise ServiceError(400, "invalid_path", f"{field} must be absolute")
    resolved = Path(os.path.realpath(candidate))
    try:
        resolved.relative_to(root)
    except ValueError:
        raise ServiceError(403, "path_outside_scratch", f"{field} is outside the scratch dir")
    if resolved == root:
        raise ServiceError(400, "invalid_path", f"{field} must name a file")
    if must_exist and not resolved.is_file():
        raise ServiceError(400, "file_not_found", f"{field} does not exist")
    return resolved


# --- text and audio helpers (pure, unit-tested in test_voices_service.py) ------

_SENTENCE_END = re.compile(r"(?<=[.!?…。！？;:])\s+")


def chunk_text(text: str, max_chars: int = CHUNK_MAX_CHARS) -> List[str]:
    """Split text into engine-sized chunks on sentence boundaries.

    A sentence longer than max_chars is split on commas, then on spaces, then
    hard-cut, so no chunk exceeds max_chars and no text is dropped.
    """
    text = re.sub(r"\s+", " ", text or "").strip()
    if not text:
        return []
    pieces: List[str] = []
    for sentence in _SENTENCE_END.split(text):
        sentence = sentence.strip()
        if not sentence:
            continue
        if len(sentence) <= max_chars:
            pieces.append(sentence)
            continue
        pieces.extend(_split_long(sentence, max_chars))
    chunks: List[str] = []
    current = ""
    for piece in pieces:
        if not current:
            current = piece
        elif len(current) + 1 + len(piece) <= max_chars:
            current = f"{current} {piece}"
        else:
            chunks.append(current)
            current = piece
    if current:
        chunks.append(current)
    return chunks


def _split_long(sentence: str, max_chars: int) -> List[str]:
    out: List[str] = []
    for sep in (", ", "，", " "):
        if sep in sentence:
            parts = sentence.split(sep)
            current = ""
            for part in parts:
                candidate = f"{current}{sep}{part}" if current else part
                if len(candidate) <= max_chars:
                    current = candidate
                    continue
                if current:
                    out.append(current)
                if len(part) <= max_chars:
                    current = part
                else:
                    out.extend(_hard_cut(part, max_chars))
                    current = ""
            if current:
                out.append(current)
            return out
    return _hard_cut(sentence, max_chars)


def _hard_cut(text: str, max_chars: int) -> List[str]:
    return [text[i:i + max_chars] for i in range(0, len(text), max_chars)]


def split_points(total: int, sample_rate: int, energy: Optional[Callable[[int, int], float]] = None,
                 chunk_seconds: float = VC_CHUNK_SECONDS,
                 search_seconds: float = VC_SEARCH_SECONDS) -> List[Tuple[int, int]]:
    """Cut [0, total) samples into windows of about chunk_seconds.

    When an energy(start, end) callback is given, each cut lands on the
    quietest 50 ms frame within the last search_seconds of the window so a
    word is not split mid-syllable.
    """
    size = max(1, int(chunk_seconds * sample_rate))
    search = max(0, int(search_seconds * sample_rate))
    frame = max(1, int(0.05 * sample_rate))
    spans: List[Tuple[int, int]] = []
    start = 0
    while start < total:
        end = min(total, start + size)
        if end < total and energy is not None and search > frame:
            best_at, best_energy = end, None
            probe = max(start + frame, end - search)
            while probe + frame <= end:
                e = energy(probe, probe + frame)
                if best_energy is None or e < best_energy:
                    best_at, best_energy = probe + frame // 2, e
                probe += frame
            end = best_at
        spans.append((start, end))
        start = end
    return spans


def clamp(value: Any, low: float, high: float, default: float) -> float:
    try:
        v = float(value)
    except (TypeError, ValueError):
        return default
    if v != v:  # NaN
        return default
    return max(low, min(high, v))


def package_version(dist: str) -> Optional[str]:
    try:
        from importlib.metadata import PackageNotFoundError, version
    except ImportError:  # pragma: no cover
        return None
    try:
        return version(dist)
    except PackageNotFoundError:
        return None


# --- engines ------------------------------------------------------------------


class Engine:
    """A lazily loaded model with its own lock and idle clock."""

    def __init__(self, name: str, group: str, loader: Callable[[], Any]) -> None:
        self.name = name
        self.group = group
        self._loader = loader
        self.lock = threading.Lock()
        self.model: Any = None
        self.last_used = 0.0

    @property
    def loaded(self) -> bool:
        return self.model is not None

    def get(self) -> Any:
        """Return the model, loading it first. Caller must hold self.lock."""
        if self.model is None:
            started = time.monotonic()
            log.info("loading %s", self.name)
            try:
                self.model = self._loader()
            except Exception as exc:  # noqa: BLE001 - surfaced as 503
                log.exception("loading %s failed", self.name)
                raise ServiceError(503, "engine_load_failed", f"{self.name}: {exc.__class__.__name__}")
            log.info("loaded %s in %.1fs", self.name, time.monotonic() - started)
        self.last_used = time.monotonic()
        return self.model

    def touch(self) -> None:
        self.last_used = time.monotonic()

    def unload(self) -> bool:
        """Drop the model. Caller must hold self.lock."""
        if self.model is None:
            return False
        self.model = None
        free_cuda()
        log.info("unloaded %s", self.name)
        return True


def free_cuda() -> None:
    gc.collect()
    if "torch" in sys.modules:
        torch = sys.modules["torch"]
        try:
            if torch.cuda.is_available():
                torch.cuda.empty_cache()
                torch.cuda.ipc_collect()
        except Exception:  # noqa: BLE001
            pass


class Voices:
    """The engines plus the request operations; HTTP-agnostic so the
    self-test and the FastAPI app share exactly one code path."""

    def __init__(self, scratch_dir: str, presets_dir: str, idle_unload_seconds: int,
                 device: str, chatterbox_repo: str, qwen_design_repo: str,
                 qwen_custom_repo: str, qwen_attn: str = "") -> None:
        self.scratch_dir = scratch_dir
        self.presets_dir = presets_dir
        self.idle_unload_seconds = idle_unload_seconds
        self.device = device
        self.chatterbox_repo = chatterbox_repo
        self.qwen_design_repo = qwen_design_repo
        self.qwen_custom_repo = qwen_custom_repo
        self.qwen_attn = qwen_attn
        self.started = time.monotonic()
        self.last_request = time.monotonic()
        self._gpu_name: Optional[str] = None
        self._watermarker: Any = None
        self._watermark_lock = threading.Lock()
        self.engines: Dict[str, Engine] = {
            "chatterbox_tts": Engine("chatterbox_tts", "chatterbox", self._load_chatterbox_tts),
            "chatterbox_vc": Engine("chatterbox_vc", "chatterbox", self._load_chatterbox_vc),
            "qwen_design": Engine("qwen_design", "qwen_tts", self._load_qwen_design),
            "qwen_custom": Engine("qwen_custom", "qwen_tts", self._load_qwen_custom),
        }

    # -- loaders (heavy imports live only here) --

    def _chatterbox_dir(self) -> str:
        from huggingface_hub import snapshot_download
        # Only the multilingual TTS + VC files (~4.3 GB), not the whole repo (~14 GB).
        return snapshot_download(repo_id=self.chatterbox_repo, allow_patterns=list(CHATTERBOX_FILES))

    def _load_chatterbox_tts(self) -> Any:
        from chatterbox.mtl_tts import ChatterboxMultilingualTTS
        if hasattr(ChatterboxMultilingualTTS, "from_local"):
            return ChatterboxMultilingualTTS.from_local(self._chatterbox_dir(), self.device)
        return ChatterboxMultilingualTTS.from_pretrained(device=self.device)

    def _load_chatterbox_vc(self) -> Any:
        from chatterbox.vc import ChatterboxVC
        if hasattr(ChatterboxVC, "from_local"):
            return ChatterboxVC.from_local(self._chatterbox_dir(), self.device)
        return ChatterboxVC.from_pretrained(self.device)

    def _load_qwen(self, repo: str) -> Any:
        import torch
        from qwen_tts import Qwen3TTSModel
        on_cuda = self.device.startswith("cuda")
        kwargs: Dict[str, Any] = {
            "device_map": "cuda:0" if self.device == "cuda" else self.device,
            "dtype": torch.bfloat16 if on_cuda else torch.float32,
        }
        if self.qwen_attn:
            kwargs["attn_implementation"] = self.qwen_attn
        return Qwen3TTSModel.from_pretrained(repo, **kwargs)

    def _load_qwen_design(self) -> Any:
        return self._load_qwen(self.qwen_design_repo)

    def _load_qwen_custom(self) -> Any:
        return self._load_qwen(self.qwen_custom_repo)

    # -- shared helpers --

    def gpu_name(self) -> Optional[str]:
        if self._gpu_name is not None:
            return self._gpu_name or None
        name = ""
        try:
            cmd = ["nvidia-smi", "--query-gpu=name", "--format=csv,noheader"]
            visible = os.environ.get("CUDA_VISIBLE_DEVICES", "").split(",")[0].strip()
            if visible:
                cmd += ["-i", visible]
            out = subprocess.run(cmd, capture_output=True, text=True, timeout=5)
            if out.returncode == 0:
                name = out.stdout.strip().splitlines()[0].strip() if out.stdout.strip() else ""
        except Exception:  # noqa: BLE001 - no GPU / no nvidia-smi is fine
            name = ""
        self._gpu_name = name
        return name or None

    def watermark(self, wav: Any, sr: int) -> Tuple[Any, bool]:
        """Apply the PerTh watermark to engine output that lacks one."""
        with self._watermark_lock:
            if self._watermarker is None:
                try:
                    import perth
                    self._watermarker = perth.PerthImplicitWatermarker()
                except Exception:  # noqa: BLE001
                    log.warning("perth watermarker unavailable; output not watermarked")
                    self._watermarker = False
            if self._watermarker is False:
                return wav, False
            return self._watermarker.apply_watermark(wav, sample_rate=sr), True

    @staticmethod
    def to_numpy(wav: Any) -> Any:
        import numpy as np
        if hasattr(wav, "detach"):
            wav = wav.detach().cpu().float().numpy()
        arr = np.asarray(wav, dtype=np.float32)
        return arr.reshape(-1) if arr.ndim > 1 else arr

    @staticmethod
    def concat(parts: List[Any], sr: int) -> Any:
        import numpy as np
        if not parts:
            return np.zeros(0, dtype=np.float32)
        gap = np.zeros(int(CHUNK_SILENCE_SECONDS * sr), dtype=np.float32)
        out: List[Any] = []
        for i, part in enumerate(parts):
            if i:
                out.append(gap)
            out.append(part)
        return np.concatenate(out)

    @staticmethod
    def write_wav(path: Path, wav: Any, sr: int) -> float:
        import soundfile as sf
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_name(f".{path.name}.{uuid.uuid4().hex}.tmp.wav")
        try:
            sf.write(str(tmp), wav, sr, subtype="PCM_16")
            os.replace(tmp, path)
        finally:
            if tmp.exists():
                tmp.unlink()
        return round(len(wav) / float(sr), 3) if sr else 0.0

    @staticmethod
    def seed_all(seed: int) -> None:
        import random
        import torch
        random.seed(seed)
        torch.manual_seed(seed)
        if torch.cuda.is_available():
            torch.cuda.manual_seed_all(seed)
        try:
            import numpy as np
            np.random.seed(seed % (2 ** 32))
        except Exception:  # noqa: BLE001
            pass

    def _text(self, raw: Any) -> List[str]:
        if not isinstance(raw, str) or not raw.strip():
            raise ServiceError(400, "text_required", "text is required")
        if len(raw) > MAX_TEXT_CHARS:
            raise ServiceError(400, "text_too_long", f"text is over {MAX_TEXT_CHARS} characters")
        return chunk_text(raw)

    @staticmethod
    def _chatterbox_language(raw: Any) -> str:
        lang = (raw or "en").strip().lower() if isinstance(raw, str) else "en"
        if lang not in CHATTERBOX_LANGUAGES:
            raise ServiceError(400, "language_not_supported", f"language {lang!r} is not supported")
        return lang

    @staticmethod
    def _qwen_language(raw: Any, model: Any = None) -> str:
        lang = (raw or "en").strip().lower() if isinstance(raw, str) else "en"
        name = QWEN_LANGUAGES.get(lang)
        if name is None:
            raise ServiceError(400, "language_not_supported", f"language {lang!r} is not supported by qwen_tts")
        supported = None
        if model is not None and hasattr(model, "get_supported_languages"):
            try:
                supported = {str(s).lower() for s in model.get_supported_languages()}
            except Exception:  # noqa: BLE001
                supported = None
        if supported and name.lower() not in supported:
            raise ServiceError(400, "language_not_supported", f"language {lang!r} is not supported by qwen_tts")
        return name

    def _begin(self) -> None:
        self.last_request = time.monotonic()

    # -- operations --

    def health(self, port: int) -> Dict[str, Any]:
        groups: Dict[str, Dict[str, Any]] = {
            "chatterbox": {"loaded": False, "version": package_version("chatterbox-tts"),
                           "repo": self.chatterbox_repo, "models": []},
            "qwen_tts": {"loaded": False, "version": package_version("qwen-tts"),
                         "repo": {"design": self.qwen_design_repo, "custom": self.qwen_custom_repo},
                         "models": []},
        }
        last_used = 0.0
        for engine in self.engines.values():
            last_used = max(last_used, engine.last_used)
            if engine.loaded:
                groups[engine.group]["loaded"] = True
                groups[engine.group]["models"].append(engine.name.split("_", 1)[1])
        reference = max(last_used, self.started)
        return {
            "status": "ok",
            "service": "mm-voices",
            "serviceVersion": SERVICE_VERSION,
            "engines": groups,
            "gpu": self.gpu_name(),
            "port": port,
            "idleSeconds": int(time.monotonic() - reference),
            "idleUnloadSeconds": self.idle_unload_seconds,
            "languages": list(CHATTERBOX_LANGUAGES),
            "designLanguages": list(QWEN_LANGUAGES.keys()),
        }

    def builtin_presets(self) -> List[Dict[str, Any]]:
        root = Path(self.presets_dir)
        if not self.presets_dir or not root.is_dir():
            return []
        out: List[Dict[str, Any]] = []
        for wav in sorted(root.glob("*.wav")):
            preset_id = wav.stem
            if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", preset_id):
                continue
            meta: Dict[str, Any] = {}
            sidecar = wav.with_suffix(".json")
            if sidecar.is_file():
                try:
                    loaded = json.loads(sidecar.read_text(encoding="utf-8"))
                    if isinstance(loaded, dict):
                        meta = loaded
                except (OSError, ValueError):
                    meta = {}
            out.append({
                "id": preset_id,
                "name": str(meta.get("name") or preset_id),
                "description": str(meta.get("description") or ""),
                "language": str(meta.get("language") or ""),
                "engine": "chatterbox",
                "kind": "builtin",
                "languages": list(CHATTERBOX_LANGUAGES),
            })
        return out

    def presets(self) -> Dict[str, Any]:
        engine = self.engines["qwen_custom"]
        speakers: List[str] = list(QWEN_DEFAULT_SPEAKERS)
        if engine.loaded and hasattr(engine.model, "get_supported_speakers"):
            try:
                speakers = [str(s) for s in engine.model.get_supported_speakers()]
            except Exception:  # noqa: BLE001
                pass
        builtin = self.builtin_presets()
        taken = {p["id"] for p in builtin}
        qwen = [{
            "id": s,
            "name": s.replace("_", " "),
            "description": "",
            "language": "",
            "engine": "qwen_tts",
            "kind": "custom_voice",
            "languages": list(QWEN_LANGUAGES.keys()),
        } for s in speakers if s not in taken]
        return {"presets": builtin + qwen}

    def tts(self, body: Dict[str, Any]) -> Dict[str, Any]:
        """Chatterbox TTS from a reference clip (or its built-in voice when
        referenceWav is omitted, used only by the self-test)."""
        self._begin()
        chunks = self._text(body.get("text"))
        lang = self._chatterbox_language(body.get("language"))
        out = resolve_in_scratch(self.scratch_dir, body.get("outWav"), "outWav", must_exist=False)
        ref: Optional[Path] = None
        if body.get("referenceWav") is not None or not body.get("_allowBuiltinVoice"):
            ref = resolve_in_scratch(self.scratch_dir, body.get("referenceWav"), "referenceWav", must_exist=True)
        return self._chatterbox_speak(chunks, lang, ref, out, body)

    def _chatterbox_speak(self, chunks: List[str], lang: str, ref: Optional[Path], out: Path,
                          body: Dict[str, Any]) -> Dict[str, Any]:
        exaggeration = clamp(body.get("exaggeration"), 0.25, 2.0, 0.5)
        cfg = clamp(body.get("cfg"), 0.0, 1.0, 0.5)
        temperature = clamp(body.get("temperature"), 0.05, 2.0, 0.8)
        engine = self.engines["chatterbox_tts"]
        started = time.monotonic()
        with engine.lock:
            model = engine.get()
            parts = []
            for i, chunk in enumerate(chunks):
                kwargs: Dict[str, Any] = {
                    "language_id": lang,
                    "exaggeration": exaggeration,
                    "cfg_weight": cfg,
                    "temperature": temperature,
                }
                # The reference conditionals are computed on the first chunk
                # and reused by the model for the rest.
                if ref is not None and i == 0:
                    kwargs["audio_prompt_path"] = str(ref)
                parts.append(self.to_numpy(model.generate(chunk, **kwargs)))
            sr = int(model.sr)
            engine.touch()
        seconds = self.write_wav(out, self.concat(parts, sr), sr)
        return {"outWav": str(out), "seconds": seconds, "sampleRate": sr, "engine": "chatterbox",
                "version": package_version("chatterbox-tts"), "watermarked": True,
                "elapsedSeconds": round(time.monotonic() - started, 2)}

    def design(self, body: Dict[str, Any]) -> Dict[str, Any]:
        """Render a reference clip of a voice designed from a description."""
        self._begin()
        description = body.get("description")
        if not isinstance(description, str) or not description.strip():
            raise ServiceError(400, "description_required", "description is required")
        if len(description) > 1000:
            raise ServiceError(400, "description_too_long", "description is over 1000 characters")
        sample = body.get("sampleText")
        chunks = self._text(sample)
        try:
            seed = int(body.get("seed"))
        except (TypeError, ValueError):
            raise ServiceError(400, "seed_required", "seed must be an integer")
        out = resolve_in_scratch(self.scratch_dir, body.get("outWav"), "outWav", must_exist=False)
        engine = self.engines["qwen_design"]
        started = time.monotonic()
        with engine.lock:
            model = engine.get()
            lang = self._qwen_language(body.get("language"), model)
            self.seed_all(seed)
            parts = []
            sr = 24000
            for chunk in chunks:
                wavs, sr = model.generate_voice_design(text=chunk, language=lang, instruct=description.strip())
                parts.append(self.to_numpy(wavs[0]))
            engine.touch()
        wav, marked = self.watermark(self.concat(parts, int(sr)), int(sr))
        seconds = self.write_wav(out, wav, int(sr))
        return {"outWav": str(out), "seconds": seconds, "sampleRate": int(sr), "engine": "qwen_tts",
                "version": package_version("qwen-tts"), "seed": seed, "watermarked": marked,
                "elapsedSeconds": round(time.monotonic() - started, 2)}

    def tts_preset(self, body: Dict[str, Any]) -> Dict[str, Any]:
        self._begin()
        chunks = self._text(body.get("text"))
        preset = body.get("preset")
        if not isinstance(preset, str) or not preset.strip():
            raise ServiceError(400, "preset_required", "preset is required")
        preset = preset.strip()
        out = resolve_in_scratch(self.scratch_dir, body.get("outWav"), "outWav", must_exist=False)
        builtin = {p["id"]: p for p in self.builtin_presets()}
        if preset in builtin:
            lang = self._chatterbox_language(body.get("language"))
            ref = Path(self.presets_dir) / f"{preset}.wav"
            return self._chatterbox_speak(chunks, lang, ref, out, body)
        engine = self.engines["qwen_custom"]
        started = time.monotonic()
        with engine.lock:
            model = engine.get()
            speakers = QWEN_DEFAULT_SPEAKERS
            if hasattr(model, "get_supported_speakers"):
                try:
                    speakers = [str(s) for s in model.get_supported_speakers()]
                except Exception:  # noqa: BLE001
                    pass
            match = next((s for s in speakers if s.lower() == preset.lower()), None)
            if match is None:
                raise ServiceError(404, "preset_not_found", f"preset {preset!r} does not exist")
            lang = self._qwen_language(body.get("language"), model)
            instruct = body.get("instruct") if isinstance(body.get("instruct"), str) else ""
            parts = []
            sr = 24000
            for chunk in chunks:
                kwargs: Dict[str, Any] = {"text": chunk, "language": lang, "speaker": match}
                if instruct.strip():
                    kwargs["instruct"] = instruct.strip()[:500]
                wavs, sr = model.generate_custom_voice(**kwargs)
                parts.append(self.to_numpy(wavs[0]))
            engine.touch()
        wav, marked = self.watermark(self.concat(parts, int(sr)), int(sr))
        seconds = self.write_wav(out, wav, int(sr))
        return {"outWav": str(out), "seconds": seconds, "sampleRate": int(sr), "engine": "qwen_tts",
                "version": package_version("qwen-tts"), "preset": match, "watermarked": marked,
                "elapsedSeconds": round(time.monotonic() - started, 2)}

    def vc(self, body: Dict[str, Any]) -> Dict[str, Any]:
        """Convert a recording to the voice of a reference clip (Chatterbox VC).

        Long inputs are cut into ~30 s windows at quiet points, converted one
        by one and joined, so a 15-minute Pro input does not need 15 minutes
        of activations at once."""
        self._begin()
        src = resolve_in_scratch(self.scratch_dir, body.get("inputWav"), "inputWav", must_exist=True)
        ref = resolve_in_scratch(self.scratch_dir, body.get("referenceWav"), "referenceWav", must_exist=True)
        out = resolve_in_scratch(self.scratch_dir, body.get("outWav"), "outWav", must_exist=False)
        return self._vc_paths(src, ref, out)

    def _vc_paths(self, src: Path, ref: Path, out: Path) -> Dict[str, Any]:
        import numpy as np
        import soundfile as sf
        audio, in_sr = sf.read(str(src), dtype="float32", always_2d=True)
        mono = audio.mean(axis=1) if audio.shape[1] > 1 else audio[:, 0]
        if mono.size == 0:
            raise ServiceError(400, "empty_input", "inputWav has no audio")

        def energy(a: int, b: int) -> float:
            seg = mono[a:b]
            return float(np.mean(seg * seg)) if seg.size else 0.0

        spans = split_points(int(mono.size), int(in_sr), energy)
        engine = self.engines["chatterbox_vc"]
        started = time.monotonic()
        work = out.parent / f".vc-{uuid.uuid4().hex}"
        work.mkdir(parents=True, exist_ok=True)
        try:
            parts = []
            with engine.lock:
                model = engine.get()
                for i, (a, b) in enumerate(spans):
                    piece = work / f"in-{i:04d}.wav"
                    sf.write(str(piece), mono[a:b], int(in_sr), subtype="PCM_16")
                    wav = model.generate(audio=str(piece), target_voice_path=str(ref))
                    parts.append(self.to_numpy(wav))
                sr = int(model.sr)
                engine.touch()
        finally:
            for f in work.glob("*"):
                try:
                    f.unlink()
                except OSError:
                    pass
            try:
                work.rmdir()
            except OSError:
                pass
        joined = np.concatenate(parts) if parts else np.zeros(0, dtype=np.float32)
        seconds = self.write_wav(out, joined, sr)
        return {"outWav": str(out), "seconds": seconds, "sampleRate": sr, "engine": "chatterbox",
                "version": package_version("chatterbox-tts"), "watermarked": True,
                "elapsedSeconds": round(time.monotonic() - started, 2)}

    def unload(self, only: Optional[str] = None) -> Dict[str, Any]:
        unloaded: List[str] = []
        for engine in self.engines.values():
            if only and only not in (engine.name, engine.group):
                continue
            with engine.lock:
                if engine.unload():
                    unloaded.append(engine.name)
        return {"unloaded": unloaded}

    def idle_sweep(self) -> List[str]:
        """Unload every engine idle for idle_unload_seconds. Non-blocking: an
        engine busy with a request is skipped until the next sweep."""
        if self.idle_unload_seconds <= 0:
            return []
        now = time.monotonic()
        unloaded: List[str] = []
        for engine in self.engines.values():
            if not engine.loaded or now - engine.last_used < self.idle_unload_seconds:
                continue
            if not engine.lock.acquire(blocking=False):
                continue
            try:
                if engine.loaded and time.monotonic() - engine.last_used >= self.idle_unload_seconds:
                    engine.unload()
                    unloaded.append(engine.name)
            finally:
                engine.lock.release()
        return unloaded

    def start_idle_thread(self) -> threading.Thread:
        def loop() -> None:
            while True:
                time.sleep(IDLE_CHECK_INTERVAL_SECONDS)
                try:
                    self.idle_sweep()
                except Exception:  # noqa: BLE001
                    log.exception("idle sweep failed")

        t = threading.Thread(target=loop, name="mm-voices-idle", daemon=True)
        t.start()
        return t


def voices_from_env() -> Voices:
    return Voices(
        scratch_dir=env_str("VOICES_SCRATCH_DIR", DEFAULT_SCRATCH_DIR),
        presets_dir=env_str("VOICES_BUILTIN_PRESETS_DIR", DEFAULT_PRESETS_DIR),
        idle_unload_seconds=env_int("VOICES_IDLE_UNLOAD_SECONDS", DEFAULT_IDLE_UNLOAD_SECONDS),
        device=env_str("VOICES_DEVICE", "cuda"),
        chatterbox_repo=env_str("VOICES_CHATTERBOX_REPO", DEFAULT_CHATTERBOX_REPO),
        qwen_design_repo=env_str("VOICES_QWEN_DESIGN_REPO", DEFAULT_QWEN_DESIGN_REPO),
        qwen_custom_repo=env_str("VOICES_QWEN_CUSTOM_REPO", DEFAULT_QWEN_CUSTOM_REPO),
        qwen_attn=env_str("VOICES_QWEN_ATTN", ""),
    )


# --- HTTP ---------------------------------------------------------------------


def build_app(voices: Voices, port: int) -> Any:
    from fastapi import FastAPI, Request
    from fastapi.responses import JSONResponse

    class SpacedJSON(JSONResponse):
        # json.dumps's default ", " / ": " separators, so `grep '"loaded": true'`
        # in the install doc's health checks matches without json.tool.
        def render(self, content: Any) -> bytes:
            return json.dumps(content).encode("utf-8")

    app = FastAPI(title="mm-voices", version=SERVICE_VERSION, default_response_class=SpacedJSON,
                  docs_url=None, redoc_url=None, openapi_url=None)

    @app.exception_handler(ServiceError)
    async def _service_error(_: Request, exc: ServiceError) -> Any:
        return SpacedJSON(status_code=exc.status, content={"error": exc.code, "message": exc.message})

    @app.exception_handler(Exception)
    async def _unexpected(_: Request, exc: Exception) -> Any:
        log.exception("request failed")
        return SpacedJSON(status_code=500, content={"error": "engine_failed", "message": exc.__class__.__name__})

    async def _body(request: Request) -> Dict[str, Any]:
        try:
            raw = await request.body()
            data = json.loads(raw or b"{}")
        except ValueError:
            raise ServiceError(400, "invalid_json", "body must be JSON")
        if not isinstance(data, dict):
            raise ServiceError(400, "invalid_json", "body must be a JSON object")
        data.pop("_allowBuiltinVoice", None)  # self-test only, never from HTTP
        return data

    @app.get("/health")
    def health() -> Any:
        return voices.health(port)

    @app.post("/presets")
    def presets() -> Any:
        return voices.presets()

    # Sync work runs in Starlette's thread pool; the per-engine locks serialise
    # each engine while letting chatterbox and qwen_tts overlap.
    from starlette.concurrency import run_in_threadpool

    @app.post("/tts")
    async def tts(request: Request) -> Any:
        return await run_in_threadpool(voices.tts, await _body(request))

    @app.post("/design")
    async def design(request: Request) -> Any:
        return await run_in_threadpool(voices.design, await _body(request))

    @app.post("/tts-preset")
    async def tts_preset(request: Request) -> Any:
        return await run_in_threadpool(voices.tts_preset, await _body(request))

    @app.post("/vc")
    async def vc(request: Request) -> Any:
        return await run_in_threadpool(voices.vc, await _body(request))

    @app.post("/unload")
    async def unload(request: Request) -> Any:
        body = await _body(request)
        only = body.get("engine") if isinstance(body.get("engine"), str) else None
        return await run_in_threadpool(voices.unload, only)

    return app


# --- self-test ----------------------------------------------------------------


def self_test(voices: Voices, out_dir: str) -> int:
    """Synthesise one sentence per engine and convert the Qwen sentence to the
    Chatterbox voice. Writes chatterbox_tts.wav, qwen_design.wav and
    chatterbox_vc.wav into out_dir and prints one JSON line."""
    started = time.monotonic()
    out = Path(out_dir).resolve()
    out.mkdir(parents=True, exist_ok=True)
    # The self-test may write anywhere the operator points it; the scratch
    # containment check guards HTTP requests, not this local command.
    voices.scratch_dir = str(out)
    result: Dict[str, Any] = {}
    ok = True

    tts_wav = out / "chatterbox_tts.wav"
    try:
        voices.tts({"text": SELF_TEST_SENTENCE, "language": "en", "outWav": str(tts_wav),
                    "_allowBuiltinVoice": True})
        result["chatterbox"] = "ok"
    except Exception as exc:  # noqa: BLE001
        ok = False
        result["chatterbox"] = f"error: {exc}"

    design_wav = out / "qwen_design.wav"
    try:
        voices.design({"description": SELF_TEST_DESCRIPTION, "language": "en", "seed": 1234,
                       "sampleText": SELF_TEST_SENTENCE, "outWav": str(design_wav)})
        result["qwen_tts"] = "ok"
    except Exception as exc:  # noqa: BLE001
        ok = False
        result["qwen_tts"] = f"error: {exc}"

    vc_wav = out / "chatterbox_vc.wav"
    if tts_wav.is_file() and design_wav.is_file():
        try:
            voices.vc({"inputWav": str(design_wav), "referenceWav": str(tts_wav), "outWav": str(vc_wav)})
            result["vc"] = "ok"
        except Exception as exc:  # noqa: BLE001
            ok = False
            result["vc"] = f"error: {exc}"
    else:
        ok = False
        result["vc"] = "error: skipped (tts or design failed)"

    result["seconds"] = round(time.monotonic() - started, 1)
    print(json.dumps(result), flush=True)
    voices.unload()
    return 0 if ok else 1


# --- main ---------------------------------------------------------------------


def parse_args(argv: Optional[List[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="voices_service.py",
        description="mm-voices: resident Chatterbox + Qwen3-TTS service for Media Manipulator.",
    )
    parser.add_argument("--host", default=env_str("VOICES_HOST", DEFAULT_HOST),
                        help="bind address (default 127.0.0.1; never expose publicly)")
    parser.add_argument("--port", type=int, default=env_int("VOICES_PORT", DEFAULT_PORT),
                        help="bind port (default 8791)")
    parser.add_argument("--self-test", action="store_true",
                        help="synthesise one sentence per engine, convert one, print JSON, exit")
    parser.add_argument("--out", default="",
                        help="output dir for --self-test (default <scratch>/selftest)")
    parser.add_argument("--log-level", default=env_str("VOICES_LOG_LEVEL", "info"))
    return parser.parse_args(argv)


def main(argv: Optional[List[str]] = None) -> int:
    args = parse_args(argv)
    logging.basicConfig(level=getattr(logging, str(args.log_level).upper(), logging.INFO),
                        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    # A legacy TRANSFORMERS_CACHE (set in the host's ~/.bashrc) makes transformers
    # fetch a second, partial copy of each model outside HF_HOME/hub, so it is
    # ignored and every weight comes from HF_HOME.
    if os.environ.pop("TRANSFORMERS_CACHE", None) is not None:
        log.info("ignoring TRANSFORMERS_CACHE; weights come from HF_HOME")
    voices = voices_from_env()
    if args.self_test:
        return self_test(voices, args.out or os.path.join(voices.scratch_dir, "selftest"))

    Path(voices.scratch_dir).mkdir(parents=True, exist_ok=True)
    import uvicorn

    app = build_app(voices, args.port)
    voices.start_idle_thread()
    log.info("mm-voices %s listening on %s:%d (idle unload %ds)", SERVICE_VERSION, args.host,
             args.port, voices.idle_unload_seconds)
    uvicorn.run(app, host=args.host, port=args.port, workers=1, log_level=str(args.log_level).lower())
    return 0


if __name__ == "__main__":
    sys.exit(main())
