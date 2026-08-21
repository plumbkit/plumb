#!/usr/bin/env python3
"""
measure-use-cases.py — regenerate the numbers behind docs/use-cases.md.

docs/use-cases.md publishes head-to-head measurements of plumb's tools against
the tools a coding agent already has (ripgrep, grep, whole-file reads, "run the
whole suite"). Hand-measured numbers rot silently: files get split, tools change
their output, and the page keeps asserting figures nobody has re-checked. This
script re-derives every number in one run so the page can be refreshed instead
of trusted.

How it measures:

    native side   real subprocesses — `rg`, `/usr/bin/grep`, `wc -c`. The byte
                  count is what the command actually writes to stdout.
    plumb side    a real MCP session. It spawns `plumb serve`, speaks JSON-RPC
                  over stdio, and measures the exact response payload the client
                  would put in its context window.

Both sides are therefore *measured payload bytes*, which is the only thing the
page claims. Token figures use ~4 characters per token, the same rough average
plumb uses internally; they are a conversion of the measurement, not a second
measurement.

Two deliberate choices:

  - `/usr/bin/grep` by absolute path. Several agent harnesses install a `grep`
    shim on PATH that quietly respects .gitignore. Measuring through one turns
    the grep-vs-ripgrep comparison into ripgrep-vs-ripgrep and erases the very
    difference the page is about.
  - Latency is sampled against an already-warm daemon, and the warm-up calls are
    discarded. Cold-start numbers measure the language server booting, not the
    tool.

Usage:
    python3 scripts/measure-use-cases.py                # human-readable report
    python3 scripts/measure-use-cases.py --json         # machine-readable
    python3 scripts/measure-use-cases.py --latency-runs 200

Requires: a built ./plumb binary (`make build`), `rg` on PATH, Python 3.11+.
"""

from __future__ import annotations

import argparse
import json
import math
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path
from statistics import median

ROOT = Path(__file__).resolve().parent.parent
PLUMB = ROOT / "plumb"
CHARS_PER_TOKEN = 4

# The samples every scenario is measured against. Kept here, not inline, so that
# a file being split or renamed is a one-line fix rather than a hunt.
SAMPLE_FILE = "internal/cli/stats.go"
# Three symbols from one file, small to large. A single sample makes the
# read_symbol ratio look like a property of the tool; it is really a property of
# how much of the file you needed, so measure the spread and publish the spread.
SAMPLE_SYMBOLS = ["axisCell", "parseAge", "runStats"]
SAMPLE_SYMBOL = SAMPLE_SYMBOLS[1]
SEARCH_TERM = "FormatSavings"
REFERENCE_SYMBOL = "FormatSavings"
REFERENCE_FILE = "internal/stats/savings.go"
MULTI_READ_FILES = [
    "internal/stats/savings.go",
    "internal/web/ui/src/lib/format.js",
    "testdata/python-fixture/main.py",
]
CROSS_LANGUAGE = [
    ("Python", "scripts/build-blog.py", "load_posts"),
    ("JavaScript", "internal/web/ui/src/lib/charts.js", "activityCalendar"),
]


def tokens(nbytes: int) -> int:
    return round(nbytes / CHARS_PER_TOKEN)


def must_search(pattern: str, text: str, what: str, flags: int = 0) -> re.Match:
    """re.search that refuses to fail quietly.

    Every published figure that came from parsing a tool response used to
    degrade to None or 0 when the response format shifted, which reads exactly
    like a real measurement. A changed format should stop the run, not silently
    republish a wrong number.
    """
    m = re.search(pattern, text, flags)
    if m is None:
        raise RuntimeError(
            f"could not parse {what} from the tool response — the output format "
            f"probably changed. Pattern: {pattern!r}\nResponse begins:\n{text[:400]}"
        )
    return m


def symbol_line_span(payload: str) -> int:
    """Line count of a symbol, taken from read_symbol's own header.

    read_symbol states its span ("# symbol: runStats (Function) lines 55-171").
    Deriving the count from the next declaration's start line instead — the
    obvious shortcut — overcounts, because it swallows the blank lines and the
    following symbol's doc comment.
    """
    m = must_search(
        r"^# symbol:.*? lines (\d+)[–-](\d+)",
        payload,
        "symbol line span",
        re.MULTILINE,
    )
    return int(m.group(2)) - int(m.group(1)) + 1


