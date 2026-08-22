package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
)

// PackageInfo is one directory-granularity package: every file in Dir that
// declares a package (one KindPackage node per file), collapsed to a single
// entry keyed by directory — the same identity convention linkImports uses
// (packageNodesByDir groups per-file package nodes by path.Dir).
type PackageInfo struct {
	Dir      string
	NumNodes int  // total indexed nodes (any kind) under Dir — the "size" unreachable buckets sort by
	IsMain   bool // true when any file in Dir declares `package main` (Go's entry-point convention)
}

// PackageGraph is the directory-granularity import graph: every indexed
// directory that owns at least one package declaration, and the directory-level
// edges derived from it.
//
// Edges are folded from the two-hop node chain linkImports produces — a
// package node's own imports edge to an import node in the same file
// (extractor-owned), followed by that import node's imports edge to a package
// node in the target directory (import-resolver-owned) — into direct
// Dir -> Dir edges. This is package-level identity by construction: it reuses
// exactly the edges matchImportDir/minImportSegments already validated, so a
// stdlib or third-party import (never linked to a local directory) never
// produces an edge here either.
type PackageGraph struct {
	Dirs  map[string]*PackageInfo
	Edges map[string]map[string]bool // fromDir -> set of toDir it directly imports
}

// LoadPackageGraph builds the full directory-granularity import graph from the
// index: every package directory (with its main-package flag and node count)
// and every directory-level import edge.
func LoadPackageGraph(ctx context.Context, db *sql.DB) (*PackageGraph, error) {
	g := &PackageGraph{Dirs: map[string]*PackageInfo{}, Edges: map[string]map[string]bool{}}
	if err := loadPackageDirs(ctx, db, g); err != nil {
		return nil, err
	}
	if err := loadPackageNodeCounts(ctx, db, g); err != nil {
		return nil, err
	}
	if err := loadPackageEdges(ctx, db, g); err != nil {
		return nil, err
	}
	return g, nil
}

