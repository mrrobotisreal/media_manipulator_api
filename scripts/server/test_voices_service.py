#!/usr/bin/env python3
"""Stdlib unit tests for voices_service.py's pure helpers (no torch, no engines).

    python3 -m unittest scripts/server/test_voices_service.py
"""
import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location("voices_service", _HERE / "voices_service.py")
vs = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(vs)  # type: ignore[union-attr]


class ResolveInScratchTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.root = os.path.realpath(self._tmp.name)
        self.scratch = os.path.join(self.root, "voices")
        os.makedirs(os.path.join(self.scratch, "job"))
        Path(self.scratch, "job", "in.wav").write_bytes(b"RIFF")
        Path(self.root, "secret.wav").write_bytes(b"RIFF")

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def code(self, raw, must_exist=False):
        try:
            vs.resolve_in_scratch(self.scratch, raw, "f", must_exist)
        except vs.ServiceError as exc:
            return exc.status, exc.code
        return 200, "ok"

    def test_inside_is_accepted(self):
        self.assertEqual(self.code(os.path.join(self.scratch, "job", "in.wav"), True), (200, "ok"))
        self.assertEqual(self.code(os.path.join(self.scratch, "job", "new.wav")), (200, "ok"))

    def test_traversal_is_refused(self):
        self.assertEqual(self.code(os.path.join(self.scratch, "..", "secret.wav")), (403, "path_outside_scratch"))
        self.assertEqual(self.code("/etc/passwd"), (403, "path_outside_scratch"))
        # A sibling dir sharing the prefix is not inside.
        self.assertEqual(self.code(self.scratch + "-evil/x.wav"), (403, "path_outside_scratch"))

    def test_symlink_out_is_refused(self):
        link = os.path.join(self.scratch, "job", "link.wav")
        os.symlink(os.path.join(self.root, "secret.wav"), link)
        self.assertEqual(self.code(link, True), (403, "path_outside_scratch"))

    def test_bad_values(self):
        self.assertEqual(self.code(""), (400, "invalid_path"))
        self.assertEqual(self.code(None), (400, "invalid_path"))
        self.assertEqual(self.code("relative.wav"), (400, "invalid_path"))
        self.assertEqual(self.code(self.scratch), (400, "invalid_path"))
        self.assertEqual(self.code(os.path.join(self.scratch, "nope.wav"), True), (400, "file_not_found"))


class ChunkTextTest(unittest.TestCase):
    def test_short_text_is_one_chunk(self):
        self.assertEqual(vs.chunk_text("  Hello   there.  "), ["Hello there."])

    def test_empty(self):
        self.assertEqual(vs.chunk_text("   "), [])

    def test_sentences_pack_up_to_limit_without_loss(self):
        text = " ".join(f"Sentence number {i} is here." for i in range(40))
        chunks = vs.chunk_text(text, max_chars=100)
        self.assertTrue(all(len(c) <= 100 for c in chunks))
        self.assertEqual(" ".join(chunks), text)

    def test_long_sentence_splits(self):
        text = "word " * 200
        chunks = vs.chunk_text(text, max_chars=50)
        self.assertTrue(all(len(c) <= 50 for c in chunks))
        self.assertEqual(" ".join(chunks).split(), text.split())

    def test_unbroken_run_is_hard_cut(self):
        chunks = vs.chunk_text("x" * 130, max_chars=50)
        self.assertEqual([len(c) for c in chunks], [50, 50, 30])


class SplitPointsTest(unittest.TestCase):
    def test_covers_everything_without_overlap(self):
        spans = vs.split_points(100 * 16000, 16000)
        self.assertEqual(spans[0][0], 0)
        self.assertEqual(spans[-1][1], 100 * 16000)
        for (a, b), (c, _) in zip(spans, spans[1:]):
            self.assertEqual(b, c)
            self.assertLess(a, b)

    def test_cuts_at_quietest_frame(self):
        sr = 1000
        quiet_at = 27000  # inside the last 5 s of the first 30 s window

        def energy(a, b):
            return 0.0 if a <= quiet_at < b else 1.0

        spans = vs.split_points(60 * sr, sr, energy)
        self.assertTrue(25000 <= spans[0][1] <= 30000)
        self.assertLess(abs(spans[0][1] - quiet_at), 50)

    def test_short_input_is_one_span(self):
        self.assertEqual(vs.split_points(5000, 1000), [(0, 5000)])


