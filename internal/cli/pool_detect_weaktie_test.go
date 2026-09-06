package cli

import (
	"fmt"
	"path/filepath"
	"testing"
)

// The weak-marker set is small, promiscuous and — unlike the strong one —
// routinely claimed by two languages at once: an ordinary frontend app ships
// `index.html` beside `package.json`, and a Python service with a frontend ships
// `requirements.txt` beside it. weakLangAt took the first match in the pool's
// language order, which is go-first-then-alphabetical, so "html" won every tie
// it appeared in and vscode-html-language-server was started for applications
// whose sources it cannot serve. These tests pin the tie on the same evidence
// strongLangAt uses: what the project actually contains.

// TestWeakLangAt_IndexHTMLBesidePackageJSONFollowsSources: the reported shape.
// Alphabetical order alone answers html; the sources answer typescript.
func TestWeakLangAt_IndexHTMLBesidePackageJSONFollowsSources(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "index.html"), "<html></html>\n")
	mustWrite(t, filepath.Join(dir, "package.json"), "{}")
	mustWrite(t, filepath.Join(dir, "about.html"), "<html></html>\n")
	for i := range 5 {
		mustWrite(t, filepath.Join(dir, "src", fmt.Sprintf("mod%d.ts", i)), "export const x = 1\n")
	}

	if got := defaultsPool(t, "typescript", "html").weakLangAt(dir); got != "typescript" {
		t.Errorf("weakLangAt = %q, want typescript — 5 .ts sources against 1 other "+
			"page decide this root, not the fact that \"html\" sorts first", got)
	}
}

// TestWeakLangAt_ContestedMarkerDoesNotVoteForItself: `index.html` is html's
// weak marker AND an html source file, so counted it arrives as one vote for the
// language it just put in the running — a marker settling the tie its own
// presence created. Strong markers were already discounted for exactly this
// reason (`build.gradle.kts` is Kotlin by extension); weak ones must be too.
//
// The fixture is deliberately balanced at one file each so nothing but the
// discount can decide it.
func TestWeakLangAt_ContestedMarkerDoesNotVoteForItself(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "index.html"), "<html></html>\n")
	mustWrite(t, filepath.Join(dir, "package.json"), "{}")
	mustWrite(t, filepath.Join(dir, "src", "app.ts"), "export const x = 1\n")

	if got := defaultsPool(t, "typescript", "html").weakLangAt(dir); got != "typescript" {
		t.Errorf("weakLangAt = %q, want typescript — index.html is the marker that "+
			"created this tie and must not also cast the vote that settles it", got)
	}
}

// TestWeakLangAt_RequirementsBesidePackageJSONFollowsSources: the shape the
// python weak markers were added for — a service whose frontend lives in a
// subdirectory. Both directions, because the interesting one is the direction
// language order does NOT already produce ("python" sorts before "typescript",
// so a python answer proves nothing on its own).
func TestWeakLangAt_RequirementsBesidePackageJSONFollowsSources(t *testing.T) {
	tests := []struct {
		name    string
		py, ts  int
		want    string
		because string
	}{
		{"python sources win", 9, 1, "python", "a service with a small build script"},
		{"typescript sources win", 1, 9, "typescript", "a frontend with one deployment script"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := freshTempDir(t)
			mustWrite(t, filepath.Join(dir, "requirements.txt"), "flask\n")
			mustWrite(t, filepath.Join(dir, "package.json"), "{}")
			for i := range tt.py {
				mustWrite(t, filepath.Join(dir, "src", fmt.Sprintf("m%d.py", i)), "x = 1\n")
			}
			for i := range tt.ts {
				mustWrite(t, filepath.Join(dir, "web", fmt.Sprintf("m%d.ts", i)), "export const x = 1\n")
			}
			if got := defaultsPool(t, "python", "typescript").weakLangAt(dir); got != tt.want {
				t.Errorf("weakLangAt = %q, want %s (%s)", got, tt.want, tt.because)
			}
		})
	}
}

// TestWeakLangAt_TruncatedTieDoesNotFallBackToMarkup is the one place the two
// tie-breaks differ, and it is deliberate. strongLangAt discards a truncated
// count and falls back to language order, because every strong candidate brought
// a build file of its own and no ordering of peers is systematically wrong.
// Weak candidates are not peers: "html" sorts first, so the same fallback hands
// html EVERY tie as soon as the tree is big enough to hit the cap — and a tree
// that big is precisely the one least likely to be a static HTML page.
//
// The fixture mirrors TestStrongLangAt_TieBreakStillTruncatesAboveItsBudget:
// the walk is a LIFO over sorted listings, so it pops web/ (200 TypeScript
// modules), then spends the budget in docs/ and stops. 20500 is literal for the
// same reason it is there — a symbolic count scales with the constant and can
// never pin it.
func TestWeakLangAt_TruncatedTieDoesNotFallBackToMarkup(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "index.html"), "<html></html>\n")
	mustWrite(t, filepath.Join(dir, "package.json"), "{}")
	for i := range 200 {
		mustWrite(t, filepath.Join(dir, "web", "src", fmt.Sprintf("mod%05d.ts", i)), "export const x = 1\n")
	}
	manyFiles(t, dir, "docs", "page", ".png", 20500)

	if got := defaultsPool(t, "typescript", "html").weakLangAt(dir); got != "typescript" {
		t.Errorf("weakLangAt = %q, want typescript — the walk stopped at the %d-file "+
			"cap, and the partial count it did manage (200 .ts) is evidence where "+
			"the language order is only a bias towards markup", got, tieScanMaxFiles)
	}
}

// A single weak claimant is unchanged by the tie-break: it is still that
// language, and no scan runs. Pinned because collecting ALL matches instead of
// returning the first is the change that made a tie possible at all.
func TestWeakLangAt_SingleClaimantIsUnchanged(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "package.json"), "{}")
	// A directory of .html files must not move the answer: html claims no weak
	// marker here, so it is not a candidate and dominantAmong never sees it.
	for i := range 20 {
		mustWrite(t, filepath.Join(dir, "public", fmt.Sprintf("p%02d.html", i)), "<html></html>\n")
	}

	if got := defaultsPool(t, "typescript", "html").weakLangAt(dir); got != "typescript" {
		t.Errorf("weakLangAt = %q, want typescript — only package.json matched, so "+
			"there is no tie for the sources to settle", got)
	}
}

// TestStrongLangAt_PyLockResolvesPython pins the python lock-file marker as a
// STRONG one: it names a project root on its own, with no sources needed and no
// weak-marker ambiguity. It is a glob, which markerPresent supports.
func TestStrongLangAt_PyLockResolvesPython(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "app.py.lock"), "{}\n")

	if got := defaultsPool(t, "python", "typescript").strongLangAt(dir); got != "python" {
		t.Errorf("strongLangAt = %q, want python — *.py.lock is a python root marker", got)
	}
}

// uv.lock is the weak half of the same addition: it travels with a
// pyproject.toml in a well-formed project and without one in a scripts repo, so
// it names the directory it sits in rather than claiming an ancestor.
func TestWeakLangAt_UvLockResolvesPython(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "uv.lock"), "version = 1\n")

	if got := defaultsPool(t, "python", "typescript").weakLangAt(dir); got != "python" {
		t.Errorf("weakLangAt = %q, want python — uv.lock is a python weak root marker", got)
	}
}
