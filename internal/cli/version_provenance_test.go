package cli

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// TestResolveProvenance covers the documented resolution order. resolveProvenance
// is pure — it takes the stamps and the flattened build-info settings as
// arguments — so these cases run in parallel without any package-level global
// being mutated underneath them.
func TestResolveProvenance(t *testing.T) {
	t.Parallel()

	const (
		stamped  = "4c6e4da9d8fafc5ca36d762460caf6abf46c5ca6"
		embedded = "1111111111111111111111111111111111111111"
	)

	tests := []struct {
		name         string
		rev          string
		dirty        string
		channel      string
		settings     map[string]string
		wantRevision string
		wantRevKnown bool
		wantDirty    bool
		wantDirtyOK  bool
		wantChannel  string
	}{
		{
			name:         "ldflags stamps win over build info",
			rev:          stamped,
			dirty:        "false",
			channel:      "dev",
			settings:     map[string]string{"vcs.revision": embedded, "vcs.modified": "true"},
			wantRevision: stamped,
			wantRevKnown: true,
			wantDirty:    false,
			wantDirtyOK:  true,
			wantChannel:  "dev",
		},
		{
			name:         "ldflags stamps report a dirty tree",
			rev:          stamped,
			dirty:        "true",
			channel:      "dev",
			wantRevision: stamped,
			wantRevKnown: true,
			wantDirty:    true,
			wantDirtyOK:  true,
			wantChannel:  "dev",
		},
		{
			// The whole point of the ldflags stamps: the embedded vcs.modified
			// describes the outer module, so it must not fill this gap. This is
			// also the case the Makefile now relies on — it emits an EMPTY dirty
			// stamp when `git status` fails, precisely so the answer lands here as
			// unknown instead of being asserted clean.
			name:         "revision stamped without a dirty stamp is dirty-unknown",
			rev:          stamped,
			settings:     map[string]string{"vcs.revision": embedded, "vcs.modified": "true"},
			wantRevision: stamped,
			wantRevKnown: true,
			wantDirty:    false,
			wantDirtyOK:  false,
		},
		{
			// GoReleaser renders {{ .FullCommit }} as the literal "none" when it
			// cannot resolve git info (a --snapshot build in a remote-less clone).
			// A failed stamp must NOT defer to the embedded settings: the only
			// scenario that produces it is also one where those settings describe
			// the OUTER module, so deferring would report plumb-ops' HEAD as this
			// repository's — confidently wrong, and strictly worse than the
			// placeholder. Verified with a real binary: a "none" stamp under the
			// old fall-through reported the superproject's SHA with
			// revision_known: true.
			name:         "placeholder revision stamp is unknown, never the outer module",
			rev:          "none",
			dirty:        "false",
			settings:     map[string]string{"vcs.revision": embedded, "vcs.modified": "true"},
			wantRevision: "",
			wantRevKnown: false,
			wantDirty:    false,
			wantDirtyOK:  false,
		},
		{
			name:         "placeholder revision stamp with no build info is unknown",
			rev:          "none",
			dirty:        "false",
			wantRevision: "",
			wantRevKnown: false,
			wantDirty:    false,
			wantDirtyOK:  false,
		},
		{
			// The dirty stamp must not survive its revision either: reporting a
			// tree state about a commit we cannot name is meaningless, and pairing
			// it with the outer module's SHA is how the wrong-commit report was
			// built in the first place.
			name:         "implausible stamp discards its dirty flag too",
			rev:          "HEAD",
			dirty:        "true",
			settings:     map[string]string{"vcs.revision": embedded, "vcs.modified": "false"},
			wantRevision: "",
			wantRevKnown: false,
			wantDirty:    false,
			wantDirtyOK:  false,
		},
		{
			name:         "unparseable dirty stamp is unknown, not clean",
			rev:          stamped,
			dirty:        "maybe",
			wantRevision: stamped,
			wantRevKnown: true,
			wantDirty:    false,
			wantDirtyOK:  false,
		},
		{
			name:         "no stamps falls back to build info",
			settings:     map[string]string{"vcs.revision": embedded, "vcs.modified": "true"},
			wantRevision: embedded,
			wantRevKnown: true,
			wantDirty:    true,
			wantDirtyOK:  true,
		},
		{
			name:         "build info without vcs.modified is dirty-unknown",
			settings:     map[string]string{"vcs.revision": embedded},
			wantRevision: embedded,
			wantRevKnown: true,
			wantDirty:    false,
			wantDirtyOK:  false,
		},
		{
			name:         "both absent is fully unknown",
			settings:     map[string]string{"GOARCH": "arm64"},
			wantRevision: "",
			wantRevKnown: false,
			wantDirty:    false,
			wantDirtyOK:  false,
		},
		{
			name:         "nil settings and no stamps is fully unknown",
			wantRevision: "",
			wantRevKnown: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveProvenance(tc.rev, tc.dirty, tc.channel, tc.settings)
			if got.Revision != tc.wantRevision || got.RevisionKnown != tc.wantRevKnown {
				t.Errorf("revision = %q known=%v, want %q known=%v",
					got.Revision, got.RevisionKnown, tc.wantRevision, tc.wantRevKnown)
			}
			if got.Dirty != tc.wantDirty || got.DirtyKnown != tc.wantDirtyOK {
				t.Errorf("dirty = %v known=%v, want %v known=%v",
					got.Dirty, got.DirtyKnown, tc.wantDirty, tc.wantDirtyOK)
			}
			if got.Channel != tc.wantChannel {
				t.Errorf("channel = %q, want %q", got.Channel, tc.wantChannel)
			}
		})
	}
}

