package topology

import (
	"database/sql"
	"path"
	"strconv"
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
	// tierLocal: `x.M()` where x is a variable the enclosing function declares
	// once with an evident type (CallSite.ReceiverType) — a method of that type.
	tierLocal
)

// receiverCandidates returns the targets a site may resolve to under the bare,
// receiver-self, typed-parameter or local-variable tier, and whether one of them
// owns the site at all. A qualifier that is none of those is left to the
// package-qualified tier (ok == false).
//
// The receiver, the parameters and the locals are checked BEFORE the import set
// because they shadow it: inside `func f(stats *Stats)` the call `stats.Open()`
// is a method call, whatever package called stats the file imports.
//
// A declared type is as strong as a receiver: the declaration states the
// variable's type, so the call's method set is known without type checking. A
// name the function declares more than once (receiverType ReceiverAmbiguous) is
// resolved by none of them, since which declaration a call sees depends on scopes
// the index does not hold. A type that is an interface resolves to the
// interface's method when the index holds one, which is the declared callee; the
// dynamic implementations stay unresolved.
func (t resolverTables) receiverCandidates(fileID, enclosing int64, callerPath, callee string, qualifier sql.NullString, receiverType string) ([]pkgTarget, callTier, bool) {
	s := t.same
	pkg, havePkg := s.filePkg[fileID]
	here := dirPkgKey(path.Dir(callerPath), pkg)
	if qualifier.Valid && qualifier.String == QualifierLocal {
		// A bare call of a name the caller declares itself: a local value, never
		// the package function of the same name.
		return nil, tierBare, true
	}
	if !qualifier.Valid {
		if !havePkg {
			return nil, tierBare, true
		}
		return s.funcs[here][callee], tierBare, true
	}
	if !havePkg {
		return nil, tierQualified, false
	}
	switch {
	case receiverType == ReceiverAmbiguous:
		return nil, tierLocal, true
	case receiverType != "":
		return t.localTypeCandidates(fileID, here, callee, receiverType), tierLocal, true
	}
	info, ok := s.enclosing[enclosing]
	if !ok {
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
	return t.methodsOf(fileID, here, typ, callee), tierParam, true
}

// methodsOf returns the methods named callee of the named type typ: "T", declared
// in the package keyed home, or "pkg.T", found through fileID's imports the way a
// package-qualified call's package is. Another package's method is callable only
// when exported.
func (t resolverTables) methodsOf(fileID int64, home, typ, callee string) []pkgTarget {
	s := t.same
	pkgName, typeName, crossPackage := strings.Cut(typ, ".")
	if !crossPackage {
		return s.methods[home][typ+"."+callee]
	}
	if !isExportedName(callee) {
		return nil
	}
	importPath, ok := t.imports[fileID][pkgName]
	if !ok {
		return nil
	}
	dir, ok := matchImportDir(importPath, t.pkgDirs)
	if !ok {
		return nil
	}
	targetPkg, ok := s.dirPkg[dir]
	if !ok {
		return nil
	}
	return s.methods[dirPkgKey(dir, targetPkg)][typeName+"."+callee]
}

// localTypeCandidates resolves a local variable's receiver type. A named type
// goes straight to methodsOf. A call's result ("call:F/i" or "call:pkg.F/i") is
// read from the called function's declared results: F must be one function of
// the caller's package, or one exported function of an imported package, and its
// i-th result a named type. An unqualified result type belongs to F's own
// package; a qualified one is read through the caller's imports only when F is in
// the caller's package (another package's file has its own import names).
func (t resolverTables) localTypeCandidates(fileID int64, here, callee, receiverType string) []pkgTarget {
	spec, fromCall := strings.CutPrefix(receiverType, ReceiverFromCall)
	if !fromCall {
		return t.methodsOf(fileID, here, receiverType, callee)
	}
	name, idxText, ok := strings.Cut(spec, "/")
	idx, err := strconv.Atoi(idxText)
	if !ok || err != nil {
		return nil
	}
	fnID, fnHome, crossPackage, ok := t.calledFunction(fileID, here, name)
	if !ok {
		return nil
	}
	results := resultTypes(t.same.sigs[fnID])
	if idx >= len(results) || !isNamedType(results[idx]) {
		return nil
	}
	typ := results[idx]
	if crossPackage && (strings.Contains(typ, ".") || !isExportedName(callee)) {
		// F's file has its own import names, and the caller can reach only the
		// exported methods of another package's type.
		return nil
	}
	return t.methodsOf(fileID, fnHome, typ, callee)
}

// calledFunction finds the single function a call spelling names: "F" in the
// caller's package, or "pkg.F" (exported) in an imported one. It returns the
// function's node id and the key of the package it is declared in.
func (t resolverTables) calledFunction(fileID int64, here, name string) (id int64, home string, crossPackage, ok bool) {
	s := t.same
	pkgName, fn, qualified := strings.Cut(name, ".")
	if !qualified {
		cands := s.funcs[here][name]
		if len(cands) != 1 {
			return 0, "", false, false
		}
		return cands[0].id, here, false, true
	}
	if strings.Contains(fn, ".") || !isExportedName(fn) {
		return 0, "", false, false
	}
	importPath, found := t.imports[fileID][pkgName]
	if !found {
		return 0, "", false, false
	}
	dir, found := matchImportDir(importPath, t.pkgDirs)
	if !found {
		return 0, "", false, false
	}
	targetPkg, found := s.dirPkg[dir]
	cands := t.targets[dir][fn]
	if !found || len(cands) != 1 {
		return 0, "", false, false
	}
	return cands[0].id, dirPkgKey(dir, targetPkg), true, true
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
	case tierLocal:
		r.resolvedLocalTyped++
	}
}

// resultTypes lists a Go declaration's result types, in order, from its stored
// signature: "func F() (*T, error)" → [T error]; "func F() (t *T, err error)" →
// [T error]. A pointer is dropped. Names in a group ("a, b T") each take the
// group's type. It relies on funcSignature's spelling, which always parenthesises
// the results.
func resultTypes(signature string) []string {
	rest, ok := strings.CutPrefix(signature, "func ")
	if !ok {
		return nil
	}
	if strings.HasPrefix(rest, "(") {
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return nil
		}
		rest = rest[end+1:]
	}
	open := strings.IndexByte(rest, '(')
	if open < 0 {
		return nil
	}
	closing := strings.IndexByte(rest[open:], ')')
	if closing < 0 {
		return nil
	}
	after := strings.TrimSpace(rest[open+closing+1:])
	if !strings.HasPrefix(after, "(") || !strings.HasSuffix(after, ")") {
		return nil
	}
	segs := strings.Split(after[1:len(after)-1], ", ")
	named := false
	for _, seg := range segs {
		named = named || strings.Contains(seg, " ")
	}
	var out []string
	pending := 0
	for _, seg := range segs {
		if !named {
			out = append(out, strings.TrimPrefix(seg, "*"))
			continue
		}
		_, typ, ok := strings.Cut(seg, " ")
		if !ok {
			pending++
			continue
		}
		for range pending + 1 {
			out = append(out, strings.TrimPrefix(typ, "*"))
		}
		pending = 0
	}
	return out
}
