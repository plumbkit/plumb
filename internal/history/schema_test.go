package history

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

func TestInitSchemaCreatesThenAcceptsV1(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.db")
	db, err := sqlitex.Open(p, sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err != nil { // idempotent
		t.Fatal(err)
	}
	if v, _ := sqlitex.Version(db); v != SchemaVersion {
		t.Fatalf("user_version = %d", v)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.db")
	db, err := sqlitex.Open(p, sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.StampVersion(db, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("initSchema on a newer db = %v, want ErrNewerSchema", err)
	}
	db.Close()
}
