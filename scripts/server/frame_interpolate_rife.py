#!/usr/bin/env python3
"""Frame interpolation helper for Media Manipulator.

Runs ffmpeg → rife-ncnn-vulkan → ffmpeg in one process. The Go API shells out
to the deployed copy at /opt/media-manipulator-ai/scripts/frame_interpolate_rife.py;
this file in the repo is the source of truth — deploy with:

    sudo cp scripts/server/frame_interpolate_rife.py \
      /opt/media-manipulator-ai/scripts/frame_interpolate_rife.py
    sudo chmod +x /opt/media-manipulator-ai/scripts/frame_interpolate_rife.py

Stdlib only. Requires ffmpeg, ffprobe, and rife-ncnn-vulkan on the server.
"""
import argparse
import json
import math
import os
import shutil
import subprocess
import tempfile
from fractions import Fraction
from pathlib import Path
from typing import Any, Dict, List, Optional


def run(cmd: List[str], *, cwd: Optional[Path] = None, env: Optional[Dict[str, str]] = None) -> None:
    print("+ " + " ".join(cmd), flush=True)
    proc = subprocess.run(cmd, cwd=str(cwd) if cwd else None, env=env)
    if proc.returncode != 0:
        raise SystemExit(f"Command failed with exit code {proc.returncode}: {' '.join(cmd)}")


def capture(cmd: List[str]) -> str:
    proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    if proc.returncode != 0:
        raise SystemExit(f"Command failed: {' '.join(cmd)}\n{proc.stderr}")
    return proc.stdout


def parse_rate(value: str) -> float:
    value = (value or "").strip()
    if not value or value == "0/0":
        return 0.0
    try:
        return float(Fraction(value))
    except Exception:
        try:
            return float(value)
        except Exception:
            return 0.0


def ffprobe(input_path: Path) -> Dict[str, Any]:
    raw = capture([
        "ffprobe",
        "-v", "error",
        "-select_streams", "v:0",
        "-show_streams",
        "-show_format",
        "-of", "json",
        str(input_path),
    ])
    payload = json.loads(raw)
    streams = payload.get("streams") or []
    if not streams:
        raise SystemExit("No video stream found")
    stream = streams[0]
    fmt = payload.get("format") or {}

    duration = 0.0
    for candidate in [stream.get("duration"), fmt.get("duration")]:
        if candidate:
            try:
                duration = float(candidate)
                break
            except Exception:
                pass

    fps = parse_rate(stream.get("avg_frame_rate") or stream.get("r_frame_rate") or "")
    width = int(stream.get("width") or 0)
    height = int(stream.get("height") or 0)

    return {
        "duration": duration,
        "fps": fps,
        "width": width,
        "height": height,
        "codec": stream.get("codec_name") or "",
        "pix_fmt": stream.get("pix_fmt") or "",
    }


def count_pngs(directory: Path) -> int:
    return len(list(directory.glob("*.png")))


def choose_source_fps(source_fps: float) -> float:
    if source_fps > 0 and math.isfinite(source_fps):
        return source_fps
    return 30.0


def quality_to_crf(quality: str) -> str:
    quality = (quality or "medium").lower()
    if quality == "low":
        return "26"
    if quality == "high":
        return "17"
    return "20"


def quality_to_preset(quality: str) -> str:
    quality = (quality or "medium").lower()
    if quality == "low":
        return "veryfast"
    if quality == "high":
        return "slow"
    return "medium"


def validate_target_fps(source_fps: float, target_fps: float) -> None:
    if target_fps <= 0:
        raise SystemExit("target fps must be greater than 0")
    if target_fps > 120:
        raise SystemExit("target fps must be <= 120")
    if source_fps > 0 and target_fps <= source_fps + 0.01:
        raise SystemExit(f"target fps ({target_fps}) must be greater than source fps ({source_fps:.3f})")
    if source_fps > 0 and target_fps / source_fps > 5.1:
        raise SystemExit("target/source FPS ratio is too high for this tool")


def build_scale_filter(max_height: int) -> Optional[str]:
    if max_height <= 0:
        return None
    return f"scale=-2:'min(ih,{max_height})'"


