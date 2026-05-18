#!/usr/bin/env python3
import argparse
from pathlib import Path

from PIL import Image
from simple_lama_inpainting import SimpleLama


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True)
    parser.add_argument("--mask", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    image = Image.open(args.input).convert("RGB")
    mask = Image.open(args.mask).convert("L")

    if mask.size != image.size:
        mask = mask.resize(image.size, Image.Resampling.NEAREST)

    simple_lama = SimpleLama()
    result = simple_lama(image, mask)

    Path(args.output).parent.mkdir(parents=True, exist_ok=True)
    result.save(args.output)
    print(f"saved={args.output}")


if __name__ == "__main__":
    main()
