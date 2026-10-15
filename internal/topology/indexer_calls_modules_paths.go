package topology

// indexer_calls_modules_paths.go — which indexed file a module specifier names,
// for the module resolver (indexer_calls_modules.go). Every answer is memoised
// for the pass, and a Python absolute import is matched through a basename
// index, so a pass costs one lookup per distinct (importing directory, module),
// not one scan of the index per call site.

import (
	"path"
	"slices"
	"strings"
)

// moduleKey identifies one module lookup. A relative specifier depends on the
// importing file's directory; an absolute one does not, so its dir is "".
type moduleKey struct {
	family, dir, spec string
}

// moduleHit is a memoised lookup: the file, or none.
type moduleHit struct {
	file int64
	ok   bool
}

// moduleFile is the indexed file the module specifier spec names from the file
// at from, when exactly one does.
func (t *moduleTables) moduleFile(lang, from, spec string) (int64, bool) {
	key := moduleKey{family: "js", spec: spec}
	if lang == "python" {
		key.family = "python"
	}
	if strings.HasPrefix(spec, ".") {
		key.dir = path.Dir(from)
	}
	if hit, ok := t.modMemo[key]; ok {
		return hit.file, hit.ok
	}
	id, ok := t.lookupModule(lang, from, spec)
	t.modMemo[key] = moduleHit{file: id, ok: ok}
	return id, ok
}

func (t *moduleTables) lookupModule(lang, from, spec string) (int64, bool) {
	var cands []string
	suffix := false
	switch {
	case lang == "python":
		cands, suffix = pythonModuleCandidates(from, spec)
	case jsFamily(lang):
		cands = jsModuleCandidates(from, spec)
	}
	for _, c := range cands {
		if id, ok := t.files[c]; ok {
			return id, true
		}
	}
	if !suffix {
		return 0, false
	}
	return t.suffixModule(cands)
}

// suffixModule finds an absolute Python module under a source root the index
// does not know (src/, a nested project): the one indexed file whose path ends
// with a candidate, and none when several do. A match must sit inside a real
// package, so a repository file that only shares a module's name (app/json.py
// against the standard library's json) is never taken for it: a single-file
// top-level module (x.py) is matched at the root only, a package (x/__init__.py)
// anywhere, and a module inside a package (x/y.py) only where x/__init__.py is
// indexed beside it.
func (t *moduleTables) suffixModule(cands []string) (int64, bool) {
	var found int64
	for _, c := range cands {
		first, rest, nested := strings.Cut(c, "/")
		if !nested {
			continue
		}
		for _, p := range t.byBase[path.Base(c)] {
			if !strings.HasSuffix(p, "/"+c) {
				continue
			}
			prefix := strings.TrimSuffix(p, c)
			if rest != "__init__.py" {
				if _, ok := t.files[prefix+first+"/__init__.py"]; !ok {
					continue
				}
			}
			id := t.files[p]
			if found != 0 && found != id {
				return 0, false
			}
			found = id
		}
	}
	return found, found != 0
}

// pythonModuleCandidates lists the files a Python module specifier may be, in
// preference order, and whether a path-suffix match is allowed (absolute
// imports only).
func pythonModuleCandidates(from, spec string) ([]string, bool) {
	dots := len(spec) - len(strings.TrimLeft(spec, "."))
	rest := strings.ReplaceAll(spec[dots:], ".", "/")
	if dots == 0 {
		return []string{rest + ".py", rest + "/__init__.py"}, true
	}
	base := path.Dir(from)
	for range dots - 1 {
		base = path.Dir(base)
	}
	if rest == "" {
		return []string{path.Join(base, "__init__.py")}, false
	}
	p := path.Join(base, rest)
	return []string{p + ".py", p + "/__init__.py"}, false
}

// joinPythonModule names a submodule of spec: "." + "a" is ".a", "pkg" + "a"
// is "pkg.a".
func joinPythonModule(spec, name string) string {
	if strings.HasSuffix(spec, ".") {
		return spec + name
	}
	return spec + "." + name
}

// jsExtensions are the files a relative specifier without an extension may be,
// in the order bundlers and tsc try them.
var jsExtensions = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

// jsModuleCandidates lists the files a relative JS/TS specifier may be. A bare
// specifier names a package (node_modules, a bundler alias) and is not followed.
func jsModuleCandidates(from, spec string) []string {
	if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
		return nil
	}
	p := path.Join(path.Dir(from), spec)
	var out []string
	if ext := path.Ext(p); slices.Contains(jsExtensions, ext) {
		out = append(out, p)
		// TypeScript's ESM convention imports "./a.js" for the source file a.ts.
		stem := strings.TrimSuffix(p, ext)
		for _, e := range []string{".ts", ".tsx", ".mts", ".cts"} {
			out = append(out, stem+e)
		}
		return out
	}
	for _, e := range jsExtensions {
		out = append(out, p+e)
	}
	for _, e := range jsExtensions {
		out = append(out, p+"/index"+e)
	}
	return out
}
