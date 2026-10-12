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
	if n.Language != "go" || !e.derivedAdmitted(ctx, n.ID) {
		return
	}
	method := n.Kind == topology.KindMethod
	open, _, err := e.store.OpenCallSites(ctx, n.Name, "go", 0)
	if err != nil {
		e.fail(ctx, err)
		return
	}
	dir := path.Dir(n.Path)
	exported := isExportedName(n.Name)
	c.Census, c.Method, c.Name = true, method, n.Name
	for _, o := range open {
		if !couldCall(o, method, exported, dir) || !e.allowedPath(o.Path) {
			continue
		}
		c.Open++
		if len(c.OpenAt) < contextCensusSites {
			c.OpenAt = append(c.OpenAt, fmt.Sprintf("%s:%d (%s)", o.Path, o.Line, o.Kind))
		}
	}
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

// censusText words a seed's caller list with what the census found.
func (c callerNote) censusText(capped bool) string {
	unseen := "a use as a function value is not a call of it"
	if c.Method {
		unseen = "a call through an interface, or a use as a method value, is not a call of it"
	}
	listed := fmt.Sprintf("%d resolved and listed%s", c.Resolved, c.matched())
	if c.Open == 0 {
		if capped {
			return fmt.Sprintf("callers of %s: every recorded call of %s in the index resolves, but the walk was cut, so the pack lists only some (%s); call_hierarchy lists them all (%s)", c.Selector, c.Name, listed, unseen)
		}
		return fmt.Sprintf("callers of %s: %s; complete: every recorded call of %s in the index resolves (%s)", c.Selector, listed, c.Name, unseen)
	}
	more := ""
	if c.Open > len(c.OpenAt) {
		more = fmt.Sprintf(" and %d more", c.Open-len(c.OpenAt))
	}
	return fmt.Sprintf("callers of %s: %s; %d recorded call(s) of %s are unresolved and may be calls of it: %s%s. Those sites, not a search of the tree, are what is left to check (%s)",
		c.Selector, listed, c.Open, c.Name, strings.Join(c.OpenAt, ", "), more, unseen)
}
