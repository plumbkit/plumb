package paths

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// volumeFoldsCase answers "does this directory's filesystem fold case?" from
// direct filesystem evidence, WITHOUT consulting CanonicalKey or anything it
// calls. Asserting CanonicalKey against its own probe would be circular: the
// pair would agree under any bug in the probe, including the bug this file
// exists to pin. Writing a real file and stating the flipped spelling is the
// only ground truth available.
func volumeFoldsCase(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "casefold-ground-truth")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(probe) })
	seeded, err := os.Lstat(probe)
	if err != nil {
		t.Fatal(err)
	}
	flipped, err := os.Lstat(filepath.Join(dir, "CASEFOLD-GROUND-TRUTH"))
	if err != nil {
		return false // the flipped spelling names nothing: case-sensitive
	}
	return os.SameFile(seeded, flipped)
}

// assertKeys checks the two spellings against the ground truth for dir: one key
// where the filesystem makes them one file, two keys where it makes them two.
// Both directions are asserted rather than skipping the one this runner cannot
// exercise, because CI runs the suite on ubuntu-latest (case-sensitive ext4)
// AND macos-latest (case-insensitive APFS), so between them every line here is
// executed for real.
func assertKeys(t *testing.T, dir, lower, upper string) {
	t.Helper()
	kLower, kUpper := CanonicalKey(lower), CanonicalKey(upper)
	if volumeFoldsCase(t, dir) {
		if kLower != kUpper {
			t.Fatalf("case-insensitive filesystem: %q and %q are ONE file but keyed as two:\n %s\n %s", lower, upper, kLower, kUpper)
		}
		return
	}
	if kLower == kUpper {
		t.Fatalf("case-sensitive filesystem: %q and %q are TWO files but keyed as one (%s)", lower, upper, kLower)
	}
}

// The defect of issue #346, at the level it lives: two spellings of one
// existing file differing only in case.
func TestCanonicalKey_ExistingFileCaseVariants(t *testing.T) {
	dir := canonicalTempDir(t)
	lower := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(lower, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, dir, lower, filepath.Join(dir, "FILE.TXT"))
}

// The case the write lock actually exists for. A path is locked BEFORE it is
// created, so the interesting pair is two spellings of a file neither of which
// is on disk yet — Canonical resolves those through the nearest live ancestor,
// and the fold has to survive that path.
func TestCanonicalKey_NotYetExistingFileCaseVariants(t *testing.T) {
	dir := canonicalTempDir(t)
	assertKeys(t, dir, filepath.Join(dir, "new.txt"), filepath.Join(dir, "NEW.TXT"))
}

// Neither the file NOR its parent directory exists — a write that will create
// an intermediate directory. The probe cannot stat the path's own directory
// here and must climb to a live ancestor to get its answer.
func TestCanonicalKey_MissingIntermediateDirCaseVariants(t *testing.T) {
	dir := canonicalTempDir(t)
	assertKeys(t, dir, filepath.Join(dir, "sub", "new.txt"), filepath.Join(dir, "sub", "NEW.TXT"))
}

// A directory whose own basename has no case to flip ("001", as t.TempDir()
// names its leaf) must not defeat the probe: it climbs until it finds an
// ancestor it can flip.
func TestCanonicalKey_UncasedDirectoryNameStillProbes(t *testing.T) {
	dir := canonicalTempDir(t)
	numeric := filepath.Join(dir, "2026")
	if err := os.Mkdir(numeric, 0o755); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, numeric, filepath.Join(numeric, "f.txt"), filepath.Join(numeric, "F.TXT"))
}

