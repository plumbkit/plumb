package cache_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// diag builds a diagnostic whose five dedup-key components (range, severity,
// code, source, message) are all set from the arguments, so tests can construct
// near-miss diagnostics that differ in exactly one component.
func diag(line uint32, sev protocol.DiagnosticSeverity, code any, source, msg string) protocol.Diagnostic {
	return protocol.Diagnostic{
		Range: protocol.Range{
			Start: protocol.Position{Line: line, Character: 0},
			End:   protocol.Position{Line: line, Character: 5},
		},
		Severity: sev,
		Code:     code,
		Source:   source,
		Message:  msg,
	}
}

// pushDiagnostics seeds the push snapshot for uri via the ordinary Handle path.
func pushDiagnostics(inv *cache.Invalidator, uri string, diags []protocol.Diagnostic) {
	params, _ := json.Marshal(protocol.PublishDiagnosticsParams{URI: uri, Diagnostics: diags})
	inv.Handle(protocol.MethodPublishDiagnostics, params)
}

func newInv(t *testing.T) *cache.Invalidator {
	t.Helper()
	c := cache.New(time.Hour)
	t.Cleanup(func() { c.Close() })
	return cache.NewInvalidator(c)
}

// RecordPullFull stores a pull snapshot and its result ID, and a later full
// report fully replaces the prior snapshot for that URI.
func TestRecordPullFull_ReplacesSnapshot(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	inv.RecordPullFull(uri, "rid-1", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "first")})
	if got := inv.Diagnostics(uri); len(got) != 1 || got[0].Message != "first" {
		t.Fatalf("after first full: got %v", got)
	}
	if id, ok := inv.PullResultID(uri); !ok || id != "rid-1" {
		t.Fatalf("PullResultID = (%q,%v), want (rid-1,true)", id, ok)
	}

	// A second full report replaces (does not append to) the snapshot.
	inv.RecordPullFull(uri, "rid-2", []protocol.Diagnostic{
		diag(2, protocol.SevWarning, "E2", "gopls", "second"),
		diag(3, protocol.SevWarning, "E3", "gopls", "third"),
	})
	got := inv.Diagnostics(uri)
	if len(got) != 2 {
		t.Fatalf("after replace: want 2 diagnostics, got %d: %v", len(got), got)
	}
	if got[0].Message != "second" || got[1].Message != "third" {
		t.Fatalf("replace did not swap the snapshot: %v", got)
	}
	if id, ok := inv.PullResultID(uri); !ok || id != "rid-2" {
		t.Fatalf("PullResultID = (%q,%v), want (rid-2,true)", id, ok)
	}
}

// An empty full report clears the URI's pull snapshot, but the URI stays
// tracked (an empty full is a real "no issues" report) and its result ID is
// still updated.
func TestRecordPullFull_EmptyClears(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	inv.RecordPullFull(uri, "rid-1", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "boom")})
	if len(inv.Diagnostics(uri)) != 1 {
		t.Fatalf("precondition: expected one diagnostic")
	}

	inv.RecordPullFull(uri, "rid-2", []protocol.Diagnostic{})
	if got := inv.Diagnostics(uri); len(got) != 0 {
		t.Fatalf("empty full must clear the snapshot, got %v", got)
	}
	if !inv.Tracked(uri) {
		t.Fatal("an empty full is still a report — Tracked must stay true")
	}
	if id, ok := inv.PullResultID(uri); !ok || id != "rid-2" {
		t.Fatalf("empty full must still update the result ID, got (%q,%v)", id, ok)
	}
}

// RecordPullUnchanged with a matching result ID refreshes the timestamp only and
// leaves the stored diagnostics intact.
func TestRecordPullUnchanged_KnownResultID(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	inv.RecordPullFull(uri, "rid-1", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "boom")})
	t0 := inv.AllDiagnosticTimes()[uri]

	time.Sleep(2 * time.Millisecond)
	if ok := inv.RecordPullUnchanged(uri, "rid-1"); !ok {
		t.Fatal("RecordPullUnchanged with the stored result ID must return true")
	}
	t1 := inv.AllDiagnosticTimes()[uri]
	if !t1.After(t0) {
		t.Errorf("unchanged should refresh the timestamp: t0=%v t1=%v", t0, t1)
	}
	if got := inv.Diagnostics(uri); len(got) != 1 || got[0].Message != "boom" {
		t.Fatalf("unchanged must leave the snapshot intact, got %v", got)
	}
}

