#!/usr/bin/env python3
"""Restore --output-dir / suffix handling in the host's patched DeepFilterNet io.py.

Background (found 2026-11-10 while building the burst audio image):
DeepFilterNet 0.5.6's `df/io.py` uses torchaudio.info/load/save, which torchaudio
2.11 no longer provides. On 2026-05-17 `io.py` in the audio-clean venv was
rewritten by hand to use soundfile. That rewrite's `save_audio` takes
`output_dir`, `suffix`, `log` and `dtype` only through `**kwargs` and ignores
them, so `deepFilter --output-dir DIR in.wav` writes its result straight to
`in.wav`: it overwrites the input and writes nothing new into DIR.

The API's `runDeepFilter` then looked for a new WAV in DIR and failed with
"deepFilter did not produce an enhanced WAV" (clean_voice,
remove_background_noise, Darkroom voice_clean).

This script replaces `save_audio` in that file with one that honours
`output_dir`, `suffix`, `log` and `dtype` the way upstream does, and still
writes with soundfile. It changes nothing else.

Usage (as the user that owns the venv; no sudo needed on the host):
    python3 patch_deepfilter_io.py            # dry run: say what it would do
    python3 patch_deepfilter_io.py --apply    # back up, patch, self-test
    python3 patch_deepfilter_io.py --io /path/to/df/io.py --apply

Safe to run twice: an already-patched file is left alone.
"""
from __future__ import annotations

import argparse
import shutil
import sys
import time
from pathlib import Path

DEFAULT_IO = "/opt/media-manipulator-ai/venvs/audio-clean/lib/python3.12/site-packages/df/io.py"
MARKER = "# mm-patch: save_audio honours output_dir/suffix (2026-11-10)"

NEW_SAVE_AUDIO = MARKER + '''
def save_audio(
    file: Union[str, Path],
    audio: torch.Tensor,
    sr: int,
    output_dir: Optional[Union[str, Path]] = None,
    suffix: Optional[str] = None,
    log: bool = False,
    dtype: Optional[torch.dtype] = None,
    subtype: Optional[str] = None,
    **kwargs,
) -> None:
    """Write `audio` ([channels, frames]) with soundfile.

    Same output-path rules as upstream DeepFilterNet: `suffix` is inserted
    before the extension ("in.wav" -> "in_<suffix>.wav") and `output_dir`, when
    given, replaces the directory. So `deepFilter --output-dir D in.wav` writes
    D/in_DeepFilterNet3.wav and never touches in.wav.
    """
    out = Path(file)
    if suffix:
        out = out.with_name(out.stem + "_" + str(suffix) + out.suffix)
    if output_dir is not None:
        out = Path(output_dir) / out.name
    out.parent.mkdir(parents=True, exist_ok=True)
    if log:
        logger.info(f"Saving audio file '{out}'")

    audio = torch.as_tensor(audio)
    # Integer PCM input (upstream callers may pass int16) -> float in [-1, 1).
    if not torch.is_floating_point(audio):
        audio = audio.to(torch.float32) / float(1 << 15)
    data = _to_soundfile_shape(audio)

    if subtype is None and out.suffix.lower() == ".wav":
        # 16-bit PCM unless the caller asked for float output.
        subtype = "FLOAT" if dtype == torch.float32 else "PCM_16"

    sf.write(str(out), data, int(sr), subtype=subtype)
    logger.debug(f"Saved audio to {out}")
'''


def find_save_audio(src: str) -> tuple[int, int]:
    """Return (start, end) character offsets of the top-level save_audio def."""
    lines = src.splitlines(keepends=True)
    start = None
    for i, line in enumerate(lines):
        if line.startswith("def save_audio("):
            start = i
            break
    if start is None:
        raise SystemExit("ERROR: no top-level 'def save_audio(' in this file; not touching it")
    # Include a marker comment directly above, if one is there.
    if start > 0 and lines[start - 1].startswith("# mm-patch:"):
        start -= 1
    end = len(lines)
    for j in range(start + 2, len(lines)):
        line = lines[j]
        if line.strip() and not line[0].isspace() and not line.startswith(")"):
            end = j
            break
    a = sum(len(x) for x in lines[:start])
    b = sum(len(x) for x in lines[:end])
    return a, b