// The symlink identity CanonicalKey inherits from Canonical must survive the
// fold: a link and its target still key together, and the key still resolves
// case on top of that rather than instead of it.
func TestCanonicalKey_KeepsSymlinkIdentity(t *testing.T) {
	dir := canonicalTempDir(t)
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	viaReal := filepath.Join(realDir, "f.txt")
	if err := os.WriteFile(viaReal, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := CanonicalKey(filepath.Join(dir, "link", "f.txt")), CanonicalKey(viaReal); got != want {
		t.Fatalf("a link and its target must key together: %q != %q", got, want)
	}
}

// Where the filesystem does not fold, the key must be Canonical's answer
// byte-for-byte — the fold is the only thing CanonicalKey adds, and it must add
// nothing at all on a case-sensitive volume.
func TestCanonicalKey_MatchesCanonicalWhenNotFolding(t *testing.T) {
	dir := canonicalTempDir(t)
	p := filepath.Join(dir, "MixedCase.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := CanonicalKey(p)
	if volumeFoldsCase(t, dir) {
		if want := strings.ToLower(Canonical(p)); got != want {
			t.Fatalf("CanonicalKey(%q) = %q, want the lowercased canonical %q", p, got, want)
		}
		return
	}
	if want := Canonical(p); got != want {
		t.Fatalf("CanonicalKey(%q) = %q, want Canonical's answer untouched %q", p, got, want)
	}
}

// A relative path must come back exactly as Canonical left it. Resolving one
// would anchor it to the daemon's working directory (issue #181), and probing
// one would ask the filesystem about a location the caller has not established.
func TestCanonicalKey_RelativePathUntouched(t *testing.T) {
	for _, p := range []string{"Foo/Bar.txt", "./MixedCase", "a/../B.txt"} {
		if got, want := CanonicalKey(p), Canonical(p); got != want {
			t.Errorf("CanonicalKey(%q) = %q, want %q — a relative path must not be probed or folded", p, got, want)
		}
	}
	if CanonicalKey("") != "" {
		t.Errorf("CanonicalKey(%q) must stay empty", "")
	}
}

// The memo must not be able to change an answer — a second call has to agree
// with the first, and clearing the cache has to reproduce it rather than flip
// it. A per-directory cache that keyed or overflowed wrongly would show up as
// exactly this disagreement.
func TestCanonicalKey_MemoIsStable(t *testing.T) {
	dir := canonicalTempDir(t)
	p := filepath.Join(dir, "Stable.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := CanonicalKey(p)
	if second := CanonicalKey(p); second != first {
		t.Fatalf("memoised call disagrees with the probe: %q then %q", first, second)
	}
	caseFoldCache.Clear()
	caseFoldCacheLen.Store(0)
	if refilled := CanonicalKey(p); refilled != first {
		t.Fatalf("answer changed after the cache was dropped: %q then %q", first, refilled)
	}
}

// The cache is bounded: a daemon outliving many workspaces must not accumulate
// an entry per directory forever. At the cap it is dropped wholesale, which
// costs a re-probe and never a wrong answer.
func TestCanonicalKey_CacheIsBounded(t *testing.T) {
	caseFoldCache.Clear()
	caseFoldCacheLen.Store(0)
	t.Cleanup(func() {
		caseFoldCache.Clear()
		caseFoldCacheLen.Store(0)
	})
	for i := range caseFoldCacheMax + 100 {
		foldsCase(filepath.Join(string(filepath.Separator), "nonexistent-"+strconv.Itoa(i)))
	}
	if got := caseFoldCacheLen.Load(); got > caseFoldCacheMax {
		t.Fatalf("cache length %d exceeded the cap %d", got, caseFoldCacheMax)
	}
	n := 0
	caseFoldCache.Range(func(_, _ any) bool { n++; return true })
	if n > caseFoldCacheMax {
		t.Fatalf("cache holds %d entries, cap is %d", n, caseFoldCacheMax)
	}
}

func TestFlipCase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"foo", "FOO"},
		{"FOO", "foo"},
		{"Foo", "foo"},
		{"fOO", "foo"},
		{"", ""},
		{"2026", ""},
		{"/", ""},
		{"_-.", ""},
		{"v1.2", "V1.2"},
	} {
		if got := flipCase(tc.in); got != tc.want {
			t.Errorf("flipCase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A sibling SYMLINK whose name is the flipped spelling must not be mistaken for
// evidence that the filesystem folds case. os.Stat would follow the link and
// report the target's identity — the same inode, so a case-SENSITIVE volume
// would be misread as folding, and two genuinely distinct files would then
// share one undo slot and one write-lock. Lstat compares the link itself.
//
// Only meaningful where the flipped spelling can exist as a separate name, so
// it asserts on a case-sensitive volume and merely requires consistency
// elsewhere.
func TestProbeFoldsCase_SiblingSymlinkIsNotEvidence(t *testing.T) {
	dir := canonicalTempDir(t)
	if volumeFoldsCase(t, dir) {
		t.Skip("needs a case-sensitive filesystem to hold both spellings as separate names")
	}
	lower := filepath.Join(dir, "probe")
	if err := os.Mkdir(lower, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(lower, filepath.Join(dir, "PROBE")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	caseFoldCache.Clear()
	caseFoldCacheLen.Store(0)
	t.Cleanup(func() {
		caseFoldCache.Clear()
		caseFoldCacheLen.Store(0)
	})
	if probeFoldsCase(lower) {
		t.Fatal("a sibling symlink named with the flipped spelling was read as a case fold")
	}
}