def run(cmd: list[str], cwd: Path = ROOT) -> tuple[str, int]:
    """Run a command, returning (stdout, exit status). stderr is discarded."""
    proc = subprocess.run(
        cmd, cwd=cwd, capture_output=True, text=True, errors="replace"
    )
    return proc.stdout, proc.returncode


def measure_command(cmd: list[str]) -> dict:
    """Byte and line count of what a command writes to stdout."""
    out, _ = run(cmd)
    return {
        "command": " ".join(cmd),
        "lines": len(out.splitlines()),
        "bytes": len(out.encode()),
        "tokens": tokens(len(out.encode())),
    }


class Serve:
    """A live `plumb serve` MCP session, measured over stdio JSON-RPC."""

    def __init__(self) -> None:
        self.transcript: list[tuple[str, dict, str]] = []
        self.proc = subprocess.Popen(
            [str(PLUMB), "serve", "--workspace", str(ROOT)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            bufsize=1,
        )
        self._id = 0

    def _send(self, obj: dict) -> None:
        assert self.proc.stdin is not None
        self.proc.stdin.write(json.dumps(obj) + "\n")
        self.proc.stdin.flush()

    def _recv(self, want: int, timeout: float = 180.0) -> dict:
        assert self.proc.stdout is not None
        deadline = time.time() + timeout
        while time.time() < deadline:
            line = self.proc.stdout.readline()
            if not line:
                raise RuntimeError("plumb serve closed stdout")
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue  # banner or log noise, not a JSON-RPC frame
            if msg.get("id") == want:
                return msg
        raise RuntimeError(f"timeout waiting for response id={want}")

    def __enter__(self) -> "Serve":
        self._id += 1
        self._send(
            {
                "jsonrpc": "2.0",
                "id": self._id,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {},
                    "clientInfo": {"name": "measure-use-cases", "version": "1"},
                },
            }
        )
        self._recv(self._id)
        self._send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        self.call("session_start", {})  # attach + warm the workspace
        return self

    def __exit__(self, *_exc) -> None:
        try:
            if self.proc.stdin is not None:
                self.proc.stdin.close()
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()

    def call(self, tool: str, args: dict) -> tuple[str, float]:
        """Call a tool; return (response payload text, elapsed milliseconds)."""
        self._id += 1
        rid = self._id
        start = time.perf_counter()
        self._send(
            {
                "jsonrpc": "2.0",
                "id": rid,
                "method": "tools/call",
                "params": {"name": tool, "arguments": args},
            }
        )
        msg = self._recv(rid)
        elapsed = (time.perf_counter() - start) * 1000
        if "error" in msg:
            raise RuntimeError(f"{tool}: {json.dumps(msg['error'])[:400]}")
        result = msg.get("result") or {}
        text = "".join(part.get("text", "") for part in result.get("content", []))
        return text, elapsed

    def measure(self, tool: str, args: dict) -> dict:
        text, elapsed = self.call(tool, args)
        self.transcript.append((tool, args, text))
        nbytes = len(text.encode())
        return {
            "tool": tool,
            "bytes": nbytes,
            "tokens": tokens(nbytes),
            "ms": round(elapsed, 1),
            "lines": len(text.splitlines()),
            "text": text,
        }


def file_bytes(rel: str) -> int:
    return (ROOT / rel).stat().st_size


def gutter_bytes(rel: str) -> int:
    """Bytes of a file as a real agent read tool would hand it over.

    "Native read" measured with `wc -c` is a baseline no agent actually gets.
    Claude Code's own Read prefixes every line with a line number, exactly as
    plumb's read_file does. Charging plumb for that framing while giving the
    native side raw bytes understates plumb in every read scenario at once, so
    both baselines are measured and both are published.

    The gutter modelled here is the cheapest honest one: the line number right
    aligned to the width of the largest, then a tab — which is what plumb emits.
    Padding to a fixed six columns (`cat -n`) would overcharge the native side
    and flatter plumb instead, which is the same error in the other direction.
    """
    data = (ROOT / rel).read_bytes()
    lines = len(data.splitlines()) or 1
    width = len(str(lines))
    return len(data) + lines * (width + 1)


