#!/usr/bin/env bash
#
# build-tui-demo.sh — regenerate the website "plumb tui" demo assets from a cast.
#
# Run this AFTER replacing site/plumb_tui.cast with a fresh recording. It re-subsets
# the web font to exactly the glyphs the new cast uses and leaves you a short summary
# of what changed and how to verify before committing.
#
# ── What this rebuilds ───────────────────────────────────────────────────────────
#   site/fonts/sarasa-term-cl-nerd.woff2   (subset of Sarasa Term CL Nerd Font)
#
# That is the ONLY per-cast asset. The asciinema-player bundle (site/asciinema/) is
# version-pinned, and the player wiring in site/index.html is stable — neither needs
# to change when you swap the cast. The rendering contract that wiring encodes (so
# nobody "fixes" it back into the old bugs):
#   • the cast MUST be recorded in Sarasa Term CL Nerd Font — the web font is a subset
#     of that exact face, so the cell metrics and box-drawing line up;
#   • terminalLineHeight is 1.0 so the U+2500–257F border glyphs connect vertically;
#   • .ap-line / .ap-terminal use overflow:visible so the 1.0em line box does not
#     slice the taller text glyphs (or clip the last row);
#   • the terminal background is fully transparent (--term-color-background + every
#     cell bg) so the page shows through in both light and dark themes.
#
# ── Recording a fresh cast ───────────────────────────────────────────────────────
#   1. Set your terminal font to "Sarasa Term CL Nerd Font" (this is non-negotiable).
#   2. Size it to 120×27 to match the existing cast's grid (cols×rows in the header).
#   3. asciinema rec site/plumb_tui.cast   (then drive `plumb tui`, q to stop)
#   4. ./scripts/build-tui-demo.sh
#
# Requirements on PATH: python3, pyftsubset (pip install fonttools brotli).

set -euo pipefail

# repo root = parent of this script's dir, regardless of where it's invoked from
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

CAST="${1:-site/plumb_tui.cast}"
OUT="site/fonts/sarasa-term-cl-nerd.woff2"

# The site card framing (card aspect, asciinema fit:'width') is tuned for this grid.
# A cast recorded at a different size still renders but may not frame the same way.
# Override deliberately with: EXPECTED_COLS=… EXPECTED_ROWS=… ./scripts/build-tui-demo.sh
EXPECTED_COLS="${EXPECTED_COLS:-120}"
EXPECTED_ROWS="${EXPECTED_ROWS:-27}"

# ── locate the Sarasa Term CL Nerd Font file ─────────────────────────────────────
find_font() {
  # 1) the exact regular face we expect
  local known="$HOME/Library/Fonts/sarasa-term-cl-regular-nerd-font.ttf"
  if [ -f "$known" ]; then echo "$known"; return 0; fi
  # 2) ask fontconfig (Linux, or macOS with fontconfig installed)
  if command -v fc-match >/dev/null 2>&1; then
    local p
    p="$(fc-match -f '%{file}' 'Sarasa Term CL Nerd Font:style=Regular' 2>/dev/null || true)"
    if [ -n "$p" ] && [ -f "$p" ] && echo "$p" | grep -qi 'sarasa.*term.*cl'; then
      echo "$p"; return 0
    fi
  fi
  # 3) glob the usual font dirs
  local hit
  hit="$(ls "$HOME/Library/Fonts/"*sarasa*term*cl*regular*nerd* 2>/dev/null | head -1 || true)"
  if [ -n "$hit" ]; then echo "$hit"; return 0; fi
  return 1
}

[ -f "$CAST" ] || { echo "build-tui-demo: cast not found: $CAST" >&2; exit 1; }

# ── validate the cast grid size (asciinema v3 header: .term.cols/.term.rows; v2: width/height)
read -r CAST_COLS CAST_ROWS < <(python3 - "$CAST" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    h = json.loads(f.readline())
term = h.get("term") or {}
cols = term.get("cols", h.get("width"))
rows = term.get("rows", h.get("height"))
print(cols if cols is not None else "?", rows if rows is not None else "?")
PY
)
echo "build-tui-demo: cast grid = ${CAST_COLS}x${CAST_ROWS} (expected ${EXPECTED_COLS}x${EXPECTED_ROWS})"
if [ "$CAST_COLS" != "$EXPECTED_COLS" ] || [ "$CAST_ROWS" != "$EXPECTED_ROWS" ]; then
  echo "build-tui-demo: WARNING — cast is ${CAST_COLS}x${CAST_ROWS}, not the expected ${EXPECTED_COLS}x${EXPECTED_ROWS}." >&2
  echo "  The site card is tuned for ${EXPECTED_COLS}x${EXPECTED_ROWS}; a different grid will still render" >&2
  echo "  but may not frame the same. Re-record at ${EXPECTED_COLS}x${EXPECTED_ROWS}, or set EXPECTED_COLS/" >&2
  echo "  EXPECTED_ROWS if you intend the new size." >&2
fi

FONT="$(find_font)" || {
  echo "build-tui-demo: 'Sarasa Term CL Nerd Font' (Regular) not found." >&2
  echo "  Install it (e.g. the Sarasa Nerd Fonts release) so the web font can be" >&2
  echo "  subset from the same face the cast is recorded in, then re-run." >&2
  exit 1
}

echo "build-tui-demo: cast = $CAST"
echo "build-tui-demo: font = $FONT"

before=""
[ -f "$OUT" ] && before="$(shasum -a 256 "$OUT" | cut -d' ' -f1)"

python3 scripts/build-tui-font.py --cast "$CAST" --font "$FONT" --out "$OUT"

after="$(shasum -a 256 "$OUT" | cut -d' ' -f1)"
size="$(( $(wc -c < "$OUT") / 1024 ))"

echo
if [ "$before" = "$after" ]; then
  echo "build-tui-demo: $OUT unchanged (${size} KB) — the cast uses the same glyph set."
else
  echo "build-tui-demo: rebuilt $OUT (${size} KB)."
fi
echo
echo "Next:"
echo "  • preview:  (cd site && python3 -m http.server 8731)  then open http://localhost:8731/"
echo "             (serve over http — asciinema fetches the .cast; file:// is blocked by CORS)"
echo "  • commit :  git add site/plumb_tui.cast $OUT && git commit"