def patched_source(src: str) -> str:
    a, b = find_save_audio(src)
    tail = src[b:]
    body = NEW_SAVE_AUDIO.rstrip("\n") + "\n"
    if tail.strip():
        body += "\n\n"
    return src[:a] + body + tail


def needs(src: str) -> list[str]:
    """Names the new save_audio uses that the file must already define."""
    missing = []
    for name, probe in (
        ("Path", "from pathlib import Path"),
        ("sf", "import soundfile as sf"),
        ("torch", "import torch"),
        ("logger", "logger"),
        ("_to_soundfile_shape", "def _to_soundfile_shape("),
        ("Optional/Union", "Optional"),
    ):
        if probe not in src:
            missing.append(name)
    return missing


def self_test(io_path: Path) -> None:
    """Run the patched save_audio the way `deepFilter --output-dir` does."""
    import importlib.util
    import tempfile

    spec = importlib.util.spec_from_file_location("df_io_patched", str(io_path))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)  # needs the venv's python (torch, soundfile)
    import torch as _torch

    with tempfile.TemporaryDirectory() as d:
        src = Path(d) / "in" / "x.wav"
        src.parent.mkdir()
        src.write_bytes(b"ORIGINAL")
        out_dir = Path(d) / "out"
        tone = _torch.zeros(1, 4800)
        mod.save_audio(str(src), tone, sr=48000, output_dir=str(out_dir), suffix="DeepFilterNet3", log=False)
        want = out_dir / "x_DeepFilterNet3.wav"
        if not want.is_file() or want.stat().st_size < 1000:
            raise SystemExit(f"SELF-TEST FAILED: {want} was not written")
        if src.read_bytes() != b"ORIGINAL":
            raise SystemExit("SELF-TEST FAILED: the input file was overwritten")
        plain = Path(d) / "plain.wav"
        mod.save_audio(plain, tone, 48000)
        if not plain.is_file():
            raise SystemExit("SELF-TEST FAILED: save_audio(path, audio, sr) no longer writes path")
    print("self-test: ok (writes <output_dir>/<name>_<suffix>.wav, leaves the input alone)")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--io", default=DEFAULT_IO, help="path to the venv's df/io.py")
    ap.add_argument("--apply", action="store_true", help="write the change (default: dry run)")
    ap.add_argument("--no-self-test", action="store_true", help="skip importing the patched file")
    args = ap.parse_args()

    io_path = Path(args.io)
    if not io_path.is_file():
        print(f"ERROR: {io_path} not found", file=sys.stderr)
        return 2
    src = io_path.read_text(encoding="utf-8")

    if "torchaudio.save" in src or "ta.save(" in src:
        print("This io.py still uses torchaudio.save (the upstream file): it already honours --output-dir. Nothing to do.")
        return 0
    if MARKER in src:
        print("Already patched. Nothing to do.")
        if args.apply and not args.no_self_test:
            self_test(io_path)
        return 0
    missing = needs(src)
    if missing:
        print(f"ERROR: this io.py is not the soundfile rewrite this patch expects (missing: {', '.join(missing)}). Not touching it.", file=sys.stderr)
        return 3

    new = patched_source(src)
    compile(new, str(io_path), "exec")  # syntax check before writing anything
    if not args.apply:
        print(f"DRY RUN: would replace save_audio in {io_path} ({len(src)} -> {len(new)} bytes).")
        print("Re-run with --apply to back up the file, patch it and self-test.")
        return 0

    backup = io_path.with_name(io_path.name + ".bak.mm-" + time.strftime("%Y%m%d-%H%M%S"))
    shutil.copy2(io_path, backup)
    io_path.write_text(new, encoding="utf-8")
    print(f"patched {io_path}")
    print(f"backup:  {backup}")
    if not args.no_self_test:
        try:
            self_test(io_path)
        except BaseException:
            shutil.copy2(backup, io_path)
            print("self-test failed: the original file was put back", file=sys.stderr)
            raise
    return 0


if __name__ == "__main__":
    sys.exit(main())