def provenance() -> dict:
    commit, _ = run(["git", "rev-parse", "--short=8", "HEAD"])
    dirty, _ = run(["git", "status", "--porcelain"])
    version, _ = run([str(PLUMB), "version"])
    # `plumb version` prints an ANSI banner first; the version is the line that
    # starts with the program name. Matching on "has a digit" picks the banner,
    # because the ANSI escapes contain digits.
    ver = next(
        (ln.strip() for ln in version.splitlines() if ln.startswith("plumb ")),
        "unknown",
    )
    return {
        "commit": commit.strip(),
        "clean": not dirty.strip(),
        "plumb_version": ver,
        "platform": sys.platform,
        "date": time.strftime("%Y-%m-%d"),
    }


def scenario_search(s: Serve) -> dict:
    """Text search: ripgrep vs real grep vs plumb, same query.

    Measured with include_enclosing_symbol both off and on, because they are
    different trades and quoting one tool's payload beside the other's feature
    would be a comparison of two calls that never happened. The flag defaults to
    false: the annotation is not free and is not on unless you ask.
    """
    rg = measure_command(["rg", "-n", SEARCH_TERM])
    grep = measure_command(["/usr/bin/grep", "-rn", SEARCH_TERM, "."])
    plain = s.measure("search_in_files", {"pattern": SEARCH_TERM})
    annotated = s.measure(
        "search_in_files",
        {"pattern": SEARCH_TERM, "include_enclosing_symbol": True},
    )
    return {
        "ripgrep": rg,
        "grep": grep,
        "plumb": plain,
        "plumb_annotated": annotated,
        "annotations": annotated["text"].count("[in:"),
        "annotations_when_off": plain["text"].count("[in:"),
    }


def scenario_checkout_noise() -> dict:
    """How much of the working checkout is not the repository."""
    tracked, _ = run(["git", "ls-files"])
    tracked_bytes = sum(
        (ROOT / p).stat().st_size
        for p in tracked.split("\n")
        if p and (ROOT / p).is_file()
    )
    worktree_bytes = 0
    for path in ROOT.rglob("*"):
        try:
            if path.is_file() and not path.is_symlink():
                worktree_bytes += path.stat().st_size
        except OSError:
            continue
    ignored, _ = run(["git", "status", "--ignored", "--porcelain", "-s"])
    ignored_paths = [
        ln[3:].strip() for ln in ignored.splitlines() if ln.startswith("!!")
    ]
    return {
        "tracked_bytes": tracked_bytes,
        "worktree_bytes": worktree_bytes,
        "ratio": round(worktree_bytes / tracked_bytes, 1) if tracked_bytes else 0,
        "ignored_paths": ignored_paths,
    }


def scenario_read_symbol(s: Serve) -> dict:
    whole = file_bytes(SAMPLE_FILE)
    whole_real = gutter_bytes(SAMPLE_FILE)
    samples = []
    for name in SAMPLE_SYMBOLS:
        sym = s.measure("read_symbol", {"path": SAMPLE_FILE, "name": name})
        samples.append(
            {
                "symbol": name,
                "lines": symbol_line_span(sym["text"]),
                "bytes": sym["bytes"],
                "tokens": sym["tokens"],
                "ms": sym["ms"],
                "ratio": round(whole / sym["bytes"], 1) if sym["bytes"] else 0,
                "ratio_real": (
                    round(whole_real / sym["bytes"], 1) if sym["bytes"] else 0
                ),
            }
        )
    return {
        "file": SAMPLE_FILE,
        "whole_file_bytes": whole,
        "whole_file_tokens": tokens(whole),
        "whole_file_gutter_bytes": whole_real,
        "whole_file_gutter_tokens": tokens(whole_real),
        "samples": samples,
    }


