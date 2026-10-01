package history

import (
	"bytes"
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
	if got.Added != 1 || got.Removed != 1 || got.Before.SHA == nil {
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
