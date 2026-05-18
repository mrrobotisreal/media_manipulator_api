#!/usr/bin/env python3
# Reference copy of the runtime face_privacy script.
#
# This file is NOT loaded by the Go API directly — the API shells out to the
# binary configured by AI_FACE_PRIVACY_SCRIPT, which defaults to
# /opt/media-manipulator-ai/scripts/face_privacy.py on the GPU host.
#
# To deploy/update on the server, copy this file over the runtime path, e.g.:
#
#   sudo cp scripts/server/face_privacy.py \
#     /opt/media-manipulator-ai/scripts/face_privacy.py
#
# The script supports two modes used by the API:
#   1. --detect-only --json-out <path>     (POST /api/ai/faces/detect)
#   2. --selection-json <path> --output ...(POST /api/upload final job)
#
# In mode 2 the JSON contains the face boxes the user saw in the preview
# overlay plus their selectionMode / selectedFaceIds, so the runtime reuses
# the same boxes instead of redetecting.
import argparse
import json
from pathlib import Path
from typing import Any, Dict, List, Optional, Sequence, Tuple

import cv2
import numpy as np


# This was previously corrupted somehow, so adding this comment to make a new commit.
FaceBox = Dict[str, Any]


def pixelate(region: np.ndarray, blocks: int = 12) -> np.ndarray:
    h, w = region.shape[:2]
    if h <= 0 or w <= 0:
        return region
    small_w = max(1, w // blocks)
    small_h = max(1, h // blocks)
    temp = cv2.resize(region, (small_w, small_h), interpolation=cv2.INTER_LINEAR)
    return cv2.resize(temp, (w, h), interpolation=cv2.INTER_NEAREST)


def odd_kernel(size: int) -> int:
    size = max(3, size)
    return size if size % 2 == 1 else size + 1


def clamp_box(x1: int, y1: int, x2: int, y2: int, width: int, height: int) -> Tuple[int, int, int, int]:
    return max(0, x1), max(0, y1), min(width, x2), min(height, y2)


def detect_faces(
    image: np.ndarray,
    model_path: str,
    confidence: float,
    padding: float,
) -> List[FaceBox]:
    height, width = image.shape[:2]

    detector = cv2.FaceDetectorYN.create(
        model=model_path,
        config="",
        input_size=(width, height),
        score_threshold=confidence,
        nms_threshold=0.3,
        top_k=5000,
    )

    _, faces = detector.detect(image)
    if faces is None:
        return []

    detected: List[FaceBox] = []
    for index, face in enumerate(faces, start=1):
        x, y, bw, bh = (int(v) for v in face[:4].astype(int))
        score = float(face[-1]) if len(face) >= 15 else float(face[4]) if len(face) > 4 else 0.0

        pad_x = int(bw * padding)
        pad_y = int(bh * padding)

        x1, y1, x2, y2 = clamp_box(
            x - pad_x,
            y - pad_y,
            x + bw + pad_x,
            y + bh + pad_y,
            width,
            height,
        )
        x1, y1, x2, y2 = int(x1), int(y1), int(x2), int(y2)

        if x2 <= x1 or y2 <= y1:
            continue

        detected.append({
            "id": f"face_{index}",
            "index": index,
            "confidence": score,
            "x": x1 / width,
            "y": y1 / height,
            "width": (x2 - x1) / width,
            "height": (y2 - y1) / height,
            "pixelBox": {
                "x": x1,
                "y": y1,
                "width": x2 - x1,
                "height": y2 - y1,
            },
        })

    return detected


def denormalize_face(face: FaceBox, image_width: int, image_height: int) -> Tuple[int, int, int, int]:
    if "pixelBox" in face and isinstance(face["pixelBox"], dict):
        box = face["pixelBox"]
        x = int(round(float(box.get("x", 0))))
        y = int(round(float(box.get("y", 0))))
        w = int(round(float(box.get("width", 0))))
        h = int(round(float(box.get("height", 0))))
        return clamp_box(x, y, x + w, y + h, image_width, image_height)

    x = int(round(float(face.get("x", 0)) * image_width))
    y = int(round(float(face.get("y", 0)) * image_height))
    w = int(round(float(face.get("width", 0)) * image_width))
    h = int(round(float(face.get("height", 0)) * image_height))
    return clamp_box(x, y, x + w, y + h, image_width, image_height)


def load_selection_json(path: Optional[str]) -> Dict[str, Any]:
    if not path:
        return {}
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def normalize_selection_mode(value: str) -> str:
    value = (value or "all").strip().lower()
    valid = {"all", "only_selected", "all_except_selected"}
    if value not in valid:
        raise SystemExit(f"Invalid selection mode: {value}. Expected one of: {', '.join(sorted(valid))}")
    return value


def parse_selected_ids(csv_value: str) -> List[str]:
    if not csv_value:
        return []
    return [item.strip() for item in csv_value.split(",") if item.strip()]


def choose_faces(
    faces: Sequence[FaceBox],
    selection_mode: str,
    selected_face_ids: Sequence[str],
) -> List[FaceBox]:
    selection_mode = normalize_selection_mode(selection_mode)
    selected = set(selected_face_ids or [])

    if selection_mode == "all":
        return list(faces)

    if selection_mode == "only_selected":
        return [face for face in faces if str(face.get("id")) in selected]

    if selection_mode == "all_except_selected":
        return [face for face in faces if str(face.get("id")) not in selected]

    return list(faces)


def apply_effect(image: np.ndarray, face: FaceBox, mode: str, pixel_blocks: int) -> bool:
    image_height, image_width = image.shape[:2]
    x1, y1, x2, y2 = denormalize_face(face, image_width, image_height)

    region = image[y1:y2, x1:x2]
    if region.size == 0:
        return False

    if mode == "blur":
        k = odd_kernel(max(region.shape[:2]) // 3)
        image[y1:y2, x1:x2] = cv2.GaussianBlur(region, (k, k), 0)
    elif mode == "pixelate":
        image[y1:y2, x1:x2] = pixelate(region, pixel_blocks)
    elif mode == "blackbox":
        image[y1:y2, x1:x2] = (0, 0, 0)
    else:
        raise SystemExit(f"Invalid mode: {mode}")

    return True


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True)
    parser.add_argument("--output")
    parser.add_argument("--model", default="/opt/media-manipulator-ai/models/yunet/face_detection_yunet_2023mar.onnx")
    parser.add_argument("--mode", choices=["blur", "pixelate", "blackbox"], default="blur")
    parser.add_argument("--confidence", type=float, default=0.85)
    parser.add_argument("--padding", type=float, default=0.18)
    parser.add_argument("--pixel-blocks", type=int, default=14)

    parser.add_argument("--detect-only", action="store_true")
    parser.add_argument("--json-out")
    parser.add_argument("--faces-json", help="JSON file containing faces to reuse instead of redetecting")
    parser.add_argument("--selection-json", help="JSON file with selectionMode, selectedFaceIds, and optionally faces")
    parser.add_argument("--selection-mode", choices=["all", "only_selected", "all_except_selected"], default="all")
    parser.add_argument("--selected-face-ids", default="", help="Comma-separated face IDs such as face_1,face_3")

    args = parser.parse_args()

    image = cv2.imread(args.input, cv2.IMREAD_COLOR)
    if image is None:
        raise SystemExit(f"Could not read input image: {args.input}")

    image_height, image_width = image.shape[:2]

    selection_json = load_selection_json(args.selection_json)
    selection_mode = normalize_selection_mode(selection_json.get("selectionMode", args.selection_mode))
    selected_face_ids = selection_json.get("selectedFaceIds", parse_selected_ids(args.selected_face_ids))
    if selected_face_ids is None:
        selected_face_ids = []
    if not isinstance(selected_face_ids, list):
        raise SystemExit("selectedFaceIds must be a list")

    faces: List[FaceBox]
    if args.faces_json:
        with open(args.faces_json, "r", encoding="utf-8") as f:
            payload = json.load(f)
        faces = payload.get("faces", payload if isinstance(payload, list) else [])
    elif "faces" in selection_json and isinstance(selection_json["faces"], list):
        faces = selection_json["faces"]
    else:
        faces = detect_faces(image, args.model, args.confidence, args.padding)

    if args.detect_only:
        payload = {
            "imageWidth": image_width,
            "imageHeight": image_height,
            "faces": faces,
        }
        if args.json_out:
            Path(args.json_out).parent.mkdir(parents=True, exist_ok=True)
            with open(args.json_out, "w", encoding="utf-8") as f:
                json.dump(payload, f, indent=2)
            print(args.json_out)
        else:
            print(json.dumps(payload, indent=2))
        return

    if not args.output:
        raise SystemExit("--output is required unless --detect-only is used")

    faces_to_modify = choose_faces(faces, selection_mode, [str(x) for x in selected_face_ids])

    count = 0
    for face in faces_to_modify:
        if apply_effect(image, face, args.mode, args.pixel_blocks):
            count += 1

    Path(args.output).parent.mkdir(parents=True, exist_ok=True)
    cv2.imwrite(args.output, image)

    print(json.dumps({
        "facesDetected": len(faces),
        "facesModified": count,
        "selectionMode": selection_mode,
        "selectedFaceIds": selected_face_ids,
        "output": args.output,
    }))


if __name__ == "__main__":
    main()
