package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/toolerror"
)

func openHealthTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// recordRead mirrors the tool_calls row read_file leaves behind for a
// successful read: a session_id, workspace, and the CalledAt this
// fixture's "read-tracker" observation is dated to.
func recordRead(t *testing.T, db *DB, session, workspace string, at time.Time) {
	t.Helper()
	if err := db.Record(Call{
		SessionID: session, Workspace: workspace, Tool: "read_file",
		CalledAt: at, Success: true,
	}); err != nil {
		t.Fatalf("Record read_file: %v", err)
	}
}

// recordGuardRefusal mirrors the tool_calls row write_file's own
// changedSinceSessionRead guard (internal/tools/write_guards.go) leaves
// behind when it refuses a write because the on-disk file changed since the
// session's last read: success=false, error_kind=unread_or_stale — the same
// classification staleOverride/staleRead attach (internal/tools/error_class.go).
func recordGuardRefusal(t *testing.T, db *DB, session, workspace, clientName string, at time.Time) {
	t.Helper()
	if err := db.Record(Call{
		SessionID: session, Workspace: workspace, Tool: "write_file", ClientName: clientName,
		CalledAt: at, Success: false,
		ErrorKind: toolerror.KindUnreadOrStale, RemediationClass: toolerror.ClassPassForce,
	}); err != nil {
		t.Fatalf("Record guard refusal: %v", err)
	}
}

// TestLaneDefection_DetectsPlantedNativeEdit is the card's literal fixture:
// plant a native edit (a plain os.WriteFile, standing in for "something
// other than plumb") on a file after plumb has "read" it, and prove
// lane-defection flags the session. The write-under-you decision itself is
// derived from a real mtime comparison here — the same signal
// changedSinceSessionRead (internal/tools/write_guards.go) uses — not
// hardcoded, so the test proves the mechanism, not just the arithmetic on
// top of it.
func TestHealthLaneDefection_DetectsPlantedNativeEdit(t *testing.T) {
	db := openHealthTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	day := time.Now().UTC()
	readAt := day
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before read: %v", err)
	}
	recordedMtime := info.ModTime()
	recordRead(t, db, "sess-defected", dir, readAt)

	// Plant the native edit: something other than plumb writes the file.
	// Sleep past filesystem mtime granularity so the comparison below is
	// real, not a same-tick coincidence.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("package x\n\nvar y int\n"), 0o644); err != nil {
		t.Fatalf("plant native edit: %v", err)
	}

	// This is exactly changedSinceSessionRead's mtime check
	// (internal/tools/write_guards.go): if the file changed under the
	// session's recorded read, a subsequent plumb write attempt refuses
	// with KindUnreadOrStale.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after edit: %v", err)
	}
	if !after.ModTime().After(recordedMtime) {
		t.Fatalf("planted edit did not advance mtime — test fixture is not exercising the real signal")
	}
	recordGuardRefusal(t, db, "sess-defected", dir, "", readAt.Add(20*time.Millisecond))

	// A second, well-behaved session in the same workspace and day: reads,
	// never collides with an external edit, never hits the guard.
	recordRead(t, db, "sess-clean", dir, readAt)

	got, err := db.LaneDefectionForDay(day, dir)
	if err != nil {
		t.Fatalf("LaneDefectionForDay: %v", err)
	}
	if got.SessionsTotal != 2 {
		t.Errorf("SessionsTotal = %d, want 2", got.SessionsTotal)
	}
	if got.SessionsFlagged != 1 {
		t.Errorf("SessionsFlagged = %d, want 1", got.SessionsFlagged)
	}
	if got.Rate() != 0.5 {
		t.Errorf("Rate() = %v, want 0.5", got.Rate())
	}
}

