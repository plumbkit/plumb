package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// TestReachabilityModeNameNotRequired pins that validate()
// skips the name check specifically for mode="reachability" — the opposite of
// TestTopologyImpact_MissingName, which pins that the classic (non-reachability)
// path still requires a name. Together they mutation-cover the branch: a
// validate() that always required name would fail this one; a validate() that
// never required name would fail TestTopologyImpact_MissingName.
func TestReachabilityModeNameNotRequired(t *testing.T) {
	a, err := parseTopologyImpactArgs(json.RawMessage(`{"mode":"reachability"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := a.validate(); err != nil {
		t.Errorf("reachability mode must not require name, got error: %v", err)
	}
}

// TestReachabilityModeUnknownRejected pins that a typo'd/unknown mode is
// rejected rather than silently falling through to the classic blast-radius
// path (independent review finding 9).
func TestReachabilityModeUnknownRejected(t *testing.T) {
	a, err := parseTopologyImpactArgs(json.RawMessage(`{"name":"foo","mode":"reachabilty"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := a.validate(); err == nil {
		t.Error("expected an error for an unrecognised mode, got nil")
	}
}

// TestReachabilityFieldsWithoutModeRejected pins that roots/path_to/layers
// are rejected, not silently ignored, when mode is not "reachability" —
// independent review finding 9: those fields doing nothing outside
// reachability mode is exactly the kind of silent no-op this project's
// memory (half-fix-passes-its-own-tests, green-but-false) warns about.
func TestReachabilityFieldsWithoutModeRejected(t *testing.T) {
	cases := []string{
		`{"name":"foo","roots":["cmd/x"]}`,
		`{"name":"foo","path_to":"internal/y"}`,
		`{"name":"foo","layers":true}`,
	}
	for _, raw := range cases {
		a, err := parseTopologyImpactArgs(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if err := a.validate(); err == nil {
			t.Errorf("%s: expected an error (reachability-only field without mode=\"reachability\"), got nil", raw)
		}
	}
}

// TestReachabilitySummaryFormat_SamplesCappedWithRemainder pins the output
// shape's byte discipline directly (no DB/extractor involved): with more
// packages than reachabilitySampleLimit in each bucket, each bucket shows
// exactly the cap and states how many more were omitted — never the raw list.
func TestReachabilitySummaryFormat_SamplesCappedWithRemainder(t *testing.T) {
	g := &topology.PackageGraph{Dirs: map[string]*topology.PackageInfo{}}
	reachable := map[string]bool{}
	g.Dirs["root"] = &topology.PackageInfo{Dir: "root"}
	reachable["root"] = true
	for i := range 15 {
		d := fmt.Sprintf("reached/%02d", i)
		g.Dirs[d] = &topology.PackageInfo{Dir: d}
		reachable[d] = true
	}
	for i := range 12 {
		d := fmt.Sprintf("dead/%02d", i)
		g.Dirs[d] = &topology.PackageInfo{Dir: d, NumNodes: 100 - i}
	}
	res := &topology.ReachabilityResult{Roots: []string{"root"}, Reachable: reachable, Predecessor: map[string]string{}}

	out := formatReachabilitySummary(g, res, nil, "")

	if !strings.Contains(out, "reachable: 16 package(s)") {
		t.Errorf("expected the true reachable count (16 = root + 15) even though samples are capped; got:\n%s", out)
	}
	if !strings.Contains(out, "[+6 more]") {
		t.Errorf("expected a '+6 more' remainder note for the reachable bucket (16-10); got:\n%s", out)
	}
	if !strings.Contains(out, "unreachable: 12 package(s)") {
		t.Errorf("expected the true unreachable count (12); got:\n%s", out)
	}
	if !strings.Contains(out, "[+2 more]") {
		t.Errorf("expected a '+2 more' remainder note for the unreachable bucket (12-10); got:\n%s", out)
	}
	// Actual per-bucket line count must be exactly the sample cap, not the
	// full membership — this is what "never the raw graph" means operationally.
	shownReached := strings.Count(out, "reached/")
	if shownReached != reachabilitySampleLimit {
		t.Errorf("expected exactly %d 'reached/*' lines shown, got %d", reachabilitySampleLimit, shownReached)
	}
	shownDead := strings.Count(out, "dead/")
	if shownDead != reachabilitySampleLimit {
		t.Errorf("expected exactly %d 'dead/*' lines shown, got %d", reachabilitySampleLimit, shownDead)
	}
}

// TestReachabilitySummaryFormat_UnreachableSortedBySize pins that the
// unreachable bucket surfaces the BIGGEST (most actionable) dead packages
// first, not insertion or alphabetical order.
// Directory names are deliberately alphabetically ASCENDING in the opposite
// order of their NumNodes (aaa=smallest .. zzz=biggest): if the sort ever
// regressed to alphabetical (or any name-based) ordering, this test would
// observe the exact REVERSE of the expected size-descending order and fail —
// unlike same-direction names, which would pass whether the code sorts by
// size or by name and so cannot catch that mutation.
func TestReachabilitySummaryFormat_UnreachableSortedBySize(t *testing.T) {
	g := &topology.PackageGraph{Dirs: map[string]*topology.PackageInfo{
		"aaa_small": {Dir: "aaa_small", NumNodes: 2},
		"zzz_big":   {Dir: "zzz_big", NumNodes: 200},
		"mmm_mid":   {Dir: "mmm_mid", NumNodes: 20},
	}}
	res := &topology.ReachabilityResult{Roots: nil, Reachable: map[string]bool{}, Predecessor: map[string]string{}}
	out := formatReachabilitySummary(g, res, nil, "")

	bigIdx := strings.Index(out, "zzz_big (200")
	midIdx := strings.Index(out, "mmm_mid (20")
	smallIdx := strings.Index(out, "aaa_small (2")
	if bigIdx == -1 || midIdx == -1 || smallIdx == -1 {
		t.Fatalf("expected all three packages with their node counts; got:\n%s", out)
	}
	if bigIdx >= midIdx || midIdx >= smallIdx {
		t.Errorf("expected zzz_big < mmm_mid < aaa_small ordering by size descending (opposite of name order); got:\n%s", out)
	}
}

// TestReachabilityBytesCap_ShortUnchangedLongTruncated mutation-covers the
// hard byte-cap backstop in both directions: a response under the cap must be
// returned verbatim (no spurious truncation note), and one over the cap must
// be cut at a line boundary with the note appended and end up at or under the
// cap plus the note's own length.
func TestReachabilityBytesCap_ShortUnchangedLongTruncated(t *testing.T) {
	short := "line one\nline two"
	if got := capReachabilityBytes(short); got != short {
		t.Errorf("short input must be returned unchanged, got %q", got)
	}

	var sb strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&sb, "package/number/%04d\n", i)
	}
	long := sb.String()
	got := capReachabilityBytes(long)
	if len(got) > reachabilityMaxBytes+64 {
		t.Errorf("truncated output too long: %d bytes (cap %d)", len(got), reachabilityMaxBytes)
	}
	if !strings.Contains(got, "[response truncated to fit the byte cap]") {
		t.Errorf("expected the truncation note, got:\n%s", got[len(got)-200:])
	}
	if got == long {
		t.Error("long input must actually be truncated, not returned verbatim")
	}
}

// TestReachabilityRootsDedup_DedupsAndSorts pins the small root-list helper used
// by default-root resolution: duplicates collapse and order is deterministic.
func TestReachabilityRootsDedup_DedupsAndSorts(t *testing.T) {
	got := dedupSortedStrings([]string{"b", "a", "b", "c", "a"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("dedupSortedStrings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupSortedStrings = %v, want %v", got, want)
		}
	}
}

// TestReachabilityRootList_LabelsCandidateSeeded pins the candidate-seeded label —
// dropping it would present a name-matched topology_routes guess as an
// authoritative root, the exact over-claim topology_routes itself warns
// against.
func TestReachabilityRootList_LabelsCandidateSeeded(t *testing.T) {
	got := formatRootList([]string{"cmd/main", "internal/handlers"}, map[string]bool{"internal/handlers": true})
	if !strings.Contains(got, "cmd/main") || strings.Contains(got, "cmd/main (candidate-seeded)") {
		t.Errorf("cmd/main must not be labelled candidate-seeded, got %q", got)
	}
	if !strings.Contains(got, "internal/handlers (candidate-seeded)") {
		t.Errorf("internal/handlers must be labelled candidate-seeded, got %q", got)
	}
}
