# Vendored C sources — provenance

`ts.wasm` (committed in the parent directory) is built from the C sources in this
directory by `make ts-wasm` (see `build.sh`). It bundles:

- **tree-sitter runtime** (the `*.c`/`*.h` at the root of `csrc/` plus `lib.c`) —
  MIT, https://github.com/tree-sitter/tree-sitter
- **tree-sitter-typescript** grammars (`typescript/typescript/` and
  `typescript/tsx/`) — MIT, https://github.com/tree-sitter/tree-sitter-typescript

`swift.wasm` is built from the same tree-sitter runtime plus:

- **tree-sitter-swift** grammar + C external scanner (`swift/`) — the canonical
  alex-pinkus grammar, MIT, https://github.com/alex-pinkus/tree-sitter-swift
  (pre-generated `parser.c`/`scanner.c` from the `tree-sitter-swift` 0.7.1 npm
  package, ABI 14). Built by `make swift-wasm` (`build-swift.sh`). Rationale: the
  pure-Go gotreesitter Swift grammar cannot parse implicitly-unwrapped optional
  types (`var x: T!`); the canonical grammar does.

These were packaged for a WASM build by https://github.com/malivvan/tree-sitter
(MIT, see `LICENSE`); plumb owns the wazero binding (`../runtime.go`) and the
extractor (`../extractor.go`) and does not depend on that module at runtime.

The two `parser.c` files are large generated parse tables and are stored
gzip-compressed (`parser.c.gz`); `build.sh` decompresses them to a temp dir
before invoking `zig cc`. Only `zig` is needed to regenerate `ts.wasm`; building
or running plumb itself needs only Go + wazero.