func loadPackageDirs(ctx context.Context, db *sql.DB, g *PackageGraph) error {
	rows, err := db.QueryContext(ctx,
		`SELECT n.name, f.path FROM topology_nodes n
           JOIN topology_files f ON f.id = n.file_id
          WHERE n.kind = ?`, string(KindPackage))
	if err != nil {
		return fmt.Errorf("topology: load package dirs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, p string
		if scanErr := rows.Scan(&name, &p); scanErr != nil {
			return fmt.Errorf("topology: load package dirs: scan: %w", scanErr)
		}
		dir := path.Dir(p)
		info, ok := g.Dirs[dir]
		if !ok {
			info = &PackageInfo{Dir: dir}
			g.Dirs[dir] = info
		}
		if name == "main" {
			info.IsMain = true
		}
	}
	return rows.Err()
}

// loadPackageNodeCounts attaches each package directory's total indexed node
// count (any kind) — the "size" the unreachable bucket sorts by, so the
// biggest (most actionable) dead packages surface first.
func loadPackageNodeCounts(ctx context.Context, db *sql.DB, g *PackageGraph) error {
	rows, err := db.QueryContext(ctx,
		`SELECT f.path, COUNT(*) FROM topology_nodes n
           JOIN topology_files f ON f.id = n.file_id
          GROUP BY f.path`)
	if err != nil {
		return fmt.Errorf("topology: load package node counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var n int
		if scanErr := rows.Scan(&p, &n); scanErr != nil {
			return fmt.Errorf("topology: load package node counts: scan: %w", scanErr)
		}
		if info, ok := g.Dirs[path.Dir(p)]; ok {
			info.NumNodes += n
		}
	}
	return rows.Err()
}

// loadPackageEdges folds the pkg -(imports)-> import -(imports)-> pkg two-hop
// chain into direct Dir -> Dir edges via a single join, rather than by
// re-walking node-level BFS: a depth-capped node BFS (the kind explore.go's
// bfs uses for a symbol neighbourhood) would silently truncate real dependency
// chains deeper than its hard cap, which is a false-negative "unreachable" —
// exactly the graph-correctness bug this feature exists to avoid. A same-
// directory self-loop (a file importing another file whose package node
// resolves to its own directory) is dropped: Go cannot produce one, and for
// languages that could, a directory "importing itself" is not the kind of
// edge reachability or cycle-detection means to report.
func loadPackageEdges(ctx context.Context, db *sql.DB, g *PackageGraph) error {
	rows, err := db.QueryContext(ctx, `
		SELECT ff.path, ft.path
		  FROM topology_edges e1
		  JOIN topology_nodes ni ON ni.id = e1.to_id
		  JOIN topology_edges e2 ON e2.from_id = ni.id
		  JOIN topology_nodes pf ON pf.id = e1.from_id
		  JOIN topology_files ff ON ff.id = pf.file_id
		  JOIN topology_nodes pt ON pt.id = e2.to_id
		  JOIN topology_files ft ON ft.id = pt.file_id
		 WHERE e1.kind = ? AND pf.kind = ? AND ni.kind = ?
		   AND e2.kind = ? AND pt.kind = ?`,
		string(EdgeImports), string(KindPackage), string(KindImport),
		string(EdgeImports), string(KindPackage))
	if err != nil {
		return fmt.Errorf("topology: load package edges: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fromPath, toPath string
		if scanErr := rows.Scan(&fromPath, &toPath); scanErr != nil {
			return fmt.Errorf("topology: load package edges: scan: %w", scanErr)
		}
		from, to := path.Dir(fromPath), path.Dir(toPath)
		if from == to {
			continue
		}
		set, ok := g.Edges[from]
		if !ok {
			set = map[string]bool{}
			g.Edges[from] = set
		}
		set[to] = true
	}
	return rows.Err()
}

// MainDirs returns every directory holding a `package main` declaration,
// sorted for determinism.
func (g *PackageGraph) MainDirs() []string {
	var out []string
	for d, info := range g.Dirs {
		if info.IsMain {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// ResolveDir maps a caller-supplied root string to an indexed directory: an
// exact match first (the common case — the caller passed a real indexed
// directory), then a unique path-suffix match so "cmd/plumb" finds
// "cmd/plumb" and a deeper repo-relative form still resolves. Returns false
// when nothing matches or more than one directory shares the suffix
// (ambiguous — a caller should be more specific rather than have this guess).
func (g *PackageGraph) ResolveDir(root string) (string, bool) {
	if _, ok := g.Dirs[root]; ok {
		return root, true
	}
	var matches []string
	for d := range g.Dirs {
		if hasPathSuffix(d, root) {
			matches = append(matches, d)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return "", false
}

func hasPathSuffix(dir, suffix string) bool {
	if dir == suffix {
		return true
	}
	if len(dir) > len(suffix) && dir[len(dir)-len(suffix)-1] == '/' {
		return dir[len(dir)-len(suffix):] == suffix
	}
	return false
}

// ReachabilityResult is the outcome of a package-level reachability
// traversal outward from a set of root directories.
type ReachabilityResult struct {
	Roots       []string          // resolved root directories actually seeded
	Reachable   map[string]bool   // directory -> reached (includes the roots themselves)
	Predecessor map[string]string // directory -> its predecessor on the BFS tree; roots have no entry
}

// ReachableFrom performs a full (not depth-capped) BFS outward over the
// directory-level import graph from roots. Full closure — not a bounded
// neighbourhood — is required for correctness here: the caller is asking "is
// X reachable at all", and a truncated walk would misreport a genuinely
// reachable package as unreachable (a false negative, the direction this
// feature's recall bias treats as worse than a false positive).
func ReachableFrom(g *PackageGraph, roots []string) *ReachabilityResult {
	res := &ReachabilityResult{
		Reachable:   map[string]bool{},
		Predecessor: map[string]string{},
	}
	var queue []string
	for _, r := range roots {
		if _, ok := g.Dirs[r]; !ok {
			continue
		}
		if res.Reachable[r] {
			continue
		}
		res.Reachable[r] = true
		res.Roots = append(res.Roots, r)
		queue = append(queue, r)
	}
	sort.Strings(res.Roots)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		neighbours := make([]string, 0, len(g.Edges[cur]))
		for nb := range g.Edges[cur] {
			neighbours = append(neighbours, nb)
		}
		sort.Strings(neighbours) // deterministic traversal order → deterministic predecessor tree
		for _, nb := range neighbours {
			if res.Reachable[nb] {
				continue
			}
			res.Reachable[nb] = true
			res.Predecessor[nb] = cur
			queue = append(queue, nb)
		}
	}
	return res
}

// Unreachable returns every package directory in g not marked reachable,
// sorted by NumNodes descending (the actionable ones — a bigger dead package
// is worth more to notice) and then by Dir for determinism.
func (g *PackageGraph) Unreachable(reachable map[string]bool) []*PackageInfo {
	var out []*PackageInfo
	for d, info := range g.Dirs {
		if !reachable[d] {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NumNodes != out[j].NumNodes {
			return out[i].NumNodes > out[j].NumNodes
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

// PathTo reconstructs the shortest root -> target directory chain from a
// ReachableFrom predecessor tree. Returns (nil, false) when target was never
// reached — a legitimate, useful answer (the target has no path from these
// roots), not an error.
func PathTo(res *ReachabilityResult, target string) ([]string, bool) {
	if !res.Reachable[target] {
		return nil, false
	}
	var chain []string
	cur := target
	for {
		chain = append([]string{cur}, chain...)
		p, ok := res.Predecessor[cur]
		if !ok {
			break // cur is a root
		}
		cur = p
	}
	return chain, true
}
