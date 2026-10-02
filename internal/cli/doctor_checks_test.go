package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

func TestCheckConfigs_WarnsOnFrozenDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	cfgDir := filepath.Join(home, ".config", "plumb")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}

	// 1. Clean custom config
	cleanConfig := `
theme = "dark"

[edits]
strict = true

[git]
protected_branches = ["main", "master", "develop"]
`
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cleanConfig), 0o600); err != nil {
		t.Fatalf("writing clean config: %v", err)
	}

	results := checkConfigs("")
	for _, r := range results {
		if r.name == "frozen defaults" {
			t.Errorf("clean config must not report frozen defaults, got %+v", r)
		}
	}

	// 2. Config with frozen defaults
	frozenConfig := `
theme = "dark"
command = []

[git]
protected_branches = ["main", "master"]

[quality]
analysers = ["golangci-lint"]
`
	if err := os.WriteFile(cfgPath, []byte(frozenConfig), 0o600); err != nil {
		t.Fatalf("writing frozen config: %v", err)
	}

	results = checkConfigs("")
	var found *checkResult
	for i := range results {
		if results[i].name == "frozen defaults" {
			found = &results[i]
			break
		}
	}

	if found == nil {
		t.Fatalf("checkConfigs did not return a 'frozen defaults' result; got: %+v", results)
	}
	if !found.ok || !found.warn {
		t.Errorf("frozen defaults result should be ok=true, warn=true; got ok=%v, warn=%v", found.ok, found.warn)
	}
	if !strings.Contains(found.detail, "command") || !strings.Contains(found.detail, "git.protected_branches") {
		t.Errorf("expected frozen keys in detail, got: %q", found.detail)
	}
	if !strings.Contains(found.fix, "delete redundant default lines") {
		t.Errorf("expected fix instruction in fix field, got: %q", found.fix)
	}
}

func TestCheckHistoryDB(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// 1. Absent file
	results := checkHistoryDB()
	if len(results) != 1 || !results[0].ok || !strings.Contains(results[0].detail, "not created yet") {
		t.Fatalf("absent history db check: %+v", results)
	}

	// 2. Seeded store without drops -> ok (positive control)
	dbPath := history.DBPath()
	s, err := history.Open(dbPath, history.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Enqueue(history.Item{Change: history.Change{
		Op:    history.OpCreate,
		Path:  "/test/a.txt",
		After: history.SideFromBytes([]byte("content\n")),
		At:    time.Now(),
		Kind:  history.KindFile,
	}})
	_ = s.Sync(context.Background())
	_ = s.Close(context.Background())

	results = checkHistoryDB()
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %+v", results)
	}
	if !results[0].ok || results[0].warn {
		t.Errorf("history db should be ok, got %+v", results[0])
	}
	if !results[1].ok || results[1].warn {
		t.Errorf("history health should be ok (no drops/errors), got %+v", results[1])
	}

	// 3. Seeded store with meta last_drop_at_ms = now -> warn
	db, err := sqlitex.Open(dbPath, sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("sqlitex.Open: %v", err)
	}
	nowMs := strconv.FormatInt(time.Now().UnixMilli(), 10)
	_, _ = db.Exec(`INSERT INTO meta(key, value) VALUES ('dropped_rows', '5') ON CONFLICT(key) DO UPDATE SET value = '5'`)
	_, _ = db.Exec(`INSERT INTO meta(key, value) VALUES ('last_drop_at_ms', ?) ON CONFLICT(key) DO UPDATE SET value = ?`, nowMs, nowMs)
	db.Close()

	results = checkHistoryDB()
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %+v", results)
	}
	if !results[1].warn {
		t.Errorf("expected warn on recent drop, got %+v", results[1])
	}
	// A warning keeps ok=true (checkResult's contract): ok=false is a FAILURE
	// and would turn a recent dropped row into a non-zero doctor exit.
	if !results[1].ok {
		t.Errorf("a warning must keep ok=true, got %+v", results[1])
	}
	if !strings.Contains(results[1].detail, "dropped_rows=5") || !strings.Contains(results[1].detail, "overflow_rows=") {
		t.Errorf("expected detail to report dropped_rows=5 and overflow_rows, got %q", results[1].detail)
	}
	if !strings.Contains(results[0].detail, "v1") {
		t.Errorf("expected the schema version in the db detail, got %q", results[0].detail)
	}

	// 4. A history.db from a newer plumb: the fix is to upgrade, not to delete it.
	db2, err := sqlitex.Open(history.DBPath(), sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.StampVersion(db2, history.SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	db2.Close()
	results = checkHistoryDB()
	if results[0].ok || !strings.Contains(results[0].fix, "upgrade plumb") || strings.Contains(results[0].fix, "remove") {
		t.Errorf("newer-schema db: want a failure whose fix is to upgrade plumb, got %+v", results[0])
	}
}
