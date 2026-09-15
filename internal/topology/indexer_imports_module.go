package topology

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// indexer_imports_module.go resolves a Go import through the module path its
// own repository declares, which is the difference between knowing where an
// import points and guessing.
//
// matchImportDir (indexer_imports.go) matches the longest SUFFIX of an import
// path that names an indexed directory, with a segment-count floor standing in
// for "did this look like it had a module prefix". That floor is a good proxy
// and nothing more: it cannot tell `github.com/boltdb/store` from
// `example.com/m/store` when the repository has a top-level store/, it lets a
// stdlib path of three segments reach a local directory sharing its tail
// (net/http/httptest → httptest/), and it refuses a one-segment dotless module
// path (`module myapp`) because that is indistinguishable from a stdlib root.
//
// go.mod removes the guessing for Go. An import either starts with a module
// path this repository declares — in which case the remainder IS its directory,
// exactly, relative to that module's own root — or it does not, in which case it
// is stdlib or third-party and names no local package at all. No suffix, no
// floor, no collision surface.
//
// Two deliberate properties:
//
//   - The module set comes from the INDEX, not from a filesystem walk. go.mod
//     already has a topology_files row (every walked file does), so this is one
//     query plus a handful of small reads, it costs nothing on the scoped
//     rebuild path that never walks, and it cannot disagree with what the
//     indexer actually saw.
//   - A workspace with no declared module leaves every import undecided, and
//     the suffix matcher answers as it does today. That is what keeps this safe:
//     the failure mode of module resolution is "I don't know", never "no local
//     package exists", so a repository whose go.mod is excluded from the index
//     loses no edges it has now.

// goModule is one module directive the index has seen: where its go.mod sits,
// and what it calls itself.
type goModule struct {
	dir  string // workspace-relative directory holding go.mod; "." at the root
	path string // the module directive's path, e.g. github.com/plumbkit/plumb
}

// maxGoModBytes bounds one go.mod read. A module file is a few hundred bytes to
// a few tens of kilobytes; anything larger is not one, and reading it would put
// an unbounded file read on the link pass.
const maxGoModBytes = 1 << 20

// goModulesInIndex returns every module declared by a go.mod the index already
// holds, LONGEST PATH FIRST so a nested module wins over the parent that
// contains it — `example.com/m/sub` must claim `example.com/m/sub/pkg` before
// `example.com/m` gets the chance to map it to a directory that does not exist.
//
// Advisory in the same way the rest of this pass is: a go.mod that cannot be
// read or parsed is skipped with a debug line rather than failing the link. The
// cost of skipping one is that its imports fall back to suffix matching — the
// behaviour they had before this existed.
func goModulesInIndex(ctx context.Context, tx *sql.Tx, workspace string) []goModule {
	rows, err := tx.QueryContext(ctx,
		`SELECT path FROM topology_files WHERE path = 'go.mod' OR path LIKE '%/go.mod'`)
	if err != nil {
		slog.Debug("topology: link imports: go.mod lookup failed", "err", err)
		return nil
	}
	defer rows.Close()

	var out []goModule
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			slog.Debug("topology: link imports: go.mod scan failed", "err", err)
			return nil
		}
		modPath := readModulePath(filepath.Join(workspace, filepath.FromSlash(rel)))
		if modPath == "" {
			continue
		}
		out = append(out, goModule{dir: path.Dir(rel), path: modPath})
	}
	if err := rows.Err(); err != nil {
		slog.Debug("topology: link imports: go.mod rows failed", "err", err)
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].path) != len(out[j].path) {
			return len(out[i].path) > len(out[j].path)
		}
		return out[i].path < out[j].path // stable for equal lengths
	})
	return out
}

// readModulePath reads one go.mod and returns its module path, or "".
func readModulePath(abs string) string {
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() || info.Size() > maxGoModBytes {
		return ""
	}
	src, err := os.ReadFile(abs) //nolint:gosec // G304: abs is a workspace path the indexer already walked
	if err != nil {
		slog.Debug("topology: link imports: go.mod unreadable", "path", abs, "err", err)
		return ""
	}
	return parseModulePath(string(src))
}

// parseModulePath extracts the module directive's path from go.mod source.
//
// Hand-parsed rather than taken from golang.org/x/mod: this needs one directive
// out of a file the indexer has already read once, and the module line is the
// one piece of go.mod syntax that has never changed. A dependency for it would
// buy nothing and bind the topology layer to the module toolchain's release
// cycle.
//
// Handles the forms that occur: a leading comment block, an inline comment after
// the path, and the quoted spelling the grammar permits. Returns "" for a file
// with no module directive, which is how a go.work or a malformed file lands
// here.
func parseModulePath(src string) string {
	for line := range strings.Lines(src) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		rest, ok := strings.CutPrefix(line, "module")
		if !ok || (rest != "" && !isSpace(rest[0])) {
			continue
		}
		if i := strings.Index(rest, "//"); i >= 0 {
			rest = rest[:i]
		}
		return strings.Trim(strings.TrimSpace(rest), `"`)
	}
	return ""
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }

// resolveGoImport maps a Go import path to the workspace-relative directory it
// names, using the declared modules.
//
// The three outcomes are distinct and the caller must keep them apart:
//
//	dir, true  — a module claims this import; dir is where it points (which may
//	             still hold no indexed package, and then nothing links)
//	"",  true  — modules are known and none claims it: stdlib or third-party,
//	             and it names no local package. REFUSE, do not fall back.
//	"",  false — no module is known here. Undecided; the suffix matcher answers.
//
// Collapsing the middle case into the last one is what the whole change is
// about: falling back there is precisely how a third-party import reaches a
// local directory that happens to share its tail.
func resolveGoImport(qualified string, mods []goModule) (string, bool) {
	if len(mods) == 0 {
		return "", false
	}
	cleaned := strings.Trim(path.Clean(strings.TrimSpace(qualified)), "/")
	if cleaned == "" || cleaned == "." {
		return "", true
	}
	for _, m := range mods {
		rest, ok := withinModule(cleaned, m.path)
		if !ok {
			continue
		}
		return path.Join(m.dir, rest), true
	}
	return "", true
}

// withinModule reports whether an import path lies inside a module, and returns
// the part of it below the module root. The module path itself resolves to the
// module root (rest ""), because a module may declare a package there.
//
// The separator check is what stops `example.com/mtools` matching module
// `example.com/m`: a prefix test alone would claim it, and the remainder
// ("ools") would name a directory nothing put there.
func withinModule(importPath, modulePath string) (string, bool) {
	if importPath == modulePath {
		return "", true
	}
	if rest, ok := strings.CutPrefix(importPath, modulePath+"/"); ok {
		return rest, true
	}
	return "", false
}

// describeModules renders the resolved module set for a debug line.
func describeModules(mods []goModule) string {
	parts := make([]string, 0, len(mods))
	for _, m := range mods {
		parts = append(parts, fmt.Sprintf("%s=%s", m.dir, m.path))
	}
	return strings.Join(parts, " ")
}
