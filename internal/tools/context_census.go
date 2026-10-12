package tools

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_census.go — whether a seed's caller list is the whole list.
//
// A ranked caller list never lets a reader stop: "3 callers, more may exist" sends
// an agent back to grep for the name, and the pack is paid for twice. The index
// records every call expression and, per call, what the resolver made of it, so it
// can say which calls of the seed's name are still open. None open means every
// recorded call of the name resolved, to this declaration or another, and the
// list is complete as far as calls go; some open means exactly those sites are what
// is left to check, and the pack names them.
//
// What it cannot see is said with the claim: a function used as a value, and a
// method called through an interface, are not calls of the declaration.

// contextCensusSites is how many open sites a seed's line names.
const contextCensusSites = 6

// census fills c from the index's per-call outcomes for a Go function or method
// seed. It leaves c alone for anything else, or when the index cannot answer, and
// the older wording stands.
func (e *expander) census(ctx context.Context, n topology.Node, c *callerNote) {
	// Asked afresh rather than through the walk's cache: a walk cut by its deadline
	// caches "not admitted" for the nodes it never reached.
	if n.Language != "go" || !e.store.DerivedCallsAdmittedFor(ctx, n.ID) {
		return
	}
	open, _, err := e.store.OpenCallSites(ctx, n.Name, "go", 0)
	if err != nil {
		e.fail(ctx, err)
		return
	}
	res, err := e.store.ImpactFrom(ctx, n, contextTraversalOpts(true, []string{string(topology.EdgeCalls)}))
	if err != nil {
		e.fail(ctx, err)
		return
	}
	var ok bool
	if c.Indexed, c.IndexedOutside, ok = callerCounts(res.DependedOnBy, n.ID, e.inScope); !ok {
		return
	}
	c.Census, c.Method, c.Name = true, n.Kind == topology.KindMethod, n.Name
	c.Unclaimable = e.censusDoubt(ctx)
	seed := censusSeed{method: c.Method, exported: isExportedName(n.Name), dir: path.Dir(n.Path), pkg: e.packageName(ctx, n.Path)}
	for _, o := range open {
		if !couldCall(o, seed) {
			continue
		}
		if !e.inScope(o.Path) {
			c.OpenOutside++ // counted, never named: the scope leaves the file out
			continue
		}
		noteOpen(c, fmt.Sprintf("%s:%d (%s)", o.Path, o.Line, o.Kind))
	}
}

// inScope is the scope filter without its side effect of recording an exclusion:
// a census counts what lies outside, it does not list it.
func (e *expander) inScope(p string) bool {
	if plumbStateReason(p) != "" {
		return false
	}
	ok, _ := e.scope.allows(p, corpusOfPath(p))
	return ok
}

// censusDoubt says why no caller list may be called complete however the census
// comes out, or "" when nothing stands in the way. A call in a file the index did
// not record is invisible to it: the index is behind the files (not fresh), or a
// Go file failed to extract (a parse error mid-edit, a file over the size cap).
func (e *expander) censusDoubt(ctx context.Context) string {
	if f := indexFreshness(e.store.Health()); f != "fresh" {
		return "the index is " + f
	}
	n, err := e.store.UnreadFiles(ctx, ".go")
	switch {
	case err != nil:
		return "the index could not say which Go files it failed to read"
	case n > 0:
		return fmt.Sprintf("%d Go file(s) could not be read by the index, so their calls are not recorded", n)
	}
	return ""
}

// packageName is a Go file's package clause, or "" when the index has none.
func (e *expander) packageName(ctx context.Context, p string) string {
	n, _ := e.packageOf(ctx, p)
	return n.Name
}

