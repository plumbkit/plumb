package tools

import (
	"context"
	"fmt"
	"path"
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
	method := n.Kind == topology.KindMethod
	open, _, err := e.store.OpenCallSites(ctx, n.Name, "go", 0)
	if err != nil {
		e.fail(ctx, err)
		return
	}
	indexed, ok := e.indexedCallers(ctx, n)
	if !ok {
		return
	}
	c.Indexed = indexed
	dir := path.Dir(n.Path)
	exported := isExportedName(n.Name)
	c.Census, c.Method, c.Name = true, method, n.Name
	listed := map[string]bool{}
	for _, o := range open {
		if !couldCall(o, method, exported, dir) || !e.allowedPath(o.Path) {
			continue
		}
		c.Open++
		// Two calls on one line are one place to look.
		at := fmt.Sprintf("%s:%d (%s)", o.Path, o.Line, o.Kind)
		if !listed[at] && len(c.OpenAt) < contextCensusSites {
			listed[at] = true
			c.OpenAt = append(c.OpenAt, at)
		}
		if listed[at] {
			c.OpenShown++
		}
	}
}

// indexedCallers counts n's resolved callers the index holds, inside the scope,
// whatever the walk listed. ok is false when the index cannot give a count: an
// error, or a hub whose callers passed the per-hop ceiling.
func (e *expander) indexedCallers(ctx context.Context, n topology.Node) (count int, ok bool) {
	res, err := e.store.ImpactFrom(ctx, n, contextTraversalOpts(true, []string{string(topology.EdgeCalls)}))
	if err != nil {
		e.fail(ctx, err)
		return 0, false
	}
	if res.DependedOnBy == nil {
		return 0, true
	}
	if res.DependedOnBy.Truncated {
		return 0, false
	}
	for _, m := range res.DependedOnBy.Nodes {
		if m.ID != n.ID && e.allowedPath(m.Path) {
			count++
		}
	}
	return count, true
}

// couldCall reports whether an open call of the seed's name could be a call of
// the seed. A bare call can be a function only, from the function's own package
// or, for an exported function, from a file that dot-imports a package; a
// receiver or unmatched qualified call can be a method only; an unexported name is
// callable from its own package only. Anything else could be either. It errs
// towards open: an open site that is not a call of the seed costs a reader a look,
// a missed one makes "complete" false.
func couldCall(o topology.OpenCall, method, exported bool, dir string) bool {
	samePkg := path.Dir(o.Path) == dir
	if !exported && !samePkg {
		return false
	}
	switch o.Kind {
	case "bare", "local":
		return !method && (samePkg || o.DotImport)
	case "receiver", "unmatched":
		return method
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
// every census line shares (labelCensus) is said once, after them.
func (c callerNote) censusText() string {
	here := fmt.Sprintf("%d in the index, %d listed here%s", c.Indexed, c.Resolved, c.matched())
	if c.Open == 0 {
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
	return fmt.Sprintf("callers of %s: %s; %d unresolved call(s) of %s may also be it: %s%s. Check these, not the whole tree",
		c.Selector, here, c.Open, c.Name, strings.Join(c.OpenAt, ", "), more)
}
