package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_seed.go — turning the caller's files and selectors into seeds: what
// resolves, what is ambiguous, what is nothing, and what is refused outright.

// plumbStateDir is plumb's own state under a workspace root: configuration,
// databases, sessions and the transaction log. It is never a seed.
const plumbStateDir = ".plumb"

// plumbStateReason is why rel cannot be a file seed, or "" when it can. Memories
// reach a pack through the memory corpus, never as a file seed.
func plumbStateReason(rel string) string {
	if rel == plumbStateDir || strings.HasPrefix(rel, plumbStateDir+"/") {
		return "plumb's own state (.plumb/: config, databases, sessions, logs) is never a seed; memories reach a pack only through the memory corpus"
	}
	return ""
}

// seedFile resolves one file seed: it must exist, be a regular file inside the
// root and pass the scope filter. A directory is refused outright. No body is
// read here.
func (c *ContextCollector) seedFile(ctx context.Context, pack *contextPack, scope contextScope, input string) error {
	abs, rel, err := c.locate(ctx, pack.Root, input)
	if err != nil {
		return err
	}
	if rel == "" {
		pack.miss(input, "outside the workspace root")
		return nil
	}
	if reason := plumbStateReason(rel); reason != "" {
		pack.miss(input, reason)
		return nil
	}
	info, statErr := os.Stat(abs)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		pack.miss(input, "file not found")
		return nil
	case statErr != nil:
		pack.miss(input, "cannot be read")
		return nil
	case info.IsDir():
		return badArgument(fmt.Errorf("context_for_task: %q is a directory, and a directory is scope, not a seed. "+
			"Narrow with within, or seed with a file inside it (find_files or workspace_search will name candidates)", input))
	case !info.Mode().IsRegular():
		pack.miss(input, "not a regular file")
		return nil
	}
	if ok, why := scope.allows(rel, corpusOfPath(rel)); !ok {
		pack.miss(input, why)
		return nil
	}
	lang, coverage := fileCoverage(rel)
	pack.Seeds = append(pack.Seeds, contextSeed{Kind: seedFile, Path: rel, Abs: abs, Language: lang, Coverage: coverage})
	return nil
}

// fileCoverage names the file's language and, when the index has no extractor
// for it, says so. An uncovered file has unknown symbols, which is not the same
// as having none.
func fileCoverage(rel string) (language, coverage string) {
	l, ok := langsupport.ByPath(rel)
	switch {
	case !ok:
		return "", "unrecognised file type: no extractor, so symbols and relations are unknown, not absent"
	case l.Structural == langsupport.EngineNone:
		return l.Name, l.Name + " is not covered by an extractor: symbols and relations are unknown, not absent"
	}
	return l.Name, ""
}

// splitSymbolSeed splits "path#Selector" at the first '#'; a bare selector has
// no path. Symbols are code selectors only, so a path that names a document is
// refused with a handoff to corpora.
func splitSymbolSeed(input string) (pathPart, selector string, err error) {
	pathPart, selector, found := strings.Cut(input, "#")
	if !found {
		return "", strings.TrimSpace(input), nil
	}
	pathPart, selector = strings.TrimSpace(pathPart), strings.TrimSpace(selector)
	if selector == "" {
		return "", "", badArgument(fmt.Errorf("context_for_task: symbol %q has no selector after '#'", input))
	}
	if pathPart != "" && isDocsPath(pathPart) {
		return "", "", badArgument(fmt.Errorf("context_for_task: symbol %q names a document, but symbols are code selectors. "+
			"Documents come through corpora: pass a code seed with corpora including \"docs\", or read the section with read_file", input))
	}
	return pathPart, selector, nil
}

// seedSymbol resolves one symbol seed through the topology index. Zero or
// several matches are reported, never resolved by choosing one. The path half of
// path#Selector is exact: the file itself, or everything under it when it names
// a directory, never a path that merely contains the text.
func (c *ContextCollector) seedSymbol(ctx context.Context, pack *contextPack, scope contextScope, index contextIndex, input string) error {
	pathPart, selector, err := splitSymbolSeed(input)
	if err != nil {
		return err
	}
	var hint topology.NodeHint
	if pathPart != "" {
		_, rel, lerr := c.locate(ctx, pack.Root, pathPart)
		if lerr != nil {
			return lerr
		}
		if rel == "" {
			pack.miss(input, "outside the workspace root")
			return nil
		}
		if reason := plumbStateReason(rel); reason != "" {
			pack.miss(input, reason)
			return nil
		}
		if rel != "." {
			hint.Path = rel
		}
	}
	if index.store == nil {
		pack.miss(input, index.unavailable(pack.Root))
		return nil
	}
	nodes, rerr := index.store.ResolveNodes(ctx, selector, hint)
	match, err := classifySymbolNodes(nodes, rerr, scope)
	if err != nil {
		return withIndexHealthErr(index.store, fmt.Errorf("context_for_task: resolving %q: %w", input, err))
	}
	pack.addMatch(input, hint.Path, match)
	return nil
}

type symbolMatchKind int

const (
	matchNone symbolMatchKind = iota
	matchOne
	matchAmbiguous
)

// symbolMatch is the outcome of resolving one selector among what the agent may
// see. The classification itself is topology's (ClassifyNodes, the rule
// ResolveSelector applies); this only carries it with the scope accounting.
type symbolMatch struct {
	kind symbolMatchKind
	// nodes are declarations: the one seed, the ambiguous set, or, for a no-match
	// whose selector exists elsewhere, the labelled candidates.
	nodes []topology.Node
	// shadowed counts the import, package and file nodes a matched declaration
	// outranked; refsOnly counts them when nothing but references matched.
	shadowed, refsOnly int
	// elsewhere marks nodes that matched the selector but not the path hint:
	// they are labelled candidates for a "no match here" answer, never seeds.
	elsewhere bool
	// excluded counts candidates the scope filter removed. Only the count is
	// kept, so nothing about an excluded path can reach the pack.
	excluded int
}