def main() -> None:
    parser = argparse.ArgumentParser(description="AI video frame interpolation using rife-ncnn-vulkan")
    parser.add_argument("--input", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--target-fps", type=float, required=True)
    parser.add_argument("--rife-bin", default="/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/rife-ncnn-vulkan")
    parser.add_argument("--rife-model", default="/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/models/rife-v4.6")
    parser.add_argument("--gpu", default="1")
    parser.add_argument("--threads", default="1:2:2")
    parser.add_argument("--quality", choices=["low", "medium", "high"], default="medium")
    parser.add_argument("--max-height", type=int, default=720)
    parser.add_argument("--max-duration-seconds", type=float, default=120)
    parser.add_argument("--temp-root", default="/opt/media-manipulator-ai/tmp")
    parser.add_argument("--keep-temp", action="store_true")
    args = parser.parse_args()

    input_path = Path(args.input).resolve()
    output_path = Path(args.output).resolve()
    rife_bin = Path(args.rife_bin).resolve()
    rife_model = Path(args.rife_model).resolve()

    if not input_path.exists():
        raise SystemExit(f"input does not exist: {input_path}")
    if not rife_bin.exists():
        raise SystemExit(f"rife binary does not exist: {rife_bin}")
    if not rife_model.exists():
        raise SystemExit(f"rife model directory does not exist: {rife_model}")

    meta = ffprobe(input_path)
    duration = float(meta["duration"] or 0.0)
    source_fps = choose_source_fps(float(meta["fps"] or 0.0))
    target_fps = float(args.target_fps)

    validate_target_fps(source_fps, target_fps)

    if duration > 0 and args.max_duration_seconds > 0 and duration > args.max_duration_seconds:
        raise SystemExit(
            f"Video duration {duration:.2f}s exceeds AI frame interpolation limit "
            f"of {args.max_duration_seconds:.2f}s. Try trimming the video or increasing "
            "AI_FRAME_INTERPOLATION_MAX_DURATION_SECONDS."
        )

    temp_root = Path(args.temp_root)
    temp_root.mkdir(parents=True, exist_ok=True)
    work_dir = Path(tempfile.mkdtemp(prefix="mm_rife_", dir=str(temp_root)))
    input_frames = work_dir / "input_frames"
    output_frames = work_dir / "output_frames"
    input_frames.mkdir(parents=True, exist_ok=True)
    output_frames.mkdir(parents=True, exist_ok=True)

    try:
        print(json.dumps({
            "stage": "probe",
            "input": str(input_path),
            "duration": duration,
            "sourceFps": source_fps,
            "targetFps": target_fps,
            "width": meta["width"],
            "height": meta["height"],
            "workDir": str(work_dir),
        }), flush=True)

        vf_parts: List[str] = []
        scale_filter = build_scale_filter(args.max_height)
        if scale_filter:
            vf_parts.append(scale_filter)
        vf_parts.append(f"fps={source_fps:.6f}")

        run([
            "ffmpeg",
            "-y",
            "-i", str(input_path),
            "-map", "0:v:0",
            "-vf", ",".join(vf_parts),
            "-vsync", "0",
            str(input_frames / "%08d.png"),
        ])

        input_count = count_pngs(input_frames)
        if input_count < 2:
            raise SystemExit("not enough frames extracted for interpolation")

        if duration > 0:
            target_count = int(round(duration * target_fps))
        else:
            target_count = int(round(input_count * (target_fps / source_fps)))

        target_count = max(target_count, input_count + 1)
        target_count = min(target_count, int(input_count * 5.1))

        print(json.dumps({
            "stage": "interpolate",
            "inputFrameCount": input_count,
            "targetFrameCount": target_count,
            "targetFps": target_fps,
        }), flush=True)

        env = os.environ.copy()
        run([
            str(rife_bin),
            "-i", str(input_frames),
            "-o", str(output_frames),
            "-n", str(target_count),
            "-m", str(rife_model),
            "-g", str(args.gpu),
            "-j", str(args.threads),
            "-f", "%08d.png",
        ], env=env)

        output_count = count_pngs(output_frames)
        if output_count < input_count:
            raise SystemExit(f"RIFE produced too few frames: {output_count}")

        output_path.parent.mkdir(parents=True, exist_ok=True)
        crf = quality_to_crf(args.quality)
        preset = quality_to_preset(args.quality)

        encode_args = [
            "ffmpeg",
            "-y",
            "-framerate", f"{target_fps:.6f}",
            "-i", str(output_frames / "%08d.png"),
            "-i", str(input_path),
            "-map", "0:v:0",
            "-map", "1:a:0?",
            "-c:v", "libx264",
            "-preset", preset,
            "-crf", crf,
            "-pix_fmt", "yuv420p",
            "-movflags", "+faststart",
            "-c:a", "aac",
            "-b:a", "192k",
            "-shortest",
            str(output_path),
        ]
        run(encode_args)

        if not output_path.exists() or output_path.stat().st_size <= 0:
            raise SystemExit("output was not created or is empty")

        print(json.dumps({
            "stage": "done",
            "output": str(output_path),
            "outputBytes": output_path.stat().st_size,
            "sourceFps": source_fps,
            "targetFps": target_fps,
            "inputFrameCount": input_count,
            "outputFrameCount": output_count,
        }), flush=True)

    finally:
        if args.keep_temp:
            print(f"Keeping temp directory: {work_dir}", flush=True)
        else:
            shutil.rmtree(work_dir, ignore_errors=True)


if __name__ == "__main__":
    main()
