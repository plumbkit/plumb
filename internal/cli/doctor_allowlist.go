package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// checkKimiLeanHint surfaces what `plumb doctor` can usefully say about Kimi
// Code's tool surface beyond "registered": either plumb is advertising its whole
// tool registry and Kimi's own mcp.json could trim it with an enabledTools
// allowlist, or the allowlist that is there does not work. ok is false when the
// config is absent, does not register plumb, or carries a working allowlist —
// there is nothing to say in those cases.
func checkKimiLeanHint() (checkResult, bool) {
	path, err := KimiCodeConfigPath()
	if err != nil {
		return checkResult{}, false
	}
	return kimiLeanHintAt(path)
}

// kimiLeanHintAt is checkKimiLeanHint's path-injectable body, so a test can
// drive an allowlist that is present, absent, degenerate, or in a config that
// does not register plumb at all.
//
// It reads the file directly rather than through readOrInitClaudeConfig, which
// creates the parent directory for an absent config — a doctor check must never
// write to the filesystem it is inspecting.
func kimiLeanHintAt(cfgPath string) (checkResult, bool) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return checkResult{}, false
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		// checkOneClient already fails the run for a config that will not parse
		// (classifyClientBinary); reporting the same fault twice would put two
		// lines against one broken file.
		return checkResult{}, false
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return checkResult{}, false
	}
	entry, ok := servers["plumb"].(map[string]any)
	if !ok {
		return checkResult{}, false
	}
	raw, has := entry["enabledTools"]
	if !has {
		return kimiFullSurfaceHint(), true
	}
	return kimiAllowlistResult(gradeToolAllowlist(raw, registeredToolNames(), tools.LeanToolNames()))
}

const kimiToolSurfaceCheck = "Kimi Code (tool surface)"

// registeredToolNames is the live set of tool names plumb advertises, used to
// tell a name that filters to a real tool from one that filters to nothing.
// mcp.SelftestToolNames is the canonical roster: the integration harness
// (cmd/smoke) asserts it equals the live tools/list, so it cannot drift from
// what a client can actually enable.
func registeredToolNames() []string { return mcp.SelftestToolNames() }

// kimiAllowlistResult turns a grade into the doctor line for it, or reports
// ok=false when the allowlist is doing its job and doctor has nothing to say.
func kimiAllowlistResult(g allowlistGrade) (checkResult, bool) {
	switch g.verdict {
	case allowlistDegenerate:
		return kimiDegenerateAllowlistResult(g.shape), true
	case allowlistUnrecognised:
		return kimiUnknownAllowlistResult(g), true
	case allowlistStale:
		return kimiStaleAllowlistResult(g), true
	default:
		return checkResult{}, false
	}
}

// kimiFullSurfaceHint is the no-allowlist result, and it is INFORMATIONAL:
// ok=true, warn=false, no fix line. A full registration is a perfectly valid
// default — it is what every other client gets — so flagging it as a warning
// would put a "!" against a healthy machine and inflate doctor's warning count
// for a preference. The suggestion goes in the detail, which prints on a clean
// pass; fix lines only render on attention.
func kimiFullSurfaceHint() checkResult {
	return checkResult{
		name: kimiToolSurfaceCheck,
		ok:   true,
		detail: fmt.Sprintf("no client-side allowlist, so Kimi loads whatever plumb advertises "+
			"(every tool under the default profile) — `plumb setup kimi-code --lean` writes an "+
			"enabledTools allowlist trimming it to the %d-tool lean set", len(tools.LeanToolNames())),
	}
}

// kimiDegenerateAllowlistResult grades an enabledTools key that cannot function
// as an allowlist — null, not a list, empty, or holding no usable name.
//
// This is a WARNING (ok=true, warn=true, with a fix), not the informational line
// above and not a failure. It is a real misconfiguration, not a preference: the
// server still starts, but Kimi filters every plumb tool out of it, so the whole
// integration is silently inert — the same shape as the golangci-lint check,
// where a capability quietly disappears and only doctor can say why. It stays
// non-fatal because doctor's exit code is reserved for plumb itself being
// broken, and this is a hand-edited client config plumb can rewrite in one
// command.
func kimiDegenerateAllowlistResult(shape string) checkResult {
	return checkResult{
		name: kimiToolSurfaceCheck,
		ok:   true,
		warn: true,
		detail: "enabledTools is " + shape + " — Kimi loads NO plumb tools at all; " +
			"the server connects but nothing it offers is callable",
		fix: kimiAllowlistFix(),
	}
}

// kimiUnknownAllowlistResult grades a list that is shaped correctly and still
// filters plumb to nothing, because not one of its entries names a tool plumb
// registers — a typo'd or wholly invented list. Same severity as a degenerate
// value: the observable outcome is identical (zero plumb tools), and the user
// cannot see it from the outside.
func kimiUnknownAllowlistResult(g allowlistGrade) checkResult {
	return checkResult{
		name: kimiToolSurfaceCheck,
		ok:   true,
		warn: true,
		detail: fmt.Sprintf("enabledTools lists %d name(s), none of which plumb registers (%s) — "+
			"Kimi loads NO plumb tools at all; the server connects but nothing it offers is callable",
			len(g.names), strings.Join(truncateNames(g.unknown, 3), ", ")),
		fix: kimiAllowlistFix(),
	}
}

