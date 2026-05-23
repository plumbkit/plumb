package treesitter

import (
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
)

// firstNamedChild returns the first named child of n, or nil when n has none.
func firstNamedChild(n *tsg.Node) *tsg.Node {
	for _, c := range n.Children() {
		if c.IsNamed() {
			return c
		}
	}
	return nil
}

// childByType returns the first child of n whose node type is typ, or nil.
func childByType(n *tsg.Node, typ string, lang *tsg.Language) *tsg.Node {
	for _, c := range n.Children() {
		if c.Type(lang) == typ {
			return c
		}
	}
	return nil
}

// lastSegment returns the final segment of a "::"-separated path, so
// "tokio::test" → "test" and a bare "test" is returned unchanged.
func lastSegment(path string) string {
	if i := strings.LastIndex(path, "::"); i >= 0 {
		return path[i+2:]
	}
	return path
}
