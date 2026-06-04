package wasmts

// node is a handle to a tree-sitter node living in the wasm module's linear
// memory. It is valid only for the duration of the parse() call that produced
// it (its backing struct is freed when the parse's arena is released). All
// methods route through runtime.call, which is a no-op once a wasm-level error
// has been recorded, so a walk degrades to empty results rather than panicking.
type node struct {
	r   *runtime
	ptr uint64
}

func (n node) isNull() bool {
	return n.ptr == 0 || n.r.call(n.r.nIsNull, n.ptr) == 1
}

func (n node) kind() string {
	return n.r.readCString(n.r.call(n.r.nType, n.ptr))
}

func (n node) startByte() int { return abiInt(n.r.call(n.r.nStartByte, n.ptr)) }
func (n node) endByte() int   { return abiInt(n.r.call(n.r.nEndByte, n.ptr)) }

func (n node) childCount() int { return abiInt(n.r.call(n.r.nChildCount, n.ptr)) }

func (n node) child(i int) node {
	res := n.r.allocNode()
	n.r.call(n.r.nChild, res, n.ptr, uint64(i)) //nolint:gosec // G115: i is a non-negative child index
	return node{r: n.r, ptr: res}
}

// children returns all (named and anonymous) children, mirroring gotreesitter's
// Node.Children() that the ported walk expects.
func (n node) children() []node {
	cc := n.childCount()
	out := make([]node, 0, cc)
	for i := 0; i < cc; i++ {
		out = append(out, n.child(i))
	}
	return out
}

// firstNamedChild returns the first named child, or a null node.
func (n node) firstNamedChild() node {
	res := n.r.allocNode()
	n.r.call(n.r.nNamedChild, res, n.ptr, 0)
	c := node{r: n.r, ptr: res}
	if c.isNull() {
		return node{r: n.r}
	}
	return c
}

// childByType returns the first child whose grammar type equals typ, or null.
func (n node) childByType(typ string) node {
	for i := 0; i < n.childCount(); i++ {
		if c := n.child(i); c.kind() == typ {
			return c
		}
	}
	return node{r: n.r}
}

// childByFieldName returns the child bound to the grammar field, or null.
func (n node) childByFieldName(field string) node {
	fp, err := n.r.writeBytes([]byte(field))
	if err != nil {
		return node{r: n.r}
	}
	res := n.r.allocNode()
	n.r.call(n.r.nChildByField, res, n.ptr, fp, uint64(len(field)))
	c := node{r: n.r, ptr: res}
	if c.isNull() {
		return node{r: n.r}
	}
	return c
}

// text returns the source slice spanned by the node.
func (n node) text(src []byte) string {
	s, e := n.startByte(), n.endByte()
	if s < 0 || e > len(src) || s > e {
		return ""
	}
	return string(src[s:e])
}
