package tools

// post_write_diag_label_test.go — PLAN-362 PR1: every non-empty post-write
// diagnostics block carries one of three fixed, machine-parseable labels so an
// agent (or a script) can tell a genuine post-write re-analysis from a
// pre-write snapshot, or an outright failed pull, without parsing prose.
// Never print diagnostic content unlabelled.

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
	unverifiedLabelLine    = "\n[diagnostics: unverified — post-write pull failed]"
)

// TestPostWriteDiagLabel_StalePathCarriesSnapshotLabel covers acceptance case
// (a): the fast-adaptive-window path, where the language server has not
// re-published since the write, always carries the snapshot label — never the
// authoritative one, and never unlabelled diagnostic content.
func TestPostWriteDiagLabel_StalePathCarriesSnapshotLabel(t *testing.T) {
	src := newStubDiag()
	src.set(errDiag("possibly stale error")) // present before the write; the stub never re-publishes

	d := WriteDeps{Diag: src, PostWriteDiagWindow: 20 * time.Millisecond}
	out := d.postWriteDiagnostics("file:///foo.go", "before", "after", postWriteDiagOpts{}, nil).text

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

	out := d.postWriteDiagnostics(edited, "a\nb", "a\nB", postWriteDiagOpts{}, baseline).text

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

	out := d.postWriteDiagnostics(edited, "a\nb", "a\nB", postWriteDiagOpts{awaitFresh: true}, baseline).text

	if !strings.HasPrefix(out, authoritativeLabelLine) {
		t.Fatalf("expected the clean-pass block to start with the fixed authoritative label, got:\n%q", out)
	}
	if !strings.Contains(out, "fresh diagnostics pass") {
		t.Fatalf("expected the clean-pass line, got:\n%q", out)
	}
}

// TestPostWriteDiagLabel_StaleEmptyNeverLabelled covers the case where the
// wait times out AND there is nothing cached to (mis)report.
func TestPostWriteDiagLabel_StaleEmptyNeverLabelled(t *testing.T) {
	t.Run("awaitFresh=false: nothing to report renders nothing", func(t *testing.T) {
		src := newStubDiag() // never set — nothing cached
		d := WriteDeps{Diag: src, PostWriteDiagWindow: 10 * time.Millisecond}
		out := d.postWriteDiagnostics("file:///foo.go", "before", "after", postWriteDiagOpts{}, nil).text
		if out != "" {
			t.Fatalf("nothing to report must render nothing, got:\n%q", out)
		}
	})

	// An agent that explicitly asked "did my change compile?" via
	// await_diagnostics:true must never get total silence just because
	// nothing happened to be cached before the wait — that reads as "no
	// answer" rather than "not confirmed". The wait timing out with an empty
	// cache still gets the snapshot label plus an explicit not-confirmed line.
	t.Run("awaitFresh=true: timeout still surfaces the labelled snapshot line", func(t *testing.T) {
		src := newStubDiag() // never set — nothing cached
		d := WriteDeps{Diag: src, PostWriteDiagWindow: 10 * time.Millisecond}
		out := d.postWriteDiagnostics("file:///foo.go", "before", "after", postWriteDiagOpts{awaitFresh: true}, nil).text
		if !strings.HasPrefix(out, snapshotLabelLine) {
			t.Fatalf("expected the block to start with the fixed snapshot label, got:\n%q", out)
		}
		if !strings.Contains(out, "not re-analysed within the wait") {
			t.Fatalf("expected an explicit not-confirmed line, got:\n%q", out)
		}
	})
}