def scenario_outline(s: Serve) -> dict:
    whole = file_bytes(SAMPLE_FILE)
    whole_real = gutter_bytes(SAMPLE_FILE)
    out = s.measure("file_outline", {"uri": SAMPLE_FILE})
    return {
        "file": SAMPLE_FILE,
        "whole_file_bytes": whole,
        "whole_file_tokens": tokens(whole),
        "whole_file_gutter_bytes": whole_real,
        "plumb": out,
        "ratio": round(whole / out["bytes"], 1) if out["bytes"] else 0,
        "ratio_real": round(whole_real / out["bytes"], 1) if out["bytes"] else 0,
    }


def scenario_references(s: Serve) -> dict:
    rg = measure_command(["rg", "-n", REFERENCE_SYMBOL])
    refs = s.measure(
        "find_references",
        {"uri": REFERENCE_FILE, "symbol_name": REFERENCE_SYMBOL},
    )
    rg_files, _ = run(["rg", "-l", REFERENCE_SYMBOL])
    text = refs["text"]
    # A reference is a position, not a line: two calls on one line are two
    # references but one grep hit. Compare like with like by also counting the
    # distinct file:line pairs, which is the unit ripgrep reports.
    positions = re.findall(r"^(/\S+?):(\d+):(\d+)\t", text, re.MULTILINE)
    lines = {(f, ln) for f, ln, _ in positions}
    return {
        "ripgrep_matches": rg["lines"],
        "ripgrep_files": len([f for f in rg_files.splitlines() if f]),
        "reference_positions": len(positions),
        "reference_lines": len(lines),
        "reference_files": len({f for f, _ in lines}),
        "plumb": refs,
    }


def scenario_multi_read(s: Serve) -> dict:
    native_bytes = sum(file_bytes(f) for f in MULTI_READ_FILES)
    native_gutter_bytes = sum(gutter_bytes(f) for f in MULTI_READ_FILES)
    multi = s.measure("read_multiple_files", {"paths": MULTI_READ_FILES})
    singles = [s.measure("read_file", {"file_path": f}) for f in MULTI_READ_FILES]
    return {
        "files": MULTI_READ_FILES,
        "native_bytes": native_bytes,
        "native_gutter_bytes": native_gutter_bytes,
        "native_turns": len(MULTI_READ_FILES),
        "plumb": multi,
        "plumb_turns": 1,
        "sum_of_singles_bytes": sum(x["bytes"] for x in singles),
        "sum_of_singles_ms": round(sum(x["ms"] for x in singles), 1),
    }


def scenario_rename(s: Serve) -> dict:
    """rename_symbol (dry run) vs what a naive find-replace would touch."""
    naive_lines = measure_command(["rg", "-n", "-w", REFERENCE_SYMBOL])
    naive_files, _ = run(["rg", "-l", "-w", REFERENCE_SYMBOL])
    rename = s.measure(
        "rename_symbol",
        {
            "uri": REFERENCE_FILE,
            "symbol_name": REFERENCE_SYMBOL,
            "new_name": REFERENCE_SYMBOL + "Renamed",
            "dry_run": True,
        },
    )
    m = must_search(
        r"across (\d+) file\(s\), (\d+) edit\(s\)", rename["text"], "rename edit counts"
    )
    return {
        "naive_matches": naive_lines["lines"],
        "naive_files": len([f for f in naive_files.splitlines() if f]),
        "rename_files": int(m.group(1)),
        "rename_edits": int(m.group(2)),
        "plumb": rename,
    }


