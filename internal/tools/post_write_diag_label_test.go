package tools

// post_write_diag_label_test.go — PLAN-362 PR1: every non-empty post-write
// diagnostics block carries one of two fixed, machine-parseable labels so an
// agent (or a script) can tell a genuine post-write re-analysis from a
// pre-write snapshot without parsing prose. Never print diagnostic content
// unlabelled.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// snapshotLabelLine / authoritativeLabelLine are the exact fixed-prefix
// strings a caller would match on. Asserting the literal text (not just
// substrings of it) keeps this test a real regression guard on the
// machine-parseable contract, not just the prose.
const (
	snapshotLabelLine      = "\n[diagnostics: pre-write snapshot — not yet re-analysed]"
	authoritativeLabelLine = "\n[diagnostics: authoritative post-write pass]"
)

// TestPostWriteDiagLabel_StalePathCarriesSnapshotLabel covers acceptance case
// (a): the fast-adaptive-window path, where the language server has not
// re-published since the write, always carries the snapshot label — never the
// authoritative one, and never unlabelled diagnostic content.
func TestPostWriteDiagLabel_StalePathCarriesSnapshotLabel(t *testing.T) {
	src := newStubDiag()
	src.set(errDiag("possibly stale error")) // present before the write; the stub never re-publishes

	d := WriteDeps{Diag: src, PostWriteDiagWindow: 20 * time.Millisecond}
	out := d.postWriteDiagnostics("file:///foo.go", "before", "after", false, nil)

	if !strings.HasPrefix(out, snapshotLabelLine) {
		t.Fatalf("expected the block to start with the fixed snapshot label, got:\n%q", out)
	}
	if strings.Contains(out, authoritativeLabelLine) {
		t.Fatalf("a stale (unconfirmed) block must never carry the authoritative label:\n%q", out)
	}
}

// TestPostWriteDiagLabel_FreshPathCarriesAuthoritativeLabel covers acceptance
// case: a publish that arrives during the wait (fresh=true) — whether via an
// explicit await_diagnostics wait or an incidentally fast default-window
// publish — is labelled authoritative, and reports the new finding.
func TestPostWriteDiagLabel_FreshPathCarriesAuthoritativeLabel(t *testing.T) {
	edited := "file:///foo.go"
	f := &fakeCrossDiag{all: map[string][]protocol.Diagnostic{edited: {}}, times: map[string]time.Time{}}
	d := WriteDeps{Diag: f}
	baseline := d.capturePreWriteBaseline(edited) // pre-write: clean

	// Simulate the language server re-publishing after the write with a new
	// finding on the touched line (mirrors fakeCrossDiag's documented usage).
	f.all[edited] = []protocol.Diagnostic{errAt("new break", 1)}

	out := d.postWriteDiagnostics(edited, "a\nb", "a\nB", false, baseline)

	if !strings.HasPrefix(out, authoritativeLabelLine) {
		t.Fatalf("expected the block to start with the fixed authoritative label, got:\n%q", out)
	}
	if strings.Contains(out, snapshotLabelLine) {
		t.Fatalf("a confirmed-fresh block must never carry the snapshot label:\n%q", out)
	}
	if !strings.Contains(out, "new break") {
		t.Fatalf("expected the new finding reported, got:\n%q", out)
	}
}

// TestPostWriteDiagLabel_FreshCleanPassCarriesAuthoritativeLabel covers the
// explicit await_diagnostics:true clean-pass case: the "✓ fresh diagnostics
// pass" line is itself a diagnostics block and must carry the label too.
func TestPostWriteDiagLabel_FreshCleanPassCarriesAuthoritativeLabel(t *testing.T) {
	edited := "file:///foo.go"
	f := &fakeCrossDiag{all: map[string][]protocol.Diagnostic{}, times: map[string]time.Time{}}
	d := WriteDeps{Diag: f}
	baseline := d.capturePreWriteBaseline(edited)

	out := d.postWriteDiagnostics(edited, "a\nb", "a\nB", true, baseline)

	if !strings.HasPrefix(out, authoritativeLabelLine) {
		t.Fatalf("expected the clean-pass block to start with the fixed authoritative label, got:\n%q", out)
	}
	if !strings.Contains(out, "fresh diagnostics pass") {
		t.Fatalf("expected the clean-pass line, got:\n%q", out)
	}
}

// TestPostWriteDiagLabel_StaleEmptyNeverLabelled covers the case where the
// wait times out AND there is nothing cached to (mis)report: no content, so
// no label either — labels only ever accompany real diagnostic content.
func TestPostWriteDiagLabel_StaleEmptyNeverLabelled(t *testing.T) {
	src := newStubDiag() // never set — nothing cached
	d := WriteDeps{Diag: src, PostWriteDiagWindow: 10 * time.Millisecond}
	out := d.postWriteDiagnostics("file:///foo.go", "before", "after", false, nil)
	if out != "" {
		t.Fatalf("nothing to report must render nothing, got:\n%q", out)
	}
}

// TestPostWriteDiagLabel_PullModeAlwaysAuthoritative covers the pull/hybrid
// path: a successful pull is synchronous with the write (the change
// notification is processed before the pull on the same connection), so it
// is always authoritative — never the snapshot label.
func TestPostWriteDiagLabel_PullModeAlwaysAuthoritative(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull"}
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind:     protocol.DiagnosticReportFull,
			ResultID: "r1",
			Items:    []protocol.Diagnostic{errAt("pulled error", 1)},
		}, nil
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a\nb", "a\nB", false, baseline)

	if !strings.HasPrefix(out, authoritativeLabelLine) {
		t.Fatalf("expected a successful pull to carry the fixed authoritative label, got:\n%q", out)
	}
	if strings.Contains(out, snapshotLabelLine) {
		t.Fatalf("a successful pull must never carry the snapshot label:\n%q", out)
	}
}

// TestPostWriteDiagLabel_PullFailureNeverAuthoritative covers the pull
// failure path: it is neither of the two labelled states (it is an explicit
// "state unverified" failure, already unambiguous), so it must never be
// dressed up as authoritative.
func TestPostWriteDiagLabel_PullFailureNeverAuthoritative(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull"}
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return nil, context.DeadlineExceeded
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", true, baseline)

	if strings.Contains(out, authoritativeLabelLine) {
		t.Fatalf("a failed pull must never carry the authoritative label:\n%q", out)
	}
	if !strings.Contains(out, "unverified") {
		t.Fatalf("expected the explicit unverified note, got:\n%q", out)
	}
}