// TestPostWriteDiagLabel_DisabledWindowNeverBlamesAWait closes a PR1 review nit
// (PLAN-362 PR2): when the post-write window is switched off there IS no wait,
// so telling the caller their answer did not arrive "within the wait" names
// something that never ran. The disabled case says so instead.
func TestPostWriteDiagLabel_DisabledWindowNeverBlamesAWait(t *testing.T) {
	t.Run("nothing cached", func(t *testing.T) {
		src := newStubDiag()
		d := WriteDeps{Diag: src, PostWriteDiagWindow: -1}
		out := d.postWriteDiagnostics("file:///foo.go", "before", "after", postWriteDiagOpts{awaitFresh: true}, nil).text
		if !strings.HasPrefix(out, snapshotLabelLine) {
			t.Fatalf("expected the snapshot label, got:\n%q", out)
		}
		if strings.Contains(out, "within the wait") {
			t.Errorf("a disabled window must not report a wait that never ran:\n%q", out)
		}
		if !strings.Contains(out, "post_write_diagnostics_ms") {
			t.Errorf("expected the disabled window named so the caller can fix it:\n%q", out)
		}
	})

	t.Run("something cached", func(t *testing.T) {
		src := newStubDiag()
		src.set(errDiag("older error"))
		d := WriteDeps{Diag: src, PostWriteDiagWindow: -1}
		out := d.postWriteDiagnostics("file:///foo.go", "before", "after", postWriteDiagOpts{awaitFresh: true}, nil).text
		if !strings.HasPrefix(out, snapshotLabelLine) {
			t.Fatalf("expected the snapshot label, got:\n%q", out)
		}
		if strings.Contains(out, "within the wait") || strings.Contains(out, "not yet re-analysed;") {
			t.Errorf("a disabled window must not imply an analysis is pending:\n%q", out)
		}
	})
}

// TestPostWriteDiagLabel_NoDiagnosticsSourceIsSaidOutLoud qualifies the
// "always labelled" claim (PR1 review, item 5): a file with NO diagnostics
// source produces no block at all on the default path, and that silence means
// "not analysed", not "clean". A caller who explicitly asks is told which.
func TestPostWriteDiagLabel_NoDiagnosticsSourceIsSaidOutLoud(t *testing.T) {
	d := WriteDeps{} // no Diag source wired at all

	if out := d.postWriteDiagnostics("file:///foo.go", "a", "b", postWriteDiagOpts{}, nil).text; out != "" {
		t.Fatalf("the default path must stay silent, got:\n%q", out)
	}

	r := d.postWriteDiagnostics("file:///foo.go", "a", "b", postWriteDiagOpts{awaitFresh: true, structured: true}, nil)
	if !strings.HasPrefix(r.text, "\n[diagnostics: "+postWriteDiagLabelNotAnalysed+"]") {
		t.Fatalf("expected the not-analysed label, got:\n%q", r.text)
	}
	if !strings.Contains(r.text, "not a clean bill of health") {
		t.Errorf("the caller must be told silence is not a pass, got:\n%q", r.text)
	}
	if r.delta.Fresh || r.delta.Scopes.EditedFile != diagScopeNoSource {
		t.Errorf("delta must report the no_source scope, got %+v", r.delta)
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
	out := d.postWriteDiagnostics(pwURI, "a\nb", "a\nB", postWriteDiagOpts{}, baseline).text

	if !strings.HasPrefix(out, authoritativeLabelLine) {
		t.Fatalf("expected a successful pull to carry the fixed authoritative label, got:\n%q", out)
	}
	if strings.Contains(out, snapshotLabelLine) {
		t.Fatalf("a successful pull must never carry the snapshot label:\n%q", out)
	}
}

// TestPostWriteDiagLabel_PullFailureNeverAuthoritative covers the pull
// failure path: it is its own third labelled state — never dressed up as
// authoritative, and never left unlabelled (a bare "state unverified" prose
// line, with no fixed prefix, would be a third UNlabelled state and break the
// "exactly N fixed labels" machine-parseable contract).
func TestPostWriteDiagLabel_PullFailureNeverAuthoritative(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull"}
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return nil, context.DeadlineExceeded
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", postWriteDiagOpts{awaitFresh: true}, baseline).text

	if !strings.HasPrefix(out, unverifiedLabelLine) {
		t.Fatalf("expected the block to start with the fixed unverified label, got:\n%q", out)
	}
	if strings.Contains(out, authoritativeLabelLine) {
		t.Fatalf("a failed pull must never carry the authoritative label:\n%q", out)
	}
	if strings.Contains(out, snapshotLabelLine) {
		t.Fatalf("a failed pull must never carry the snapshot label either — it is its own state:\n%q", out)
	}
	if !strings.Contains(out, "unverified") {
		t.Fatalf("expected the explicit unverified note, got:\n%q", out)
	}
}
