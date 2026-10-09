package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
)

// history_side_cap_test.go covers PLAN-457: with history OFF the before-side of a
// write serves only the response diff, and a response withholds anything past
// maxResponseDiffBytes. A file over that cap must therefore be answered from its
// stat — not by reading it, and not by hashing it when it is also over
// history.HardMaxContentBytes, which is what the old path did before printing
// "diff withheld: file too large".

// sparseFile creates a file of the given size without writing its bytes.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// historyOnDeps is a deps value with recording ENABLED and the response diff on:
// the case where the bytes have a second consumer and must still be read.
func historyOnDeps() WriteDeps {
	return WriteDeps{
		ShowWriteDiff:    true,
		HistoryFn:        func(context.Context, history.Change) {},
		HistoryEnabledFn: func() bool { return true },
	}
}

func TestContentSide_OverTheCapWithHistoryOffIsStatOnly(t *testing.T) {
	const size = int64(maxResponseDiffBytes) + 1
	path := filepath.Join(t.TempDir(), "big.bin")
	sparseFile(t, path, size)

	deps := WriteDeps{ShowWriteDiff: true} // history off: the response is the only consumer
	got := deps.contentSide(path)

	if !got.Exists {
		t.Fatal("the side must report that the file exists")
	}
	if got.Size != size {
		t.Errorf("Size = %d, want %d", got.Size, size)
	}
	if got.Content != nil {
		t.Errorf("no byte may be read for a response-only side, got %d", len(got.Content))
	}
	if got.SHA() != nil {
		t.Errorf("a stat-only side must carry no hash, got %x", got.SHA())
	}
	if sideOf(got).State != sideUnknown {
		t.Error("the response must see an unknown side, which is what renders the too-large marker")
	}
}

func TestContentSide_UnderTheCapWithHistoryOffStillReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.txt")
	if err := os.WriteFile(path, []byte("small\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{ShowWriteDiff: true}
	if got := deps.contentSide(path); string(got.Content) != "small\n" {
		t.Errorf("a file under the cap must still be read for the diff, got %q", got.Content)
	}
}

// TestContentSide_HistoryOnStillReadsOrHashes pins the other half of the contract:
// with history ON the store needs the bytes (or, over the hard cap, the hash), so
// nothing about that path changes.
func TestContentSide_HistoryOnStillReadsOrHashes(t *testing.T) {
	t.Run("over the response cap but under the hard cap, the bytes are carried", func(t *testing.T) {
		const size = int64(maxResponseDiffBytes) + 1
		path := filepath.Join(t.TempDir(), "carried.bin")
		sparseFile(t, path, size)

		got := historyOnDeps().contentSide(path)
		if int64(len(got.Content)) != size {
			t.Errorf("history on must still carry the bytes (got %d, want %d)", len(got.Content), size)
		}
	})
	t.Run("over the hard cap, the side is hashed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "huge.bin")
		sparseFile(t, path, int64(history.HardMaxContentBytes)+1)

		got := historyOnDeps().contentSide(path)
		if got.Content != nil {
			t.Errorf("over the hard cap the side must not be carried, got %d bytes", len(got.Content))
		}
		if got.SHA() == nil {
			t.Error("history on must still hash a side it cannot carry")
		}
	})
}

// TestSideStat_CarriesNoHashToLieWith is the unit guard for the trap the new
// constructor closes: a side that exists with no content used to hash as the EMPTY
// input, which is a digest nobody computed.
func TestSideStat_CarriesNoHashToLieWith(t *testing.T) {
	s := history.SideStat(4096)
	if !s.Exists || s.Size != 4096 {
		t.Fatalf("SideStat = %+v, want Exists with the size", s)
	}
	if s.SHA() != nil {
		t.Errorf("SideStat SHA = %x, want nil", s.SHA())
	}
	if sideOf(s).State != sideUnknown {
		t.Error("a stat-only side must render as unknown, not as an empty file")
	}
}

func TestContentSide_NoConsumerReadsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unused.txt")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (WriteDeps{}).contentSide(path); got.Exists {
		t.Errorf("with no consumer the side must stay absent, got %+v", got)
	}
}

// TestCopyFile_OverTheCapDestinationReportsTheTooLargeDiff is the card's own
// acceptance: history off, show_write_diff on, a copy over an existing
// destination far larger than the cap, and the response carries the marker.
func TestCopyFile_OverTheCapDestinationReportsTheTooLargeDiff(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(from, []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(dir, "dest.bin")
	sparseFile(t, to, 50<<20) // 50 MiB, the size the card names

	tool := NewCopyFile(WriteDeps{ShowWriteDiff: true})
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"from": from, "to": to, "overwrite": true,
	}))
	if err != nil {
		t.Fatalf("copy over a large destination: %v", err)
	}
	if !strings.Contains(out, withheldTooLargeNote) {
		t.Errorf("the response must say the diff was withheld as too large:\n%s", out)
	}
}

// TestDeleteFile_OverTheCapSideIsNeverRead is the no-read proof, and it is not a
// timing test: the file is made UNREADABLE, so any attempt to read it would fail
// and degrade the side to absent — which renders differently from the too-large
// marker. The marker can only appear if nothing read the file.
func TestDeleteFile_OverTheCapSideIsNeverRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	sparseFile(t, path, int64(maxResponseDiffBytes)+1)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	tool := NewDeleteFile(WriteDeps{ShowWriteDiff: true})
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(out, withheldTooLargeNote) {
		t.Errorf("the too-large marker can only come from a stat, not a read:\n%s", out)
	}
}