// SAFETY INVARIANT (cache level): RecordPullUnchanged with an unknown result ID
// returns false and MUTATES NOTHING — no snapshot is created, no timestamp is
// recorded, no result ID is stored, and any pre-existing push data is untouched.
func TestRecordPullUnchanged_UnknownResultID_MutatesNothing(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	// Case 1: no state at all for the URI.
	if ok := inv.RecordPullUnchanged(uri, "rid-unknown"); ok {
		t.Fatal("unchanged on an unknown result ID must return false")
	}
	if inv.Tracked(uri) {
		t.Fatal("unknown unchanged must not mark the URI tracked")
	}
	if _, ok := inv.PullResultID(uri); ok {
		t.Fatal("unknown unchanged must not record a result ID")
	}
	if d := inv.Diagnostics(uri); d != nil {
		t.Fatalf("unknown unchanged must not create diagnostics, got %v", d)
	}
	if _, ok := inv.AllDiagnosticTimes()[uri]; ok {
		t.Fatal("unknown unchanged must not record a timestamp")
	}

	// Case 2: URI has PUSH data — unchanged for an unknown pull ID must not
	// disturb it (a false clean must never overwrite real push diagnostics).
	pushDiagnostics(inv, uri, []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "push-boom")})
	before := inv.Diagnostics(uri)
	if ok := inv.RecordPullUnchanged(uri, "rid-unknown"); ok {
		t.Fatal("unchanged on an unknown result ID must return false even when push data exists")
	}
	if _, ok := inv.PullResultID(uri); ok {
		t.Fatal("unknown unchanged must not record a result ID")
	}
	after := inv.Diagnostics(uri)
	if len(after) != len(before) || after[0].Message != "push-boom" {
		t.Fatalf("unknown unchanged corrupted push data: before=%v after=%v", before, after)
	}
}

// RecordPullUnchanged with a result ID that does not match the stored one is
// treated as unknown: false, mutate nothing.
func TestRecordPullUnchanged_MismatchResultID_MutatesNothing(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	inv.RecordPullFull(uri, "rid-1", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "boom")})
	t0 := inv.AllDiagnosticTimes()[uri]

	time.Sleep(2 * time.Millisecond)
	if ok := inv.RecordPullUnchanged(uri, "rid-WRONG"); ok {
		t.Fatal("a mismatched result ID must return false")
	}
	if id, _ := inv.PullResultID(uri); id != "rid-1" {
		t.Fatalf("mismatch must not overwrite the stored result ID, got %q", id)
	}
	if t1 := inv.AllDiagnosticTimes()[uri]; !t1.Equal(t0) {
		t.Errorf("mismatch must not refresh the timestamp: t0=%v t1=%v", t0, t1)
	}
}

// A full report with an empty result ID stores the snapshot but records no
// known result ID (the client cannot cache against it), so a subsequent
// unchanged is always treated as unknown.
func TestPullResultID_EmptyResultID(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	inv.RecordPullFull(uri, "", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "boom")})
	if _, ok := inv.PullResultID(uri); ok {
		t.Fatal("an empty result ID must not count as a known result ID")
	}
	if len(inv.Diagnostics(uri)) != 1 {
		t.Fatal("an empty-result-ID full still records its diagnostics")
	}
	if ok := inv.RecordPullUnchanged(uri, ""); ok {
		t.Fatal("unchanged with an empty result ID must be treated as unknown")
	}
}

// The empty-URI boundary check mirrors Handle(): RecordPullFull for an empty URI
// records nothing.
func TestRecordPullFull_EmptyURI_NoMutation(t *testing.T) {
	inv := newInv(t)
	inv.RecordPullFull("", "rid", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "boom")})
	if inv.Tracked("") {
		t.Fatal("empty URI must not be tracked")
	}
	if len(inv.AllDiagnostics()) != 0 {
		t.Fatal("empty URI must not create any entry")
	}
}

