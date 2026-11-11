#!/usr/bin/env python3
"""Tests for the MM139 Lab voice wrappers. No torch, GPU or model weights: the
wrappers' argument handling, ffmpeg normalisation, the argv they hand to each
model's own CLI, sidecar writing and the runtime manifest are exercised, and the
model call itself is replaced by a stand-in that writes a WAV.

    python3 -m unittest scripts/lab/test_voice_wrappers.py
"""
import contextlib
import importlib.util
import io
import json
import math
import os
import shutil
import struct
import subprocess
import sys
import tempfile
import types
import unittest
import wave
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import voice_common as vc  # noqa: E402
import runtime_manifest  # noqa: E402


def load(name):
    spec = importlib.util.spec_from_file_location(name, HERE / (name + ".py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def write_wav(path, seconds=6.0, rate=16000):
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        n = int(seconds * rate)
        w.writeframes(b"".join(struct.pack("<h", int(8000 * math.sin(i / 20.0))) for i in range(n)))


@unittest.skipUnless(shutil.which("ffmpeg"), "needs ffmpeg")
class Wrappers(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mm139-"))
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.ref = self.tmp / "ref.wav"
        write_wav(self.ref)

    def test_normalize_trims_and_rejects(self):
        long = self.tmp / "long.wav"
        write_wav(long, 80)
        out = self.tmp / "n.wav"
        dur = vc.normalize_audio(str(long), str(out))
        self.assertAlmostEqual(dur, 60.0, delta=0.5)
        sr, d = vc.wav_info(str(out))
        self.assertEqual(sr, 44100)
        short = self.tmp / "short.wav"
        write_wav(short, 1)
        with self.assertRaises(SystemExit) as cm, contextlib.redirect_stderr(io.StringIO()):
            vc.normalize_audio(str(short), str(self.tmp / "s.wav"))
        self.assertEqual(cm.exception.code, 2)
        bad = self.tmp / "bad.wav"
        bad.write_bytes(b"not audio")
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
            vc.normalize_audio(str(bad), str(self.tmp / "b.wav"))

    def test_emotion_vector(self):
        self.assertIsNone(vc.parse_emo_vector(""))
        self.assertEqual(vc.parse_emo_vector("0,0,0,0,0,0,0.6,0.4")[6], 0.6)
        for bad in ["1,2", "a,b,c,d,e,f,g,h", "0,0,0,0,0,0,0,2"]:
            with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
                vc.parse_emo_vector(bad)

    def test_fish_argv_matches_the_tools_flags(self):
        fish = load("voice_fish_s2")
        calls = []

        def fake_run(cmd, cwd=None, env=None):
            calls.append((cmd, cwd))
            write_wav(Path(cmd[cmd.index("--output") + 1]), 2)

        fish.vc.run_cli = fake_run
        out = self.tmp / "o.wav"
        fish.main(["--ref", str(self.ref), "--ref-text", "Hi there", "--text", "Hello", "--style", "[whisper]", "--seed", "5", "--out", str(out)])
        cmd, cwd = calls[0]
        self.assertTrue(cmd[1].endswith("fish_speech/models/text2semantic/inference.py"))
        self.assertTrue(cwd.endswith("/repos/lab-voice/fish_s2"))
        flags = {f: cmd[i + 1] for i, f in enumerate(cmd) if f.startswith("--") and i + 1 < len(cmd)}
        # every flag used is one the tool's click options define (verified against inference.py at the pinned commit)
        known = {"--text", "--prompt-text", "--prompt-audio", "--output", "--output-dir", "--checkpoint-path", "--temperature", "--top-p", "--seed", "--half"}
        self.assertTrue(set(flags) <= known, set(flags) - known)
        self.assertEqual(flags["--text"], "[whisper] Hello")
        self.assertEqual(flags["--seed"], "5")
        self.assertTrue(flags["--checkpoint-path"].endswith("/models/lab-voice/fish_s2/s2-pro"))
        side = json.loads(out.with_suffix(".json").read_text())
        self.assertEqual(side["model"], "fish_s2")
        self.assertGreater(side["durationSeconds"], 1)
        self.assertEqual(side["sampleRate"], 16000)

    def test_seedvc_takes_the_one_vc_file(self):
        svc = load("voice_seedvc")
        calls = []

        def fake_run(cmd, cwd=None, env=None):
            calls.append(cmd)
            outdir = Path(cmd[cmd.index("--output") + 1])
            write_wav(outdir / "vc_source_reference_1.0_10_0.7.wav", 2, 22050)

        svc.vc.run_cli = fake_run
        out = self.tmp / "sub" / "o.wav"
        svc.main(["--source", str(self.ref), "--ref", str(self.ref), "--diffusion-steps", "10", "--f0-condition", "true", "--semitone-shift", "-3", "--out", str(out)])
        cmd = calls[0]
        known = {"--source", "--target", "--output", "--diffusion-steps", "--length-adjust", "--inference-cfg-rate", "--f0-condition", "--auto-f0-adjust", "--semi-tone-shift", "--fp16"}
        self.assertTrue({c for c in cmd if c.startswith("--")} <= known)
        self.assertEqual(cmd[cmd.index("--f0-condition") + 1], "true")
        self.assertEqual(cmd[cmd.index("--semi-tone-shift") + 1], "-3")
        self.assertEqual(json.loads(out.with_suffix(".json").read_text())["sampleRate"], 22050)

    def test_seedvc_fails_when_the_tool_writes_nothing(self):
        svc = load("voice_seedvc")
        svc.vc.run_cli = lambda cmd, cwd=None, env=None: None
        with self.assertRaises(SystemExit) as cm, contextlib.redirect_stderr(io.StringIO()):
            svc.main(["--source", str(self.ref), "--ref", str(self.ref), "--out", str(self.tmp / "o.wav")])
        self.assertEqual(cm.exception.code, 3)

    def test_indextts_infer_kwargs(self):
        idx = load("voice_indextts2")
        ns = types.SimpleNamespace(text="Hi", out="/o.wav", emo_alpha=0.6, use_random=False, emo_vector="0,0,0,0,0,0,0.6,0.4", emo_text="", version="2", lang="EN", duration_factor=1.2)
        kw = idx.build_infer_kwargs(ns, "/r.wav", "/e.wav")
        self.assertEqual(kw["emo_audio_prompt"], "/e.wav")
        self.assertEqual(len(kw["emo_vector"]), 8)
        self.assertNotIn("lang", kw)
        self.assertNotIn("duration_factor", kw)
        self.assertNotIn("use_emo_text", kw)
        ns.version, ns.emo_text = "2.5", "calm"
        kw = idx.build_infer_kwargs(ns, "/r.wav", "")
        self.assertEqual((kw["lang"], kw["duration_factor"], kw["use_emo_text"], kw["emo_text"]), ("EN", 1.2, True, "calm"))
        self.assertNotIn("emo_audio_prompt", kw)
        # only names infer() really takes (indextts/infer_v2.py, infer_v2_5.py at the pinned commit)
        real = {"spk_audio_prompt", "text", "output_path", "lang", "emo_audio_prompt", "emo_alpha", "emo_vector", "use_emo_text", "emo_text", "use_random", "verbose", "duration_factor"}
        self.assertTrue(set(kw) <= real, set(kw) - real)

    def test_finish_refuses_a_missing_or_corrupt_wav(self):
        for content in [None, b"junk"]:
            p = self.tmp / "x.wav"
            if p.exists():
                p.unlink()
            if content:
                p.write_bytes(content)
            with self.assertRaises(SystemExit) as cm, contextlib.redirect_stderr(io.StringIO()):
                vc.finish(str(p), "m", vc.time.time())
            self.assertEqual(cm.exception.code, 3)

    def test_runtime_manifest_is_deterministic_and_sensitive(self):
        w, s = self.tmp / "w", self.tmp / "s"
        for m in ("a", "b"):
            (w / m).mkdir(parents=True)
            (w / m / "versions.json").write_text(json.dumps({"model": m, "repoCommit": "abc", "weights": ["x@1"]}))
        s.mkdir()
        (s / "voice_a.py").write_text("print(1)\n")
        h1 = runtime_manifest.compute(str(w), str(s))
        self.assertRegex(h1, r"^[0-9a-f]{64}$")
        self.assertEqual(h1, runtime_manifest.compute(str(w), str(s)))
        (w / "a" / "versions.json").write_text(json.dumps({"model": "a", "repoCommit": "abd", "weights": ["x@1"]}))
        self.assertNotEqual(h1, runtime_manifest.compute(str(w), str(s)))
        (w / "a" / "versions.json").write_text(json.dumps({"weights": ["x@1"], "repoCommit": "abc", "model": "a"}))  # key order is irrelevant
        self.assertEqual(h1, runtime_manifest.compute(str(w), str(s)))
        (s / "voice_a.py").write_text("print(2)\n")
        self.assertNotEqual(h1, runtime_manifest.compute(str(w), str(s)))
        out = subprocess.run([sys.executable, "-I", str(HERE / "runtime_manifest.py"), "--weights", str(w), "--wrappers", str(s)], capture_output=True, text=True)
        self.assertEqual(out.returncode, 0)
        self.assertRegex(out.stdout.strip(), r"^[0-9a-f]{64}$")


if __name__ == "__main__":
    unittest.main()