// kimiStaleAllowlistResult grades a list that is recognisably plumb's own
// snapshot but no longer equals the lean set — the allowlist's one documented
// failure mode, since it is written once and never refreshed.
//
// INFORMATIONAL (ok=true, warn=false, no fix), like the no-allowlist hint: the
// list still works, every tool in it is still callable, and the user may be
// pinning an older set deliberately. Drift is worth saying out loud, not worth a
// "!".
func kimiStaleAllowlistResult(g allowlistGrade) checkResult {
	var b strings.Builder
	fmt.Fprintf(&b, "enabledTools is a snapshot of an older lean set (%d name(s) listed, %d today)",
		len(g.names), len(tools.LeanToolNames()))
	if len(g.missing) > 0 {
		fmt.Fprintf(&b, "; missing: %s", strings.Join(truncateNames(g.missing, 3), ", "))
	}
	if len(g.unknown) > 0 {
		fmt.Fprintf(&b, "; no longer registered: %s", strings.Join(truncateNames(g.unknown, 3), ", "))
	}
	b.WriteString(" — re-run `plumb setup kimi-code --lean` to refresh it")
	return checkResult{name: kimiToolSurfaceCheck, ok: true, detail: b.String()}
}

func kimiAllowlistFix() string {
	return fmt.Sprintf("run `plumb setup kimi-code --lean` to write the %d-tool lean allowlist, "+
		"or delete the enabledTools key to restore the full tool surface", len(tools.LeanToolNames()))
}

// truncateNames renders at most n names, appending "+N more" for the rest, so a
// detail line cannot grow with the size of a hand-edited list.
func truncateNames(names []string, n int) []string {
	if len(names) <= n {
		return names
	}
	return append(append([]string{}, names[:n]...), fmt.Sprintf("+%d more", len(names)-n))
}

// allowlistVerdict grades a CLIENT-side tool allowlist — a list of tool names
// held in the client's own MCP config, which the client applies before plumb
// ever sees a call.
type allowlistVerdict int

const (
	// allowlistUsable: the list enables real plumb tools. Either it is exactly
	// the set plumb would write, or it is a deliberate hand-picked selection —
	// which is the user's business, not doctor's.
	allowlistUsable allowlistVerdict = iota
	// allowlistDegenerate: the value cannot function as an allowlist at all
	// (null, not a list, empty, or holding no usable name).
	allowlistDegenerate
	// allowlistUnrecognised: a well-shaped list, but not one of its names is a
	// tool plumb registers — the same practical outcome as degenerate.
	allowlistUnrecognised
	// allowlistStale: recognisably plumb's own written set, but no longer equal
	// to it — the snapshot has aged past the tool set it pinned.
	allowlistStale
)

// allowlistGrade is a verdict plus the facts a message needs to be specific.
type allowlistGrade struct {
	verdict allowlistVerdict
	shape   string   // degenerate only: how the value is shaped, in JSON vocabulary
	names   []string // the usable (non-empty string) entries
	unknown []string // entries naming no registered tool
	missing []string // pinned names the list lacks
}

// gradeToolAllowlist grades one client-side tool allowlist against what plumb
// actually offers. raw is the value as decoded from the client's config;
// registered is the live set of tool names plumb advertises; pinned is the set
// plumb would write for this client today (tools.LeanToolNames() for Kimi's
// --lean). Both name sets are parameters rather than package lookups so the same
// grader serves the next client that grows an allowlist.
//
// Shape alone is not a grade. `["", ""]`, `[null]`, `[1, 2, 3]` and
// `["not_a_plumb_tool"]` are all well-formed non-empty JSON lists that leave the
// client with zero plumb tools, so a check that stopped at "is it a non-empty
// list?" would report a dead integration as healthy — the one failure mode of a
// client-side allowlist that a user cannot see from the outside.
func gradeToolAllowlist(raw any, registered, pinned []string) allowlistGrade {
	list, isList := raw.([]any)
	if !isList || len(list) == 0 {
		return allowlistGrade{verdict: allowlistDegenerate, shape: allowlistShape(raw)}
	}
	g := allowlistGrade{names: usableNames(list)}
	if len(g.names) == 0 {
		return allowlistGrade{verdict: allowlistDegenerate, shape: "a list holding no tool name"}
	}

	known := nameSet(registered)
	for _, n := range g.names {
		if !known[n] {
			g.unknown = append(g.unknown, n)
		}
	}
	if len(g.unknown) == len(g.names) {
		g.verdict = allowlistUnrecognised
		return g
	}

	listed := nameSet(g.names)
	for _, p := range pinned {
		if !listed[p] {
			g.missing = append(g.missing, p)
		}
	}
	g.verdict = driftVerdict(g, pinned)
	return g
}

// driftVerdict decides whether a usable list counts as plumb's own aged
// snapshot or as a deliberate selection.
//
// A list holding the whole pinned set and nothing plumb has retired is current,
// however many extra tools the user added on top — adding to plumb's set is a
// choice, not drift. Otherwise the test is majority overlap: a stale snapshot
// WAS the pinned set when it was written, so it still holds most of it and
// differs by the name or two the set has since gained or lost. A hand-picked
// list (`["read_file", "edit_file"]`) overlaps far less, and telling that user
// their list "is stale" would be nagging them about a choice they made on
// purpose.
func driftVerdict(g allowlistGrade, pinned []string) allowlistVerdict {
	if len(g.missing) == 0 && len(g.unknown) == 0 {
		return allowlistUsable
	}
	if matched := len(pinned) - len(g.missing); matched*2 > len(pinned) {
		return allowlistStale
	}
	return allowlistUsable
}

// usableNames returns the list entries that could name a tool: non-empty
// strings. A number, null, or an object is not a name a client can match.
func usableNames(list []any) []string {
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// allowlistShape names a value that cannot be an allowlist, so the detail line
// says which hand-edit produced it.
func allowlistShape(raw any) string {
	switch v := raw.(type) {
	case nil:
		return "null"
	case []any:
		return "an empty list"
	default:
		return fmt.Sprintf("not a list (%T)", v)
	}
}
