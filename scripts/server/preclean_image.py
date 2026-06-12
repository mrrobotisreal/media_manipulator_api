#!/usr/bin/env python3
"""Fidelity-preserving pre-clean helper for Media Manipulator's AI Image Restoration.

Removes degradation from a single image WITHOUT synthesizing new content — the
forensic counterpoint to the generative face models. One of three models per
invocation:

    fbcnn   blind JPEG / compression-artifact removal (optional QF override)
    scunet  blind real-noise denoising (PSNR-trained weights — not the GAN variant)
    nafnet  motion deblurring (GoPro weights)

Each runs at 1x (output dimensions == input). The Go API runs them sequentially
in the fixed order fbcnn → scunet → nafnet, each on the previous step's output.

The Go API shells out to the deployed copy at
/opt/media-manipulator-ai/scripts/preclean_image.py; this file in the repo is
the source of truth — deploy with:

    sudo cp scripts/server/preclean_image.py \
      /opt/media-manipulator-ai/scripts/preclean_image.py
    sudo chmod +x /opt/media-manipulator-ai/scripts/preclean_image.py

Stdlib-only orchestration; torch/cv2/model imports happen lazily inside the
preclean venv python (AI_PRECLEAN_PYTHON). Network *definitions* are imported
from the cloned repos via sys.path (FBCNN, SCUNet, NAFNet's vendored basicsr) —
the wrapper constructs the network class directly and loads the state dict; it
does NOT use the repos' training/option machinery.

Progress protocol: one "PROGRESS <done>/<total>" line per processed tile and a
final "PROGRESS total/total" on stdout. Fatal errors: one-line
"ERROR: <safe message>" and a non-zero exit. Honors --gpu (addresses the
physical CUDA device; the Go caller does not remap CUDA_VISIBLE_DEVICES). Never
writes outside --out-dir.
"""
import argparse
import sys
from pathlib import Path
from typing import Callable

FBCNN_WEIGHTS = "fbcnn_color.pth"
SCUNET_WEIGHTS = "scunet_color_real_psnr.pth"
NAFNET_WEIGHTS = "NAFNet-GoPro-width64.pth"

TILE = 512
TILE_OVERLAP = 32


def fail(message: str) -> None:
    raise SystemExit(f"ERROR: {message}")


def emit_progress(done: int, total: int) -> None:
    print(f"PROGRESS {done}/{total}", flush=True)


def load_state_dict(weights: Path):
    import torch  # noqa: PLC0415 — venv import by design

    state = torch.load(str(weights), map_location="cpu")
    if isinstance(state, dict):
        for key in ("params_ema", "params", "state_dict", "model"):
            if key in state and isinstance(state[key], dict):
                return state[key]
    return state


# ---------------------------------------------------------------------------
# Network construction. Definitions match the official release configs.
# ---------------------------------------------------------------------------

def build_fbcnn(repos_dir: Path, weights: Path, device: str, qf_override: int):
    import torch  # noqa: PLC0415

    repo = repos_dir / "FBCNN"
    if not repo.is_dir():
        fail(f"FBCNN repo not cloned at {repo}")
    sys.path.insert(0, str(repo))
    try:
        from models.network_fbcnn import FBCNN  # type: ignore
    except Exception as exc:  # noqa: BLE001
        fail(f"could not import FBCNN network definition: {exc}")
        raise

    net = FBCNN(in_nc=3, out_nc=3, nc=[64, 128, 256, 512], nb=4,
                act_mode="R", downsample_mode="strideconv",
                upsample_mode="convtranspose")
    net.load_state_dict(load_state_dict(weights), strict=True)
    net = net.eval().to(device)

    qf_tensor = None
    if qf_override and 1 <= qf_override <= 100:
        qf_tensor = torch.tensor([[qf_override / 100.0]], dtype=torch.float32, device=device)

    def infer(x):
        # FBCNN returns (restored, estimated_QF). With a QF override we pass a
        # fixed quality factor; otherwise it predicts blindly.
        out = net(x, qf_tensor) if qf_tensor is not None else net(x)
        if isinstance(out, (tuple, list)):
            out = out[0]
        return out

    return infer


def build_scunet(repos_dir: Path, weights: Path, device: str):
    repo = repos_dir / "SCUNet"
    if not repo.is_dir():
        fail(f"SCUNet repo not cloned at {repo}")
    sys.path.insert(0, str(repo))
    try:
        from models.network_scunet import SCUNet  # type: ignore
    except Exception as exc:  # noqa: BLE001
        fail(f"could not import SCUNet network definition: {exc}")
        raise

    net = SCUNet(in_nc=3, config=[4, 4, 4, 4, 4, 4, 4], dim=64)
    net.load_state_dict(load_state_dict(weights), strict=True)
    net = net.eval().to(device)
    return lambda x: net(x)


def build_nafnet(repos_dir: Path, weights: Path, device: str):
    repo = repos_dir / "NAFNet"
    if not repo.is_dir():
        fail(f"NAFNet repo not cloned at {repo}")
    # NAFNet vendors its own basicsr fork in-repo; prepend it so the arch import
    # resolves to the vendored copy.
    sys.path.insert(0, str(repo))
    try:
        from basicsr.models.archs.NAFNet_arch import NAFNet  # type: ignore
    except Exception as exc:  # noqa: BLE001
        fail(f"could not import NAFNet network definition: {exc}")
        raise

    net = NAFNet(img_channel=3, width=64, enc_blk_nums=[1, 1, 1, 28],
                 middle_blk_num=1, dec_blk_nums=[1, 1, 1, 1])
    net.load_state_dict(load_state_dict(weights), strict=True)
    net = net.eval().to(device)
    return lambda x: net(x)


