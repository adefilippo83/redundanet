#!/usr/bin/env python3
"""Synthetic backup source for the scale test: many small files in many
directories, every file unique (random content, so convergent encryption
never deduplicates two of them) and above Tahoe's LIT threshold, so each
one becomes a CHK upload with real shares.

    gen_tree.py OUT --files 20000 --dirs 2000          # create
    gen_tree.py OUT --churn-percent 1                   # rewrite 1% of the files

Directories are a two-level tree (about sqrt(dirs) top-level directories,
each with the rest below), and files are spread round-robin over the leaf
directories. A churn pass rewrites every k-th file with new content of the
same size, which is what ``tahoe backup`` sees as a changed file.
"""

from __future__ import annotations

import argparse
import math
import os
import random
import sys
from pathlib import Path


def leaf_dirs(out: Path, dirs: int) -> list[Path]:
    top = max(1, math.isqrt(dirs))
    per_top = max(1, math.ceil(dirs / top))
    leaves: list[Path] = []
    for t in range(top):
        for s in range(per_top):
            if len(leaves) >= dirs:
                break
            leaves.append(out / f"d{t:03d}" / f"s{s:03d}")
    return leaves


def create(out: Path, files: int, dirs: int, min_bytes: int, max_bytes: int, seed: int) -> None:
    rng = random.Random(seed)  # noqa: S311 - sizes, not secrets
    leaves = leaf_dirs(out, dirs)
    for leaf in leaves:
        leaf.mkdir(parents=True, exist_ok=True)
    for i in range(files):
        size = rng.randint(min_bytes, max_bytes)
        (leaves[i % len(leaves)] / f"f{i:07d}.bin").write_bytes(os.urandom(size))
        if i and i % 10000 == 0:
            print(f"  {i}/{files} files", flush=True)
    print(f"created {files} files in {len(leaves)} directories under {out}")


def churn(out: Path, percent: float) -> None:
    every = max(1, round(100 / percent))
    changed = 0
    for n, path in enumerate(sorted(out.rglob("f*.bin"))):
        if n % every:
            continue
        path.write_bytes(os.urandom(path.stat().st_size))
        changed += 1
    print(f"rewrote {changed} files ({percent}%) under {out}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("out", type=Path)
    parser.add_argument("--files", type=int)
    parser.add_argument("--dirs", type=int, default=1000)
    parser.add_argument("--min-bytes", type=int, default=200)  # LIT caps stop at 55 bytes
    parser.add_argument("--max-bytes", type=int, default=4096)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--churn-percent", type=float)
    args = parser.parse_args()
    if args.churn_percent is not None:
        churn(args.out, args.churn_percent)
    elif args.files:
        create(args.out, args.files, args.dirs, args.min_bytes, args.max_bytes, args.seed)
    else:
        parser.error("--files N to create, or --churn-percent P to rewrite")
    return 0


if __name__ == "__main__":
    sys.exit(main())
