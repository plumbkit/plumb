package cli

// Regression tests for the post-merge adversarial review of #580 (CLI side).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/sqlitex"
	"github.com/plumbkit/plumb/internal/stats"
)

func callSummaryForTest() stats.CallSummary {
	return stats.CallSummary{Tool: "edit_file", CalledAt: time.UnixMilli(1_790_000_000_000), DurationMs: 3, Success: true}
}

// seedOne records one create of rel under ws in the test's history.db.
func seedOne(t *testing.T, ws, rel, callID string) {
	t.Helper()
	st, err := history.Open(history.DBPath(), history.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Enqueue(history.Item{
		Change: history.Change{
			Op: history.OpCreate, Tool: "write_file", Kind: history.KindFile, At: time.Now(),
			Path: filepath.Join(ws, rel), After: history.SideFromBytes([]byte("content\n")),
		},
		Workspace: ws,
		CallID:    callID,
	})
	if err := st.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// listWith runs `plumb history` with the given workspace/file flags from cwd and
// returns the output.
func listWith(t *testing.T, cwd, ws, file string, all bool) string {
	t.Helper()
	oldWs, oldFile, oldAll, oldJSON := historyFlagWorkspace, historyFlagFile, historyFlagAll, historyFlagJSON
	defer func() {
		historyFlagWorkspace, historyFlagFile, historyFlagAll, historyFlagJSON = oldWs, oldFile, oldAll, oldJSON
	}()
	historyFlagWorkspace, historyFlagFile, historyFlagAll, historyFlagJSON = ws, file, all, false
	t.Chdir(cwd)
	return captureStdout(t, func() {
		if err := runHistoryList(nil, nil); err != nil {
			t.Fatalf("runHistoryList: %v", err)
		}
	})
}

func TestHistoryCLIResolvesRelativeAndAbsolutePaths(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedOne(t, ws, "sub/g.txt", "01REL")

	cases := []struct {
		name, cwd, ws, file string
		all                 bool
	}{
		{"--workspace . from the root", ws, ".", "", false},
		{"--file relative to the cwd", filepath.Join(ws, "sub"), ws, "g.txt", false},
		{"--all with an absolute --file", ws, "", filepath.Join(ws, "sub", "g.txt"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := listWith(t, c.cwd, c.ws, c.file, c.all); !strings.Contains(out, "g.txt") {
				t.Fatalf("no row listed:\n%s", out)
			}
		})
	}
	// Positive control: a file that was never written lists nothing.
	if out := listWith(t, ws, ws, "nope.txt", false); strings.Contains(out, "g.txt") {
		t.Fatalf("control: an unrelated --file listed g.txt:\n%s", out)
	}
}

func TestHistoryListRejectsSinceAfterUntil(t *testing.T) {
	oldS, oldU := historyFlagSince, historyFlagUntil
	defer func() { historyFlagSince, historyFlagUntil = oldS, oldU }()
	historyFlagSince, historyFlagUntil = "1d", "7d" // since one day ago, until a week ago
	if _, err := buildHistoryFilter(""); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("buildHistoryFilter = %v; a --since later than --until must be refused", err)
	}
	historyFlagSince, historyFlagUntil = "7d", "1d"
	if _, err := buildHistoryFilter(""); err != nil {
		t.Fatalf("control: an ordered range was refused: %v", err)
	}
}

func TestHistoryShowJSONUsesSnakeCaseCallKeys(t *testing.T) {
	out := captureStdout(t, func() {
		e := history.Entry{Seq: 1, Op: history.OpCreate, Path: "a", Content: history.ContentNone}
		if err := renderShowJSON([]history.Entry{e}, []string{""}, true, callSummaryForTest()); err != nil {
			t.Fatal(err)
		}
	})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("output = %s, %v", out, err)
	}
	call, ok := rows[0]["call"].(map[string]any)
	if !ok {
		t.Fatalf("no call object: %s", out)
	}
	for _, k := range []string{"tool", "called_at", "duration_ms", "success"} {
		if _, ok := call[k]; !ok {
			t.Errorf("call object lacks %q (keys must be snake_case like the rest): %v", k, call)
		}
	}
}

func TestHistoryStoreStopsRetryingANewerSchema(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(history.DBPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlitex.Open(history.DBPath(), sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.StampVersion(db, history.SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	db.Close()

	h := newHistoryStore(nil)
	defer h.Close()
	if h.store() != nil {
		t.Fatal("a newer-schema history.db opened")
	}
	h.mu.Lock()
	h.failedAt = time.Now().Add(-2 * time.Minute) // the retry window has passed
	h.mu.Unlock()
	if h.store() != nil {
		t.Fatal("a newer-schema history.db opened on retry")
	}
	h.mu.Lock()
	disabled := h.disabled
	h.mu.Unlock()
	if !disabled {
		t.Fatal("a newer schema must disable recording for the daemon's life, not be retried every minute")
	}
}