// TestVersionLineUnstampedIsUnchanged pins the human output for a build with no
// revision stamp to the exact bytes plumb has always printed. Anything scraping
// it — including `make install`, which echoes the last line — must keep working.
func TestVersionLineUnstampedIsUnchanged(t *testing.T) {
	t.Parallel()

	got := versionLine(BuildProvenance{}, "go1.26.4")
	want := "plumb " + Version + " (go1.26.4)\n"
	if got != want {
		t.Fatalf("versionLine = %q, want %q", got, want)
	}
}

// TestVersionLineWithRevision checks that a known revision extends the single
// existing line (never adds a second one) and that the dirty marker appears only
// when the build actually knows the tree state.
func TestVersionLineWithRevision(t *testing.T) {
	t.Parallel()

	const rev = "4c6e4da9d8fafc5ca36d762460caf6abf46c5ca6"
	tests := []struct {
		name string
		prov BuildProvenance
		want string
	}{
		{
			name: "clean",
			prov: BuildProvenance{Revision: rev, RevisionKnown: true, DirtyKnown: true},
			want: "plumb " + Version + " (go1.26.4, rev 4c6e4da9d8fa)\n",
		},
		{
			name: "dirty",
			prov: BuildProvenance{Revision: rev, RevisionKnown: true, Dirty: true, DirtyKnown: true},
			want: "plumb " + Version + " (go1.26.4, rev 4c6e4da9d8fa-dirty)\n",
		},
		{
			// Distinct from clean on purpose. This is the normal rendering when a
			// stamper could not measure the tree (the Makefile's `git status`
			// failing), and the human line must not be the one surface where dirty
			// is readable without dirty_known.
			name: "dirty unknown is marked, not silently clean",
			prov: BuildProvenance{Revision: rev, RevisionKnown: true},
			want: "plumb " + Version + " (go1.26.4, rev 4c6e4da9d8fa-dirty?)\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := versionLine(tc.prov, "go1.26.4")
			if got != tc.want {
				t.Errorf("versionLine = %q, want %q", got, tc.want)
			}
			if strings.Count(got, "\n") != 1 {
				t.Errorf("versionLine = %q, want exactly one line", got)
			}
		})
	}
}

