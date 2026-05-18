#!/usr/bin/env python3
import argparse
import cv2
import numpy as np
from pathlib import Path

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

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--model", default="/opt/media-manipulator-ai/models/yunet/face_detection_yunet_2023mar.onnx")
    parser.add_argument("--mode", choices=["blur", "pixelate", "blackbox"], default="blur")
    parser.add_argument("--confidence", type=float, default=0.85)
    parser.add_argument("--padding", type=float, default=0.18)
    parser.add_argument("--pixel-blocks", type=int, default=14)
    args = parser.parse_args()

    image = cv2.imread(args.input, cv2.IMREAD_COLOR)
    if image is None:
        raise SystemExit(f"Could not read input image: {args.input}")

    h, w = image.shape[:2]
    detector = cv2.FaceDetectorYN.create(
        model=args.model,
        config="",
        input_size=(w, h),
        score_threshold=args.confidence,
        nms_threshold=0.3,
        top_k=5000,
    )

    _, faces = detector.detect(image)
    if faces is None:
        Path(args.output).parent.mkdir(parents=True, exist_ok=True)
        cv2.imwrite(args.output, image)
        print("faces=0")
        return

    count = 0
    for face in faces:
        x, y, bw, bh = face[:4].astype(int)

        pad_x = int(bw * args.padding)
        pad_y = int(bh * args.padding)

        x1 = max(0, x - pad_x)
        y1 = max(0, y - pad_y)
        x2 = min(w, x + bw + pad_x)
        y2 = min(h, y + bh + pad_y)

        region = image[y1:y2, x1:x2]
        if region.size == 0:
            continue

        if args.mode == "blur":
            k = odd_kernel(max(region.shape[:2]) // 3)
            image[y1:y2, x1:x2] = cv2.GaussianBlur(region, (k, k), 0)
        elif args.mode == "pixelate":
            image[y1:y2, x1:x2] = pixelate(region, args.pixel_blocks)
        else:
            image[y1:y2, x1:x2] = (0, 0, 0)

        count += 1

    Path(args.output).parent.mkdir(parents=True, exist_ok=True)
    cv2.imwrite(args.output, image)
    print(f"faces={count}")

if __name__ == "__main__":
    main()
