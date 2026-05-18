#!/usr/bin/env python3
import argparse
import re
from pathlib import Path

import cv2
import easyocr
import numpy as np


PII_PATTERNS = [
    re.compile(r"\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b", re.I),
    re.compile(r"\b(?:\+?1[-.\s]?)?(?:\(?\d{3}\)?[-.\s]?)\d{3}[-.\s]?\d{4}\b"),
    re.compile(r"\b\d{3}[-\s]?\d{2}[-\s]?\d{4}\b"),
    re.compile(r"\b(?:\d[ -]*?){13,19}\b"),
    re.compile(
        r"\b\d{1,6}\s+[A-Za-z0-9.\s]{2,60}\s+"
        r"(?:Street|St|Avenue|Ave|Road|Rd|Drive|Dr|Lane|Ln|Boulevard|Blvd|Way|Court|Ct|Circle|Cir|Place|Pl)\b",
        re.I,
    ),
]


def should_redact(text: str, detect: str) -> bool:
    value = text.strip()
    if not value:
        return False

    if detect == "all-text":
        return True

    return any(pattern.search(value) for pattern in PII_PATTERNS)


def pixelate(region: np.ndarray, blocks: int = 12) -> np.ndarray:
    h, w = region.shape[:2]
    if h <= 0 or w <= 0:
        return region

    small = cv2.resize(
        region,
        (max(1, w // blocks), max(1, h // blocks)),
        interpolation=cv2.INTER_LINEAR,
    )
    return cv2.resize(small, (w, h), interpolation=cv2.INTER_NEAREST)


def draw_redaction(image: np.ndarray, box, style: str):
    pts = np.array(box, dtype=np.int32)
    x, y, w, h = cv2.boundingRect(pts)

    pad = max(3, int(max(w, h) * 0.08))
    x1 = max(0, x - pad)
    y1 = max(0, y - pad)
    x2 = min(image.shape[1], x + w + pad)
    y2 = min(image.shape[0], y + h + pad)

    region = image[y1:y2, x1:x2]
    if region.size == 0:
        return

    if style == "blackbox":
        cv2.rectangle(image, (x1, y1), (x2, y2), (0, 0, 0), thickness=-1)
    elif style == "pixelate":
        image[y1:y2, x1:x2] = pixelate(region)
    else:
        k = max(11, (max(region.shape[:2]) // 3) | 1)
        image[y1:y2, x1:x2] = cv2.GaussianBlur(region, (k, k), 0)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--languages", default="en")
    parser.add_argument("--detect", choices=["pii", "all-text"], default="pii")
    parser.add_argument("--redaction", choices=["blackbox", "blur", "pixelate"], default="blackbox")
    parser.add_argument("--gpu", action="store_true")
    args = parser.parse_args()

    image = cv2.imread(args.input, cv2.IMREAD_COLOR)
    if image is None:
        raise SystemExit(f"Could not read input image: {args.input}")

    langs = [x.strip() for x in args.languages.split(",") if x.strip()]
    reader = easyocr.Reader(langs, gpu=args.gpu)

    results = reader.readtext(args.input)
    count = 0

    for box, text, conf in results:
        if should_redact(text, args.detect):
            draw_redaction(image, box, args.redaction)
            count += 1

    Path(args.output).parent.mkdir(parents=True, exist_ok=True)
    cv2.imwrite(args.output, image)
    print(f"redactions={count}")


if __name__ == "__main__":
    main()