// Mixed push+pull for one URI exposes the deduplicated union through the
// existing readers. The dedup key is URI+range+severity+code+source+message:
// near-miss diagnostics differing in exactly one component are all distinct.
func TestMixedPushPull_DedupExactKey(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	base := diag(1, protocol.SevError, "E100", "gopls", "boom")
	// Each variant differs from base in exactly one dedup-key component.
	vRange := diag(2, protocol.SevError, "E100", "gopls", "boom")
	vSev := diag(1, protocol.SevWarning, "E100", "gopls", "boom")
	vCode := diag(1, protocol.SevError, "E200", "gopls", "boom")
	vSource := diag(1, protocol.SevError, "E100", "vet", "boom")
	vMsg := diag(1, protocol.SevError, "E100", "gopls", "kaboom")

	pushDiagnostics(inv, uri, []protocol.Diagnostic{base})
	inv.RecordPullFull(uri, "rid-1", []protocol.Diagnostic{base, vRange, vSev, vCode, vSource, vMsg})

	got := inv.Diagnostics(uri)
	// base is identical across push and pull → deduped to one; each of the five
	// near-misses is distinct → union. 1 + 5 = 6.
	if len(got) != 6 {
		t.Fatalf("expected 6 deduped diagnostics (base once + 5 near-misses), got %d: %v", len(got), got)
	}

	// Every distinct key must be present exactly once.
	type key struct {
		line uint32
		sev  protocol.DiagnosticSeverity
		code any
		src  string
		msg  string
	}
	counts := map[key]int{}
	for _, d := range got {
		counts[key{d.Range.Start.Line, d.Severity, d.Code, d.Source, d.Message}]++
	}
	if counts[key{1, protocol.SevError, "E100", "gopls", "boom"}] != 1 {
		t.Errorf("base should appear exactly once, got %d", counts[key{1, protocol.SevError, "E100", "gopls", "boom"}])
	}
	for _, k := range []key{
		{2, protocol.SevError, "E100", "gopls", "boom"},
		{1, protocol.SevWarning, "E100", "gopls", "boom"},
		{1, protocol.SevError, "E200", "gopls", "boom"},
		{1, protocol.SevError, "E100", "vet", "boom"},
		{1, protocol.SevError, "E100", "gopls", "kaboom"},
	} {
		if counts[k] != 1 {
			t.Errorf("near-miss %+v should appear exactly once, got %d", k, counts[k])
		}
	}
}

// Distinct push and pull diagnostics union deterministically, push first.
func TestMixedPushPull_Union_Deterministic(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	a := diag(1, protocol.SevError, "E1", "gopls", "push-only")
	b := diag(2, protocol.SevWarning, "E2", "gopls", "pull-only")

	pushDiagnostics(inv, uri, []protocol.Diagnostic{a})
	inv.RecordPullFull(uri, "rid-1", []protocol.Diagnostic{b})

	got := inv.Diagnostics(uri)
	if len(got) != 2 {
		t.Fatalf("expected the union of 2 distinct diagnostics, got %v", got)
	}
	if got[0].Message != "push-only" || got[1].Message != "pull-only" {
		t.Fatalf("union order must be push-first then pull: %v", got)
	}
	if _, ok := inv.AllDiagnostics()[uri]; !ok {
		t.Fatal("AllDiagnostics must include the mixed URI")
	}
	if all := inv.AllDiagnostics()[uri]; len(all) != 2 {
		t.Fatalf("AllDiagnostics must expose the same 2-way union, got %v", all)
	}
}

