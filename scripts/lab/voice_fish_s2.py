#!/usr/bin/env python3
"""Fish Audio S2 Pro wrapper (Lab, MM139). Calls the model's own CLI.

Verified against fishaudio/fish-speech@0f8a12e8 fish_speech/models/text2semantic/inference.py:
--prompt-audio encodes the reference with the codec, --output decodes the result to a WAV (written
exactly at --output when --num-samples is 1), --prompt-text is the reference transcript, emotion and
style are inline [tags] in --text. There is no language flag (80+ languages are detected).
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import voice_common as vc  # noqa: E402

MODEL = "fish_s2"


def build_cmd(a, ref_wav, tmp):
    base = os.path.join(vc.WEIGHTS, MODEL)
    cmd = [sys.executable, "fish_speech/models/text2semantic/inference.py",
           "--text", a.text, "--prompt-text", a.ref_text, "--prompt-audio", ref_wav,
           "--output", a.out, "--output-dir", tmp,
           "--checkpoint-path", os.path.join(base, "s2-pro"),
           "--temperature", str(a.temperature), "--top-p", str(a.top_p), "--seed", str(a.seed)]
    if a.half:
        cmd.append("--half")
    return cmd, os.path.join(vc.REPOS, MODEL)


def main(argv=None):
    p = vc.base_parser("Fish Audio S2 Pro zero-shot clone + TTS")
    p.add_argument("--ref", required=True)
    p.add_argument("--ref-text", required=True, help="transcript of the reference audio")
    p.add_argument("--text", required=True, help="text to speak; inline [whisper], [excited] ... tags allowed")
    p.add_argument("--style", default="", help="optional inline tag put in front of the text, e.g. whisper, excited, sad")
    p.add_argument("--temperature", type=float, default=0.7)
    p.add_argument("--top-p", type=float, default=0.7)
    p.add_argument("--half", action="store_true", help="float16 instead of bfloat16")
    a = p.parse_args(argv)
    if a.seed is None:
        a.seed = 42
    if not a.text.strip() or not a.ref_text.strip():
        vc.die("text and the reference transcript must not be empty")
    style = a.style.strip().strip("[]")
    if style:
        a.text = "[%s] %s" % (style, a.text)
    t0 = vc.time.time()
    tmp = vc.tmpdir("fish-")
    ref = os.path.join(tmp, "ref.wav")
    vc.normalize_audio(a.ref, ref, label="reference")
    cmd, cwd = build_cmd(a, ref, tmp)
    vc.run_cli(cmd, cwd=cwd)
    vc.finish(a.out, MODEL, t0, {"revisions": vc.read_versions(MODEL), "seed": a.seed})


if __name__ == "__main__":
    main()