def scenario_affected(s: Serve) -> dict:
    """Which tests to run after an edit, vs running the whole suite.

    max_results is set well above the expected answer on purpose. The default of
    50 truncates an answer of ~1000. The response does mark it — a final
    "[truncated: max_results reached]" line, which the `truncated` field below
    checks for — but the list above that marker reads as complete, and because
    every co-located hit carries the same 0.5 confidence the cut falls in path
    order rather than by relevance, which dropped the one test that actually
    exercises the changed function. A truncated list read as a complete one is
    exactly the wrong number to publish.
    """
    affected = s.measure(
        "topology_affected", {"files": [REFERENCE_FILE], "max_results": 2000}
    )
    pkgs, _ = run(["go", "list", "./..."])
    all_pkgs = [p for p in pkgs.splitlines() if p]
    test_files, _ = run(["git", "ls-files", "*_test.go"])
    total_tests, _ = run(
        ["git", "grep", "-c", "-h", "^func Test", "--", "*_test.go"]
    )
    text = affected["text"]
    # The recall question that matters: is the test for the changed function in
    # the list at all? If not, the tool has not answered "which tests to run".
    own_test = "savings_test.go" in text
    # The tool answers in packages, one row each with its test count. Parse that
    # rather than counting per-test lines: importing packages are summarised, so
    # per-test lines exist only for the changed package and counting them would
    # report a fraction of the real total as if it were all of it.
    #
    # The row LABEL is deliberately not pinned to a command shape. It used to be
    # matched as `go test ./<pkg>/...`, which silently stopped matching anything
    # the moment topology_affected became language-aware (PLAN-378) and dropped
    # the hardcoded `go test ` prefix — this scenario then reported "0 tests in 0
    # packages" with no error, which reads as a tool regression rather than a
    # broken parser. Anchor on the stable part of the row (the count and reason)
    # and treat the label as opaque.
    pkg_rows = re.findall(r"^ {2}(\S+)\s+(\d+) tests\s{2,}(.+)$", text, re.M)
    if not pkg_rows and "run these packages" in text:
        raise RuntimeError(
            "topology_affected returned packages but no row matched the parser — "
            "its output format changed. Fix this regex rather than publishing a "
            f"zero.\n{text[:1500]}"
        )
    selected_pkgs = [pkg_name(label) for label, _, _ in pkg_rows]
    selected_tests = sum(int(n) for _, n, _ in pkg_rows)
    reasons = {pkg_name(label): r.strip() for label, _, r in pkg_rows}
    return {
        "changed": REFERENCE_FILE,
        "total_packages": len(all_pkgs),
        "total_test_files": len([f for f in test_files.splitlines() if f]),
        "total_test_funcs": sum(int(n) for n in total_tests.split() if n.isdigit()),
        "selected_tests": selected_tests,
        "selected_packages": selected_pkgs,
        "package_reasons": reasons,
        "truncated": "max_results reached" in text,
        "includes_own_test": own_test,
        "plumb": affected,
    }


def pkg_name(label: str) -> str:
    """Reduce a run-target label to the package path it names.

    The label is whatever the workspace's test runner takes — `./internal/x/...`
    for go, a bare path for python/pytest, or just the directory when no command
    could be inferred. All of them reduce to the same package path for reporting.
    """
    return label.removeprefix("./").removesuffix("/...").removesuffix("/")


def scenario_latency(s: Serve, runs: int) -> dict:
    """Warm-daemon round-trip latency for the cheapest useful call."""
    for _ in range(5):  # discard warm-up
        s.call("read_file", {"file_path": REFERENCE_FILE})
    samples = []
    for _ in range(runs):
        _, ms = s.call("read_file", {"file_path": REFERENCE_FILE})
        samples.append(ms)
    samples.sort()
    # Nearest-rank percentile: ceil(p * n), 1-indexed. Truncating instead is
    # off by one whenever p*n is not an integer.
    rank = max(1, math.ceil(0.95 * len(samples)))
    return {
        "runs": runs,
        "p50_ms": round(median(samples), 2),
        "p95_ms": round(samples[rank - 1], 2),
        "max_ms": round(samples[-1], 2),
    }


def scenario_cross_language(s: Serve) -> list[dict]:
    out = []
    for lang, rel, symbol in CROSS_LANGUAGE:
        whole = file_bytes(rel)
        whole_real = gutter_bytes(rel)
        sym = s.measure("read_symbol", {"path": rel, "name": symbol})
        outline = s.measure("file_outline", {"uri": rel})
        out.append(
            {
                "language": lang,
                "file": rel,
                "symbol": symbol,
                "symbol_lines": symbol_line_span(sym["text"]),
                "whole_file_bytes": whole,
                "whole_file_tokens": tokens(whole),
                "whole_file_gutter_bytes": whole_real,
                "read_symbol": sym,
                "file_outline": outline,
                "symbol_ratio": round(whole / sym["bytes"], 1) if sym["bytes"] else 0,
                "symbol_ratio_real": (
                    round(whole_real / sym["bytes"], 1) if sym["bytes"] else 0
                ),
                "outline_ratio": (
                    round(whole / outline["bytes"], 1) if outline["bytes"] else 0
                ),
                "outline_ratio_real": (
                    round(whole_real / outline["bytes"], 1) if outline["bytes"] else 0
                ),
            }
        )
    return out


