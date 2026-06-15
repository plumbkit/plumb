package protocol

import (
	"encoding/json"
	"testing"
)

// TestHierarchyItem_PreservesData guards the opaque `data` field on
// CallHierarchyItem and TypeHierarchyItem. The LSP spec requires the client to
// echo it verbatim from the prepare response into the incoming/outgoing (resp.
// supertypes/subtypes) request; sourcekit-lsp returns no results without it.
// Dropping it from the struct silently breaks call/type hierarchy.
func TestHierarchyItem_PreservesData(t *testing.T) {
	const rng = `{"start":{"line":1,"character":0},"end":{"line":1,"character":4}}`
	data := `{"usr":"s:1A4showyyF","n":[1,2]}`

	t.Run("call hierarchy", func(t *testing.T) {
		raw := `{"name":"show","kind":6,"uri":"file:///a.swift","range":` + rng +
			`,"selectionRange":` + rng + `,"data":` + data + `}`
		var item CallHierarchyItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		assertDataRoundTrips(t, item.Data, item)
	})

	t.Run("type hierarchy", func(t *testing.T) {
		raw := `{"name":"PanelController","kind":5,"uri":"file:///a.swift","range":` + rng +
			`,"selectionRange":` + rng + `,"data":` + data + `}`
		var item TypeHierarchyItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		assertDataRoundTrips(t, item.Data, item)
	})
}

func assertDataRoundTrips(t *testing.T, got json.RawMessage, item any) {
	t.Helper()
	if len(got) == 0 {
		t.Fatal("data dropped on unmarshal")
	}
	out, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var rt struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &rt); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(rt.Data) == 0 {
		t.Fatal("data dropped on re-marshal (would not reach the server)")
	}
}
