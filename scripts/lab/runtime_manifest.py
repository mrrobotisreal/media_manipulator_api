#!/usr/bin/env python3
"""Runtime manifest of the lab-voice image (MM139): one sha256 over what is installed.

A fresh-install family has no host runtime to fingerprint, so the builder
records, per model, versions.json (repo commit, weight revisions, pip freeze
hash, python/torch) and this script hashes those files together with the
wrapper scripts. The digest becomes the AMI's `runtime-manifest` tag. Stdlib
only (unit-tested without torch).

    runtime_manifest.py --weights DIR --wrappers DIR     -> 64 hex chars on stdout
"""
import argparse
import hashlib
import json
import os
import sys


def compute(weights_dir, wrappers_dir):
    h = hashlib.sha256()
    h.update(b"mm-burst-manifest fresh v1 lab-voice\n")
    models = sorted(d for d in os.listdir(weights_dir) if os.path.isfile(os.path.join(weights_dir, d, "versions.json")))
    if not models:
        raise SystemExit("no versions.json found under %s" % weights_dir)
    for m in models:
        with open(os.path.join(weights_dir, m, "versions.json")) as f:
            canon = json.dumps(json.load(f), sort_keys=True, separators=(",", ":"))
        h.update(("model\t%s\t%s\n" % (m, canon)).encode())
    for name in sorted(os.listdir(wrappers_dir)):
        if name.startswith("voice_") and name.endswith(".py"):
            with open(os.path.join(wrappers_dir, name), "rb") as f:
                h.update(("wrapper\t%s\t%s\n" % (name, hashlib.sha256(f.read()).hexdigest())).encode())
    return h.hexdigest()


def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument("--weights", required=True)
    p.add_argument("--wrappers", required=True)
    a = p.parse_args(argv)
    print(compute(a.weights, a.wrappers))


if __name__ == "__main__":
    sys.exit(main())
