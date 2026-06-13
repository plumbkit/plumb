#!/usr/bin/env python3
"""
build-tui-video.py — render a plumb TUI asciicast into light + dark web videos.

Pipeline, per theme:

    asciicast --(colour transform)--> agg (Nerd font, themed) --> GIF --> ffmpeg --> .webm + .mp4

The DARK variant renders the recording's own colours on the site's dark background.
The LIGHT variant lightness-inverts every colour (HSL L -> 1-L, hue/saturation kept)
and renders on the site's light background, so each one blends into its theme on the
page. Box-drawing tiles perfectly because agg rasterises through a real terminal grid
(unlike web-text SVG), and the Nerd font is baked into the frames.

Output (into --out-dir, default site/):
    <name>.webm        <name>.mp4         dark
    <name>_light.webm  <name>_light.mp4   light

Requirements on PATH: agg (brew install agg), ffmpeg. Plus the Nerd font installed
(default "FiraCode Nerd Font Mono"; install the Nerd Fonts cask or pass --font).

Usage:
    python3 scripts/build-tui-video.py            # cast=site/plumb_tui.cast -> site/
    python3 scripts/build-tui-video.py --cast rec.cast --name plumb_tui --out-dir site
"""

from __future__ import annotations

import argparse
import colorsys
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

# ---- defaults (match the site palette in site/index.html :root) ----------------
FONT = "FiraCode Nerd Font Mono"   # must cover box-drawing, braille, blocks, Nerd icons
FONT_SIZE = 24                      # render px; downscaled to SCALE_W for retina sharpness
SPEED = 1.2                         # playback speed-up
IDLE = 2.0                          # cap idle gaps (seconds) so dead time is trimmed
SCALE_W = 1440                      # output width (crisp at the ~960px it displays)
CRF_WEBM = 37
CRF_MP4 = 28
DARK_BG = "121310"                 # site --bg dark
LIGHT_BG = "faf9f5"                # site --bg light
# --------------------------------------------------------------------------------

SGR = re.compile(r"\x1b\[([0-9;]*)m")


def die(msg: str) -> None:
    sys.exit(f"build-tui-video: {msg}")


def inv_rgb(r: int, g: int, b: int) -> tuple[int, int, int]:
    """Lightness inversion: flip L in HSL, keep hue + saturation."""
    h, l, s = colorsys.rgb_to_hls(r / 255, g / 255, b / 255)
    r, g, b = colorsys.hls_to_rgb(h, 1 - l, s)
    clamp = lambda v: max(0, min(255, round(v * 255)))
    return clamp(r), clamp(g), clamp(b)


def inv_hex(hx: str) -> str:
    hx = hx.lstrip("#")
    r, g, b = inv_rgb(int(hx[0:2], 16), int(hx[2:4], 16), int(hx[4:6], 16))
    return f"{r:02x}{g:02x}{b:02x}"


def _fix_sgr(m: re.Match) -> str:
    """Invert any 38;2;r;g;b / 48;2;r;g;b truecolor triples inside one SGR sequence."""
    p = m.group(1).split(";")
    out: list[str] = []
    i = 0
    while i < len(p):
        if p[i] in ("38", "48") and i + 1 < len(p) and p[i + 1] == "2" and i + 4 < len(p):
            r, g, b = inv_rgb(int(p[i + 2] or 0), int(p[i + 3] or 0), int(p[i + 4] or 0))
            out += [p[i], "2", str(r), str(g), str(b)]
            i += 5
            continue
        out.append(p[i])
        i += 1
    return "\x1b[" + ";".join(out) + "m"


def _trailing_incomplete(data: str) -> int:
    """Index where a truncated escape sequence begins at the end of `data`, else -1.

    Casts split SGR sequences across "o" events; agg's VT reassembles them when it
    renders, but a per-event regex would miss them. We carry the fragment forward so
    every sequence is whole before inversion.
    """
    idx = data.rfind("\x1b")
    if idx == -1:
        return -1
    seg = data[idx:]
    if seg == "\x1b":
        return idx
    if seg.startswith("\x1b["):
        return idx if not re.search(r"[@-~]", seg[2:]) else -1  # no CSI final byte yet
    if seg.startswith("\x1b]"):
        return idx if ("\x07" not in seg and "\x1b\\" not in seg[2:]) else -1  # unterminated OSC
    return -1


def read_cast(path: str):
    """Parse asciicast v2 or v3 -> (cols, rows, fg, bg, palette[16], [(abs_t, data)])."""
    with open(path, encoding="utf-8") as f:
        lines = [ln for ln in f if ln.strip()]
    if not lines:
        die(f"empty cast: {path}")
    hdr = json.loads(lines[0])
    ver = hdr.get("version")
    if ver == 3:
        term = hdr.get("term", {})
        cols, rows = term.get("cols"), term.get("rows")
        theme = term.get("theme", {})
        relative = True
    else:  # v1/v2
        cols, rows = hdr.get("width"), hdr.get("height")
        theme = hdr.get("theme", {})
        relative = False
    if not cols or not rows:
        die("cast header missing width/height (cols/rows)")

    fg = (theme.get("fg") or "bbc3d4").lstrip("#")
    bg = (theme.get("bg") or "101315").lstrip("#")
    pal = [c.lstrip("#") for c in (theme.get("palette") or "").split(":") if c]
    if len(pal) < 16:  # fall back to a Nord-ish 16 if the recording carried none
        pal = "191d24:bf616a:a3be8c:ebcb8b:5e81ac:b48ead:8fbcbb:bbc3d4:3b4252:c5727a:b1c89d:efd49f:88c0d0:be9d88:9fc6c5:d8dee9".split(":")
    pal = pal[:16]

    events: list[tuple[float, str]] = []
    t = 0.0
    for ln in lines[1:]:
        ev = json.loads(ln)
        if not (isinstance(ev, list) and len(ev) >= 3 and ev[1] == "o"):
            continue
        t = round(t + float(ev[0]), 6) if relative else float(ev[0])
        events.append((t, ev[2]))
    return cols, rows, fg, bg, pal, events


