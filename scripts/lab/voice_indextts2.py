#!/usr/bin/env python3
"""IndexTTS-2 / IndexTTS-2.5 wrapper (Lab, MM139). In-process: the project has no CLI for v2.

Verified against index-tts/index-tts@d9e41aac:
  v2   indextts/infer_v2.py     IndexTTS2(cfg_path, model_dir, use_fp16, use_cuda_kernel, use_deepspeed,
                                          use_qwen_emo, ...).infer(spk_audio_prompt, text, output_path,
                                          emo_audio_prompt, emo_alpha, emo_vector, use_emo_text, emo_text,
                                          use_random, ...)          (no duration control, no lang)
  v2.5 indextts/infer_v2_5.py   IndexTTS2(cfg_path, model_dir, use_bf16, use_cuda_kernel, use_qwen_emo)
                                .infer(..., lang, duration_factor, ...)
infer() writes output_path itself (save_pcm_wav, 22.05 kHz). use_cuda_kernel is forced off: the fused
BigVGAN kernel would JIT-compile with nvcc at run time. Auxiliary models were pre-placed under
<model_dir>/hf_cache at build time, so nothing is downloaded here.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import voice_common as vc  # noqa: E402

MODEL = "indextts2"
DIRS = {"2": "checkpoints_2", "2.5": "checkpoints_25"}


def build_infer_kwargs(a, ref_wav, emo_wav):
    kw = {"spk_audio_prompt": ref_wav, "text": a.text, "output_path": a.out, "emo_alpha": a.emo_alpha,
          "use_random": a.use_random, "verbose": True}
    vec = vc.parse_emo_vector(a.emo_vector)
    if vec is not None:
        kw["emo_vector"] = vec
    if a.emo_text.strip():
        kw["use_emo_text"] = True
        kw["emo_text"] = a.emo_text
    if emo_wav:
        kw["emo_audio_prompt"] = emo_wav
    if a.version == "2.5":
        kw["lang"] = a.lang
        kw["duration_factor"] = a.duration_factor
    return kw


def main(argv=None):
    p = vc.base_parser("IndexTTS-2 / 2.5 zero-shot clone + TTS with emotion control")
    p.add_argument("--ref", required=True)
    p.add_argument("--emo-ref", default="", help="optional emotion reference audio")
    p.add_argument("--text", required=True)
    p.add_argument("--version", choices=sorted(DIRS), default="2")
    p.add_argument("--emo-alpha", type=float, default=1.0)
    p.add_argument("--emo-vector", default="", help="8 numbers: happy,angry,sad,afraid,disgusted,melancholic,surprised,calm")
    p.add_argument("--emo-text", default="", help="describe the emotion in words (loads the Qwen emotion model)")
    p.add_argument("--use-random", action="store_true")
    p.add_argument("--lang", default="EN", help="2.5 only")
    p.add_argument("--duration-factor", type=float, default=1.0, help="2.5 only; above 1 slows speech")
    p.add_argument("--fp16", type=vc.tf, default=True)
    a = p.parse_args(argv)
    if not a.text.strip():
        vc.die("text must not be empty")
    repo, mdir = os.path.join(vc.REPOS, MODEL), os.path.join(vc.WEIGHTS, MODEL, DIRS[a.version])
    t0 = vc.time.time()
    tmp = vc.tmpdir("indextts-")
    ref = os.path.join(tmp, "ref.wav")
    vc.normalize_audio(a.ref, ref, label="reference")
    emo = ""
    if a.emo_ref:
        emo = os.path.join(tmp, "emo.wav")
        vc.normalize_audio(a.emo_ref, emo, label="emotion reference")
    os.makedirs(os.path.dirname(os.path.abspath(a.out)), exist_ok=True)
    kw = build_infer_kwargs(a, ref, emo)
    os.chdir(repo)
    sys.path.insert(0, repo)
    os.environ.setdefault("USE_MODELSCOPE", "false")
    use_emo_text = bool(kw.get("use_emo_text"))
    if a.version == "2":
        from indextts.infer_v2 import IndexTTS2

        tts = IndexTTS2(cfg_path=os.path.join(mdir, "config.yaml"), model_dir=mdir, use_fp16=a.fp16,
                        use_cuda_kernel=False, use_deepspeed=False, use_qwen_emo=use_emo_text)
    else:
        from indextts.infer_v2_5 import IndexTTS2

        tts = IndexTTS2(cfg_path=os.path.join(mdir, "config.yaml"), model_dir=mdir, use_bf16=True,
                        use_cuda_kernel=False, use_deepspeed=False, use_qwen_emo=use_emo_text)
    tts.infer(**kw)
    vc.finish(a.out, MODEL, t0, {"revisions": vc.read_versions(MODEL), "version": a.version})


if __name__ == "__main__":
    main()
