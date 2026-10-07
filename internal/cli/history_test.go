package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// 1. Days: "7d"
	t7d, err := parseWhen("7d", now)
	if err != nil {
		t.Fatalf("parseWhen(7d): %v", err)
	}
	want7d := now.Add(-7 * 24 * time.Hour)
	if !t7d.Equal(want7d) {
		t.Errorf("parseWhen(7d) = %v, want %v", t7d, want7d)
	}

	// 2. Duration: "2h"
	t2h, err := parseWhen("2h", now)
	if err != nil {
		t.Fatalf("parseWhen(2h): %v", err)
	}
	want2h := now.Add(-2 * time.Hour)
	if !t2h.Equal(want2h) {
		t.Errorf("parseWhen(2h) = %v, want %v", t2h, want2h)
	}

	// 3. Date: "2026-10-01"
	tDate, err := parseWhen("2026-10-01", now)
	if err != nil {
		t.Fatalf("parseWhen(2026-10-01): %v", err)
	}
	wantDate := time.Date(2026, 10, 1, 0, 0, 0, 0, now.Location())
	if !tDate.Equal(wantDate) {
		t.Errorf("parseWhen(2026-10-01) = %v, want %v", tDate, wantDate)
	}

	// 4. RFC 3339
	rfc := "2026-09-30T15:04:05Z"
	tRfc, err := parseWhen(rfc, now)
	if err != nil {
		t.Fatalf("parseWhen(RFC 3339): %v", err)
	}
	if tRfc.Format(time.RFC3339) != rfc {
		t.Errorf("parseWhen(RFC 3339) = %v, want %v", tRfc.Format(time.RFC3339), rfc)
	}

	// 5. Bogus
	if _, err := parseWhen("bogus", now); err == nil {
		t.Errorf("parseWhen(bogus) expected error, got nil")
	}
}

func TestHistoryListShowsNewestFirstWithGapLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	ws := t.TempDir()

	st, err := history.Open(history.DBPath(), history.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Two edits on f.txt with divergent SHA in between -> gap
	st.Enqueue(history.Item{
		Change: history.Change{
			Op:    history.OpCreate,
			Tool:  "write_file",
			Path:  filepath.Join(ws, "f.txt"),
			At:    time.Now().Add(-2 * time.Minute),
			After: history.SideFromBytes([]byte("line1\n")),
			Kind:  history.KindFile,
		},
		Workspace: ws,
		CallID:    "01CALL1",
	})
	st.Enqueue(history.Item{
		Change: history.Change{
			Op:     history.OpUpdate,
			Tool:   "edit_file",
			Path:   filepath.Join(ws, "f.txt"),
			At:     time.Now().Add(-1 * time.Minute),
			Before: history.SideFromBytes([]byte("out-of-band\n")),
			After:  history.SideFromBytes([]byte("line2\n")),
			Kind:   history.KindFile,
		},
		Workspace: ws,
		CallID:    "01CALL2",
	})
	if err := st.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = st.Close(context.Background())

	oldWs, oldAll, oldJSON := historyFlagWorkspace, historyFlagAll, historyFlagJSON
	defer func() {
		historyFlagWorkspace, historyFlagAll, historyFlagJSON = oldWs, oldAll, oldJSON
	}()
	historyFlagWorkspace, historyFlagAll, historyFlagJSON = ws, false, false

	out := captureStdout(t, func() {
		if err := runHistoryList(nil, nil); err != nil {
			t.Fatalf("runHistoryList: %v", err)
		}
	})

	if !strings.Contains(out, "f.txt") {
		t.Fatalf("output missing f.txt:\n%s", out)
	}
	if !strings.Contains(out, "⋯ unrecorded change (outside plumb's write tools)") {
		t.Fatalf("output missing gap line:\n%s", out)
	}
}

