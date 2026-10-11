package golang

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// receiverTypes extracts src and returns each qualified call's ReceiverType by
// "qualifier.callee".
func receiverTypes(t *testing.T, src string) map[string]string {
	t.Helper()
	_, _, sites, err := New().ExtractWithCallSites(context.Background(), "p/p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range sites {
		if s.Kind == topology.CallSiteCall && s.Qualifier != "" {
			out[s.Qualifier+"."+s.Callee] = s.ReceiverType
		}
	}
	return out
}

// A local variable's call carries the type its single declaration states, or the
// called function and result index for the resolver to finish.
func TestReceiverType_ALocalsDeclarationGivesItsType(t *testing.T) {
	got := receiverTypes(t, `package p

import "example.com/m/store"

func Use(in any) {
	lit := Cart{}
	ptr := &Cart{}
	fresh := new(Cart)
	var declared *store.DB
	asserted := in.(*Cart)
	made := NewCart()
	db, err := store.Open("x")
	_, second := pair()
	lit.Total()
	ptr.Total()
	fresh.Total()
	declared.Query()
	asserted.Total()
	made.Total()
	db.Query()
	second.Total()
	err.Error()
}
`)
	for call, want := range map[string]string{
		"lit.Total":      "Cart",
		"ptr.Total":      "Cart",
		"fresh.Total":    "Cart",
		"declared.Query": "store.DB",
		"asserted.Total": "Cart",
		"made.Total":     topology.ReceiverFromCall + "NewCart/0",
		"db.Query":       topology.ReceiverFromCall + "store.Open/0",
		"second.Total":   topology.ReceiverFromCall + "pair/1",
		"err.Error":      topology.ReceiverFromCall + "store.Open/1",
	} {
		if got[call] != want {
			t.Errorf("%s: ReceiverType %q, want %q", call, got[call], want)
		}
	}
}

// A name the function declares more than once is ambiguous: no tier may read a
// type for it, the receiver included. Names it never declares (imports, package
// variables) and declarations with no evident type carry nothing.
func TestReceiverType_ARedeclaredNameIsAmbiguous(t *testing.T) {
	got := receiverTypes(t, `package p

import "strings"

func (s *Server) Shutdown(conns []Conn) {
	for _, s := range s.peers {
		s.Close()
	}
	c := Conn{}
	if true {
		c := Other{}
		c.Close()
	}
	var b strings.Builder
	b.WriteString("x")
	strings.ToUpper("x")
	for _, conn := range conns {
		conn.Close()
	}
}
`)
	for call, want := range map[string]string{
		"s.Close":         topology.ReceiverAmbiguous, // the loop variable re-declares the receiver
		"c.Close":         topology.ReceiverAmbiguous, // two := in nested blocks
		"b.WriteString":   "strings.Builder",
		"strings.ToUpper": "", // an import, not a local
		"conn.Close":      "", // a range variable: no evident type
	} {
		if got[call] != want {
			t.Errorf("%s: ReceiverType %q, want %q", call, got[call], want)
		}
	}
}
