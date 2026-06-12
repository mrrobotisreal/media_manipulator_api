#!/usr/bin/env python3
"""Video-restoration helper for Media Manipulator's AI Video Restoration.

Runs one of the sequence-native restoration models — BasicVSR++ (mmagic),
RVRT or VRT (KAIR-style repos) — over the PNGs in --frames-dir and writes the
enhanced PNGs, same filenames, into <--out-dir>/frames. The Go API stitches;
this script never encodes video. Deployed copy lives at
/opt/media-manipulator-ai/scripts/restore_video.py; this file in the repo is
the source of truth — deploy with:

    sudo cp scripts/server/restore_video.py \
      /opt/media-manipulator-ai/scripts/restore_video.py
    sudo chmod +x /opt/media-manipulator-ai/scripts/restore_video.py

Stdlib-only orchestration; torch/mmagic imports happen lazily inside the venv
python the Go API selects (restore-vsr-mm for basicvsrpp, restore-sr for
rvrt/vrt).

Progress protocol: "PROGRESS <done>/<total>" lines on stdout — per window for
BasicVSR++, coarse (output-file count, sampled every 2s) for RVRT/VRT. Fatal
errors: one-line "ERROR: <safe message>" and a non-zero exit. Honors
CUDA_VISIBLE_DEVICES (set by the Go caller). Never writes outside --out-dir
and --temp-root.

KAIR scripts (RVRT/VRT) run with cwd inside a staging dir; their model_zoo is
symlinked from the repo clone so weights are shared, and they save into a
`results/` tree relative to cwd. The staging dir defaults to a temp dir UNDER
--out-dir (so the work is co-located with the job and visible on the same
volume as the frame-by-frame models, e.g.
<out-dir>/_kair_stage_XXXX/results/...); pass --temp-root to override. Two
KAIR gotchas this wrapper handles: (1) the test scripts only WRITE frames when
their `--save_result` flag is set, so we detect that flag in the installed
script and pass it in the correct form; (2) the exact results/ subpath varies
by repo version, so after the run we scan the whole staging tree (minus the
copied LQ inputs) for produced PNGs rather than assuming one fixed subdir.
"""
import argparse
import re
import shutil
import subprocess
import sys
import tempfile
import threading
from pathlib import Path
from typing import List, Optional

BASICVSRPP_CONFIG = "configs/basicvsr_pp/basicvsr-pp_c64n7_8xb1-600k_reds4.py"
BASICVSRPP_WEIGHTS = "basicvsr_plusplus_c64n7_8x1_600k_reds4_20210217-db622b2f.pth"
KAIR_DEFAULT_TASKS = {
    "rvrt": "001_RVRT_videosr_bi_REDS_30frames",
    "vrt": "001_VRT_videosr_bi_REDS_6frames",
}


def fail(message: str) -> None:
    raise SystemExit(f"ERROR: {message}")


def emit_progress(done: int, total: int) -> None:
    print(f"PROGRESS {done}/{total}", flush=True)


def run(cmd: List[str], *, cwd: Optional[Path] = None) -> None:
    print("+ " + " ".join(cmd), flush=True)
    proc = subprocess.run(cmd, cwd=str(cwd) if cwd else None)
    if proc.returncode != 0:
        fail(f"subprocess exited with code {proc.returncode}")


def list_frames(frames_dir: Path) -> List[Path]:
    frames = sorted(p for p in frames_dir.glob("*.png") if p.is_file())
    if not frames:
        fail("no input frames found")
    return frames


def resolve_weights(models_dir: Path, model: str, preferred: str) -> Path:
    exact = models_dir / model / preferred
    if exact.exists():
        return exact
    candidates = sorted((models_dir / model).glob("*.pth")) if (models_dir / model).is_dir() else []
    if candidates:
        return candidates[0]
    fail(f"weights for {model} not found under {models_dir / model} (expected {preferred})")
    raise AssertionError  # unreachable


# ---------------------------------------------------------------------------
# BasicVSR++ via mmagic. We call the generator network directly (bypassing the
# inferencer's data pipeline) over fixed-size windows so VRAM stays bounded.
# ---------------------------------------------------------------------------

