#!/usr/bin/env python3
"""
build-tui-font.py — subset a Nerd Font to just the glyphs the TUI asciicast uses.

asciinema-player renders the cast as live DOM text, so the page needs a web font that
covers everything the recording prints: box-drawing, braille, block elements, geometric
shapes, plus the prompt's Nerd-Font private-use icons and math-bold letters. Shipping a
full Nerd Font woff2 is megabytes; this subsets it to the codepoints actually present in
the cast (plus a few safety ranges) so the asset stays small.

Ligatures are dropped (--layout-features='') — a terminal never ligates.

Requirements on PATH: pyftsubset (pip install fonttools brotli).

The cast is recorded in Sarasa Term CL Nerd Font, so the page must render in the same
face for cell metrics and box-drawing to line up.

Usage:
    python3 scripts/build-tui-font.py \
        --cast site/plumb_tui.cast \
        --font ~/Library/Fonts/sarasa-term-cl-regular-nerd-font.ttf \
        --out site/fonts/sarasa-term-cl-nerd.woff2
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys

CSI = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]")          # CSI ... final byte
OSC = re.compile(r"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)")    # OSC ... BEL / ST
ESC = re.compile(r"\x1b[@-Z\\-_]")                        # lone two-byte escapes


def strip_ansi(s: str) -> str:
    s = OSC.sub("", s)
    s = CSI.sub("", s)
    s = ESC.sub("", s)
    return s


def cast_codepoints(path: str) -> set[int]:
    cps: set[int] = set()
    with open(path, encoding="utf-8") as f:
        for i, ln in enumerate(f):
            ln = ln.strip()
            if not ln:
                continue
            if i == 0:  # header
                continue
            try:
                ev = json.loads(ln)
            except json.JSONDecodeError:
                continue
            if not (isinstance(ev, list) and len(ev) >= 3 and ev[1] == "o"):
                continue
            for ch in strip_ansi(ev[2]):
                o = ord(ch)
                if o >= 0x20 and o != 0x7F:
                    cps.add(o)
    return cps


def safety_ranges() -> set[int]:
    """Whole blocks the TUI draws from, so a minor re-record can't drop a glyph."""
    out: set[int] = set()
    for lo, hi in [
        (0x20, 0x7E),     # ASCII printable
        (0x2500, 0x257F),  # box drawing
        (0x2580, 0x259F),  # block elements
        (0x25A0, 0x25FF),  # geometric shapes (■ ▽ …)
        (0x2800, 0x28FF),  # braille (sparklines)
    ]:
        out.update(range(lo, hi + 1))
    return out


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--cast", default="site/plumb_tui.cast")
    ap.add_argument("--font", default=os.path.expanduser("~/Library/Fonts/sarasa-term-cl-regular-nerd-font.ttf"))
    ap.add_argument("--out", default="site/fonts/sarasa-term-cl-nerd.woff2")
    args = ap.parse_args()

    for p in (args.cast, args.font):
        if not os.path.isfile(p):
            sys.exit(f"build-tui-font: not found: {p}")

    cps = cast_codepoints(args.cast) | safety_ranges()
    os.makedirs(os.path.dirname(args.out), exist_ok=True)
    unicodes = ",".join(f"U+{c:04X}" for c in sorted(cps))
    print(f"build-tui-font: {len(cps)} codepoints from {args.cast}")

    cmd = [
        "pyftsubset", args.font,
        f"--output-file={args.out}",
        "--flavor=woff2",
        f"--unicodes={unicodes}",
        "--layout-features=",      # no ligatures
        "--no-hinting",
        "--desubroutinize",
        "--name-IDs=",
        "--notdef-outline",
        "--recommended-glyphs",
    ]
    subprocess.run(cmd, check=True)
    kb = os.path.getsize(args.out) // 1024
    print(f"build-tui-font: -> {args.out} ({kb} KB)")


if __name__ == "__main__":
    main()
