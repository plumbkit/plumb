package topology

// selector_resolution.go — what a selector names in the index, as one of three
// answers a caller must handle: nothing, exactly one node, or several.
//
// ResolveNodes returns a bare slice, and every caller hand-rolls the meaning of
// its length — which is how a caller ends up taking cands[0] of an ambiguous
// name and answering about an arbitrary declaration. This type makes the
// three cases explicit, so "ambiguous" is a value the caller has to look at
// rather than a length it may forget to check.

import (
	"context"
	"database/sql"
)

// ResolutionKind is which of the three answers a selector got.
type ResolutionKind int

const (
	// ResolutionNone: nothing in the index answers to the selector.
	ResolutionNone ResolutionKind = iota
	// ResolutionOne: exactly one node does; it is in Node.
	ResolutionOne
	// ResolutionAmbiguous: several do; they are in Candidates, in ResolveNodes'
	// deterministic order. A caller reports them; it never picks one.
	ResolutionAmbiguous
)

// SelectorResolution is a selector's answer. Node is set only for
// ResolutionOne, Candidates only for ResolutionAmbiguous. Shadowed holds the
// import, package and file nodes that also matched but were set aside because a
// declaration did; it is empty when nothing was set aside. A caller discloses
// it rather than letting the preference pass silently.
type SelectorResolution struct {
	Kind       ResolutionKind
	Node       Node
	Candidates []Node
	Shadowed   []Node
}

// IsReference reports a node kind that names something declared elsewhere — an
// import, a package clause, a file — rather than being a declaration itself.
func IsReference(k NodeKind) bool {
	return k == KindImport || k == KindPackage || k == KindFile
}

// ResolveSelector resolves name (any form SelectorVariants accepts) under hint,
// as ResolveNodes does, and classifies the result. A hint that excludes every
// candidate still returns ResolveNodes' *HintMismatchError, unchanged.
//
// Declarations outrank references: when at least one candidate is a
// declaration, the import, package and file nodes that merely share the name
// are not counted — the same preference resolveNode's ORDER BY applies. A
// selector "strings" in a file that imports strings and declares nothing of
// that name is therefore still ambiguous among its references, while one that
// also declares a function named strings resolves to that function.
func ResolveSelector(ctx context.Context, db *sql.DB, name string, hint NodeHint) (SelectorResolution, error) {
	nodes, err := ResolveNodes(ctx, db, name, hint)
	if err != nil {
		return SelectorResolution{}, err
	}
	return classifyResolution(nodes), nil
}

// ClassifyNodes sorts nodes already resolved for one selector into the same
// none, one or ambiguous answer ResolveSelector gives, by the same rule. It is
// for a caller that must narrow the candidates itself before classifying (an
// access scope that must not even influence ambiguity): handing it the narrowed
// set keeps a single classifier rather than a second, drifting one.
func ClassifyNodes(nodes []Node) SelectorResolution { return classifyResolution(nodes) }

func classifyResolution(nodes []Node) SelectorResolution {
	var decls, refs []Node
	for _, n := range nodes {
		if IsReference(n.Kind) {
			refs = append(refs, n)
		} else {
			decls = append(decls, n)
		}
	}
	var shadowed []Node
	if len(decls) > 0 {
		nodes, shadowed = decls, refs
	}
	switch len(nodes) {
	case 0:
		return SelectorResolution{Kind: ResolutionNone}
	case 1:
		return SelectorResolution{Kind: ResolutionOne, Node: nodes[0], Shadowed: shadowed}
	}
	return SelectorResolution{Kind: ResolutionAmbiguous, Candidates: nodes, Shadowed: shadowed}
}

// DerivedCallsAdmitted reports whether derived (call-resolver) call edges may be
// served for a traversal centred on nodeID: the admission rule applied to the
// node's own subject. Every error, and an unknown node, answers false — derived
// edges are opt-in, so failing closed only ever withholds them, never invents
// them.
func DerivedCallsAdmitted(ctx context.Context, db *sql.DB, nodeID int64) bool {
	subject, err := CallGraphSubjectForNode(ctx, db, nodeID)
	if err != nil {
		return false
	}
	admission, err := AdmitCallGraph(ctx, db, subject)
	return err == nil && admission.Admitted
}
