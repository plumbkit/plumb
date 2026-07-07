package tools

func (t *InsertBeforeSymbol) WithWriteDeps(deps WriteDeps) *InsertBeforeSymbol {
	t.deps = deps
	t.hasDeps = true
	if deps.Cache != nil {
		t.cache = deps.Cache
	}
	return t
}

func (t *InsertAfterSymbol) WithWriteDeps(deps WriteDeps) *InsertAfterSymbol {
	t.deps = deps
	t.hasDeps = true
	if deps.Cache != nil {
		t.cache = deps.Cache
	}
	return t
}

func (t *ReplaceSymbolBody) WithWriteDeps(deps WriteDeps) *ReplaceSymbolBody {
	t.deps = deps
	t.hasDeps = true
	if deps.Cache != nil {
		t.cache = deps.Cache
	}
	return t
}

func (t *SafeDeleteSymbol) WithWriteDeps(deps WriteDeps) *SafeDeleteSymbol {
	t.deps = deps
	t.hasDeps = true
	if deps.Cache != nil {
		t.cache = deps.Cache
	}
	return t
}

func writeDepsPtr(ok bool, deps *WriteDeps) *WriteDeps {
	if !ok {
		return nil
	}
	return deps
}
