package paths

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// CanonicalKey is the tree's "same FILE?" answer, where Canonical is its "same
// PLACE?" answer. It resolves p exactly as Canonical does and then, when the
// filesystem holding it folds case, lowercases the result.
//
// The return value is an OPAQUE IDENTITY KEY, never a path. On a
// case-insensitive volume it names no file: "/Users/Ada/File.txt" keys as
// "/users/ada/file.txt", which opens fine there and opens nothing at all if the
// string is later carried to a case-sensitive one. Callers that need a path to
// WRITE to want Canonical — safeWrite is the live example, and folding inside
// Canonical would have handed it a spelling to create files under.
//
// The split matters because a case-insensitive filesystem (APFS, HFS+, NTFS)
// makes "dir/file.txt" and "dir/FILE.txt" one file while EvalSymlinks — which
// resolves links, not spellings — leaves them two distinct strings. Everything
// keyed on the write lock's key then treats one file as two: two non-reentrant
// mutexes where lockPaths promises one, two undo snapshots, a write-tracker
// entry under only one of the spellings, and a WorkspaceEdit naming both
// applied twice with the first edit's result thrown away (issue #346, the
// lost update of #314 surviving its own fix for this spelling class).
//
// Case sensitivity is probed, never assumed from GOOS. It is a property of the
// MOUNT: a case-sensitive APFS volume is a supported macOS configuration, and
// NTFS has been per-directory since Windows 10. Guessing costs more than it
// saves, because over-folding is the dangerous direction — it merges two files
// that really are distinct into one undo slot and one tracker entry, which is a
// wrong answer rather than a missed merge. Every failure path below therefore
// declines to fold.
//
// Two residual imprecisions, stated rather than papered over:
//
//   - The probe answers for the directory the path lives in. A case-sensitive
//     mount nested under a case-insensitive ancestor is probed correctly for
//     its own files, but a path whose ancestors span both kinds is lowercased
//     whole, including components the probe never tested.
//   - strings.ToLower is simple Unicode lowering, not the exact fold APFS
//     applies. A pair it disagrees with fails to merge, which is the behaviour
//     that already exists today; it never merges a pair that should not be.
//
// A relative path is returned as Canonical left it. Resolving one would anchor
// it to the daemon's working directory (issue #181), and probing one would ask
// the filesystem about a location the caller has not established yet.
func CanonicalKey(p string) string {
	c := Canonical(p)
	if c == "" || !filepath.IsAbs(c) {
		return c
	}
	if !foldsCase(filepath.Dir(c)) {
		return c
	}
	return strings.ToLower(c)
}

// caseFoldCache memoises the per-directory verdict. lockPathKey sits on every
// write, every write-tracker lookup and every undo lookup, so the probe must
// not cost a stat pair each time — though even uncached it is small against the
// per-component lstat Canonical already pays inside EvalSymlinks.
var caseFoldCache sync.Map // directory -> bool

// caseFoldCacheLen tracks the entry count so the map cannot grow without bound
// in a daemon that outlives many workspaces. sync.Map has no length and no
// eviction; at the cap the whole cache is dropped and refills on demand, which
// costs one stat pair per directory still in use and never a wrong answer.
var caseFoldCacheLen atomic.Int64

const caseFoldCacheMax = 4096

// foldsCase reports whether the filesystem holding dir is case-insensitive.
func foldsCase(dir string) bool {
	if v, ok := caseFoldCache.Load(dir); ok {
		return v.(bool)
	}
	verdict := probeFoldsCase(dir)
	if _, loaded := caseFoldCache.LoadOrStore(dir, verdict); !loaded {
		if caseFoldCacheLen.Add(1) > caseFoldCacheMax {
			caseFoldCache.Clear()
			caseFoldCacheLen.Store(0)
		}
	}
	return verdict
}

// probeFoldsCase walks up from dir to the first ancestor that exists AND has a
// basename with cased letters to flip, then asks the filesystem directly:
// does the flipped spelling name the same directory?
//
// The walk is needed twice over. dir itself frequently does not exist — a write
// creating "a/b/c.txt" where only "a" is there is exactly the case the write
// lock exists for — and a basename like "/" or "2026" has no case to flip.
// Any ancestor on the same volume answers the same question, which is why
// climbing is sound; only a nested mount of the other kind could disagree, and
// that is the imprecision CanonicalKey documents.
func probeFoldsCase(dir string) bool {
	for {
		if v, ok := caseFoldCache.Load(dir); ok {
			return v.(bool)
		}
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			if flipped := flipCase(filepath.Base(dir)); flipped != "" {
				// Lstat, not Stat, on the flipped spelling: on a case-SENSITIVE
				// volume a sibling symlink "Foo" -> "foo" would make Stat report
				// the target's identity and fake a fold. Lstat compares the link
				// itself, which is correctly a different file. On a folding
				// volume both spellings reach the same inode either way.
				other, err := os.Lstat(filepath.Join(filepath.Dir(dir), flipped))
				if err != nil {
					// Missing is the answer, not a failure: a volume that folds
					// case would have resolved the flipped spelling. Unreadable
					// lands here too, and declining to fold is the safe reading.
					return false
				}
				return os.SameFile(info, other)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false // reached the root with nothing probeable
		}
		dir = parent
	}
}

// flipCase returns s with its case inverted — lowercase if s carries any
// uppercase, uppercase otherwise — or "" when s has no cased characters and so
// cannot be used to probe anything.
func flipCase(s string) string {
	lower := strings.ToLower(s)
	if lower == strings.ToUpper(s) {
		return ""
	}
	if s != lower {
		return lower
	}
	return strings.ToUpper(s)
}
