#!/usr/bin/env bash
# Regenerate swift.wasm from the vendored tree-sitter runtime + the canonical
# alex-pinkus/tree-sitter-swift grammar (0.7.1, ABI14). Requires `zig` (provides
# the wasm32-wasi clang). Output: ../swift.wasm.
#
# Building or running plumb itself needs only Go + wazero — this script is a
# dev-only step, run when the grammar or runtime is updated. See NOTICE.md.
#
# Why WASM for Swift: the pure-Go gotreesitter port cannot reduce an implicitly-
# unwrapped optional type (`var x: T!`) — it emits an ERROR that collapses the
# enclosing type. The canonical grammar + its canonical C external scanner parse
# it correctly, so we run them via wazero.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
out="$here/../swift.wasm"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# parser.c is stored gzipped (~18 MB raw). Decompress next to the grammar's
# tree_sitter/parser.h + scanner.c so its `#include "tree_sitter/parser.h"`
# resolves, then compile from the temp tree.
mkdir -p "$tmp/swift/tree_sitter"
cp "$here/swift/scanner.c" "$tmp/swift/scanner.c"
cp "$here/swift/tree_sitter/parser.h" "$tmp/swift/tree_sitter/parser.h"
gunzip -c "$here/swift/parser.c.gz" > "$tmp/swift/parser.c"

zig cc --target=wasm32-wasi-musl -mexec-model=reactor -I "$here" -I "$tmp/swift" \
  "$here/lib.c" \
  "$tmp/swift/parser.c" "$tmp/swift/scanner.c" \
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
  -Wl,--export=tree_sitter_swift

echo "wrote $out ($(wc -c < "$out") bytes)"
