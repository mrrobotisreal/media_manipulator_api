#!/usr/bin/env python3
"""Fun-CosyVoice3 0.5B wrapper (Lab comparator, MM139). In-process.

Verified against QwenAudio/CosyVoice@074ca6dc example.py (cosyvoice3_example) and cosyvoice/cli/cosyvoice.py:
AutoModel(model_dir=...) -> CosyVoice3; inference_zero_shot(tts_text, prompt_text, prompt_wav, stream=False,
speed=1.0) with prompt_text = 'You are a helpful assistant.<|endofprompt|>' + transcript;
inference_cross_lingual(tts_text, prompt_wav) with the same prefix on tts_text. Each call yields dicts with
'tts_speech' (a [1, N] tensor) at cosyvoice.sample_rate; chunks are concatenated into one WAV.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import voice_common as vc  # noqa: E402

MODEL = "cosyvoice3"
PREFIX = "You are a helpful assistant.<|endofprompt|>"


def main(argv=None):
    p = vc.base_parser("Fun-CosyVoice3 zero-shot / cross-lingual clone + TTS")
    p.add_argument("--ref", required=True)
    p.add_argument("--ref-text", default="")
    p.add_argument("--text", required=True)
    p.add_argument("--mode", choices=["zero_shot", "cross_lingual"], default="zero_shot")
    p.add_argument("--speed", type=float, default=1.0)
    a = p.parse_args(argv)
    if not a.text.strip():
        vc.die("text must not be empty")
    if a.mode == "zero_shot" and not a.ref_text.strip():
        vc.die("zero-shot needs the transcript of the reference audio (or use cross_lingual mode)")
    repo = os.path.join(vc.REPOS, MODEL)
    t0 = vc.time.time()
    tmp = vc.tmpdir("cosy-")
    ref = os.path.join(tmp, "ref.wav")
    vc.normalize_audio(a.ref, ref, label="reference")
    os.makedirs(os.path.dirname(os.path.abspath(a.out)), exist_ok=True)
    os.chdir(repo)
    sys.path[:0] = [repo, os.path.join(repo, "third_party", "Matcha-TTS")]
    import torch
    import torchaudio
    from cosyvoice.cli.cosyvoice import AutoModel

    model = AutoModel(model_dir=os.path.join(vc.WEIGHTS, MODEL, "Fun-CosyVoice3-0.5B"))
    if a.mode == "zero_shot":
        gen = model.inference_zero_shot(a.text, PREFIX + a.ref_text, ref, stream=False, speed=a.speed)
    else:
        gen = model.inference_cross_lingual(PREFIX + a.text, ref, stream=False, speed=a.speed)
    chunks = [j["tts_speech"] for j in gen]
    if not chunks:
        vc.die("CosyVoice3 produced no audio", code=3)
    torchaudio.save(a.out, torch.cat(chunks, dim=1), model.sample_rate)
    vc.finish(a.out, MODEL, t0, {"revisions": vc.read_versions(MODEL), "mode": a.mode})


if __name__ == "__main__":
    main()
