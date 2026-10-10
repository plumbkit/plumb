package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_body.go — bodies for symbol seeds, sliced from ONE snapshot of each
// file (Invariant 5).
//
// The index records a line span for every declaration, but the file may have
// changed since it was indexed, and a span applied to newer bytes slices the
// wrong lines while looking authoritative. So the span is only trusted when the
// index parsed exactly the bytes of the snapshot (the index's content hash equals
// the snapshot's); otherwise the declaration is found again by re-extracting from
// the snapshot's own bytes, which needs no second read of the file. If neither
// yields exactly one declaration, the body is withheld with a precise reason and
// a body is never sliced from a stale span.

// contextSourceReadFactor bounds the source read for one call at this multiple
// of max_bytes, so a tiny budget never reads a huge file only to discard it.
const contextSourceReadFactor = 4

type bodyState int

const (
	bodyNone        bodyState = iota // no body was attempted (a file seed)
	bodyReady                        // Text is the declaration's source in the snapshot
	bodyUnavailable                  // Why says why no body exists
)

// bodyFile is the one snapshot every body from a file was sliced from. MTime and
// SHA are that snapshot's own; they are what a read record and an edit guard
// must carry, and they are never taken from the index.
type bodyFile struct {
	Path  string
	Abs   string
	MTime time.Time
	SHA   string // SHA-256 of the whole snapshot
	Size  int64
	// Changed marks a snapshot that differs from what the index parsed.
	Changed bool
}

// contextBody is one symbol seed's body, or the reason it has none. Text is the
// declaration's source lines as the snapshot holds them (newline-terminated, CR
// stripped, no line-number gutter), and SHA is the SHA-256 of exactly Text.
type contextBody struct {
	Seed       int // index into pack.Seeds
	File       int // index into pack.Files; -1 when no snapshot was taken
	State      bodyState
	Why        string
	Start, End int // 1-based inclusive line span in the snapshot
	Text       string
	SHA        string
	// Signature and Doc describe the declaration as the snapshot has it when the
	// span was re-validated, and as the index has it otherwise.
	Signature, Doc string
}

// fileGroup is the symbol seeds that share one file.
type fileGroup struct {
	path, abs string
	seeds     []int // indices into pack.Seeds
}

func symbolGroups(seeds []contextSeed) []fileGroup {
	var groups []fileGroup
	at := map[string]int{}
	for i, s := range seeds {
		if s.Kind != seedSymbol {
			continue
		}
		g, ok := at[s.Path]
		if !ok {
			g = len(groups)
			at[s.Path] = g
			groups = append(groups, fileGroup{path: s.Path, abs: s.Abs})
		}
		groups[g].seeds = append(groups[g].seeds, i)
	}
	return groups
}

// sourceBudget is the cumulative source-read allowance for one call.
type sourceBudget struct{ spent, limit int64 }

func (b *sourceBudget) take(n int64) bool {
	if b.spent+n > b.limit {
		return false
	}
	b.spent += n
	return true
}

// collectBodies slices a body for every symbol seed, reading each file once. It
// returns the gaps the bodies imply (a file that changed since it was indexed).
func (c *ContextCollector) collectBodies(ctx context.Context, pack *contextPack, store *topology.Store) []string {
	pack.Bodies = make([]contextBody, len(pack.Seeds))
	budget := &sourceBudget{limit: int64(contextSourceReadFactor) * int64(pack.MaxBytes)}
	var gaps []string
	for _, g := range symbolGroups(pack.Seeds) {
		src, why := openBodySource(ctx, g, store, budget)
		if src == nil {
			for _, i := range g.seeds {
				seed := pack.Seeds[i]
				pack.Bodies[i] = contextBody{
					Seed: i, File: -1, State: bodyUnavailable, Why: why,
					Signature: seed.Signature, Doc: seed.Doc,
				}
			}
			continue
		}
		fileIdx := len(pack.Files)
		pack.Files = append(pack.Files, bodyFile{
			Path: g.path, Abs: g.abs, MTime: src.snap.mtime, SHA: src.snap.sha, Size: src.snap.size, Changed: !src.trusted,
		})
		for _, i := range g.seeds {
			pack.Bodies[i] = src.slice(ctx, pack.Seeds[i], i, fileIdx)
		}
		if !src.trusted {
			gaps = append(gaps, fmt.Sprintf("%s changed since it was indexed: its bodies come from the current snapshot, "+
				"but relationships and line numbers the index holds may be stale", g.path))
		}
	}
	return gaps
}

// bodySource is one file's snapshot while its bodies are sliced.
type bodySource struct {
	path    string
	store   *topology.Store
	lines   []string
	snap    fileSnapshot
	trusted bool // the index parsed exactly these bytes, so its spans describe them

	fresh     []topology.Node // declarations re-extracted from the snapshot, once
	freshDone bool
	freshErr  bool
}

