package cache_test

import (
	"reflect"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestRecordPullResult_ReturnsDeterministicAppliedAndUnresolvedURIs(t *testing.T) {
	inv := newInv(t)
	mainURI := "file:///p/main.go"
	knownURI := "file:///p/a-known.go"
	fullURI := "file:///p/b-full.go"
	unknownURI := "file:///p/z-unknown.go"
	malformedURI := "file:///p/m-malformed.go"
	inv.RecordPullFull(knownURI, "known-result", []protocol.Diagnostic{
		diag(1, protocol.SevWarning, "W1", "gopls", "known warning"),
	})

	applied, unresolved := inv.RecordPullResult(mainURI, protocol.DocumentDiagnosticReport{
		Kind: protocol.DiagnosticReportFull,
		RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
			unknownURI: {
				Kind:     protocol.DiagnosticReportUnchanged,
				ResultID: "unknown-result",
			},
			knownURI: {
				Kind:     protocol.DiagnosticReportUnchanged,
				ResultID: "known-result",
			},
			fullURI: {
				Kind:     protocol.DiagnosticReportFull,
				ResultID: "full-result",
			},
			malformedURI: {Kind: "sideways"},
		},
	})

	wantApplied := []string{knownURI, fullURI, mainURI}
	wantUnresolved := []string{malformedURI, unknownURI}
	if !reflect.DeepEqual(applied, wantApplied) {
		t.Fatalf("applied = %#v, want %#v", applied, wantApplied)
	}
	if !reflect.DeepEqual(unresolved, wantUnresolved) {
		t.Fatalf("unresolved = %#v, want %#v", unresolved, wantUnresolved)
	}
	if inv.Tracked(unknownURI) || inv.Tracked(malformedURI) {
		t.Fatal("unresolved reports must mutate nothing")
	}
	if !inv.Tracked(knownURI) || !inv.Tracked(fullURI) || !inv.Tracked(mainURI) {
		t.Fatal("successfully applied reports must remain tracked")
	}
}

func TestRecordPullResult_MismatchedUnchangedMutatesNothing(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/stale.go"
	original := []protocol.Diagnostic{diag(2, protocol.SevError, "E1", "gopls", "standing error")}
	inv.RecordPullFull(uri, "known-result", original)
	beforeTime := inv.AllDiagnosticTimes()[uri]

	applied, unresolved := inv.RecordPullResult(uri, protocol.DocumentDiagnosticReport{
		Kind:     protocol.DiagnosticReportUnchanged,
		ResultID: "mismatched-result",
	})

	if len(applied) != 0 || !reflect.DeepEqual(unresolved, []string{uri}) {
		t.Fatalf("outcome = applied %#v unresolved %#v", applied, unresolved)
	}
	if got := inv.Diagnostics(uri); len(got) != 1 || got[0].Message != "standing error" {
		t.Fatalf("mismatched unchanged mutated diagnostics: %#v", got)
	}
	if id, ok := inv.PullResultID(uri); !ok || id != "known-result" {
		t.Fatalf("mismatched unchanged mutated result ID: %q, %v", id, ok)
	}
	if got := inv.AllDiagnosticTimes()[uri]; !got.Equal(beforeTime) {
		t.Fatalf("mismatched unchanged mutated timestamp: before %v after %v", beforeTime, got)
	}
}

func TestRecordPullResult_UnresolvedWinsForDuplicateURI(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/duplicate.go"
	applied, unresolved := inv.RecordPullResult(uri, protocol.DocumentDiagnosticReport{
		Kind: protocol.DiagnosticReportFull,
		RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
			uri: {
				Kind:     protocol.DiagnosticReportUnchanged,
				ResultID: "not-the-full-result",
			},
		},
	})
	if len(applied) != 0 || !reflect.DeepEqual(unresolved, []string{uri}) {
		t.Fatalf("unresolved must win for duplicate URI: applied %#v unresolved %#v", applied, unresolved)
	}
}
