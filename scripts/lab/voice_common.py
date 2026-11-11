"""Shared helpers for the MM139 Lab voice wrappers (voice_<model>.py).

Pure standard library on purpose: the wrappers' argument handling, audio
normalisation (ffmpeg), sidecar JSON and result checks are unit-tested without
torch (test_voice_wrappers.py). The model code itself is imported lazily by each
wrapper, inside the model's own venv.

Contract of every wrapper (ticket decision 3):
    voice_<model>.py --ref REF.wav --text "..." --out OUT.wav [model params]
writes OUT.wav (mono PCM WAV) and OUT.json (model, sample rate, duration,
seconds, revisions). Exit 0 only when OUT.wav exists and decodes.
"""
import argparse
import json
import os
import subprocess
import sys
import tempfile
import time
import wave

# Layout inside the lab-voice image. Only scripts/, venvs/, models/ and repos/ are
# subtrees of /opt/media-manipulator-ai the burst executor lets a Spec name
# (internal/burst/exec allowedRuntime), so the models live under those:
#   scripts/lab/voice_<m>.py   venvs/lab-voice-<m>/   models/lab-voice/<m>/ (weights, versions.json)
#   repos/lab-voice/<m>/ (the model's code at a pinned commit)
WEIGHTS = os.environ.get("MM_LAB_VOICE_WEIGHTS", "/opt/media-manipulator-ai/models/lab-voice")
REPOS = os.environ.get("MM_LAB_VOICE_REPOS", "/opt/media-manipulator-ai/repos/lab-voice")
MAX_REF_SECONDS = 60.0
MIN_REF_SECONDS = 3.0


def die(msg, code=2):
    print("ERROR: " + msg, file=sys.stderr)
    sys.exit(code)


def base_parser(description):
    p = argparse.ArgumentParser(description=description)
    p.add_argument("--out", required=True, help="output WAV path; OUT.json is written beside it")
    p.add_argument("--seed", type=int, default=None)
    return p


def sidecar_path(out):
    return os.path.splitext(out)[0] + ".json"


def wav_info(path):
    """(sample_rate, duration_seconds) of a WAV, or (None, None) if it does not decode."""
    try:
        with wave.open(path, "rb") as w:
            return w.getframerate(), w.getnframes() / float(w.getframerate())
    except Exception:  # float WAVs are not readable by the stdlib; soundfile is in every venv
        pass
    try:
        import soundfile as sf

        i = sf.info(path)
        return i.samplerate, i.frames / float(i.samplerate)
    except Exception:
        return None, None


def normalize_audio(src, dst, max_seconds=MAX_REF_SECONDS, min_seconds=MIN_REF_SECONDS, label="audio"):
    """Any upload -> mono 44.1 kHz PCM16 WAV, trimmed to max_seconds. Refuses empty/too-short input."""
    cmd = ["ffmpeg", "-v", "error", "-y", "-i", src, "-t", str(max_seconds), "-vn", "-ac", "1", "-ar", "44100",
           "-c:a", "pcm_s16le", dst]
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0 or not os.path.isfile(dst):
        die("could not decode the %s file (ffmpeg: %s)" % (label, r.stderr.strip()[-200:]))
    _, dur = wav_info(dst)
    if dur is None or dur < min_seconds:
        die("the %s is too short (%.1f s; need at least %.0f s, 5 to 60 s is best)" % (label, dur or 0.0, min_seconds))
    return dur


def run_cli(cmd, cwd=None, env=None):
    """Run a model's own CLI, streaming its output to ours (the case log); non-zero exit aborts."""
    full_env = dict(os.environ)
    full_env.update(env or {})
    print("+ " + " ".join(cmd), flush=True)
    r = subprocess.run(cmd, cwd=cwd, env=full_env)
    if r.returncode != 0:
        die("model command failed with exit %d" % r.returncode, code=r.returncode or 1)


def finish(out, model, t0, extra=None):
    """Checks OUT decodes and writes the sidecar. Call last."""
    sr, dur = wav_info(out)
    if not os.path.isfile(out) or sr is None or not dur or dur <= 0:
        die("the model produced no valid WAV at %s" % out, code=3)
    side = {"model": model, "sampleRate": sr, "durationSeconds": round(dur, 3),
            "wallSeconds": round(time.time() - t0, 2)}
    side.update(extra or {})
    with open(sidecar_path(out), "w") as f:
        json.dump(side, f, indent=2, sort_keys=True)
    print("done: %s %.2fs @ %d Hz in %.1fs" % (out, dur, sr, time.time() - t0), flush=True)


def read_versions(model):
    """The recorded revisions of the installed model (written at build time), for the sidecar."""
    p = os.path.join(WEIGHTS, model, "versions.json")
    try:
        with open(p) as f:
            return json.load(f)
    except Exception:
        return {"unrecorded": True}


def tmpdir(prefix):
    return tempfile.mkdtemp(prefix=prefix)


def parse_emo_vector(s):
    """'0,0,0,0,0,0,0.6,0.4' -> 8 floats in [0,1] ([happy, angry, sad, afraid, disgusted, melancholic, surprised, calm]); '' -> None."""
    s = (s or "").strip().strip("[]")
    if not s:
        return None
    try:
        v = [float(x) for x in s.split(",")]
    except ValueError:
        die("emotion vector must be 8 comma-separated numbers")
    if len(v) != 8 or any(x < 0 or x > 1 for x in v):
        die("emotion vector must be 8 numbers between 0 and 1 (happy, angry, sad, afraid, disgusted, melancholic, surprised, calm)")
    return v


def tf(v):
    """argparse type for true/false flags."""
    return str(v).lower() in ("1", "true", "yes", "y", "t")
