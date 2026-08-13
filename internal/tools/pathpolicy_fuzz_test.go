package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathpolicy_fuzz_test.go — the boundary policy, fuzzed against the KERNEL.
//
// TODO_IMPROVE_3 §9 names symlink traversal and path canonicalisation as
// parsers taking attacker-influenced input with no fuzz coverage. They take the
// most attacker-influenced input in the tree: every path in every tool call
// arrives from an agent whose instructions can be steered by any file it has
// read.
//
// The oracle is not "did not panic" and not "the string looks contained". It is
// the filesystem itself: when Check ALLOWS a write, we perform the write and ask
// the kernel where the byte actually landed. If the resolved location is outside
// the root Check named, the policy ruled on one file while the syscall touched
// another.
//
// That is precisely the shape of #264, the escape that reached shipped code:
// canonicalPathForBoundary canonicalised with filepath.Abs, which Cleans `sub/..`
// away LEXICALLY before any symlink is resolved, while the kernel resolves `sub`
// first and applies `..` to wherever that landed. The string the policy ruled on
// and the string the syscall received were the same text naming two different
// files. A string-comparison oracle cannot see that; only asking the filesystem
// can, which is why this target does the write.
//
// Safety: a write is attempted ONLY when the candidate path is inside the test's
// own sandbox. A Check that allows a path outside the sandbox entirely is
// reported as a finding without writing anything — the point is to detect the
// escape, not to perform it.
//
// WHAT THE MUTATION TESTING SHOWED, because it is not what the code comments
// suggest and a future reader will otherwise get it backwards.
//
// Deleting BOTH `..` refusals — requireCanonicalPath in PathPolicy.Check and
// hasParentTraversal in match — leaves every payload here still refused. They
// are defence in depth for this class, not the defence. The load-bearing code is
// canonicalRoot: it resolves symlinks for the path AND its nearest existing
// ancestor, so `ws/toOut/secret` and `ws/link/..` resolve to their real
// locations and simply fail to match any root.
//
// Reverting canonicalRoot to its pre-#264 lexical form (filepath.Abs, which
// Cleans `sub/..` before any symlink resolves) reproduces the escape
// immediately, and this target catches it — the write lands in the out-of-policy
// directory and overwrites the canary.
//
// So the refusals must not be removed on the grounds that "canonicalRoot handles
// it" (they cover the ambiguity canonicalRoot cannot see, where the caller named
// a path whose meaning depends on who resolves it), and canonicalRoot's symlink
// resolution must not be simplified on the grounds that "the refusal handles it"
// — which is the mistake this note exists to prevent, because only the second of
// those two is load-bearing here.

// boundarySandbox is the filesystem the policy is tested against: one writable
// root, one directory outside it, and a set of symlinks that are the classic
// escape primitives.
type boundarySandbox struct {
	root    string // EvalSymlinks'd sandbox root; contains ws and outside
	ws      string // the single AccessReadWrite root
	outside string // NOT in the policy — anything landing here is an escape
	policy  *PathPolicy
}