def run_basicvsrpp(repos_dir: Path, models_dir: Path, frames: List[Path], out_frames: Path, gpu: str, max_seq_len: int) -> None:
    try:
        import cv2  # noqa: PLC0415
        import numpy as np  # noqa: PLC0415
        import torch  # noqa: PLC0415
        from mmagic.apis import init_model  # noqa: PLC0415
    except Exception as exc:  # noqa: BLE001
        fail(f"mmagic environment not importable: {exc}")
        raise

    config = repos_dir / "mmagic" / BASICVSRPP_CONFIG
    if not config.exists():
        fail(f"mmagic config not found at {config} (clone the mmagic repo)")
    weights = resolve_weights(models_dir, "basicvsrpp", BASICVSRPP_WEIGHTS)
    if not torch.cuda.is_available():
        fail("CUDA is not available in this venv (check driver / CUDA_VISIBLE_DEVICES)")
    device = f"cuda:{gpu}"

    model = init_model(str(config), str(weights), device=device)
    generator = getattr(model, "generator", None)
    if generator is None:
        fail("loaded mmagic model has no generator network (unexpected mmagic version)")
    generator.eval()

    total = len(frames)
    done = 0
    window = max(1, max_seq_len)
    with torch.no_grad():
        for start in range(0, total, window):
            batch_frames = frames[start : start + window]
            imgs = []
            for frame in batch_frames:
                img = cv2.imread(str(frame), cv2.IMREAD_COLOR)
                if img is None:
                    fail(f"could not read frame {frame.name}")
                img = img.astype(np.float32) / 255.0
                imgs.append(torch.from_numpy(np.transpose(img[:, :, [2, 1, 0]], (2, 0, 1))))
            lqs = torch.stack(imgs).unsqueeze(0).to(device)  # (1, t, c, h, w)
            outs = generator(lqs)
            if isinstance(outs, (list, tuple)):
                outs = outs[0]
            outs = outs.squeeze(0).float().cpu().clamp_(0, 1).numpy()
            for i, frame in enumerate(batch_frames):
                out = np.transpose(outs[i][[2, 1, 0], :, :], (1, 2, 0))
                out = (out * 255.0).round().astype(np.uint8)
                if not cv2.imwrite(str(out_frames / frame.name), out):
                    fail(f"could not write enhanced frame {frame.name}")
            done += len(batch_frames)
            emit_progress(done, total)
            torch.cuda.empty_cache()


# ---------------------------------------------------------------------------
# RVRT / VRT via the KAIR-style repo test scripts. We stage the frames into
# the folder-of-folders layout main_test_*.py expects, run with cwd inside our
# temp dir (model_zoo symlinked from the repo so weights are shared but the
# results/ tree lands in temp), then move the outputs into place.
# ---------------------------------------------------------------------------

# collect_outputs finds every produced PNG anywhere under the staging tree,
# excluding the LQ inputs we copied in and the model_zoo symlink (rglob follows
# symlinks on newer Python). KAIR's results/ subpath varies by repo version
# (results/<task>/<folder>/..., sometimes an extra level), so scanning the whole
# tree is more robust than assuming one fixed directory.
def collect_outputs(work: Path, lq_root: Path) -> List[Path]:
    model_zoo = work / "model_zoo"
    excluded = {lq_root, model_zoo}
    return sorted(
        p
        for p in work.rglob("*.png")
        if p.is_file() and excluded.isdisjoint(p.parents)
    )


def watch_results(work: Path, lq_root: Path, total: int, stop: threading.Event) -> None:
    while not stop.wait(2.0):
        done = len(collect_outputs(work, lq_root)) if work.exists() else 0
        emit_progress(min(done, total), total)


# kair_save_result_args inspects the installed test script and returns the argv
# needed to make it actually WRITE frames. KAIR's main_test_{rvrt,vrt}.py only
# save output when their `--save_result` flag is set; without it the model runs
# to completion and discards every frame (the "ran for 15 minutes, found 0
# frames" bug). The flag's form differs by version (store_true vs int), so we
# match whatever the script defines and never pass an arg it would reject.
def kair_save_result_args(test_script: Path) -> List[str]:
    try:
        text = test_script.read_text(errors="ignore")
    except OSError:
        return []
    match = re.search(r"add_argument\(\s*['\"]--save_result['\"](.*?)\)", text, re.S)
    if not match:
        # No such flag — this version always saves. Nothing to add.
        return []
    spec = match.group(1)
    if "store_true" in spec or "store_const" in spec:
        return ["--save_result"]
    return ["--save_result", "1"]