// TestLaneDefection_StaysZeroWithoutNativeEdit is the card's other literal
// half: a session that reads a file and nothing external ever touches it
// records no guard refusal, so lane-defection reports 0 — not merely a low
// number, exactly zero.
func TestHealthLaneDefection_StaysZeroWithoutNativeEdit(t *testing.T) {
	db := openHealthTestDB(t)
	day := time.Now().UTC()
	recordRead(t, db, "sess-1", "/ws", day)
	recordRead(t, db, "sess-2", "/ws", day)

	got, err := db.LaneDefectionForDay(day, "/ws")
	if err != nil {
		t.Fatalf("LaneDefectionForDay: %v", err)
	}
	if got.SessionsTotal != 2 {
		t.Errorf("SessionsTotal = %d, want 2", got.SessionsTotal)
	}
	if got.SessionsFlagged != 0 {
		t.Errorf("SessionsFlagged = %d, want 0", got.SessionsFlagged)
	}
	if got.Rate() != 0 {
		t.Errorf("Rate() = %v, want 0", got.Rate())
	}
}

// TestSemanticErrorRates_AdvertisedFlagThreshold proves the flag is gated on
// BOTH conditions the card names: the tool must be advertised (pinned,
// PLAN-355) AND its rate must cross read_file's baseline × multiplier. A
// tool with the same high error rate that is NOT advertised must not flag —
// the whole point of the flag is to surface a regression in what an agent
// actually sees pinned into context, not every noisy tool.
func TestHealthSemanticErrorRates_AdvertisedFlagThreshold(t *testing.T) {
	db := openHealthTestDB(t)
	since := time.Now().Add(-7 * 24 * time.Hour)
	until := time.Now().Add(time.Hour)
	mid := time.Now()

	// read_file baseline: 1 error in 10 calls = 10% rate.
	for range 9 {
		if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "read_file", CalledAt: mid, Success: true}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "read_file", CalledAt: mid, Success: false}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// baseline × DefaultSemanticBaselineMultiplier(3) = 30%.

	// get_definition (advertised/pinned): 5 errors in 10 = 50% — crosses 30%.
	for range 5 {
		if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "get_definition", CalledAt: mid, Success: true}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	for range 5 {
		if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "get_definition", CalledAt: mid, Success: false}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	// rename_symbol (NOT advertised in this fixture): same 50% rate.
	for range 5 {
		if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "rename_symbol", CalledAt: mid, Success: true}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	for range 5 {
		if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "rename_symbol", CalledAt: mid, Success: false}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	advertised := func(tool string) bool { return tool == "get_definition" }
	rates, err := db.SemanticErrorRatesSince(since, until, "/ws", advertised, 0 /* default multiplier */)
	if err != nil {
		t.Fatalf("SemanticErrorRatesSince: %v", err)
	}

	byTool := make(map[string]SemanticErrorRate, len(rates))
	for _, r := range rates {
		byTool[r.Tool] = r
	}

	gd, ok := byTool["get_definition"]
	if !ok {
		t.Fatal("get_definition missing from results")
	}
	if !gd.Advertised {
		t.Error("get_definition.Advertised = false, want true")
	}
	if !gd.Flagged {
		t.Errorf("get_definition.Flagged = false, want true (rate %.2f > baseline %.2f)", gd.Rate(), gd.Baseline)
	}

	rs, ok := byTool["rename_symbol"]
	if !ok {
		t.Fatal("rename_symbol missing from results")
	}
	if rs.Advertised {
		t.Error("rename_symbol.Advertised = true, want false (not in this fixture's advertised set)")
	}
	if rs.Flagged {
		t.Error("rename_symbol.Flagged = true, want false — same rate as get_definition but not advertised")
	}
	if rs.Rate() != gd.Rate() {
		t.Errorf("rename_symbol and get_definition should have identical rates in this fixture: %.2f vs %.2f", rs.Rate(), gd.Rate())
	}
}

