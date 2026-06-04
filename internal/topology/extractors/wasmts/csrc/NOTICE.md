# Vendored C sources — provenance

`ts.wasm` (committed in the parent directory) is built from the C sources in this
directory by `make ts-wasm` (see `build.sh`). It bundles:

- **tree-sitter runtime** (the `*.c`/`*.h` at the root of `csrc/` plus `lib.c`) —
  MIT, https://github.com/tree-sitter/tree-sitter
- **tree-sitter-typescript** grammars (`typescript/typescript/` and
  `typescript/tsx/`) — MIT, https://github.com/tree-sitter/tree-sitter-typescript

These were packaged for a WASM build by https://github.com/malivvan/tree-sitter
(MIT, see `LICENSE`); plumb owns the wazero binding (`../runtime.go`) and the
extractor (`../extractor.go`) and does not depend on that module at runtime.

The two `parser.c` files are large generated parse tables and are stored
gzip-compressed (`parser.c.gz`); `build.sh` decompresses them to a temp dir
before invoking `zig cc`. Only `zig` is needed to regenerate `ts.wasm`; building
or running plumb itself needs only Go + wazero.