# ---------------------------------------------------------------------------
# Tiled 1x inference. Tiles keep VRAM bounded on large crops; each tile is
# padded to a multiple of `pad` (the networks downsample internally) and cropped
# back. Overlapping tiles are blended by averaging.
# ---------------------------------------------------------------------------

def run_tiled(infer: Callable, img_lq, tile: int, overlap: int, pad: int, device: str):
    import torch  # noqa: PLC0415

    b, c, h, w = img_lq.size()
    out = torch.zeros(b, c, h, w, dtype=img_lq.dtype, device=device)
    weight = torch.zeros_like(out)

    if tile <= 0:
        tile = max(h, w)
    tile = min(tile, h, w)
    stride = max(1, tile - overlap)
    h_idx = list(range(0, max(1, h - tile), stride)) + [max(0, h - tile)]
    w_idx = list(range(0, max(1, w - tile), stride)) + [max(0, w - tile)]
    h_idx = sorted(set(h_idx))
    w_idx = sorted(set(w_idx))

    total = len(h_idx) * len(w_idx)
    done = 0
    for hi in h_idx:
        for wi in w_idx:
            patch = img_lq[..., hi:hi + tile, wi:wi + tile]
            ph, pw = patch.size(2), patch.size(3)
            # Pad up to a multiple of `pad` (reflect) for the network, then crop.
            pad_h = (pad - ph % pad) % pad
            pad_w = (pad - pw % pad) % pad
            if pad_h or pad_w:
                patch = torch.nn.functional.pad(patch, (0, pad_w, 0, pad_h), mode="reflect")
            res = infer(patch)
            res = res[..., :ph, :pw]
            out[..., hi:hi + ph, wi:wi + pw].add_(res)
            weight[..., hi:hi + ph, wi:wi + pw].add_(torch.ones_like(res))
            done += 1
            emit_progress(done, total)
            torch.cuda.empty_cache()
    return out.div_(weight.clamp_(min=1.0))


def main() -> None:
    parser = argparse.ArgumentParser(description="Fidelity-preserving pre-clean (FBCNN/SCUNet/NAFNet)")
    parser.add_argument("--model", required=True, choices=["fbcnn", "scunet", "nafnet"])
    parser.add_argument("--input", required=True)
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--models-dir", default="/opt/media-manipulator-ai/models/restore")
    parser.add_argument("--repos-dir", default="/opt/media-manipulator-ai/repos")
    parser.add_argument("--gpu", default="0")
    parser.add_argument("--fbcnn-qf", type=int, default=0, help="FBCNN only: 1..100, omit/0 = blind/auto")
    args = parser.parse_args()

    input_path = Path(args.input).resolve()
    out_dir = Path(args.out_dir).resolve()
    models_dir = Path(args.models_dir).resolve()
    repos_dir = Path(args.repos_dir).resolve()
    if not input_path.is_file():
        fail(f"input image does not exist: {input_path}")
    out_dir.mkdir(parents=True, exist_ok=True)

    try:
        import cv2  # noqa: PLC0415
        import numpy as np  # noqa: PLC0415
        import torch  # noqa: PLC0415
    except Exception as exc:  # noqa: BLE001
        fail(f"required packages not importable in this venv: {exc}")
        raise
    if not torch.cuda.is_available():
        fail("CUDA is not available in this venv (check driver / --gpu)")
    device = f"cuda:{args.gpu}"

    img = cv2.imread(str(input_path), cv2.IMREAD_COLOR)
    if img is None:
        fail("could not read the input image")
    img = img.astype(np.float32) / 255.0
    tensor = torch.from_numpy(np.transpose(img[:, :, [2, 1, 0]], (2, 0, 1))).unsqueeze(0).to(device)

    if args.model == "fbcnn":
        weights = models_dir / "fbcnn" / FBCNN_WEIGHTS
        if not weights.exists():
            fail(f"FBCNN weights not found at {weights}")
        infer = build_fbcnn(repos_dir, weights, device, args.fbcnn_qf)
        pad = 16
    elif args.model == "scunet":
        weights = models_dir / "scunet" / SCUNET_WEIGHTS
        if not weights.exists():
            fail(f"SCUNet weights not found at {weights}")
        infer = build_scunet(repos_dir, weights, device)
        pad = 64
    else:
        weights = models_dir / "nafnet" / NAFNET_WEIGHTS
        if not weights.exists():
            fail(f"NAFNet weights not found at {weights}")
        infer = build_nafnet(repos_dir, weights, device)
        pad = 32

    with torch.no_grad():
        out = run_tiled(infer, tensor, TILE, TILE_OVERLAP, pad, device)

    out = out.data.squeeze().float().cpu().clamp_(0, 1).numpy()
    out = np.transpose(out[[2, 1, 0], :, :], (1, 2, 0))
    out = (out * 255.0).round().astype(np.uint8)
    if not cv2.imwrite(str(out_dir / "cleaned.png"), out):
        fail("could not write the cleaned image")
    emit_progress(1, 1)


if __name__ == "__main__":
    main()
