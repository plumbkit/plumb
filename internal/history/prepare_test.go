package history

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareWithholdsSensitiveContentBeforeItIsQueued(t *testing.T) {
	it := Item{Workspace: "/w", Change: Change{
		Op: OpUpdate, Path: "/w/app/.env",
		Before: SideFromBytes([]byte("A=1\n")), After: SideFromBytes([]byte("A=2\n")),
	}}
	got := Prepare(it, Policy{SensitiveGlobs: []string{".env"}, MaxContentBytes: 8 << 20})
	if got.Content != ContentSensitive || got.Before.Content != nil || got.After.Content != nil {
		t.Fatalf("sensitive item still carries content: %+v", got)
	}
	if got.Added != 1 || got.Removed != 1 || got.Before.SHA() == nil {
		t.Fatalf("counts/shas lost: %+v", got)
	}
	// Positive control: a non-sensitive path keeps its content for the writer.
	plain := Prepare(Item{Workspace: "/w", Change: Change{
		Op: OpUpdate, Path: "/w/app/main.go",
		Before: SideFromBytes([]byte("a\n")), After: SideFromBytes([]byte("b\n")),
	}}, Policy{SensitiveGlobs: []string{".env"}})
	if plain.Content != "" || plain.After.Content == nil {
		t.Fatalf("plain item was withheld: %+v", plain)
	}
}

func TestPrepareWithholdsOversizedContent(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 100)
	got := Prepare(Item{Workspace: "/w", Change: Change{Op: OpCreate, Path: "/w/big", After: SideFromBytes(big)}},
		Policy{MaxContentBytes: 10})
	if got.Content != ContentTooLarge || got.After.Content != nil || got.After.Size != 100 {
		t.Fatalf("oversized not withheld: %+v", got)
	}
}

func TestPrepareRenameWithUnchangedContentNeedsNoDiff(t *testing.T) {
	s := SideFromBytes([]byte("same\n"))
	got := Prepare(Item{Workspace: "/w", Change: Change{Op: OpRename, From: "/w/a", Path: "/w/b", Before: s, After: s}}, Policy{})
	if got.Content != ContentNone || got.After.Content != nil {
		t.Fatalf("rename = %+v", got)
	}
}

func TestMatchSensitiveByBaseNameAndRelativePath(t *testing.T) {
	globs := []string{".env.*", "config/secrets.*"}
	for path, want := range map[string]bool{
		"/w/.env.local": true, "/w/sub/.env.prod": true, "/w/config/secrets.yaml": true,
		"/w/other/secrets.yaml": false, "/w/main.go": false,
	} {
		if got := MatchSensitive(globs, "/w", path); got != want {
			t.Errorf("MatchSensitive(%q) = %v, want %v", path, got, want)
		}
	}
}

// sensitiveItem is an update whose content would be stored verbatim unless it is
// classified sensitive.
func sensitiveItem(ws, path, from string) Item {
	return Item{Workspace: ws, Change: Change{
		Op: OpUpdate, Path: path, From: from,
		Before: SideFromBytes([]byte("DB_PASSWORD=old\n")),
		After:  SideFromBytes([]byte("DB_PASSWORD=new\n")),
	}}
}

func TestPrepareWithholdsAWriteThroughASymlinkToASensitiveFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "notes.txt")
	if err := os.Symlink(filepath.Join(ws, ".env"), link); err != nil {
		t.Fatal(err)
	}
	// The tool keeps the link spelling; the writer files the row under the
	// resolved .env, so classification must see the resolved path too.
	got := Prepare(sensitiveItem(ws, link, ""), Policy{SensitiveGlobs: []string{".env"}})
	if got.Content != ContentSensitive || got.After.Content != nil {
		t.Fatalf("write through a symlink to .env kept content: %+v", got)
	}
}

func TestPrepareWithholdsACopyOfASensitiveFile(t *testing.T) {
	ws := t.TempDir()
	got := Prepare(sensitiveItem(ws, filepath.Join(ws, "backup", "env.bak"), filepath.Join(ws, ".env")),
		Policy{SensitiveGlobs: []string{".env"}})
	if got.Content != ContentSensitive || got.After.Content != nil {
		t.Fatalf("copy of .env kept content: %+v", got)
	}
	// Positive control: a copy of an ordinary file keeps its content.
	plain := Prepare(sensitiveItem(ws, filepath.Join(ws, "b.txt"), filepath.Join(ws, "a.txt")),
		Policy{SensitiveGlobs: []string{".env"}})
	if plain.Content != "" || plain.After.Content == nil {
		t.Fatalf("ordinary copy was withheld: %+v", plain)
	}
}

func TestPrepareMatchesRelativeGlobsUnderAnAliasedRoot(t *testing.T) {
	realDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realDir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	// The workspace is known by its alias; the path arrives resolved.
	path := filepath.Join(realDir, "config", "secrets.yaml")
	got := Prepare(sensitiveItem(alias, path, ""), Policy{SensitiveGlobs: []string{"config/secrets.*"}})
	if got.Content != ContentSensitive {
		t.Fatalf("relative glob missed under an aliased root: %+v", got)
	}
}