class ClampTest(unittest.TestCase):
    def test_clamp(self):
        self.assertEqual(vs.clamp(None, 0, 1, 0.5), 0.5)
        self.assertEqual(vs.clamp("x", 0, 1, 0.5), 0.5)
        self.assertEqual(vs.clamp(float("nan"), 0, 1, 0.5), 0.5)
        self.assertEqual(vs.clamp(5, 0, 1, 0.5), 1)
        self.assertEqual(vs.clamp(-5, 0, 1, 0.5), 0)
        self.assertEqual(vs.clamp(0.3, 0, 1, 0.5), 0.3)


class VoicesWithoutEnginesTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        root = Path(self._tmp.name)
        self.presets = root / "presets"
        self.presets.mkdir()
        (self.presets / "mr_wintrow.wav").write_bytes(b"RIFF")
        (self.presets / "mr_wintrow.json").write_text(json.dumps({"name": "Mr. Wintrow", "language": "en"}))
        (self.presets / "bad name!.wav").write_bytes(b"RIFF")
        self.voices = vs.Voices(str(root / "scratch"), str(self.presets), 600, "cpu",
                                "a/b", "c/d", "e/f")

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_health_shape_with_nothing_loaded(self):
        h = self.voices.health(8791)
        self.assertEqual(h["port"], 8791)
        self.assertFalse(h["engines"]["chatterbox"]["loaded"])
        self.assertFalse(h["engines"]["qwen_tts"]["loaded"])
        self.assertIn("version", h["engines"]["chatterbox"])
        self.assertIn("idleSeconds", h)
        self.assertIn("en", h["languages"])
        json.dumps(h)  # serialisable

    def test_builtin_presets_dir_is_a_file_drop(self):
        presets = self.voices.presets()["presets"]
        ids = [p["id"] for p in presets]
        self.assertEqual(ids[0], "mr_wintrow")
        self.assertEqual(presets[0]["name"], "Mr. Wintrow")
        self.assertEqual(presets[0]["engine"], "chatterbox")
        self.assertNotIn("bad name!", ids)
        self.assertIn("Vivian", ids)

    def test_missing_presets_dir_is_empty(self):
        self.voices.presets_dir = str(self.presets / "nope")
        self.assertEqual(self.voices.builtin_presets(), [])

    def test_requests_refuse_paths_before_loading_an_engine(self):
        with self.assertRaises(vs.ServiceError) as ctx:
            self.voices.tts({"text": "hi", "language": "en", "referenceWav": "/etc/passwd",
                             "outWav": "/tmp/x.wav"})
        self.assertEqual(ctx.exception.code, "path_outside_scratch")
        self.assertFalse(any(e.loaded for e in self.voices.engines.values()))

    def test_validation_errors(self):
        with self.assertRaises(vs.ServiceError) as ctx:
            self.voices.tts({"text": "", "language": "en"})
        self.assertEqual(ctx.exception.code, "text_required")
        with self.assertRaises(vs.ServiceError) as ctx:
            self.voices.tts({"text": "hi", "language": "xx"})
        self.assertEqual(ctx.exception.code, "language_not_supported")
        with self.assertRaises(vs.ServiceError) as ctx:
            self.voices.design({"description": "", "sampleText": "hi", "seed": 1})
        self.assertEqual(ctx.exception.code, "description_required")
        with self.assertRaises(vs.ServiceError) as ctx:
            self.voices.design({"description": "a", "sampleText": "hi", "seed": "x"})
        self.assertEqual(ctx.exception.code, "seed_required")

    def test_idle_sweep_unloads_only_idle_engines(self):
        e = self.voices.engines["chatterbox_tts"]
        e.model = object()
        e.last_used = 0.0  # monotonic origin: long idle
        busy = self.voices.engines["qwen_design"]
        busy.model = object()
        busy.touch()
        self.assertEqual(self.voices.idle_sweep(), ["chatterbox_tts"])
        self.assertFalse(e.loaded)
        self.assertTrue(busy.loaded)


if __name__ == "__main__":
    unittest.main()