func newBoundarySandbox(t *testing.T) *boundarySandbox {
	t.Helper()
	// macOS t.TempDir() returns /var/... while canonicalisation yields
	// /private/var/.... Resolving up front is what stops a containment assertion
	// passing for the wrong reason.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(TempDir): %v", err)
	}
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{
		ws,
		outside,
		filepath.Join(ws, "sub"),
		filepath.Join(ws, "sub", "deeper"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The escape primitives, all reachable from inside the workspace.
	links := map[string]string{
		// The #264 payload: a symlink whose target is the parent, so `link/..`
		// resolves ABOVE the workspace for the kernel but Cleans to the workspace
		// lexically.
		filepath.Join(ws, "link"):     base,
		filepath.Join(ws, "toOut"):    outside,
		filepath.Join(ws, "toRoot"):   string(filepath.Separator),
		filepath.Join(ws, "selfLoop"): filepath.Join(ws, "selfLoop"),
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	return &boundarySandbox{
		root:    base,
		ws:      ws,
		outside: outside,
		policy:  NewPathPolicy(ws, []AllowedRoot{{Path: ws, Access: AccessReadWrite, Label: "workspace"}}),
	}
}

// insideSandbox reports whether a path is lexically under the sandbox, used only
// to decide whether performing a write is SAFE — never as the containment
// oracle.
func (s *boundarySandbox) insideSandbox(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	rel, err := filepath.Rel(s.root, filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func FuzzPathPolicyCheckAgainstKernel(f *testing.F) {
	// The #264 family: `..` through a symlink.
	f.Add("link/..")
	f.Add("link/../outside/secret")
	f.Add("sub/../link/../outside/secret")
	f.Add("toOut/secret")
	f.Add("toRoot/etc/hosts")
	// Ordinary, must keep working.
	f.Add("file.txt")
	f.Add("sub/file.txt")
	f.Add("sub/deeper/new.txt")
	f.Add("./sub/./file.txt")
	// Traversal without symlinks.
	f.Add("../outside/secret")
	f.Add("sub/../../outside/secret")
	f.Add("../../../../../../etc/passwd")
	// Degenerate shapes.
	f.Add("")
	f.Add(".")
	f.Add("..")
	f.Add("selfLoop/x")
	f.Add("sub//file.txt")
	f.Add("sub/./../sub/file.txt")
	// Separators and NUL-adjacent oddities the parser may mishandle.
	f.Add("sub\\file.txt")
	f.Add(" sub/file.txt")
	f.Add("sub/file.txt ")

	f.Fuzz(func(t *testing.T, suffix string) {
		s := newBoundarySandbox(t)

		// Two candidate spellings: joined onto the workspace (the ordinary agent
		// argument), and taken as-is when the fuzzer produced an absolute path.
		candidates := []string{filepath.Join(s.ws, suffix)}
		if filepath.IsAbs(suffix) {
			candidates = append(candidates, suffix)
		}

		for _, path := range candidates {
			root, err := s.policy.Check(path, AccessReadWrite)
			if err != nil {
				continue // refused: nothing to verify, refusal is always safe here
			}
			// Check ALLOWED a write. Two things must now hold.

			// 1. The root it named must be the workspace. There is only one root in
			//    this policy, so anything else means match() invented one.
			if root.Path != canonicalRoot(s.ws) {
				t.Errorf("Check allowed %q under an unexpected root %q (want the workspace %q)",
					path, root.Path, canonicalRoot(s.ws))
				continue
			}

			// 2. Ask the KERNEL. If the path is outside the sandbox we do not write —
			//    an allowed path outside the sandbox is itself the finding.
			if !s.insideSandbox(path) {
				t.Errorf("Check allowed a path outside the test sandbox entirely: %q\n"+
					"the policy's only root is %q", path, s.ws)
				continue
			}
			landed, ok := writeAndResolve(t, path)
			if !ok {
				continue // the write could not be performed (missing parent, ENOTDIR, …)
			}
			if !withinResolved(s.ws, landed) {
				t.Errorf("BOUNDARY ESCAPE: Check allowed %q, but the write landed at %q,\n"+
					"which is outside the only allowed root %q.\n"+
					"The policy ruled on one file and the syscall touched another — the #264 class.",
					path, landed, s.ws)
			}
			if withinResolved(s.outside, landed) {
				t.Errorf("BOUNDARY ESCAPE: %q reached the out-of-policy directory: landed at %q", path, landed)
			}
		}
	})
}

// writeAndResolve performs the write the policy just authorised and returns
// where the byte ACTUALLY landed, resolved by the kernel. ok is false when the
// write could not be attempted at all, which is not a policy question.
//
// The resolution is done on the containing directory after the write, so
// symlinked components are followed exactly as the kernel followed them —
// deliberately NOT by re-running plumb's own canonicaliser, which would make the
// oracle circular and blind to the very disagreement it exists to detect.
func writeAndResolve(t *testing.T, path string) (string, bool) {
	t.Helper()
	if err := os.WriteFile(path, []byte("plumb-fuzz-marker"), 0o600); err != nil {
		return "", false
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, filepath.Base(path)), true
}

// withinResolved reports containment of an already-resolved path under an
// already-resolved root, by path components rather than string prefix — so
// "/a/bc" is not treated as inside "/a/b".
func withinResolved(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestPathPolicy_KnownEscapePayloadsAreRefused pins the #264 payloads as
// ordinary cases, so they are exercised by `make test` whether or not anyone
// fuzzes again. Each is asserted on the FILESYSTEM EFFECT, not on an error
// string.
func TestPathPolicy_KnownEscapePayloadsAreRefused(t *testing.T) {
	s := newBoundarySandbox(t)
	for _, suffix := range []string{
		"link/..",
		"link/../outside/secret",
		"sub/../link/../outside/secret",
		"toOut/secret",
		"../outside/secret",
	} {
		path := filepath.Join(s.ws, suffix)
		if _, err := s.policy.Check(path, AccessReadWrite); err == nil {
			landed, ok := writeAndResolve(t, path)
			if ok && !withinResolved(s.ws, landed) {
				t.Errorf("payload %q was allowed and escaped to %q", suffix, landed)
			}
		}
		// The secret must never have been overwritten, whatever the policy said.
		got, err := os.ReadFile(filepath.Join(s.outside, "secret"))
		if err != nil {
			t.Fatalf("reading the canary: %v", err)
		}
		if string(got) != "secret" {
			t.Fatalf("payload %q overwrote a file outside every allowed root", suffix)
		}
	}
}