def run_kair(model: str, repos_dir: Path, frames: List[Path], out_frames: Path, tile: str, tile_overlap: str, task: str, temp_root: Path) -> None:
    repo = repos_dir / model.upper()
    test_script = repo / f"main_test_{model}.py"
    if not test_script.exists():
        fail(f"{model.upper()} repo not cloned at {repo}")
    model_zoo = repo / "model_zoo"
    if not model_zoo.is_dir():
        fail(f"{model.upper()} model_zoo missing at {model_zoo} (download weights per INSTALL_VIDEO_RESTORATION.md)")

    tile_parts = [p.strip() for p in tile.split(",") if p.strip()]
    overlap_parts = [p.strip() for p in tile_overlap.split(",") if p.strip()]
    if len(tile_parts) != 3 or len(overlap_parts) != 3:
        fail('expected --tile and --tile-overlap as three comma-separated ints (e.g. "12,128,128")')

    temp_root.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix=f"_kair_stage_{model}_", dir=str(temp_root)))
    try:
        lq_root = work / "lq"
        clip_dir = lq_root / "clip"
        clip_dir.mkdir(parents=True, exist_ok=True)
        for frame in frames:
            shutil.copy2(str(frame), str(clip_dir / frame.name))
        # Relative model_zoo lookups resolve inside cwd — share the repo's.
        (work / "model_zoo").symlink_to(model_zoo, target_is_directory=True)

        stop = threading.Event()
        watcher = threading.Thread(target=watch_results, args=(work, lq_root, len(frames), stop), daemon=True)
        watcher.start()
        try:
            run(
                [
                    sys.executable,
                    str(test_script),
                    "--task", task,
                    "--folder_lq", str(lq_root),
                    "--tile", *tile_parts,
                    "--tile_overlap", *overlap_parts,
                    *kair_save_result_args(test_script),
                ],
                cwd=work,
            )
        finally:
            stop.set()
            watcher.join(timeout=5)

        produced = collect_outputs(work, lq_root)
        if len(produced) != len(frames):
            fail(
                f"{model.upper()} produced {len(produced)} frames, expected {len(frames)} "
                f"(searched {work}/**/*.png). If 0, the test script likely did not save — "
                f"check that it accepts --save_result."
            )
        # Output names vary by repo version — map sorted outputs onto the
        # sorted input names.
        for src, frame in zip(produced, frames):
            shutil.move(str(src), str(out_frames / frame.name))
        emit_progress(len(frames), len(frames))
    finally:
        shutil.rmtree(work, ignore_errors=True)


def main() -> None:
    parser = argparse.ArgumentParser(description="Video restoration (BasicVSR++ / RVRT / VRT) for AI Video Restoration")
    parser.add_argument("--model", required=True, choices=["basicvsrpp", "rvrt", "vrt"])
    parser.add_argument("--frames-dir", required=True)
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--gpu", default="0")
    parser.add_argument("--models-dir", default="/opt/media-manipulator-ai/models/restore")
    parser.add_argument("--repos-dir", default="/opt/media-manipulator-ai/repos")
    parser.add_argument("--max-seq-len", type=int, default=16)
    parser.add_argument("--tile", default="")
    parser.add_argument("--tile-overlap", default="")
    parser.add_argument("--task", default="", help="KAIR task name override for rvrt/vrt")
    parser.add_argument(
        "--temp-root",
        default="",
        help="staging root for RVRT/VRT (default: a temp dir under --out-dir, co-located with the job)",
    )
    args = parser.parse_args()

    frames_dir = Path(args.frames_dir).resolve()
    out_dir = Path(args.out_dir).resolve()
    models_dir = Path(args.models_dir).resolve()
    repos_dir = Path(args.repos_dir).resolve()
    if not frames_dir.is_dir():
        fail(f"frames dir does not exist: {frames_dir}")
    out_frames = out_dir / "frames"
    out_frames.mkdir(parents=True, exist_ok=True)

    frames = list_frames(frames_dir)

    if args.model == "basicvsrpp":
        run_basicvsrpp(repos_dir, models_dir, frames, out_frames, args.gpu, args.max_seq_len)
        return

    task = args.task or KAIR_DEFAULT_TASKS[args.model]
    tile = args.tile or ("30,128,128" if args.model == "rvrt" else "12,128,128")
    overlap = args.tile_overlap or "2,20,20"
    # Default the KAIR staging dir to UNDER --out-dir so the runtime work is
    # co-located with the job (same volume as the frame-by-frame models) and
    # visible while it runs, instead of a global /opt tmp.
    temp_root = Path(args.temp_root).resolve() if args.temp_root.strip() else out_dir
    run_kair(args.model, repos_dir, frames, out_frames, tile, overlap, task, temp_root)


if __name__ == "__main__":
    main()
