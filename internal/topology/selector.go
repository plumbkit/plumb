package topology

import (
	"fmt"
	"strings"
)

// GoReceiverType strips Go receiver decoration so a dotted-name parent of
// "(*Foo)", "*Foo", or "Foo" all normalise to "Foo".
func GoReceiverType(parent string) string {
	return strings.TrimPrefix(strings.Trim(parent, "()"), "*")
}

// StripTypeParams drops the type-parameter list of a generic type: "S[T]"
// and "M[K, V]" both normalise to S / M.
func StripTypeParams(name string) string {
	i := strings.IndexByte(name, '[')
	if i <= 0 || name[len(name)-1] != ']' || strings.Contains(name[:i], "]") {
		return name
	}
	depth := 0
	for j := i; j < len(name); j++ {
		switch name[j] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 && j != len(name)-1 {
				return name
			}
		}
	}
	if depth != 0 {
		return name
	}
	return name[:i]
}

// GoMethodReceiver splits a Go method symbol name — "(*Recv).Method" or
// "(Recv).Method" — into its receiver type and method. ok is false for any name
// not in that form.
func GoMethodReceiver(symName string) (recv, method string, ok bool) {
	if !strings.HasPrefix(symName, "(") {
		return "", "", false
	}
	i := strings.Index(symName, ").")
	if i < 0 {
		return "", "", false
	}
	recv = strings.TrimPrefix(symName[1:i], "*")
	method = symName[i+2:]
	if recv == "" || method == "" || strings.Contains(method, ".") {
		return "", "", false
	}
	return recv, method, true
}

// SelectorVariants returns candidate symbol names across supported receiver
// formats (e.g. "Recv.Method", "(*Recv).Method", "(Recv).Method", "Recv/Method")
// so that query resolution in topology tools normalises to the same declaration.
func SelectorVariants(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	variants := []string{name}
	add := func(v string) {
		for _, existing := range variants {
			if existing == v {
				return
			}
		}
		variants = append(variants, v)
	}

	var recv, method string
	if r, m, ok := GoMethodReceiver(name); ok {
		recv, method = r, m
	} else if p, c, ok := strings.Cut(name, "."); ok {
		recv, method = GoReceiverType(p), c
	} else if p, c, ok := strings.Cut(name, "/"); ok {
		recv, method = GoReceiverType(p), c
	}

	if recv != "" && method != "" {
		recvBase := StripTypeParams(recv)
		add(fmt.Sprintf("(*%s).%s", recv, method))
		add(fmt.Sprintf("(%s).%s", recv, method))
		add(fmt.Sprintf("%s.%s", recv, method))
		add(fmt.Sprintf("*%s.%s", recv, method))
		add(fmt.Sprintf("%s/%s", recv, method))
		if recvBase != recv {
			add(fmt.Sprintf("(*%s).%s", recvBase, method))
			add(fmt.Sprintf("(%s).%s", recvBase, method))
			add(fmt.Sprintf("%s.%s", recvBase, method))
			add(fmt.Sprintf("*%s.%s", recvBase, method))
			add(fmt.Sprintf("%s/%s", recvBase, method))
		}
	}
	return variants
}