// TestVersionJSONKeys pins the exact key set of `plumb version --json`. These
// keys are a consumer contract: renaming or dropping one must be a deliberate
// act that updates this test, not a silent refactor side effect.
func TestVersionJSONKeys(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(newVersionReport(BuildProvenance{}, "go1.26.4"))
	if err != nil {
		t.Fatalf("marshalling version report: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshalling version report: %v", err)
	}

	got := make([]string, 0, len(decoded))
	for k := range decoded {
		got = append(got, k)
	}
	sort.Strings(got)

	want := []string{
		"arch", "build_channel", "dirty", "dirty_known", "go_version",
		"os", "revision", "revision_known", "version",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("JSON keys = %v, want %v", got, want)
	}
}

// TestVersionJSONUnknownIsNotClean is the reason revision_known / dirty_known
// exist: an unstamped build must not be readable as "clean at an empty commit".
func TestVersionJSONUnknownIsNotClean(t *testing.T) {
	t.Parallel()

	report := newVersionReport(BuildProvenance{}, "go1.26.4")
	if report.RevisionKnown || report.DirtyKnown {
		t.Fatalf("unstamped build reports known provenance: %+v", report)
	}
	if report.Revision != "" {
		t.Errorf("revision = %q, want empty for an unstamped build", report.Revision)
	}
	if report.BuildChannel != "" {
		t.Errorf("build_channel = %q, want empty for an unstamped build", report.BuildChannel)
	}
}

// TestVersionLineDistinguishesAllThreeDirtyStates asserts the human renderings
// are mutually distinct. Clean and dirty-unknown rendering identically was the
// original conflation; a future edit that collapses any pair goes red here.
func TestVersionLineDistinguishesAllThreeDirtyStates(t *testing.T) {
	t.Parallel()

	const rev = "4c6e4da9d8fafc5ca36d762460caf6abf46c5ca6"
	clean := versionLine(BuildProvenance{Revision: rev, RevisionKnown: true, DirtyKnown: true}, "go1.26.4")
	dirty := versionLine(BuildProvenance{Revision: rev, RevisionKnown: true, Dirty: true, DirtyKnown: true}, "go1.26.4")
	unknown := versionLine(BuildProvenance{Revision: rev, RevisionKnown: true}, "go1.26.4")

	if clean == dirty || clean == unknown || dirty == unknown {
		t.Fatalf("dirty states must render distinctly:\nclean:   %q\ndirty:   %q\nunknown: %q", clean, dirty, unknown)
	}
}

// TestLooksLikeRevision guards the plausibility check that keeps a failed stamp
// from being reported as a known revision.
func TestLooksLikeRevision(t *testing.T) {
	t.Parallel()

	valid := []string{
		"4c6e4da9d8fafc5ca36d762460caf6abf46c5ca6",                         // SHA-1
		"4c6e4da9d8fafc5ca36d762460caf6abf46c5ca64c6e4da9d8fafc5ca36d7624", // SHA-256
		"4C6E4DA9D8FA",
		"4c6e4da", // shortest accepted abbreviation
	}
	for _, s := range valid {
		if !looksLikeRevision(s) {
			t.Errorf("looksLikeRevision(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",
		"none",    // GoReleaser's placeholder when git info is unresolvable
		"unknown", // the other common placeholder
		"dev",
		"4c6e4d",     // too short to be a useful abbreviation
		"v0.16.4",    // a tag, not a commit
		"4c6e4da-x1", // hex-ish but not hex
		"4c6e4da9d8fafc5ca36d762460caf6abf46c5ca64c6e4da9d8fafc5ca36d76245", // longer than SHA-256
	}
	for _, s := range invalid {
		if looksLikeRevision(s) {
			t.Errorf("looksLikeRevision(%q) = true, want false", s)
		}
	}
}

// TestShortRevision covers the abbreviation used in the human line: long SHAs
// are cut to git's 12-character default, shorter values pass through untouched.
func TestShortRevision(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"4c6e4da9d8fafc5ca36d762460caf6abf46c5ca6": "4c6e4da9d8fa",
		"4c6e4da9d8fa": "4c6e4da9d8fa",
		"4c6e4da":      "4c6e4da",
		"":             "",
	}
	for in, want := range tests {
		if got := shortRevision(in); got != want {
			t.Errorf("shortRevision(%q) = %q, want %q", in, got, want)
		}
	}
}
