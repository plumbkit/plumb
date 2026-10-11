package topology

import (
	"database/sql"
	"path"
	"strings"
)

// callTier names which rule a call site is resolved under.
type callTier int

const (
	// tierQualified: a package-qualified call, resolved through the caller's
	// import set (resolveOne).
	tierQualified callTier = iota
	// tierBare: `F()`, a top-level function of the caller's own package.
	tierBare
	// tierSelf: `r.M()` inside `func (r *T) …`, a method of T.
	tierSelf
	// tierParam: `p.M()` where p is a parameter of the enclosing declaration
	// with a named type, `p *T` or `p *pkg.T` — a method of that type.
	tierParam
)

// receiverCandidates returns the targets a site may resolve to under the bare,
// receiver-self or typed-parameter tier, and whether one of them owns the site
// at all. A qualifier that is neither the enclosing receiver nor a typed
// parameter is left to the package-qualified tier (ok == false).
//
// The receiver and the parameters are checked BEFORE the import set because
// they shadow it: inside `func f(stats *Stats)` the call `stats.Open()` is a
// method call, whatever package called stats the file imports.
//
// A typed parameter is as strong as a receiver: the declaration states the
// variable's type, so the call's method set is known without type checking. The
// residual risk is the same shadowing one (a closure parameter or a local
// re-declaring the name), and these edges carry the same lower confidence. A
// parameter whose type is an interface resolves to the interface's method when
// the index holds one, which is the declared callee; the dynamic implementations
// stay unresolved.
func (t resolverTables) receiverCandidates(fileID, enclosing int64, callerPath, callee string, qualifier sql.NullString) ([]pkgTarget, callTier, bool) {
	s := t.same
	pkg, havePkg := s.filePkg[fileID]
	here := dirPkgKey(path.Dir(callerPath), pkg)
	if !qualifier.Valid {
		if !havePkg {
			return nil, tierBare, true
		}
		return s.funcs[here][callee], tierBare, true
	}
	info, ok := s.enclosing[enclosing]
	if !ok || !havePkg {
		return nil, tierQualified, false
	}
	q := qualifier.String
	if info.recvName == q {
		return s.methods[here][info.recvBase+"."+callee], tierSelf, true
	}
	typ, ok := info.params[q]
	if !ok {
		return nil, tierQualified, false
	}
	pkgName, typeName, crossPackage := strings.Cut(typ, ".")
	if !crossPackage {
		return s.methods[here][typ+"."+callee], tierParam, true
	}
	// A method of another package's type is callable only when exported, and
	// the type's package is found the way a package-qualified call's is.
	if !isExportedName(callee) {
		return nil, tierParam, true
	}
	importPath, ok := t.imports[fileID][pkgName]
	if !ok {
		return nil, tierParam, true
	}
	dir, ok := matchImportDir(importPath, t.pkgDirs)
	if !ok {
		return nil, tierParam, true
	}
	targetPkg, ok := s.dirPkg[dir]
	if !ok {
		return nil, tierParam, true
	}
	return s.methods[dirPkgKey(dir, targetPkg)][typeName+"."+callee], tierParam, true
}

// namedParams extracts the parameters with a named type from a Go declaration's
// stored signature: "func (s *S) Run(ctx context.Context, a, b int, st *Store)"
// → {ctx: context.Context, a: int, b: int, st: Store}. A pointer is dropped. A
// parameter whose type is not a plain or package-qualified name (slice, map,
// function, variadic) is left out, as is the blank identifier.
//
// The parsing relies on the extractor's signature spelling (funcSignature):
// fields joined by ", ", names joined by ", " before one space and the type,
// and composite types rendered without commas or parentheses.
func namedParams(signature string) map[string]string {
	rest, ok := strings.CutPrefix(signature, "func ")
	if !ok {
		return nil
	}
	if strings.HasPrefix(rest, "(") {
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return nil
		}
		rest = strings.TrimSpace(rest[end+1:])
	}
	open := strings.IndexByte(rest, '(')
	if open < 0 {
		return nil
	}
	closing := strings.IndexByte(rest[open:], ')')
	if closing <= 1 {
		return nil
	}
	var out map[string]string
	var pending []string
	for _, seg := range strings.Split(rest[open+1:open+closing], ", ") {
		name, typ, named := strings.Cut(seg, " ")
		if !named {
			// Either the first names of a group (`a, b int`) or an unnamed
			// parameter; Go forbids mixing the two, so an unnamed list never
			// reaches a named segment and is dropped with pending.
			pending = append(pending, seg)
			continue
		}
		names := append(pending, name)
		pending = nil
		base := strings.TrimPrefix(typ, "*")
		if !isNamedType(base) {
			continue
		}
		for _, n := range names {
			if n == "_" {
				continue
			}
			if out == nil {
				out = map[string]string{}
			}
			out[n] = base
		}
	}
	return out
}

// isNamedType reports whether typ is `Name` or `pkg.Name`.
func isNamedType(typ string) bool {
	parts := strings.Split(typ, ".")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !isIdentifier(p) {
			return false
		}
	}
	return true
}

// isIdentifier reports whether s is an ASCII Go identifier other than "_".
func isIdentifier(s string) bool {
	if s == "" || s == "_" {
		return false
	}
	for i, r := range s {
		letter := r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// countTier records which rule produced a resolved site.
func (r *callResolution) countTier(tier callTier) {
	switch tier {
	case tierBare:
		r.resolvedSamePackage++
	case tierSelf:
		r.resolvedReceiverSelf++
	case tierParam:
		r.resolvedParamTyped++
	}
}
