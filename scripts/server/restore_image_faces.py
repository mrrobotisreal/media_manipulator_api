#!/usr/bin/env python3
"""Face-restoration helper for Media Manipulator's AI Image Restoration.

Runs GFPGAN v1.4 or CodeFormer over the faces in a single image and writes the
restored image as <--out-dir>/restored.png. These are GENERATIVE face priors —
they synthesize plausible facial detail and do NOT recover ground truth (the Go
side and UI surface this prominently).

    gfpgan      GFPGAN v1.4 (clean arch)
    codeformer  CodeFormer (fidelity weight `w`); gated behind a license flag

`--upscale` is the model's own upscale factor: 2/4 when run on the working
source (= the effective scale), 1 when chained on an already-upscaled general
result. Background up-sampling is intentionally DISABLED (bg_upsampler=None) so
the comparison stays honest — general models / chaining handle the background.

The Go API shells out to the deployed copy at
/opt/media-manipulator-ai/scripts/restore_image_faces.py; this file in the repo
is the source of truth — deploy with:

    sudo cp scripts/server/restore_image_faces.py \
      /opt/media-manipulator-ai/scripts/restore_image_faces.py
    sudo chmod +x /opt/media-manipulator-ai/scripts/restore_image_faces.py

Stdlib-only orchestration; torch/model imports happen lazily inside the
face-restore venv python (AI_FACE_RESTORE_PYTHON). For CodeFormer the wrapper
prepends repos/CodeFormer to sys.path and chdir's into it so its vendored
basicsr modules win. facexlib detection/parsing weights are pre-placed in the
venv (no first-run downloads mid-job).

Progress protocol: one "PROGRESS <done>/<numFaces>" line per restored face and
a final "PROGRESS total/total" on stdout. Fatal errors: one-line
"ERROR: <safe message>" and a non-zero exit (e.g.
"ERROR: No faces were detected in this image"). Honors --gpu. Never writes
outside --out-dir.
"""
import argparse
import os
import sys
from pathlib import Path

GFPGAN_WEIGHTS = "GFPGANv1.4.pth"
CODEFORMER_WEIGHTS = "codeformer.pth"


def fail(message: str) -> None:
    raise SystemExit(f"ERROR: {message}")


def emit_progress(done: int, total: int) -> None:
    print(f"PROGRESS {done}/{total}", flush=True)


def run_gfpgan(input_path: Path, out_dir: Path, models_dir: Path, upscale: int, device: str) -> None:
    import cv2  # noqa: PLC0415
    from gfpgan import GFPGANer  # noqa: PLC0415

    weights = models_dir / "gfpgan" / GFPGAN_WEIGHTS
    if not weights.exists():
        fail(f"GFPGAN weights not found at {weights}")

    img = cv2.imread(str(input_path), cv2.IMREAD_COLOR)
    if img is None:
        fail("could not read the input image")

    restorer = GFPGANer(
        model_path=str(weights),
        upscale=upscale,
        arch="clean",
        channel_multiplier=2,
        bg_upsampler=None,  # §2.2 — background up-sampling stays disabled
        device=device,
    )
    _, restored_faces, restored_img = restorer.enhance(
        img, has_aligned=False, only_center_face=False, paste_back=True
    )
    n = len(restored_faces) if restored_faces is not None else 0
    if n == 0 or restored_img is None:
        fail("No faces were detected in this image")
    for i in range(n):
        emit_progress(i + 1, n)
    if not cv2.imwrite(str(out_dir / "restored.png"), restored_img):
        fail("could not write the restored image")
    emit_progress(n, n)