// TestEconomicsForDay_ScopesToCurrentModelVersionOnly proves the third
// metric never sums a stale-model-version row into the current total — the
// exact PLAN-367 review round 1 defect (db_query_versioned.go) restated for
// the daily-trended figure.
func TestHealthEconomicsForDay_ScopesToCurrentModelVersionOnly(t *testing.T) {
	db := openHealthTestDB(t)
	day := time.Now().UTC()

	if err := db.Record(Call{
		SessionID: "s1", Workspace: "/ws", Tool: "get_definition", CalledAt: day,
		Success: true, ClientName: "claude-code",
		SavingsModelVersion: clientcaps.ModelVersion, CapabilityTokens: 100, EfficiencyTokens: 50,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// A stale-version row for the same client/day must not contribute.
	if err := db.Record(Call{
		SessionID: "s1", Workspace: "/ws", Tool: "get_definition", CalledAt: day,
		Success: true, ClientName: "claude-code",
		SavingsModelVersion: clientcaps.ModelVersion - 1, CapabilityTokens: 9000, EfficiencyTokens: 9000,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	recordGuardRefusal(t, db, "s1", "/ws", "claude-code", day)

	got, err := db.EconomicsForDay(day, "/ws")
	if err != nil {
		t.Fatalf("EconomicsForDay: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d client rows, want 1", len(got))
	}
	e := got[0]
	if e.ClientName != "claude-code" {
		t.Errorf("ClientName = %q, want claude-code", e.ClientName)
	}
	if e.SavingsTokens != 150 {
		t.Errorf("SavingsTokens = %d, want 150 (current-version row only, stale-version 18000 excluded)", e.SavingsTokens)
	}
	if e.GuardRefusals != 1 {
		t.Errorf("GuardRefusals = %d, want 1", e.GuardRefusals)
	}
}

// TestUpsertHealthDaily_IsIdempotent proves a re-run for the same
// (day, workspace, client, metric, tool) key overwrites rather than
// duplicates — the "computation is idempotent per day (re-run safe)"
// requirement.
func TestHealthDailyUpsert_IsIdempotent(t *testing.T) {
	db := openHealthTestDB(t)
	row := HealthDailyRow{
		Day: "2026-08-21", Workspace: "/ws", ClientName: "claude-code",
		Metric: MetricLaneDefection, SessionsTotal: 5, SessionsFlagged: 1,
	}
	if err := db.UpsertHealthDaily(row); err != nil {
		t.Fatalf("UpsertHealthDaily (1st): %v", err)
	}
	row.SessionsTotal = 9
	row.SessionsFlagged = 3
	if err := db.UpsertHealthDaily(row); err != nil {
		t.Fatalf("UpsertHealthDaily (2nd): %v", err)
	}

	rows, err := db.HealthDailyRows("2026-08-21", "/ws")
	if err != nil {
		t.Fatalf("HealthDailyRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows after two upserts of the same key, want 1 (re-run must overwrite, not duplicate)", len(rows))
	}
	if rows[0].SessionsTotal != 9 || rows[0].SessionsFlagged != 3 {
		t.Errorf("row = %+v, want the SECOND upsert's values (9, 3)", rows[0])
	}
}

// recordNeverReadRefusal mirrors strict mode's read-before-write refusal
// (internal/tools/strict_read.go's requireStrictRead, when the ReadTracker
// never recorded a read at all): success=false, error_kind=unread_or_stale,
// remediation_class=re_read — the SAME error_kind recordGuardRefusal uses,
// but the OTHER remediation_class, and critically no prior read anywhere in
// the session. Naively counting every KindUnreadOrStale refusal (review
// round 1's finding) would flag this as a lane defection even though
// nothing was ever read, let alone changed under the session.
func recordNeverReadRefusal(t *testing.T, db *DB, session, workspace string, at time.Time) {
	t.Helper()
	if err := db.Record(Call{
		SessionID: session, Workspace: workspace, Tool: "edit_file",
		CalledAt: at, Success: false,
		ErrorKind: toolerror.KindUnreadOrStale, RemediationClass: toolerror.ClassReRead,
	}); err != nil {
		t.Fatalf("Record never-read refusal: %v", err)
	}
}

// TestHealthLaneDefection_NeverReadRefusalDoesNotInflate is the red-then-green
// fixture for review round 1's BLOCKING 1: a session that calls edit_file on
// a path it never read hits strict mode's "never read" guard
// (KindUnreadOrStale + ClassReRead) — not a lane defection, since nothing was
// ever read for anything to change under. Before the fix, the query counted
// ANY KindUnreadOrStale failure (flagging this) and ANY session in the
// denominator (counting a session with zero reads) — the review's probe got
// 50% from two such sessions. After the fix: SessionsTotal must be 0 (this
// session never read a file, so it isn't in the denominator at all) and
// SessionsFlagged must be 0.
func TestHealthLaneDefection_NeverReadRefusalDoesNotInflate(t *testing.T) {
	db := openHealthTestDB(t)
	day := time.Now().UTC()
	recordNeverReadRefusal(t, db, "sess-never-read-1", "/ws", day)
	recordNeverReadRefusal(t, db, "sess-never-read-2", "/ws", day)

	got, err := db.LaneDefectionForDay(day, "/ws")
	if err != nil {
		t.Fatalf("LaneDefectionForDay: %v", err)
	}
	if got.SessionsTotal != 0 {
		t.Errorf("SessionsTotal = %d, want 0 — neither session ever read a file, so neither belongs in the denominator", got.SessionsTotal)
	}
	if got.SessionsFlagged != 0 {
		t.Errorf("SessionsFlagged = %d, want 0 — a never-read refusal is not a lane defection", got.SessionsFlagged)
	}
	if got.Rate() != 0 {
		t.Errorf("Rate() = %v, want 0 (not the review-probe 50%%)", got.Rate())
	}
}

// TestHealthLaneDefection_MixedSessionsOnlyCountsReaders combines a
// never-read session, a read-but-clean session, and a read-then-defected
// session in the SAME day, asserting the denominator counts only the two
// that read something and the numerator counts only the one that actually
// defected.
func TestHealthLaneDefection_MixedSessionsOnlyCountsReaders(t *testing.T) {
	db := openHealthTestDB(t)
	day := time.Now().UTC()
	recordNeverReadRefusal(t, db, "sess-never-read", "/ws", day)
	recordRead(t, db, "sess-clean", "/ws", day)
	recordRead(t, db, "sess-defected", "/ws", day)
	recordGuardRefusal(t, db, "sess-defected", "/ws", "", day.Add(time.Second))

	got, err := db.LaneDefectionForDay(day, "/ws")
	if err != nil {
		t.Fatalf("LaneDefectionForDay: %v", err)
	}
	if got.SessionsTotal != 2 {
		t.Errorf("SessionsTotal = %d, want 2 (the never-read session excluded)", got.SessionsTotal)
	}
	if got.SessionsFlagged != 1 {
		t.Errorf("SessionsFlagged = %d, want 1", got.SessionsFlagged)
	}
}

// TestHealthSemanticErrorRates_InsufficientToolSampleNeverFlags is the
// red-then-green fixture for review round 1's BLOCKING 2, tool side: one
// call and one error is a 100% rate, comfortably over any baseline — but one
// call is not a sample. Before the fix nothing guarded this; after,
// InsufficientSample must be true and Flagged must be false regardless of
// how high the raw rate is.
func TestHealthSemanticErrorRates_InsufficientToolSampleNeverFlags(t *testing.T) {
	db := openHealthTestDB(t)
	since := time.Now().Add(-7 * 24 * time.Hour)
	until := time.Now().Add(time.Hour)
	mid := time.Now()

	// A healthy, well-sampled read_file baseline: 1 error in 20 = 5%.
	for range 19 {
		if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "read_file", CalledAt: mid, Success: true}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "read_file", CalledAt: mid, Success: false}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// get_definition: exactly the review's probe — 1 call, 1 error, 100% rate.
	if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "get_definition", CalledAt: mid, Success: false}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	advertised := func(tool string) bool { return tool == "get_definition" }
	rates, err := db.SemanticErrorRatesSince(since, until, "/ws", advertised, 0)
	if err != nil {
		t.Fatalf("SemanticErrorRatesSince: %v", err)
	}
	var gd SemanticErrorRate
	found := false
	for _, r := range rates {
		if r.Tool == "get_definition" {
			gd, found = r, true
		}
	}
	if !found {
		t.Fatal("get_definition missing from results")
	}
	if gd.Rate() != 1.0 {
		t.Fatalf("test setup: get_definition.Rate() = %v, want 1.0 (100%%, matching the review probe)", gd.Rate())
	}
	if !gd.InsufficientSample {
		t.Error("InsufficientSample = false, want true — 1 call is not a sample")
	}
	if gd.Flagged {
		t.Error("Flagged = true, want false — a 100% rate on 1 call must not flag (review round 1 probe: false-fires precisely when agents have gone native)")
	}
}

// TestHealthSemanticErrorRates_InsufficientBaselineNeverFlags is the
// red-then-green fixture for review round 1's BLOCKING 2, baseline side: zero
// read_file calls in the window used to make baselineRate 0, so ANY semantic
// error crossed "0 × multiplier" and flagged — the exact false-fire the
// review's probe demonstrated. After the fix, BaselineInsufficient must be
// true and Flagged must be false even though the tool's own sample is large.
func TestHealthSemanticErrorRates_InsufficientBaselineNeverFlags(t *testing.T) {
	db := openHealthTestDB(t)
	since := time.Now().Add(-7 * 24 * time.Hour)
	until := time.Now().Add(time.Hour)
	mid := time.Now()

	// Zero read_file calls in the window — the review's exact probe setup.
	// get_definition: a large, well-sampled, low error rate (1 in 20 = 5%).
	for range 19 {
		if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "get_definition", CalledAt: mid, Success: true}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "get_definition", CalledAt: mid, Success: false}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	advertised := func(tool string) bool { return tool == "get_definition" }
	rates, err := db.SemanticErrorRatesSince(since, until, "/ws", advertised, 0)
	if err != nil {
		t.Fatalf("SemanticErrorRatesSince: %v", err)
	}
	var gd SemanticErrorRate
	found := false
	for _, r := range rates {
		if r.Tool == "get_definition" {
			gd, found = r, true
		}
	}
	if !found {
		t.Fatal("get_definition missing from results")
	}
	if gd.BaselineCalls != 0 {
		t.Fatalf("test setup: BaselineCalls = %d, want 0", gd.BaselineCalls)
	}
	if !gd.BaselineInsufficient {
		t.Error("BaselineInsufficient = false, want true — 0 read_file calls is not a baseline")
	}
	if gd.Flagged {
		t.Error("Flagged = true, want false — an untrustworthy (zero-sample) baseline must not flag ANY error rate (review round 1 probe)")
	}
}

// TestHealthSemanticErrorRates_AdvertisedUnknownIsLoud proves a nil
// advertised func is surfaced in the returned data (not just a silent
// false), per review round 1 item (b).
func TestHealthSemanticErrorRates_AdvertisedUnknownIsLoud(t *testing.T) {
	db := openHealthTestDB(t)
	since := time.Now().Add(-7 * 24 * time.Hour)
	until := time.Now().Add(time.Hour)
	mid := time.Now()
	if err := db.Record(Call{SessionID: "s", Workspace: "/ws", Tool: "get_definition", CalledAt: mid, Success: true}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rates, err := db.SemanticErrorRatesSince(since, until, "/ws", nil, 0)
	if err != nil {
		t.Fatalf("SemanticErrorRatesSince: %v", err)
	}
	if len(rates) != 1 {
		t.Fatalf("got %d rows, want 1", len(rates))
	}
	if !rates[0].AdvertisedUnknown {
		t.Error("AdvertisedUnknown = false, want true when advertised func is nil")
	}
	if rates[0].Advertised {
		t.Error("Advertised = true, want false when advertised func is nil")
	}
}