// DiagnosticSources reports, per URI, which acquisition channels contributed —
// deterministic push-before-pull ordering, empty when untracked.
func TestDiagnosticSources(t *testing.T) {
	inv := newInv(t)

	pushOnly := "file:///p/push.go"
	pullOnly := "file:///p/pull.go"
	both := "file:///p/both.go"

	pushDiagnostics(inv, pushOnly, []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "p")})
	inv.RecordPullFull(pullOnly, "rid", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "q")})
	pushDiagnostics(inv, both, []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "r")})
	inv.RecordPullFull(both, "rid", []protocol.Diagnostic{diag(2, protocol.SevError, "E2", "gopls", "s")})

	if got := inv.DiagnosticSources(pushOnly); len(got) != 1 || got[0] != cache.SourcePush {
		t.Errorf("push-only sources = %v, want [lsp-push]", got)
	}
	if got := inv.DiagnosticSources(pullOnly); len(got) != 1 || got[0] != cache.SourcePull {
		t.Errorf("pull-only sources = %v, want [lsp-pull]", got)
	}
	if got := inv.DiagnosticSources(both); len(got) != 2 || got[0] != cache.SourcePush || got[1] != cache.SourcePull {
		t.Errorf("hybrid sources = %v, want [lsp-push lsp-pull]", got)
	}
	if got := inv.DiagnosticSources("file:///p/never.go"); len(got) != 0 {
		t.Errorf("untracked sources = %v, want empty", got)
	}
}

// ClearPullState drops all pull result IDs and pull snapshots but leaves push
// diagnostics (which a fresh server re-publishes) untouched.
func TestClearPullState(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	pushDiagnostics(inv, uri, []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "push")})
	inv.RecordPullFull(uri, "rid-1", []protocol.Diagnostic{diag(2, protocol.SevWarning, "E2", "gopls", "pull")})

	if len(inv.Diagnostics(uri)) != 2 {
		t.Fatalf("precondition: expected the 2-way union")
	}

	inv.ClearPullState()

	if id, ok := inv.PullResultID(uri); ok {
		t.Errorf("ClearPullState must drop result IDs, got %q", id)
	}
	got := inv.Diagnostics(uri)
	if len(got) != 1 || got[0].Message != "push" {
		t.Fatalf("ClearPullState must leave only push data, got %v", got)
	}
	if !inv.Tracked(uri) {
		t.Error("push data keeps the URI tracked after ClearPullState")
	}
	if src := inv.DiagnosticSources(uri); len(src) != 1 || src[0] != cache.SourcePush {
		t.Errorf("after clear, sources = %v, want [lsp-push]", src)
	}
	if _, ok := inv.AllDiagnosticTimes()[uri]; !ok {
		t.Error("push timestamp must survive ClearPullState")
	}
}

// RecordPullResult routes the primary URI and every related document through the
// same full/unchanged path AND the same empty-URI boundary check.
func TestRecordPullResult_RelatedDocsBoundaryChecks(t *testing.T) {
	inv := newInv(t)
	mainURI := "file:///p/main.go"
	relURI := "file:///p/related.go"

	report := protocol.DocumentDiagnosticReport{
		Kind:     protocol.DiagnosticReportFull,
		ResultID: "rid-main",
		Items:    []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "main-boom")},
		RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
			relURI: {
				Kind:     protocol.DiagnosticReportFull,
				ResultID: "rid-rel",
				Items:    []protocol.Diagnostic{diag(2, protocol.SevWarning, "E2", "gopls", "rel-boom")},
			},
			"": { // empty URI related doc — must be skipped by the boundary check
				Kind:  protocol.DiagnosticReportFull,
				Items: []protocol.Diagnostic{diag(3, protocol.SevError, "E3", "gopls", "bad")},
			},
		},
	}
	inv.RecordPullResult(mainURI, report)

	if got := inv.Diagnostics(mainURI); len(got) != 1 || got[0].Message != "main-boom" {
		t.Fatalf("primary URI not recorded: %v", got)
	}
	if id, ok := inv.PullResultID(mainURI); !ok || id != "rid-main" {
		t.Fatalf("primary result ID = (%q,%v), want rid-main", id, ok)
	}
	if got := inv.Diagnostics(relURI); len(got) != 1 || got[0].Message != "rel-boom" {
		t.Fatalf("related URI not recorded through the same path: %v", got)
	}
	if id, ok := inv.PullResultID(relURI); !ok || id != "rid-rel" {
		t.Fatalf("related result ID = (%q,%v), want rid-rel", id, ok)
	}
	if inv.Tracked("") {
		t.Fatal("the empty-URI related doc must be skipped by the boundary check")
	}
}