func TestHistoryListAcrossPathSpellings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	realDir := t.TempDir()
	linkParent := t.TempDir()
	symDir := filepath.Join(linkParent, "symlink-ws")
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Fatal(err)
	}

	st, err := history.Open(history.DBPath(), history.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Seed with symlink spelling
	st.Enqueue(history.Item{
		Change: history.Change{
			Op:    history.OpCreate,
			Tool:  "write_file",
			Path:  filepath.Join(symDir, "f.txt"),
			At:    time.Now(),
			After: history.SideFromBytes([]byte("content\n")),
			Kind:  history.KindFile,
		},
		Workspace: symDir,
		CallID:    "01CALLSYM",
	})
	if err := st.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = st.Close(context.Background())

	oldWs, oldFile, oldJSON := historyFlagWorkspace, historyFlagFile, historyFlagJSON
	defer func() {
		historyFlagWorkspace, historyFlagFile, historyFlagJSON = oldWs, oldFile, oldJSON
	}()
	historyFlagWorkspace, historyFlagFile, historyFlagJSON = realDir, filepath.Join(symDir, "f.txt"), false

	out := captureStdout(t, func() {
		if err := runHistoryList(nil, nil); err != nil {
			t.Fatalf("runHistoryList: %v", err)
		}
	})

	if !strings.Contains(out, "f.txt") {
		t.Fatalf("output missing f.txt across symlink spellings:\n%s", out)
	}
}

func TestHistoryShowByCallPrintsStatsAndDiffs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	ws := t.TempDir()

	sdb, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	if err := sdb.Record(stats.Call{
		CallID:      "C1",
		Tool:        "write_file",
		Workspace:   ws,
		SessionID:   "sess-1",
		SessionName: "swift-falcon",
		DurationMs:  42,
		Success:     true,
		CalledAt:    time.Now(),
	}); err != nil {
		t.Fatalf("sdb.Record: %v", err)
	}
	sdb.Close()

	st, err := history.Open(history.DBPath(), history.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Enqueue(history.Item{
		Change: history.Change{
			Op:     history.OpUpdate,
			Tool:   "write_file",
			Path:   filepath.Join(ws, "f.txt"),
			At:     time.Now(),
			Before: history.SideFromBytes([]byte("before\n")),
			After:  history.SideFromBytes([]byte("after\n")),
			Kind:   history.KindFile,
		},
		Workspace: ws,
		CallID:    "C1",
	})
	if err := st.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = st.Close(context.Background())

	oldJSON := historyFlagJSON
	defer func() { historyFlagJSON = oldJSON }()
	historyFlagJSON = false

	out := captureStdout(t, func() {
		if err := runHistoryShow(nil, []string{"C1"}); err != nil {
			t.Fatalf("runHistoryShow: %v", err)
		}
	})

	if !strings.Contains(out, "write_file") || !strings.Contains(out, "42ms") {
		t.Fatalf("output missing call metadata:\n%s", out)
	}
	if !strings.Contains(out, "--- a/f.txt") || !strings.Contains(out, "+++ b/f.txt") {
		t.Fatalf("output missing diff header:\n%s", out)
	}
}

func TestHistoryShowWithoutStatsStillPrintsDiffs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	ws := t.TempDir()

	st, err := history.Open(history.DBPath(), history.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Enqueue(history.Item{
		Change: history.Change{
			Op:     history.OpUpdate,
			Tool:   "edit_file",
			Path:   filepath.Join(ws, "f.txt"),
			At:     time.Now(),
			Before: history.SideFromBytes([]byte("a\n")),
			After:  history.SideFromBytes([]byte("b\n")),
			Kind:   history.KindFile,
		},
		Workspace: ws,
		CallID:    "C2",
	})
	if err := st.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = st.Close(context.Background())

	oldJSON := historyFlagJSON
	defer func() { historyFlagJSON = oldJSON }()
	historyFlagJSON = false

	out := captureStdout(t, func() {
		if err := runHistoryShow(nil, []string{"C2"}); err != nil {
			t.Fatalf("runHistoryShow: %v", err)
		}
	})

	if !strings.Contains(out, "(call metadata unavailable: stats.db has no row for this call)") {
		t.Fatalf("output missing unavailable note:\n%s", out)
	}
	if !strings.Contains(out, "--- a/f.txt") || !strings.Contains(out, "+++ b/f.txt") {
		t.Fatalf("output missing diff header:\n%s", out)
	}
}

func TestHistoryListJSONShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	ws := t.TempDir()

	st, err := history.Open(history.DBPath(), history.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Enqueue(history.Item{
		Change: history.Change{
			Op:    history.OpCreate,
			Tool:  "write_file",
			Path:  filepath.Join(ws, "f.txt"),
			At:    time.Now(),
			After: history.SideFromBytes([]byte("content\n")),
			Kind:  history.KindFile,
		},
		Workspace: ws,
		CallID:    "01CALLJSON",
	})
	if err := st.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = st.Close(context.Background())

	oldWs, oldAll, oldJSON := historyFlagWorkspace, historyFlagAll, historyFlagJSON
	defer func() {
		historyFlagWorkspace, historyFlagAll, historyFlagJSON = oldWs, oldAll, oldJSON
	}()
	historyFlagWorkspace, historyFlagAll, historyFlagJSON = ws, false, true

	out := captureStdout(t, func() {
		if err := runHistoryList(nil, nil); err != nil {
			t.Fatalf("runHistoryList: %v", err)
		}
	})

	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("parse JSON (%v):\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	for _, req := range []string{"seq", "at", "call_id", "op", "tool", "path", "added", "removed", "content"} {
		if _, ok := r[req]; !ok {
			t.Errorf("JSON row missing required field %q: %+v", req, r)
		}
	}
	if _, ok := r["diff"]; ok {
		t.Errorf("list JSON row should not have 'diff' field: %+v", r)
	}
}

func TestHistoryPruneRequiresYes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	ws := t.TempDir()

	st, err := history.Open(history.DBPath(), history.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Enqueue(history.Item{
		Change: history.Change{
			Op:    history.OpCreate,
			Tool:  "write_file",
			Path:  filepath.Join(ws, "f.txt"),
			At:    time.Now().Add(-10 * time.Hour),
			After: history.SideFromBytes([]byte("content\n")),
			Kind:  history.KindFile,
		},
		Workspace: ws,
		CallID:    "01CALLPRUNE",
	})
	if err := st.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = st.Close(context.Background())

	oldBefore, oldYes, oldVacuum := historyFlagPruneBefore, historyFlagPruneYes, historyFlagPruneVacuum
	defer func() {
		historyFlagPruneBefore, historyFlagPruneYes, historyFlagPruneVacuum = oldBefore, oldYes, oldVacuum
	}()
	historyFlagPruneBefore, historyFlagPruneYes, historyFlagPruneVacuum = "1h", false, false

	// Without --yes in non-interactive mode: must refuse
	if err := runHistoryPrune(nil, nil); err == nil {
		t.Fatal("runHistoryPrune without --yes should have failed")
	}

	// Verify nothing was pruned
	r, err := history.OpenReadOnlyAt(history.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	cnt, err := r.Count()
	r.Close()
	if err != nil || cnt != 1 {
		t.Fatalf("count = %d, want 1", cnt)
	}

	// With --yes: succeeds and removes rows
	historyFlagPruneYes = true
	out := captureStdout(t, func() {
		if err := runHistoryPrune(nil, nil); err != nil {
			t.Fatalf("runHistoryPrune with --yes: %v", err)
		}
	})
	if !strings.Contains(out, "Pruned 1 changes") {
		t.Errorf("output missing prune counts:\n%s", out)
	}

	r, err = history.OpenReadOnlyAt(history.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	cnt, err = r.Count()
	r.Close()
	if err != nil || cnt != 0 {
		t.Fatalf("count after prune = %d, want 0", cnt)
	}
}

func TestHistoryPruneVacuumRefusedWhileDaemonRuns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	oldDaemonAlive := daemonAlive
	daemonAlive = func() bool { return true }
	defer func() { daemonAlive = oldDaemonAlive }()

	oldBefore, oldYes, oldVacuum := historyFlagPruneBefore, historyFlagPruneYes, historyFlagPruneVacuum
	defer func() {
		historyFlagPruneBefore, historyFlagPruneYes, historyFlagPruneVacuum = oldBefore, oldYes, oldVacuum
	}()
	historyFlagPruneBefore, historyFlagPruneYes, historyFlagPruneVacuum = "1h", true, true

	err := runHistoryPrune(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "history prune --vacuum needs exclusive access") {
		t.Fatalf("expected vacuum refusal error, got %v", err)
	}
}

// To a terminal, recorded paths and diffs are display text and an escape
// sequence in them must not run; piped, the output stays byte-exact.
func TestHistoryDisplay_ScrubsOnlyForATerminal(t *testing.T) {
	in := "+boom\x1b[2J\tx"
	if got := historyDisplay(true, in); got != "+boom^[[2J\tx" {
		t.Errorf("terminal: got %q", got)
	}
	if got := historyDisplay(false, in); got != in {
		t.Errorf("piped: got %q, want it unchanged", got)
	}
}
