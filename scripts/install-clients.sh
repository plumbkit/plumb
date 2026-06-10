#!/usr/bin/env bash
# install-clients.sh — install the MCP client CLIs exercised by the on-demand
# clientsmoke harness (cmd/clientsmoke). Idempotent: a client already on PATH is
# skipped. This installs the CLIs only — it never configures API keys. After
# this, run `make clients-test` (connection tier) or, with keys exported,
# `make clients-test-auth` (auth tier).
#
# Prerequisites: Node 20+ (npm) for the npm clients; Python 3.8+ (pip) for
# hermes; curl for the script-installed clients. Missing prerequisites are
# reported, not fatal — the harness skips any client whose binary is absent.
set -u

installed=()
skipped=()
failed=()
missing_prereq=()

have() { command -v "$1" >/dev/null 2>&1; }

# install <binary> <kind> <payload...>
#   kind=npm  payload=<package>          → npm install -g <package>
#   kind=pip  payload=<package>          → pip install --user <package>
#   kind=sh   payload=<install command>  → run as given (curl|bash style)
install() {
  local bin="$1" kind="$2"; shift 2
  if have "$bin"; then
    skipped+=("$bin"); echo "✓ $bin already installed — skipping"; return
  fi
  case "$kind" in
    npm)
      if ! have npm; then missing_prereq+=("$bin (needs npm/Node 20+)"); echo "⚠ $bin: npm not found"; return; fi
      echo "→ installing $bin via npm ($1)"; npm install -g "$1" ;;
    pip)
      local pipbin=""; for c in pip3 pip; do have "$c" && { pipbin="$c"; break; }; done
      if [ -z "$pipbin" ]; then missing_prereq+=("$bin (needs pip/Python 3.8+)"); echo "⚠ $bin: pip not found"; return; fi
      echo "→ installing $bin via $pipbin ($1)"; "$pipbin" install --user "$1" ;;
    sh)
      if ! have curl; then missing_prereq+=("$bin (needs curl)"); echo "⚠ $bin: curl not found"; return; fi
      echo "→ installing $bin via install script"; bash -c "$1" ;;
  esac
  if have "$bin"; then installed+=("$bin"); else failed+=("$bin"); echo "✗ $bin still not on PATH after install"; fi
}

echo "Installing MCP client CLIs for the clientsmoke harness…"
echo

# npm-distributed
install gemini  npm "@google/gemini-cli"
install qwen    npm "@qwen-code/qwen-code"
install codex   npm "@openai/codex"
install auggie  npm "@augmentcode/auggie"
install crush   npm "@charmland/crush"
install claude  npm "@anthropic-ai/claude-code"

# pip-distributed. The [mcp] extra pulls the Python MCP SDK hermes needs to
# actually speak to a stdio MCP server (without it, `hermes mcp test` fails).
install hermes  pip "hermes-agent[mcp]"

# script-distributed
install opencode     sh "curl -fsSL https://opencode.ai/install | bash"
install goose        sh "curl -fsSL https://github.com/block/goose/releases/download/stable/download_cli.sh | CONFIGURE=false bash"
install cursor-agent sh "curl https://cursor.com/install -fsS | bash"

echo
echo "──────── summary ────────"
echo "installed:      ${installed[*]:-none}"
echo "already present: ${skipped[*]:-none}"
[ ${#failed[@]}        -gt 0 ] && echo "failed:         ${failed[*]}"
[ ${#missing_prereq[@]} -gt 0 ] && echo "missing prereqs: ${missing_prereq[*]}"
echo
echo "Some script-installed CLIs (opencode, goose, cursor-agent) install into"
echo "~/.local/bin or ~/.opencode/bin — open a new shell or update PATH if a"
echo "binary isn't found. Then: make clients-test"

# Non-fatal: the harness skips any client that isn't on PATH.
exit 0
