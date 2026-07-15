package cli

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestRoutingInvProxy_RelatedDocumentsRouteIndependently(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})
	installEntry(pool, rootB, &stubClient{id: "B"})
	invA := newInv(t)
	invB := newInv(t)
	pool.entries[poolKey{rootA, "go"}].inv = invA
	pool.entries[poolKey{rootB, "go"}].inv = invB

	ri := newRoutingInvProxy(pool)
	ri.setPrimary(rootA, "go", invA)
	uriA := "file://" + filepath.Join(rootA, "main.go")
	uriB := "file://" + filepath.Join(rootB, "related.go")
	applied, unresolved := ri.RecordPullResult(uriA, protocol.DocumentDiagnosticReport{
		Kind: protocol.DiagnosticReportFull,
		RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
			uriB: {
				Kind:     protocol.DiagnosticReportFull,
				ResultID: "related-result",
				Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "belongs to B"}},
			},
		},
	})
	if !reflect.DeepEqual(applied, []string{uriA, uriB}) || len(unresolved) != 0 {
		t.Fatalf("routing outcome = applied %#v unresolved %#v", applied, unresolved)
	}

	if invA.Tracked(uriB) {
		t.Fatal("a related document from root B must not land in root A's invalidator")
	}
	if !invB.Tracked(uriB) {
		t.Fatal("the related document must land in root B's invalidator")
	}
	if id, ok := invB.PullResultID(uriB); !ok || id != "related-result" {
		t.Fatalf("related result ID = (%q, %v), want (related-result, true)", id, ok)
	}
}

func TestRoutingInvProxy_RelatedDocumentsRespectConnectionBoundary(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})
	installEntry(pool, rootB, &stubClient{id: "B"})
	invA := newInv(t)
	invB := newInv(t)
	pool.entries[poolKey{rootA, "go"}].inv = invA
	pool.entries[poolKey{rootB, "go"}].inv = invB

	ri := newRoutingInvProxy(pool)
	ri.setPrimary(rootA, "go", invA)
	ri.setBoundaryGuard(func(path string) error {
		if filepath.Dir(path) == rootA {
			return nil
		}
		return errors.New("outside connection boundary")
	})

	uriA := "file://" + filepath.Join(rootA, "main.go")
	uriB := "file://" + filepath.Join(rootB, "secret.go")
	applied, unresolved := ri.RecordPullResult(uriA, protocol.DocumentDiagnosticReport{
		Kind: protocol.DiagnosticReportFull,
		RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
			uriB: {
				Kind:  protocol.DiagnosticReportFull,
				Items: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "must not cross boundary"}},
			},
		},
	})
	if !reflect.DeepEqual(applied, []string{uriA}) || len(unresolved) != 0 {
		t.Fatalf("out-of-bound URI leaked through outcome: applied %#v unresolved %#v", applied, unresolved)
	}

	if invA.Tracked(uriB) || invB.Tracked(uriB) {
		t.Fatal("an out-of-bound related document must not mutate any invalidator")
	}
}