// openBodySource takes the file's one snapshot and decides whether the index's
// spans are true of it. A nil result carries the reason no body can be sliced.
func openBodySource(ctx context.Context, g fileGroup, store *topology.Store, budget *sourceBudget) (*bodySource, string) {
	info, err := os.Stat(g.abs)
	if err != nil {
		return nil, "the file cannot be read"
	}
	if !budget.take(info.Size()) {
		return nil, fmt.Sprintf("source-read cap reached (%d B, %dx max_bytes); the file was not read", budget.limit, contextSourceReadFactor)
	}
	lines, snap, err := snapshotLines(g.abs)
	if err != nil {
		return nil, "the file cannot be read"
	}
	src := &bodySource{path: g.path, store: store, lines: lines, snap: snap}
	if store != nil {
		hash, ok, herr := store.IndexedContentHash(ctx, g.path)
		src.trusted = herr == nil && ok && hash == snap.sha
	}
	return src, ""
}

// slice returns the body for one symbol seed. A trusted span is used after a
// cheap sanity check (the narrow window between resolving the seed and
// snapshotting the file could still have moved it); anything else is found again
// from the snapshot's own bytes.
func (s *bodySource) slice(ctx context.Context, seed contextSeed, seedIdx, fileIdx int) contextBody {
	b := contextBody{Seed: seedIdx, File: fileIdx, Start: seed.Line, End: seed.EndLine, Signature: seed.Signature, Doc: seed.Doc}
	text, ok := sliceLines(s.lines, b.Start, b.End)
	if !s.trusted || !ok || !strings.Contains(text, seed.Name) {
		node, why := s.revalidate(ctx, seed)
		if why != "" {
			b.State, b.Why = bodyUnavailable, why
			return b
		}
		b.Start, b.End, b.Signature, b.Doc = node.StartLine, node.EndLine, node.Signature, firstLine(node.Docstring)
		if text, ok = sliceLines(s.lines, b.Start, b.End); !ok {
			b.State, b.Why = bodyUnavailable, "stale span: the declaration's lines lie outside the current file"
			return b
		}
	}
	if !utf8.ValidString(text) {
		b.State, b.Why = bodyUnavailable, "the body is not valid UTF-8 text, so a hash of it would not describe what a client receives"
		return b
	}
	sum := sha256.Sum256([]byte(text))
	b.State, b.Text, b.SHA = bodyReady, text, hex.EncodeToString(sum[:])
	return b
}

// revalidate finds seed's declaration again in the snapshot's own bytes. It
// answers with exactly one node, or the precise reason it cannot.
func (s *bodySource) revalidate(ctx context.Context, seed contextSeed) (topology.Node, string) {
	nodes, ok := s.freshNodes(ctx)
	if !ok {
		return topology.Node{}, "stale span: the file changed since it was indexed and its declarations cannot be re-read from the snapshot"
	}
	var match []topology.Node
	for _, n := range nodes {
		if nodeSelector(n) == seed.Selector && string(n.Kind) == seed.NodeKind {
			match = append(match, n)
		}
	}
	switch len(match) {
	case 1:
		return match[0], ""
	case 0:
		return topology.Node{}, "stale span: the declaration is no longer in the current file (the index is out of date)"
	}
	return topology.Node{}, "stale span: the declaration appears more than once in the current file, so its span cannot be chosen"
}

// freshNodes parses the snapshot's bytes with the registered extractor, once. The
// bytes are rebuilt from the lines the snapshot was split into, which is exact:
// Split and Join on "\n" are inverses.
func (s *bodySource) freshNodes(ctx context.Context) ([]topology.Node, bool) {
	if !s.freshDone {
		s.freshDone = true
		if s.store != nil {
			nodes, err := s.store.ExtractSource(ctx, s.path, []byte(strings.Join(s.lines, "\n")))
			s.fresh, s.freshErr = nodes, err != nil || len(nodes) == 0
		} else {
			s.freshErr = true
		}
	}
	return s.fresh, !s.freshErr
}

// sliceLines returns lines start..end (1-based, inclusive) as the source text
// they hold: each line newline-terminated with a trailing CR stripped, as
// read_symbol prints them. ok is false for a span outside the snapshot.
func sliceLines(lines []string, start, end int) (string, bool) {
	if start < 1 || end < start || end > len(lines) {
		return "", false
	}
	var sb strings.Builder
	for _, l := range lines[start-1 : end] {
		sb.WriteString(strings.TrimSuffix(l, "\r"))
		sb.WriteByte('\n')
	}
	return sb.String(), true
}