def write_v2(path: str, cols: int, rows: int, events, invert: bool) -> None:
    """Write an asciicast v2; optionally lightness-invert all truecolor (carry-aware)."""
    out = [json.dumps({"version": 2, "width": cols, "height": rows})]
    pending = ""
    n = len(events)
    for k, (t, data) in enumerate(events):
        data = pending + data
        pending = ""
        if invert:
            cut = _trailing_incomplete(data)
            if cut != -1 and k < n - 1:
                pending, data = data[cut:], data[:cut]
            data = SGR.sub(_fix_sgr, data)
        out.append(json.dumps([t, "o", data]))
    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(out) + "\n")


def run(cmd: list[str], quiet: bool = True) -> None:
    res = subprocess.run(cmd, stdout=subprocess.DEVNULL if quiet else None,
                         stderr=subprocess.DEVNULL if quiet else None)
    if res.returncode != 0:
        die(f"command failed ({res.returncode}): {' '.join(cmd[:3])} ...")


def render_variant(label, cast_v2, bg, fg, palette, gif, out_base, args):
    # agg custom theme order is: background, foreground, then 16 palette colours.
    theme = ",".join([bg, fg, *palette])
    print(f"  [{label}] agg render ({FONT} {args.font_size}px)…", flush=True)
    run(["agg", "--font-family", args.font, "--font-size", str(args.font_size),
         "--theme", theme, "--idle-time-limit", str(args.idle), "--speed", str(args.speed),
         cast_v2, gif])

    vf = f"scale={args.scale}:-2,pad=ceil(iw/2)*2:ceil(ih/2)*2:color=0x{bg}"
    print(f"  [{label}] ffmpeg webm + mp4…", flush=True)
    run(["ffmpeg", "-y", "-i", gif, "-vf", vf, "-c:v", "libvpx-vp9",
         "-crf", str(args.crf_webm), "-b:v", "0", "-pix_fmt", "yuv420p", "-an",
         f"{out_base}.webm"])
    run(["ffmpeg", "-y", "-i", gif, "-vf", vf, "-c:v", "libx264", "-preset", "slow",
         "-crf", str(args.crf_mp4), "-pix_fmt", "yuv420p", "-movflags", "+faststart",
         "-an", f"{out_base}.mp4"])
    for ext in ("webm", "mp4"):
        p = f"{out_base}.{ext}"
        print(f"  [{label}] -> {p}  ({os.path.getsize(p) // 1024} KB)")


def main() -> None:
    ap = argparse.ArgumentParser(description="Render a plumb TUI asciicast into light + dark web videos.")
    ap.add_argument("--cast", default="site/plumb_tui.cast", help="asciicast file (v2 or v3). Default: site/plumb_tui.cast")
    ap.add_argument("--out-dir", default="site", help="output directory. Default: site")
    ap.add_argument("--name", default="plumb_tui", help="output basename. Default: plumb_tui")
    ap.add_argument("--font", default=FONT)
    ap.add_argument("--font-size", type=int, default=FONT_SIZE)
    ap.add_argument("--speed", type=float, default=SPEED)
    ap.add_argument("--idle", type=float, default=IDLE)
    ap.add_argument("--scale", type=int, default=SCALE_W, help="output width in px")
    ap.add_argument("--crf-webm", type=int, default=CRF_WEBM)
    ap.add_argument("--crf-mp4", type=int, default=CRF_MP4)
    ap.add_argument("--dark-bg", default=DARK_BG, help="hex (no #) site dark background")
    ap.add_argument("--light-bg", default=LIGHT_BG, help="hex (no #) site light background")
    ap.add_argument("--only", choices=["dark", "light"], help="render only one variant")
    args = ap.parse_args()

    for tool in ("agg", "ffmpeg"):
        if not shutil.which(tool):
            die(f"'{tool}' not found on PATH. Install it (e.g. `brew install {tool}`).")
    if not os.path.isfile(args.cast):
        die(f"cast not found: {args.cast}")
    os.makedirs(args.out_dir, exist_ok=True)

    cols, rows, fg, bg_rec, pal, events = read_cast(args.cast)
    print(f"build-tui-video: {args.cast} ({cols}x{rows}, {len(events)} frames)")

    with tempfile.TemporaryDirectory() as tmp:
        base = os.path.join(args.out_dir, args.name)

        if args.only != "light":
            cast_d = os.path.join(tmp, "dark.cast")
            write_v2(cast_d, cols, rows, events, invert=False)
            render_variant("dark", cast_d, args.dark_bg, fg, pal,
                           os.path.join(tmp, "dark.gif"), base, args)

        if args.only != "dark":
            cast_l = os.path.join(tmp, "light.cast")
            write_v2(cast_l, cols, rows, events, invert=True)
            light_fg = inv_hex(fg)
            light_pal = [inv_hex(c) for c in pal]
            render_variant("light", cast_l, args.light_bg, light_fg, light_pal,
                           os.path.join(tmp, "light.gif"), f"{base}_light", args)

    print("build-tui-video: done.")


if __name__ == "__main__":
    main()
