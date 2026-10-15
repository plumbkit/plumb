package session

import (
	"strings"
	"testing"
)

func TestInNamespace(t *testing.T) {
	all := []Info{{ID: "a"}, {ID: "b", Namespace: "ns-1"}, {ID: "c", Namespace: "ns-2"}, {ID: "d"}}
	ids := func(in []Info) string {
		var b strings.Builder
		for _, i := range in {
			b.WriteString(i.ID)
		}
		return b.String()
	}
	if got := ids(InNamespace(all, "")); got != "ad" {
		t.Fatalf("default namespace = %q, want ad", got)
	}
	if got := ids(InNamespace(all, "ns-1")); got != "b" {
		t.Fatalf("ns-1 = %q, want b", got)
	}
	if got := ids(InNamespaceOf(all, "c")); got != "c" {
		t.Fatalf("namespace of c = %q, want c", got)
	}
	if got := ids(InNamespaceOf(all, "missing")); got != "ad" {
		t.Fatalf("an unknown self = %q, want the default namespace", got)
	}
	if len(all) != 4 || all[1].ID != "b" {
		t.Fatal("InNamespace modified its input")
	}
}
