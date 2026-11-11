#!/usr/bin/env python3
"""Seed-VC wrapper (Lab, MM139): speech -> speech voice conversion. Calls the model's own CLI.

Verified against Plachtaa/seed-vc@51383efd inference.py: flags --source --target --output
--diffusion-steps --length-adjust --inference-cfg-rate --f0-condition --auto-f0-adjust
--semi-tone-shift --fp16. The tool writes
<output>/vc_<source>_<target>_<length_adjust>_<steps>_<cfg>.wav, so the wrapper gives it a fresh
directory and takes the one WAV in it. It downloads weights into ./checkpoints relative to the
working directory, so the wrapper runs it from the repo dir (weights were prefetched at build time).
"""
import glob
import os
import shutil
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import voice_common as vc  # noqa: E402

MODEL = "seedvc"


def build_cmd(a, src_wav, ref_wav, outdir):
    return [sys.executable, "inference.py", "--source", src_wav, "--target", ref_wav, "--output", outdir,
            "--diffusion-steps", str(a.diffusion_steps), "--length-adjust", str(a.length_adjust),
            "--inference-cfg-rate", str(a.cfg_rate), "--f0-condition", str(a.f0_condition).lower(),
            "--auto-f0-adjust", str(a.auto_f0_adjust).lower(), "--semi-tone-shift", str(a.semitone_shift),
            "--fp16", str(a.fp16).lower()]


def main(argv=None):
    p = vc.base_parser("Seed-VC voice conversion")
    p.add_argument("--source", required=True, help="the speech to convert")
    p.add_argument("--ref", required=True, help="reference of the target voice")
    p.add_argument("--diffusion-steps", type=int, default=30)
    p.add_argument("--length-adjust", type=float, default=1.0)
    p.add_argument("--cfg-rate", type=float, default=0.7)
    p.add_argument("--f0-condition", type=vc.tf, default=False, help="pitch-conditioned 44.1 kHz singing model")
    p.add_argument("--auto-f0-adjust", type=vc.tf, default=False)
    p.add_argument("--semitone-shift", type=int, default=0)
    p.add_argument("--fp16", type=vc.tf, default=True)
    a = p.parse_args(argv)
    t0 = vc.time.time()
    tmp = vc.tmpdir("seedvc-")
    src, ref = os.path.join(tmp, "source.wav"), os.path.join(tmp, "reference.wav")
    vc.normalize_audio(a.source, src, max_seconds=300, min_seconds=1, label="source")
    vc.normalize_audio(a.ref, ref, label="reference")
    outdir = os.path.join(tmp, "out")
    os.makedirs(outdir)
    vc.run_cli(build_cmd(a, src, ref, outdir), cwd=os.path.join(vc.REPOS, MODEL))
    made = sorted(glob.glob(os.path.join(outdir, "vc_*.wav")))
    if len(made) != 1:
        vc.die("expected one vc_*.wav from Seed-VC, found %d" % len(made), code=3)
    os.makedirs(os.path.dirname(os.path.abspath(a.out)), exist_ok=True)
    shutil.move(made[0], a.out)
    vc.finish(a.out, MODEL, t0, {"revisions": vc.read_versions(MODEL), "f0Condition": a.f0_condition})


if __name__ == "__main__":
    main()
