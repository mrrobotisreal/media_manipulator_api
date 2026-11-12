"""Tests for restore_video.py's BasicVSR++ loader (stdlib only: no torch, no mmagic).

    python3 -m unittest scripts/server/test_restore_video.py

From 2026-06-10 to 2026-11-12 every BasicVSR++ job failed with "mmagic
environment not importable": `from mmagic.apis import init_model` imports every
editor mmagic ships, and two of them do not import in the restore-vsr-mm venv.
The loader now imports only the network's own module. These tests build a fake
mmagic package whose package __init__ files raise exactly like the real ones
and a fake torch, and check that the loader gets the class anyway, strips the
"generator." prefix, loads strictly, and reports a bad checkpoint.
"""
import importlib.util
import os
import sys
import tempfile
import textwrap
import types
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def load_script():
    spec = importlib.util.spec_from_file_location("restore_video_under_test", os.path.join(HERE, "restore_video.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def write(path, body):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(textwrap.dedent(body))


class FakeMmagic:
    """A minimal mmagic tree on sys.path with the real package's failure points."""

    def __init__(self, broken_net=False):
        self.dir = tempfile.mkdtemp(prefix="fake-mmagic-")
        m = os.path.join(self.dir, "mmagic")
        write(os.path.join(m, "__init__.py"), "version = 'fake'\n")
        # The real ones import controlnet_aux / diffusers and fail in the venv.
        write(os.path.join(m, "apis", "__init__.py"), "raise AttributeError(\"module 'mediapipe' has no attribute 'solutions'\")\n")
        write(os.path.join(m, "models", "__init__.py"), "raise ModuleNotFoundError(\"No module named 'diffusers.pipelines.paint_by_example'\")\n")
        write(os.path.join(m, "models", "editors", "__init__.py"), "raise ModuleNotFoundError('editors must not be imported')\n")
        write(os.path.join(m, "models", "editors", "basicvsr", "__init__.py"), "raise ModuleNotFoundError('basicvsr package init must not run')\n")
        write(os.path.join(m, "models", "editors", "basicvsr", "basicvsr_net.py"), "class SPyNet:\n    pass\n")
        write(os.path.join(m, "models", "editors", "basicvsr_plusplus_net", "__init__.py"), "raise ModuleNotFoundError('net package init must not run')\n")
        # These two ARE imported for real by the network module, as in mmagic.
        write(os.path.join(m, "models", "archs", "__init__.py"), "class PixelShufflePack:\n    pass\n")
        write(os.path.join(m, "models", "utils", "__init__.py"), "def flow_warp():\n    return 'warp'\n")
        net = """
            from mmagic.models.archs import PixelShufflePack
            from mmagic.models.utils import flow_warp
            from ..basicvsr.basicvsr_net import SPyNet


            class BasicVSRPlusPlusNet:
                def __init__(self, **kwargs):
                    self.kwargs = kwargs
                    self.loaded = None
                    self.strict = None
                    self.device = None
                    self.evaled = False

                def load_state_dict(self, state, strict=False):
                    self.loaded, self.strict = state, strict
                    if strict and set(state) != {"spynet.w", "upsample.w"}:
                        raise RuntimeError("Missing key(s) in state_dict")

                def to(self, device):
                    self.device = device
                    return self

                def eval(self):
                    self.evaled = True
                    return self
        """
        if broken_net:
            net = "raise ImportError('mmcv.ops could not be loaded')\n"
        write(os.path.join(m, "models", "editors", "basicvsr_plusplus_net", "basicvsr_plusplus_net.py"), net)

    def __enter__(self):
        sys.path.insert(0, self.dir)
        return self

    def __exit__(self, *exc):
        sys.path.remove(self.dir)
        for name in [n for n in sys.modules if n == "mmagic" or n.startswith("mmagic.")]:
            del sys.modules[name]
        sys.modules.pop("torch", None)


def fake_torch(checkpoint, calls):
    t = types.ModuleType("torch")

    def load(path, map_location=None, weights_only=None):
        calls.append((path, map_location, weights_only))
        return checkpoint

    t.load = load
    return t


class BasicVSRPPLoaderTest(unittest.TestCase):
    def setUp(self):
        self.rv = load_script()

    def test_class_is_imported_without_the_broken_package_inits(self):
        with FakeMmagic():
            cls = self.rv.import_basicvsrpp_net_class()
            self.assertEqual(cls.__name__, "BasicVSRPlusPlusNet")
            # The unrelated code was never imported, and the two real dependencies were.
            self.assertNotIn("mmagic.apis", sys.modules)
            self.assertIn("mmagic.models.archs", sys.modules)
            self.assertIn("mmagic.models.utils", sys.modules)
            # A second call reuses what is registered.
            self.assertIs(self.rv.import_basicvsrpp_net_class(), cls)

    def test_the_old_import_would_still_fail(self):
        # Guards the test itself: the fake reproduces the production failure.
        with FakeMmagic():
            with self.assertRaises(AttributeError):
                importlib.import_module("mmagic.apis")

    def test_generator_weights_are_loaded_strictly_without_the_prefix(self):
        calls = []
        ck = {"meta": {}, "state_dict": {"generator.spynet.w": 1, "generator.upsample.w": 2, "step_counter": 3}}
        with FakeMmagic():
            sys.modules["torch"] = fake_torch(ck, calls)
            net = self.rv.load_basicvsrpp_generator(self.rv.Path("/models/w.pth"), "cuda:0")
            self.assertEqual(net.loaded, {"spynet.w": 1, "upsample.w": 2})
            self.assertTrue(net.strict)
            self.assertEqual(net.device, "cuda:0")
            self.assertTrue(net.evaled)
            # The config's generator arguments; no SPyNet download.
            self.assertEqual(net.kwargs, {"mid_channels": 64, "num_blocks": 7, "is_low_res_input": True, "spynet_pretrained": None})
            # Weights are read as data only (no pickle code execution), on the CPU first.
            self.assertEqual(calls, [("/models/w.pth", "cpu", True)])

    def test_a_checkpoint_without_generator_weights_is_refused(self):
        with FakeMmagic():
            sys.modules["torch"] = fake_torch({"state_dict": {"discriminator.w": 1}}, [])
            with self.assertRaises(SystemExit) as cm:
                self.rv.load_basicvsrpp_generator(self.rv.Path("/models/w.pth"), "cuda:0")
            self.assertIn("no generator weights", str(cm.exception))

    def test_mismatched_weights_are_an_error_not_a_silent_partial_load(self):
        with FakeMmagic():
            sys.modules["torch"] = fake_torch({"state_dict": {"generator.spynet.w": 1}}, [])
            with self.assertRaises(RuntimeError):
                self.rv.load_basicvsrpp_generator(self.rv.Path("/models/w.pth"), "cuda:0")

    def test_the_script_no_longer_uses_the_import_that_failed(self):
        with open(os.path.join(HERE, "restore_video.py")) as f:
            code = "\n".join(line for line in f.read().splitlines() if not line.lstrip().startswith("#"))
        self.assertNotIn("import init_model", code)
        self.assertNotIn("from mmagic.apis", code)


if __name__ == "__main__":
    unittest.main()