// callerCounts counts a declaration's resolved callers in an inward
// neighbourhood, inside and outside the scope. ok is false when the
// neighbourhood was cut at the per-hop ceiling: a cut list gives no count to state.
func callerCounts(nb *topology.Neighbourhood, self int64, inScope func(string) bool) (in, out int, ok bool) {
	if nb == nil {
		return 0, 0, true
	}
	if nb.Truncated {
		return 0, 0, false
	}
	for _, m := range nb.Nodes {
		switch {
		case m.ID == self:
		case inScope(m.Path):
			in++
		default:
			out++
		}
	}
	return in, out, true
}

// noteOpen records one open call at its place. Two calls on one line are one place
// to look, and at most contextCensusSites places are named; OpenShown counts the
// calls the named places hold, so "and k more" counts only the rest.
func noteOpen(c *callerNote, at string) {
	c.Open++
	if !slices.Contains(c.OpenAt, at) {
		if len(c.OpenAt) == contextCensusSites {
			return
		}
		c.OpenAt = append(c.OpenAt, at)
	}
	c.OpenShown++
}

// censusSeed is what couldCall needs to know of the declaration.
type censusSeed struct {
	method, exported bool
	dir, pkg         string // its directory and package clause
}

// couldCall reports whether an open call of the seed's name could be a call of
// the seed. A bare call can be a function only, from the function's own package
// or, for an exported function, from a file that dot-imports a package; a
// receiver or unmatched qualified call can be a method only; an external call can
// be an exported function whose package clause its qualifier names (the import
// path may simply have failed to map onto the directory); an unexported name is
// callable from its own package only. Anything else could be either. It errs
// towards open: an open site that is not a call of the seed costs a reader a look,
// a missed one makes "complete" false.
func couldCall(o topology.OpenCall, seed censusSeed) bool {
	samePkg := path.Dir(o.Path) == seed.dir
	if !seed.exported && !samePkg {
		return false
	}
	switch o.Kind {
	case "bare", "local":
		return !seed.method && (samePkg || o.DotImport)
	case "receiver", "unmatched":
		return seed.method
	case "external":
		return !seed.method && seed.pkg != "" && o.Qualifier == seed.pkg
	}
	return true
}

func isExportedName(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}

// censusText words a seed's caller list with what the census found. Its counts
// are the index's, not the walk's, so a cut walk cannot shrink them; the caveat
// every census line shares (labelCensus) is said once, after them. "Complete" needs
// no open call of the name anywhere, inside the scope or outside it, and nothing
// that would hide one (censusDoubt).
func (c callerNote) censusText() string {
	here := fmt.Sprintf("%d in the index, %d listed here%s", c.Indexed, c.Resolved, c.matched())
	if c.IndexedOutside > 0 {
		here += fmt.Sprintf(" (and %d outside the scope)", c.IndexedOutside)
	}
	outside := ""
	if c.OpenOutside > 0 {
		outside = fmt.Sprintf("; %d unresolved call(s) of %s outside the scope are not listed", c.OpenOutside, c.Name)
	}
	if c.Open == 0 {
		switch {
		case c.Unclaimable != "":
			return fmt.Sprintf("callers of %s: %s%s; not claimed complete: %s", c.Selector, here, outside, c.Unclaimable)
		case c.OpenOutside > 0:
			return fmt.Sprintf("callers of %s: %s%s, so the list is not complete", c.Selector, here, outside)
		}
		rest := ""
		if c.Indexed > c.Resolved {
			rest = "; call_hierarchy lists the rest"
		}
		return fmt.Sprintf("callers of %s: complete (every recorded call of %s resolves): %s%s", c.Selector, c.Name, here, rest)
	}
	more := ""
	if c.Open > c.OpenShown {
		more = fmt.Sprintf(" and %d more", c.Open-c.OpenShown)
	}
	doubt := ""
	if c.Unclaimable != "" {
		doubt = "; and " + c.Unclaimable
	}
	return fmt.Sprintf("callers of %s: %s; %d unresolved call(s) of %s may also be it: %s%s%s%s. Check these, not the whole tree",
		c.Selector, here, c.Open, c.Name, strings.Join(c.OpenAt, ", "), more, outside, doubt)
}