def run_codeformer(input_path: Path, out_dir: Path, models_dir: Path, repos_dir: Path, upscale: int, fidelity: float, device: str) -> None:
    repo = repos_dir / "CodeFormer"
    if not repo.is_dir():
        fail(f"CodeFormer repo not cloned at {repo}")
    # Prepend the repo and chdir into it so its vendored basicsr modules win
    # over any pip-installed basicsr where they differ.
    sys.path.insert(0, str(repo))
    os.chdir(str(repo))

    import cv2  # noqa: PLC0415
    import torch  # noqa: PLC0415
    from torchvision.transforms.functional import normalize  # noqa: PLC0415
    from basicsr.utils import img2tensor, tensor2img  # noqa: PLC0415
    from basicsr.utils.registry import ARCH_REGISTRY  # noqa: PLC0415
    from facexlib.utils.face_restoration_helper import FaceRestoreHelper  # noqa: PLC0415

    weights = models_dir / "codeformer" / CODEFORMER_WEIGHTS
    if not weights.exists():
        fail(f"CodeFormer weights not found at {weights}")

    net = ARCH_REGISTRY.get("CodeFormer")(
        dim_embd=512, codebook_size=1024, n_head=8, n_layers=9,
        connect_list=["32", "64", "128", "256"],
    ).to(device)
    checkpoint = torch.load(str(weights), map_location="cpu")
    state = checkpoint.get("params_ema", checkpoint) if isinstance(checkpoint, dict) else checkpoint
    net.load_state_dict(state)
    net.eval()

    img = cv2.imread(str(input_path), cv2.IMREAD_COLOR)
    if img is None:
        fail("could not read the input image")

    face_helper = FaceRestoreHelper(
        upscale,
        face_size=512,
        crop_ratio=(1, 1),
        det_model="retinaface_resnet50",
        save_ext="png",
        use_parse=True,
        device=device,
    )
    face_helper.read_image(img)
    num = face_helper.get_face_landmarks_5(only_center_face=False, resize=640, eye_dist_threshold=5)
    if num == 0:
        fail("No faces were detected in this image")
    face_helper.align_warp_face()

    for idx, cropped_face in enumerate(face_helper.cropped_faces):
        t = img2tensor(cropped_face / 255.0, bgr2rgb=True, float32=True)
        normalize(t, (0.5, 0.5, 0.5), (0.5, 0.5, 0.5), inplace=True)
        t = t.unsqueeze(0).to(device)
        try:
            with torch.no_grad():
                output = net(t, w=fidelity, adain=True)[0]
                restored = tensor2img(output, rgb2bgr=True, min_max=(-1, 1))
            del output
            torch.cuda.empty_cache()
        except Exception as exc:  # noqa: BLE001
            torch.cuda.empty_cache()
            restored = tensor2img(t, rgb2bgr=True, min_max=(-1, 1))
            print(f"codeformer: face {idx} fell back to input ({exc})", flush=True)
        restored = restored.astype("uint8")
        face_helper.add_restored_face(restored)
        emit_progress(idx + 1, num)

    face_helper.get_inverse_affine(None)
    # upsample_img=None → background is resized by `upscale`, not model-enhanced.
    restored_img = face_helper.paste_faces_to_input_image(upsample_img=None)
    if not cv2.imwrite(str(out_dir / "restored.png"), restored_img):
        fail("could not write the restored image")
    emit_progress(num, num)


def main() -> None:
    parser = argparse.ArgumentParser(description="Face restoration (GFPGAN / CodeFormer)")
    parser.add_argument("--model", required=True, choices=["gfpgan", "codeformer"])
    parser.add_argument("--input", required=True)
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--upscale", type=int, default=2, choices=[1, 2, 4])
    parser.add_argument("--fidelity", type=float, default=0.7, help="CodeFormer only: 0..1")
    parser.add_argument("--models-dir", default="/opt/media-manipulator-ai/models/restore")
    parser.add_argument("--repos-dir", default="/opt/media-manipulator-ai/repos")
    parser.add_argument("--gpu", default="0")
    args = parser.parse_args()

    input_path = Path(args.input).resolve()
    out_dir = Path(args.out_dir).resolve()
    models_dir = Path(args.models_dir).resolve()
    repos_dir = Path(args.repos_dir).resolve()
    if not input_path.is_file():
        fail(f"input image does not exist: {input_path}")
    out_dir.mkdir(parents=True, exist_ok=True)

    try:
        import torch  # noqa: PLC0415
    except Exception as exc:  # noqa: BLE001
        fail(f"torch not importable in this venv: {exc}")
        raise
    if not torch.cuda.is_available():
        fail("CUDA is not available in this venv (check driver / --gpu)")
    device = f"cuda:{args.gpu}"

    fidelity = min(1.0, max(0.0, args.fidelity))
    if args.model == "gfpgan":
        run_gfpgan(input_path, out_dir, models_dir, args.upscale, device)
    else:
        run_codeformer(input_path, out_dir, models_dir, repos_dir, args.upscale, fidelity, device)


if __name__ == "__main__":
    main()
