package fsync_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/fsync"
)

func TestAtomicWrite_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	if err := fsync.AtomicWrite(path, []byte("hello"), fsync.Options{}); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}

func TestAtomicWrite_OverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsync.AtomicWrite(path, []byte("new"), fsync.Options{}); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}

// TestAtomicWrite_PreservesExistingMode is the regression for the `plumb setup`
// bug: os.CreateTemp always makes 0600, so a writer that never chmods silently
// downgrades a third-party config the first time it rewrites it.
func TestAtomicWrite_PreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ask for 0600 explicitly — an existing file's mode must still win.
	if err := fsync.AtomicWrite(path, []byte("new"), fsync.Options{Mode: 0o600}); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %04o, want 0644 (an existing file's permissions must survive a rewrite)", got)
	}
}

func TestAtomicWrite_NewFileUsesRequestedMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := t.TempDir()

	for _, tc := range []struct {
		name string
		opts fsync.Options
		want os.FileMode
	}{
		{"default is private", fsync.Options{}, 0o600},
		{"explicit 0644", fsync.Options{Mode: 0o644}, 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_"))
			if err := fsync.AtomicWrite(path, []byte("x"), tc.opts); err != nil {
				t.Fatalf("AtomicWrite: %v", err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Errorf("mode = %04o, want %04o", got, tc.want)
			}
		})
	}
}

// TestAtomicWrite_LeavesNoTempOnSuccess guards the property the staging file
// exists for: nothing is left behind for a directory scan to trip over.
func TestAtomicWrite_LeavesNoTempOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	if err := fsync.AtomicWrite(path, []byte("{}"), fsync.Options{TempPattern: ".x-*.json.tmp"}); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "out.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want only out.json", names)
	}
}

// TestAtomicWriteFunc_FailureLeavesTargetAndDirClean is the point of staging:
// a write that fails part-way must not touch the target, and must not leave
// its staging file behind either.
func TestAtomicWriteFunc_FailureLeavesTargetAndDirClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("encoder blew up")
	err := fsync.AtomicWriteFunc(path, fsync.Options{}, func(w io.Writer) error {
		_, _ = w.Write([]byte("partial"))
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the writer's error", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "original" {
		t.Errorf("target = %q, want it untouched (%q)", got, "original")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want the staging file cleaned up", names)
	}
}

func TestAtomicWriteFunc_StreamsContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	err := fsync.AtomicWriteFunc(path, fsync.Options{}, func(w io.Writer) error {
		for _, chunk := range []string{"a", "b", "c"} {
			if _, err := io.WriteString(w, chunk); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("AtomicWriteFunc: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "abc" {
		t.Errorf("content = %q, want %q", got, "abc")
	}
}

// TestAtomicWrite_ConcurrentWritersDoNotClobber is the regression for the
// fixed-temp-name bug in memory/store.go, which staged into a shared
// "<path>.tmp" with no lock around it.
//
// The observed failure mode is not a spliced file — O_TRUNC plus a single
// write is quick enough that the survivor usually looks intact. It is that
// most writers FAIL: whichever one renames first moves the shared staging
// file away, and every other writer's rename (or its cleanup) then operates
// on a path that is gone. Replaying the old algorithm here with 8 writers
// errors 4-5 of them every run. So the load-bearing assertion below is the
// per-writer error check; the content check guards the splice case too.
func TestAtomicWrite_ConcurrentWritersDoNotClobber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.md")

	const writers = 8
	payloads := make([]string, writers)
	for i := range payloads {
		// Long enough that an interleaved write would be visible as a splice.
		payloads[i] = strings.Repeat(string(rune('a'+i)), 4096)
	}

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fsync.AtomicWrite(path, []byte(payloads[i]), fsync.Options{})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	var matched bool
	for _, p := range payloads {
		if string(got) == p {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("final content (%d bytes) is not any single writer's payload — writers spliced", len(got))
	}
	if len(got) != 4096 {
		t.Errorf("final content is %d bytes, want one complete 4096-byte payload", len(got))
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want only the target — staging files leaked", len(entries))
	}
}
