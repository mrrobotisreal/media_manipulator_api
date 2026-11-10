#!/usr/bin/env python3
"""Tests for patch_deepfilter_io.py. No torch, soundfile or GPU needed:
tiny stand-ins for those modules are put in sys.modules, and the patch runs
against a copy of the host's hand-rewritten io.py (testdata/df_io_host.py).

    python3 -m unittest scripts/server/test_patch_deepfilter_io.py
"""
import importlib.util
import shutil
import subprocess
import sys
import tempfile
import types
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
PATCH = HERE / "patch_deepfilter_io.py"
HOST_IO = HERE / "testdata" / "df_io_host.py"


class _Tensor:
    """The few tensor methods io.py's save path uses."""

    def __init__(self, floating=True):
        self.ndim = 2
        self.floating = floating

    def detach(self):
        return self

    def cpu(self):
        return self

    def unsqueeze(self, _):
        return self

    def transpose(self, *_):
        return self

    def contiguous(self):
        return self

    def numpy(self):
        return [[0.0]]

    def to(self, *_a, **_k):
        return _Tensor(True)

    def __truediv__(self, _):
        return _Tensor(True)


def _install_stubs(written):
    torch = types.ModuleType("torch")
    torch.Tensor = _Tensor
    torch.float32 = "float32"
    torch.int16 = "int16"
    torch.dtype = type("dtype", (), {})
    torch.device = type("device", (), {})
    torch.as_tensor = lambda a: a
    torch.is_floating_point = lambda a: a.floating
    torch.from_numpy = lambda a: _Tensor()
    taf = types.ModuleType("torchaudio.functional")
    taf.resample = lambda audio, **_k: audio
    ta = types.ModuleType("torchaudio")
    ta.functional = taf
    np = types.ModuleType("numpy")
    np.ndarray = list
    np.ascontiguousarray = lambda a: a
    sf = types.ModuleType("soundfile")

    def write(path, data, sr, subtype=None):
        Path(path).write_bytes(b"RIFF" + b"\0" * 2000)
        written.append((str(path), int(sr), subtype))

    sf.write = write
    sf.info = lambda p: None
    sf.read = lambda *a, **k: ([[0.0]], 48000)
    loguru = types.ModuleType("loguru")
    loguru.logger = types.SimpleNamespace(info=lambda *a, **k: None, debug=lambda *a, **k: None)
    for name, mod in (("torch", torch), ("torchaudio", ta), ("torchaudio.functional", taf),
                      ("numpy", np), ("soundfile", sf), ("loguru", loguru)):
        sys.modules[name] = mod


def _load(path):
    spec = importlib.util.spec_from_file_location("df_io_under_test_%d" % id(path), str(path))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _run_patch(io_path, *flags):
    return subprocess.run([sys.executable, "-I", str(PATCH), "--io", str(io_path), *flags],
                          capture_output=True, text=True)


class PatchDeepFilterIO(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.dir, True)
        self.io = self.dir / "io.py"
        shutil.copy(HOST_IO, self.io)
        self.written = []
        saved = {k: sys.modules.get(k) for k in
                 ("torch", "torchaudio", "torchaudio.functional", "numpy", "soundfile", "loguru")}
        self.addCleanup(lambda: [sys.modules.__setitem__(k, v) if v else sys.modules.pop(k, None)
                                 for k, v in saved.items()])
        _install_stubs(self.written)

    def test_host_rewrite_ignores_output_dir_and_overwrites_the_input(self):
        """The bug: this is what the host's io.py does today."""
        mod = _load(self.io)
        src = self.dir / "work" / "input_48k_mono.wav"
        src.parent.mkdir()
        src.write_bytes(b"ORIGINAL")
        out_dir = self.dir / "out"
        out_dir.mkdir()
        mod.save_audio(str(src), _Tensor(), sr=48000, output_dir=str(out_dir),
                       suffix="DeepFilterNet3", log=False)
        self.assertEqual(list(out_dir.iterdir()), [], "nothing is written to --output-dir")
        self.assertNotEqual(src.read_bytes(), b"ORIGINAL", "the input file is overwritten")

    def test_patched_writes_suffixed_file_in_output_dir_and_keeps_the_input(self):
        r = _run_patch(self.io, "--apply", "--no-self-test")
        self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
        mod = _load(self.io)
        src = self.dir / "work" / "input_48k_mono.wav"
        src.parent.mkdir()
        src.write_bytes(b"ORIGINAL")
        out_dir = self.dir / "out"
        mod.save_audio(str(src), _Tensor(), sr=48000, output_dir=str(out_dir),
                       suffix="DeepFilterNet3", log=False)
        want = out_dir / "input_48k_mono_DeepFilterNet3.wav"
        self.assertTrue(want.is_file(), "the name the API's findEnhancedWav prefers")
        self.assertEqual(src.read_bytes(), b"ORIGINAL", "the input must not be touched")
        self.assertEqual(self.written[-1], (str(want), 48000, "PCM_16"))

    def test_patched_plain_call_and_no_suffix_still_work(self):
        self.assertEqual(_run_patch(self.io, "--apply", "--no-self-test").returncode, 0)
        mod = _load(self.io)
        plain = self.dir / "a" / "b.wav"
        mod.save_audio(plain, _Tensor(), 48000)  # creates the parent dir, as before
        self.assertTrue(plain.is_file())
        out_dir = self.dir / "o"
        mod.save_audio("x/y.wav", _Tensor(), 16000, output_dir=out_dir, suffix=None)  # --no-suffix
        self.assertTrue((out_dir / "y.wav").is_file())
        mod.save_audio(self.dir / "f.wav", _Tensor(floating=False), 48000, dtype=sys.modules["torch"].float32)
        self.assertEqual(self.written[-1][2], "FLOAT")

    def test_dry_run_changes_nothing_and_apply_is_idempotent_with_a_backup(self):
        before = self.io.read_text()
        r = _run_patch(self.io)
        self.assertEqual(r.returncode, 0)
        self.assertIn("DRY RUN", r.stdout)
        self.assertEqual(self.io.read_text(), before)
        self.assertEqual(_run_patch(self.io, "--apply", "--no-self-test").returncode, 0)
        once = self.io.read_text()
        backups = list(self.dir.glob("io.py.bak.mm-*"))
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].read_text(), before)
        r = _run_patch(self.io, "--apply", "--no-self-test")
        self.assertIn("Already patched", r.stdout)
        self.assertEqual(self.io.read_text(), once)
        self.assertEqual(len(list(self.dir.glob("io.py.bak.mm-*"))), 1)
        # Everything except save_audio is byte-identical to the host file.
        self.assertEqual(once.split("# mm-patch:")[0], before.split("def save_audio(")[0])

    def test_refuses_files_it_does_not_recognise(self):
        upstream = self.dir / "up.py"
        upstream.write_text("import torchaudio as ta\n\ndef save_audio(file, audio, sr, output_dir=None):\n    ta.save(file, audio, sr)\n")
        r = _run_patch(upstream, "--apply")
        self.assertEqual(r.returncode, 0)
        self.assertIn("Nothing to do", r.stdout)
        other = self.dir / "other.py"
        other.write_text("def save_audio(file):\n    pass\n")
        r = _run_patch(other, "--apply")
        self.assertEqual(r.returncode, 3)
        self.assertEqual(other.read_text(), "def save_audio(file):\n    pass\n")
        self.assertEqual(_run_patch(self.dir / "missing.py", "--apply").returncode, 2)


if __name__ == "__main__":
    unittest.main()