def strip_text(obj):
    """Drop captured payloads from the JSON dump — they are large and noisy."""
    if isinstance(obj, dict):
        return {k: strip_text(v) for k, v in obj.items() if k != "text"}
    if isinstance(obj, list):
        return [strip_text(v) for v in obj]
    return obj


def report(data: dict) -> None:
    p = data["provenance"]
    print(f"# use-cases measurements — {p['commit']} ({p['date']})")
    print(f"  {p['plumb_version']} | {p['platform']} | clean={p['clean']}\n")

    s = data["search"]
    print("## Text search")
    print(f"  rg           {s['ripgrep']['lines']:>5} matches  {s['ripgrep']['bytes']:>8,} B  ~{s['ripgrep']['tokens']:,} tok")
    print(f"  /usr/bin/grep{s['grep']['lines']:>5} matches  {s['grep']['bytes']:>8,} B  ~{s['grep']['tokens']:,} tok")
    print(f"  search_in_files            {s['plumb']['bytes']:>8,} B  ~{s['plumb']['tokens']:,} tok  ({s['plumb']['ms']} ms)")
    print(f"    + enclosing symbol       {s['plumb_annotated']['bytes']:>8,} B  ~{s['plumb_annotated']['tokens']:,} tok  ({s['plumb_annotated']['ms']} ms)")
    print(f"    annotations: {s['annotations']} with the flag on, {s['annotations_when_off']} with it off (it defaults to off)\n")

    n = data["checkout_noise"]
    print("## Working checkout vs repository")
    print(f"  tracked   {n['tracked_bytes']/1e6:>8.1f} MB")
    print(f"  worktree  {n['worktree_bytes']/1e6:>8.1f} MB   ({n['ratio']}x)")
    print(f"  ignored:  {', '.join(n['ignored_paths'][:8])}\n")

    r = data["read_symbol"]
    print(f"## Read one function ({r['file']})")
    print(f"  whole file, raw       {r['whole_file_bytes']:>8,} B  ~{r['whole_file_tokens']:,} tok")
    print(f"  whole file, w/ gutter {r['whole_file_gutter_bytes']:>8,} B  ~{r['whole_file_gutter_tokens']:,} tok   <- what a real read tool returns")
    for smp in r["samples"]:
        print(f"  read_symbol {smp['symbol']:<10}{smp['bytes']:>8,} B  ({smp['lines']:>3} lines)  -> {smp['ratio']}x vs raw, {smp['ratio_real']}x vs real")
    print()

    o = data["outline"]
    print("## File shape")
    print(f"  whole file, raw       {o['whole_file_bytes']:>8,} B  ~{o['whole_file_tokens']:,} tok")
    print(f"  whole file, w/ gutter {o['whole_file_gutter_bytes']:>8,} B")
    print(f"  file_outline          {o['plumb']['bytes']:>8,} B  ~{o['plumb']['tokens']:,} tok  -> {o['ratio']}x vs raw, {o['ratio_real']}x vs real\n")

    f = data["references"]
    print("## References")
    print(f"  rg               {f['ripgrep_matches']} matching lines across {f['ripgrep_files']} files")
    print(f"  find_references  {f['reference_positions']} positions on {f['reference_lines']} lines across {f['reference_files']} files ({f['plumb']['ms']} ms)\n")

    m = data["multi_read"]
    n, g = m["native_bytes"], m["native_gutter_bytes"]
    singles, multi = m["sum_of_singles_bytes"], m["plumb"]["bytes"]
    print("## Batched reads")
    print(f"  {m['native_turns']} native reads, raw     {n:>8,} B  {n / g:>5.2f}x  in {m['native_turns']} turns")
    print(f"  {m['native_turns']} native reads, gutter  {g:>8,} B   1.00x  in {m['native_turns']} turns  <- the real baseline")
    print(f"  {m['native_turns']}x read_file            {singles:>8,} B  {singles / g:>5.2f}x  in {m['native_turns']} turns")
    print(f"  1x read_multiple_files  {multi:>8,} B  {multi / g:>5.2f}x  in 1 turn ({m['plumb']['ms']} ms)")
    print(f"  plumb's per-read provenance header costs {singles - g:,} B; batching adds {multi - singles:,} B more\n")

    rn = data["rename"]
    print("## Rename")
    print(f"  naive find-replace  {rn['naive_matches']} whole-word matches across {rn['naive_files']} files")
    print(f"  rename_symbol       {rn['rename_edits']} edits across {rn['rename_files']} files ({rn['plumb']['ms']} ms)\n")

    a = data["affected"]
    print("## Which tests to run")
    print(f"  whole suite       {a['total_packages']} packages / {a['total_test_files']} test files / {a['total_test_funcs']} test funcs")
    print(f"  topology_affected {a['selected_tests']} tests in {len(a['selected_packages'])} packages, {a['plumb']['bytes']:,} B ({a['plumb']['ms']} ms)")
    for pkg in a["selected_packages"]:
        print(f"                    {pkg:<24} {a['package_reasons'][pkg]}")
    print(f"  truncated={a['truncated']}  includes the changed file's own test={a['includes_own_test']}\n")

    lat = data["latency"]
    print("## Warm latency (read_file)")
    print(f"  n={lat['runs']}  p50 {lat['p50_ms']} ms  p95 {lat['p95_ms']} ms  max {lat['max_ms']} ms\n")

    print("## Cross-language")
    for c in data["cross_language"]:
        print(f"  {c['language']:<11} {c['file']}")
        print(f"    whole file raw {c['whole_file_bytes']:>7,} B / gutter {c['whole_file_gutter_bytes']:>7,} B")
        print(f"    read_symbol {c['symbol']} ({c['symbol_lines']} lines) {c['read_symbol']['bytes']:>6,} B -> {c['symbol_ratio']}x raw, {c['symbol_ratio_real']}x real")
        print(f"    file_outline {c['file_outline']['bytes']:>6,} B -> {c['outline_ratio']}x raw, {c['outline_ratio_real']}x real")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--json", action="store_true", help="emit JSON instead of a report")
    ap.add_argument("--latency-runs", type=int, default=200)
    ap.add_argument(
        "--dump",
        metavar="DIR",
        help="also write every measured tool payload here, so a surprising "
        "number can be checked against the response it came from",
    )
    args = ap.parse_args()

    if not PLUMB.exists():
        sys.exit(f"no plumb binary at {PLUMB} — run `make build` first")
    for tool in ("rg", "go"):
        if shutil.which(tool) is None:
            sys.exit(f"{tool} not found on PATH")
    if not Path("/usr/bin/grep").exists():
        sys.exit("/usr/bin/grep not found — see the module docstring on grep shims")

    data: dict = {"provenance": provenance()}
    data["checkout_noise"] = scenario_checkout_noise()
    with Serve() as s:
        data["search"] = scenario_search(s)
        data["read_symbol"] = scenario_read_symbol(s)
        data["outline"] = scenario_outline(s)
        data["references"] = scenario_references(s)
        data["multi_read"] = scenario_multi_read(s)
        data["rename"] = scenario_rename(s)
        data["affected"] = scenario_affected(s)
        data["latency"] = scenario_latency(s, args.latency_runs)
        data["cross_language"] = scenario_cross_language(s)
        transcript = list(s.transcript)

    if args.dump:
        dest = Path(args.dump)
        dest.mkdir(parents=True, exist_ok=True)
        for i, (tool, call_args, text) in enumerate(transcript):
            name = f"{i:02d}-{tool}.txt"
            (dest / name).write_text(f"# args: {json.dumps(call_args)}\n\n{text}")
        print(f"wrote {len(transcript)} payloads to {dest}\n", file=sys.stderr)

    if args.json:
        print(json.dumps(strip_text(data), indent=2))
    else:
        report(data)


if __name__ == "__main__":
    main()
