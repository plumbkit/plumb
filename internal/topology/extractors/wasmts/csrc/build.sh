#!/usr/bin/env bash
# Regenerate ts.wasm from the vendored tree-sitter runtime + tree-sitter-typescript
# grammars. Requires `zig` (provides the wasm32-wasi clang). Output: ../ts.wasm.
#
# Building or running plumb itself needs only Go + wazero — this script is a
# dev-only step, run when the grammar or runtime is updated. See NOTICE.md.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
out="$here/../ts.wasm"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The two parser.c parse tables are stored gzipped (≈8.5 MB each raw). Decompress
# each next to its parser.h/scanner.* so the grammar's local `#include "parser.h"`
# resolves, then compile from the temp tree.
for lang in typescript tsx; do
  mkdir -p "$tmp/$lang"
  cp "$here/typescript/$lang/parser.h" "$here/typescript/$lang/scanner.c" "$here/typescript/$lang/scanner.h" "$tmp/$lang/"
  gunzip -c "$here/typescript/$lang/parser.c.gz" > "$tmp/$lang/parser.c"
done

zig cc --target=wasm32-wasi-musl -mexec-model=reactor -I "$here" \
  "$here/lib.c" \
  "$tmp/typescript/parser.c" "$tmp/typescript/scanner.c" \
  "$tmp/tsx/parser.c" "$tmp/tsx/scanner.c" \
  -o "$out" -Os -fPIC -Wl,--no-entry -Wl,--strip-debug \
  -Wl,--export=malloc -Wl,--export=free -Wl,--export=strlen \
  -Wl,--export=ts_parser_new -Wl,--export=ts_parser_parse_string \
  -Wl,--export=ts_parser_set_language -Wl,--export=ts_parser_delete \
  -Wl,--export=ts_tree_root_node -Wl,--export=ts_tree_delete \
  -Wl,--export=ts_node_child_count -Wl,--export=ts_node_named_child_count \
  -Wl,--export=ts_node_child -Wl,--export=ts_node_named_child \
  -Wl,--export=ts_node_child_by_field_name -Wl,--export=ts_node_type \
  -Wl,--export=ts_node_start_byte -Wl,--export=ts_node_end_byte \
  -Wl,--export=ts_node_is_error -Wl,--export=ts_node_is_named -Wl,--export=ts_node_is_null \
  -Wl,--export=ts_node_string -Wl,--export=ts_language_version \
  -Wl,--export=tree_sitter_typescript -Wl,--export=tree_sitter_tsx

echo "wrote $out ($(wc -c < "$out") bytes)"