// A related document reported as unchanged refreshes only when its result ID is
// known; an unknown one mutates nothing (safety invariant applied to related
// docs too).
func TestRecordPullResult_RelatedUnchanged(t *testing.T) {
	inv := newInv(t)
	mainURI := "file:///p/main.go"
	relURI := "file:///p/related.go"
	unseenURI := "file:///p/unseen.go"

	// relURI already has a full pull snapshot under rid-rel.
	inv.RecordPullFull(relURI, "rid-rel", []protocol.Diagnostic{diag(2, protocol.SevWarning, "E2", "gopls", "rel-boom")})
	t0 := inv.AllDiagnosticTimes()[relURI]
	time.Sleep(2 * time.Millisecond)

	report := protocol.DocumentDiagnosticReport{
		Kind:     protocol.DiagnosticReportFull,
		ResultID: "rid-main",
		Items:    []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "main-boom")},
		RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
			relURI:    {Kind: protocol.DiagnosticReportUnchanged, ResultID: "rid-rel"},
			unseenURI: {Kind: protocol.DiagnosticReportUnchanged, ResultID: "rid-never"},
		},
	}
	inv.RecordPullResult(mainURI, report)

	// Known related unchanged: snapshot intact, timestamp refreshed.
	if got := inv.Diagnostics(relURI); len(got) != 1 || got[0].Message != "rel-boom" {
		t.Fatalf("known related unchanged must keep the snapshot, got %v", got)
	}
	if t1 := inv.AllDiagnosticTimes()[relURI]; !t1.After(t0) {
		t.Errorf("known related unchanged must refresh timestamp: t0=%v t1=%v", t0, t1)
	}
	// Unknown related unchanged: nothing recorded.
	if inv.Tracked(unseenURI) {
		t.Fatal("unknown related unchanged must not create state")
	}
}

// An unrecognised report kind never clears an existing snapshot (a malformed
// success report must not read as a false clean).
func TestRecordPullResult_UnknownKind_NoMutation(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/a.go"

	inv.RecordPullFull(uri, "rid-1", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "boom")})
	inv.RecordPullResult(uri, protocol.DocumentDiagnosticReport{Kind: "garbage"})

	if got := inv.Diagnostics(uri); len(got) != 1 || got[0].Message != "boom" {
		t.Fatalf("unknown report kind must not clear the snapshot, got %v", got)
	}
	if id, ok := inv.PullResultID(uri); !ok || id != "rid-1" {
		t.Fatalf("unknown report kind must not touch the result ID, got (%q,%v)", id, ok)
	}
}

// -race: concurrent push (Handle) and pull (RecordPullFull/Unchanged) writes on
// one URI, plus concurrent readers, must be data-race free.
func TestRace_PushAndPullOnOneURI(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/race.go"
	// Seed a result ID so RecordPullUnchanged has something to match.
	inv.RecordPullFull(uri, "rid-seed", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "seed")})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(5)
		go func() {
			defer wg.Done()
			pushDiagnostics(inv, uri, []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "push")})
		}()
		go func() {
			defer wg.Done()
			inv.RecordPullFull(uri, "rid-seed", []protocol.Diagnostic{diag(2, protocol.SevWarning, "E2", "gopls", "pull")})
		}()
		go func() { defer wg.Done(); inv.RecordPullUnchanged(uri, "rid-seed") }()
		go func() { defer wg.Done(); _ = inv.Diagnostics(uri); _ = inv.DiagnosticSources(uri) }()
		go func() { defer wg.Done(); _ = inv.AllDiagnostics(); _ = inv.AllDiagnosticTimes() }()
	}
	wg.Wait()
}

// -race: ClearPullState concurrent with readers and writers.
func TestRace_ClearPullStateConcurrent(t *testing.T) {
	inv := newInv(t)
	uri := "file:///p/race.go"

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(4)
		go func() {
			defer wg.Done()
			inv.RecordPullFull(uri, "rid", []protocol.Diagnostic{diag(1, protocol.SevError, "E1", "gopls", "pull")})
		}()
		go func() { defer wg.Done(); inv.ClearPullState() }()
		go func() { defer wg.Done(); _, _ = inv.PullResultID(uri) }()
		go func() { defer wg.Done(); _ = inv.Diagnostics(uri) }()
	}
	wg.Wait()
}
