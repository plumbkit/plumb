package cli

// conn_attach_home_note_test.go — issue #316: an explicit session_start pin of
// the home directory succeeds (issue #182's contract) but attached with no
// language server and nothing naming why. The session record must carry the
// LSP-skip note for a home pin, and a normal pin must not.

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/sessionstate"
)

func TestHomePin_RecordsLSPSkipNote(t *testing.T) {
	pool := enableTestPool()
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := newRefreshSession(t, pool)
	if _, err := s.repinWorkspace(context.Background(), home, "", false); err != nil {
		t.Fatalf("repinWorkspace(home): %v", err)
	}

	info := sessionRecord(t, s.sessID)
	if info.Language != LanguageNone {
		t.Fatalf("session record Language = %q, want %q", info.Language, LanguageNone)
	}
	if info.DetectedLanguage != homeLSPSkipNote {
		t.Fatalf("session record DetectedLanguage = %q, want the LSP-skip note %q — a home pin must name why no language server is attached", info.DetectedLanguage, homeLSPSkipNote)
	}
	if got := s.lspHomeSkipNote(); got != homeLSPSkipNote {
		t.Fatalf("lspHomeSkipNote() = %q, want %q — the orientation accessor must agree with the record", got, homeLSPSkipNote)
	}
}

// TestAttachSynthetic_HomePin_RecordsLSPSkipNote pins the FIRST-ATTACH half
// (the onBeforeTool seeding path) rather than the re-pin one: attachSynthetic
// computes DetectedLanguage directly and was the one writer left on
// detectAnyLanguageAt, which returns "" at a home root.
func TestAttachSynthetic_HomePin_RecordsLSPSkipNote(t *testing.T) {
	pool := enableTestPool()
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := newRefreshSession(t, pool)
	s.attachSynthetic(context.Background(), home, sessionstate.PinSourceSessionStart, pinTriggerLive)

	info := sessionRecord(t, s.sessID)
	if info.DetectedLanguage != homeLSPSkipNote {
		t.Fatalf("session record DetectedLanguage = %q, want the LSP-skip note %q", info.DetectedLanguage, homeLSPSkipNote)
	}
}

func TestNormalSyntheticPin_DoesNotRecordLSPSkipNote(t *testing.T) {
	pool := enableTestPool()
	root := freshTempDir(t) // markerless: synthesises to itself, language none

	s := newRefreshSession(t, pool)
	if _, err := s.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("repinWorkspace(root): %v", err)
	}

	info := sessionRecord(t, s.sessID)
	if info.DetectedLanguage == homeLSPSkipNote {
		t.Fatalf("session record DetectedLanguage = the LSP-skip note for an ordinary workspace; the note is specific to a home-directory root")
	}
}

// TestAttachSynthetic_NormalPin_DoesNotRecordLSPSkipNote is the attachSynthetic
// half of the negative above. The re-pin test alone could not fail under a
// regression that mis-sets the home-skip flag on the FIRST-ATTACH path only —
// found by mutation during #326's review (mutant survived the whole suite),
// which would have mislabelled every markerless workspace's session record.
func TestAttachSynthetic_NormalPin_DoesNotRecordLSPSkipNote(t *testing.T) {
	pool := enableTestPool()
	root := freshTempDir(t) // markerless and nowhere near $HOME: an ordinary synthetic pin

	s := newRefreshSession(t, pool)
	s.attachSynthetic(context.Background(), root, sessionstate.PinSourceSessionStart, pinTriggerLive)

	info := sessionRecord(t, s.sessID)
	if info.DetectedLanguage == homeLSPSkipNote {
		t.Fatalf("session record DetectedLanguage = the LSP-skip note for an ordinary synthetic pin; " +
			"the note is specific to a home-directory root and must not fire on the first-attach path")
	}
}