// classifySymbolNodes sorts a ResolveNodes outcome into none, one or ambiguous.
// Document sections are not declarations a selector can seed, and candidates
// outside the scope are dropped (and counted) before classifying, so ambiguity
// is judged among what the agent may see. References (imports, package clauses,
// files) are classified with the rest: they are set aside when a declaration
// matches, and when only they do, the answer is none.
func classifySymbolNodes(nodes []topology.Node, err error, scope contextScope) (symbolMatch, error) {
	var m symbolMatch
	if err != nil {
		mismatch, ok := errors.AsType[*topology.HintMismatchError](err)
		if !ok {
			return symbolMatch{}, err
		}
		nodes, m.elsewhere = mismatch.Candidates, true
	}
	var kept []topology.Node
	for _, n := range nodes {
		if !seedableNode(n) {
			continue
		}
		if ok, _ := scope.allows(n.Path, corpusCode); !ok {
			m.excluded++
			continue
		}
		kept = append(kept, n)
	}
	res := topology.ClassifyNodes(kept)
	m.shadowed = len(res.Shadowed)
	m.nodes = resolutionNodes(res)
	if len(m.nodes) > 0 && topology.IsReference(m.nodes[0].Kind) {
		m.refsOnly, m.nodes = len(m.nodes), nil
	}
	switch {
	case m.elsewhere || m.refsOnly > 0 || res.Kind == topology.ResolutionNone:
		m.kind = matchNone
	case res.Kind == topology.ResolutionOne:
		m.kind = matchOne
	default:
		m.kind = matchAmbiguous
	}
	return m, nil
}

// resolutionNodes is the nodes a resolution names: its one node, its ambiguous
// candidates, or none.
func resolutionNodes(r topology.SelectorResolution) []topology.Node {
	switch r.Kind {
	case topology.ResolutionOne:
		return []topology.Node{r.Node}
	case topology.ResolutionAmbiguous:
		return r.Candidates
	}
	return nil
}

// seedableNode reports whether n is something a symbol selector may name:
// anything but a document section.
func seedableNode(n topology.Node) bool {
	return n.Kind != topology.KindSection && n.Language != "markdown"
}

// nodeSelector is the selector text to paste back into symbols: the qualified
// name when the index has one, else the bare name.
func nodeSelector(n topology.Node) string {
	if n.Qualified != "" {
		return n.Qualified
	}
	return n.Name
}

func candidateOf(n topology.Node) contextCandidate {
	return contextCandidate{Path: n.Path, Selector: nodeSelector(n), NodeKind: string(n.Kind), Line: n.StartLine}
}

// addMatch records a classified outcome on the pack.
func (p *contextPack) addMatch(input, pathHint string, m symbolMatch) {
	switch m.kind {
	case matchOne:
		n := m.nodes[0]
		p.Seeds = append(p.Seeds, contextSeed{
			Kind: seedSymbol, Path: n.Path, Abs: absUnder(p.Root, n.Path), Selector: nodeSelector(n), Name: n.Name,
			NodeKind: string(n.Kind), Line: n.StartLine, EndLine: n.EndLine, Language: n.Language,
			Signature: n.Signature, Doc: firstLine(n.Docstring), Shadowed: m.shadowed,
		})
	case matchAmbiguous:
		reason := fmt.Sprintf("%d declarations match; none was chosen", len(m.nodes))
		if m.shadowed > 0 {
			reason += fmt.Sprintf("; %s also match", referenceNodes(m.shadowed))
		}
		miss := contextMiss{Input: input, Ambiguous: true, Reason: reason}
		miss.Candidates, miss.More = boundedCandidates(m.nodes)
		p.Misses = append(p.Misses, miss)
	default:
		miss := contextMiss{Input: input, Reason: noMatchReason(pathHint, m)}
		miss.Candidates, miss.More = boundedCandidates(m.nodes)
		p.Misses = append(p.Misses, miss)
	}
}

// referenceNodes counts import, package and file nodes the way every line that
// mentions them does.
func referenceNodes(n int) string {
	if n == 1 {
		return "1 import/package/file node"
	}
	return fmt.Sprintf("%d import/package/file nodes", n)
}

// noMatchReason words a none outcome so it cannot be read as a suggestion: the
// candidates under it, if any, are labelled and are not seeds. The text carries
// the caller's path, so the renderer makes it terminal-safe like every other
// piece of caller-influenced text.
func noMatchReason(pathHint string, m symbolMatch) string {
	reason := "no indexed declaration matches; nothing was invented"
	if m.elsewhere && len(m.nodes) > 0 {
		reason = fmt.Sprintf("no declaration matches within %s; the selector exists elsewhere (candidates, not seeds)", pathHint)
	}
	if m.refsOnly > 0 {
		reason += fmt.Sprintf("; only %s match, and a reference is not a declaration", referenceNodes(m.refsOnly))
	}
	if m.excluded > 0 {
		reason += fmt.Sprintf("; %d more excluded by within/corpora", m.excluded)
	}
	return reason
}

func boundedCandidates(nodes []topology.Node) (shown []contextCandidate, more int) {
	for i, n := range nodes {
		if i >= contextMaxCandidates {
			return shown, len(nodes) - contextMaxCandidates
		}
		shown = append(shown, candidateOf(n))
	}
	return shown, 0
}
