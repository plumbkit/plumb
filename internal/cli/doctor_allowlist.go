package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// checkLeanAllowlists surfaces what `plumb doctor` can usefully say about each
// --lean-capable client's tool surface beyond "registered": either plumb is
// advertising its whole tool registry and the client's own config could trim it
// with an allowlist, or the allowlist that is there does not work.
//
// One parameterised check serves all three clients (leanAllowlistClients) —
// their configs differ only in serialisation, server key, and the name of the
// allowlist key, all of which the leanClient descriptor carries.
func checkLeanAllowlists() []checkResult {
	var out []checkResult
	for _, c := range leanAllowlistClients() {
		path, err := c.pathFn()
		if err != nil {
			continue
		}
		if r, ok := leanHintAt(c, path); ok {
			out = append(out, r)
		}
	}
	return out
}

// leanHintAt is checkLeanAllowlists' path-injectable body, so a test can drive
// an allowlist that is present, absent, degenerate, or in a config that does not
// register plumb at all. ok is false when the config is absent, does not register
// plumb, or carries a working allowlist — there is nothing to say in those cases,
// and an exactly-current allowlist passing in silence is deliberate: doctor
// speaks up on drift and misconfiguration, not on a healthy default.
//
// It reads through c.parse rather than the setup-side reader, which creates the
// parent directory for an absent config — a doctor check must never write to the
// filesystem it is inspecting.
func leanHintAt(c leanClient, cfgPath string) (checkResult, bool) {
	raw, has, ok := leanAllowlistValue(c, cfgPath)
	if !ok {
		return checkResult{}, false
	}
	if !has {
		return leanFullSurfaceHint(c), true
	}
	return leanAllowlistResult(c, gradeToolAllowlist(raw, registeredToolNames(), tools.LeanToolNames()))
}

// leanAllowlistValue reads c's allowlist value out of cfgPath, creating nothing.
// ok is false when the config is absent, will not parse, or does not register
// plumb — there is nothing to say about any of those here (checkOneClient
// already fails the run for a config that will not parse, and reporting the same
// fault twice would put two lines against one broken file). has distinguishes a
// plumb entry carrying the key from one without it.
func leanAllowlistValue(c leanClient, cfgPath string) (raw any, has, ok bool) {
	cfg, err := c.parse(cfgPath)
	if err != nil {
		return nil, false, false
	}
	servers, isMap := cfg[c.serversKey].(map[string]any)
	if !isMap {
		return nil, false, false
	}
	entry, isMap := servers["plumb"].(map[string]any)
	if !isMap {
		return nil, false, false
	}
	raw, has = entry[c.key]
	return raw, has, true
}

// leanAllowlistPresent reports whether cfgPath currently carries c's allowlist
// key on its plumb entry — the signal repointFix needs to keep `--lean` on the
// command it tells the user to run.
func leanAllowlistPresent(c leanClient, cfgPath string) bool {
	_, has, ok := leanAllowlistValue(c, cfgPath)
	return ok && has
}

// repointFix is the fix line for a client whose registered binary is missing or
// stale: the `plumb setup` invocation that repoints it.
//
// It keeps `--lean` when that client's config carries a tool allowlist today,
// and that is not cosmetic. For Codex and Gemini CLI a bare re-register CLEARS
// the key, so the unqualified command doctor used to print would have widened
// the user's tool surface from 21 back to 57 as a side effect of following
// doctor's own advice about a moved binary. Kimi Code preserves the key either
// way, but `--lean` is still the better suggestion there: it refreshes a
// snapshot that may have aged past the current lean set.
func repointFix(c setupTarget, cfgPath string) string {
	cmd := "plumb setup " + c.use
	if lc, ok := leanClientFor(c.use); ok && leanAllowlistPresent(lc, cfgPath) {
		cmd += " --lean"
	}
	return fmt.Sprintf("run `%s` to repoint at the current binary", cmd)
}

// registeredToolNames is the live set of tool names plumb advertises, used to
// tell a name that filters to a real tool from one that filters to nothing.
// mcp.SelftestToolNames is the canonical roster: the integration harness
// (cmd/smoke) asserts it equals the live tools/list, so it cannot drift from
// what a client can actually enable.
func registeredToolNames() []string { return mcp.SelftestToolNames() }

// leanAllowlistResult turns a grade into the doctor line for it, or reports
// ok=false when the allowlist is doing its job and doctor has nothing to say.
func leanAllowlistResult(c leanClient, g allowlistGrade) (checkResult, bool) {
	switch g.verdict {
	case allowlistDegenerate:
		return degenerateAllowlistResult(c, g), true
	case allowlistUnrecognised:
		return unknownAllowlistResult(c, g), true
	case allowlistStale:
		return staleAllowlistResult(c, g), true
	default:
		return checkResult{}, false
	}
}

// leanFullSurfaceHint is the no-allowlist result, and it is INFORMATIONAL:
// ok=true, warn=false, no fix line. A full registration is a perfectly valid
// default — it is what every other client gets — so flagging it as a warning
// would put a "!" against a healthy machine and inflate doctor's warning count
// for a preference. The suggestion goes in the detail, which prints on a clean
// pass; fix lines only render on attention.
func leanFullSurfaceHint(c leanClient) checkResult {
	return checkResult{
		name: c.checkName(),
		ok:   true,
		detail: fmt.Sprintf("no client-side allowlist, so %s loads whatever plumb advertises "+
			"(every tool under the default profile) — %s writes an "+
			"%s allowlist trimming it to the %d-tool lean set",
			c.name, c.leanCmd(), c.key, len(tools.LeanToolNames())),
	}
}

// degenerateAllowlistResult grades an allowlist key that cannot function
// as an allowlist. All three shapes are a WARNING (ok=true, warn=true, with a
// fix) — none is a value plumb ever writes, and each leaves the integration in a
// state the user cannot see from the outside. It stays non-fatal because
// doctor's exit code is reserved for plumb itself being broken, and this is a
// hand-edited client config plumb can rewrite in one command.
//
// The message is per-shape because the shapes do not mean the same thing, and
// one sentence for all three asserted a client behaviour only the empty list
// plausibly has:
//
//   - EMPTY LIST — the definite case. An allowlist that permits nothing
//     filters every plumb tool out, so the server connects with nothing callable.
//   - NULL — most clients read a null option as "unset", i.e. the full tool
//     surface, so claiming it loads nothing is probably wrong. plumb cannot
//     verify which way the client takes it, and says so rather than guessing.
//   - NOT A LIST — plumb cannot verify how the client parses a value of the
//     wrong type at all: ignored, coerced, or the whole server entry rejected.
func degenerateAllowlistResult(c leanClient, g allowlistGrade) checkResult {
	res := checkResult{name: c.checkName(), ok: true, warn: true, fix: leanAllowlistFix(c)}
	switch g.shape {
	case shapeNull:
		res.detail = c.key + " is null — a client most likely reads that as no allowlist at all " +
			"(the full tool surface), but plumb cannot verify how " + c.name + " takes it"
		res.fix = fmt.Sprintf("delete the %s key to mean the full tool surface unambiguously, "+
			"or run %s to pin the %d-tool lean allowlist", c.key, c.leanCmd(), len(tools.LeanToolNames()))
	case shapeWrongType:
		res.detail = c.key + " is " + g.found + ", not a list — plumb cannot verify how " + c.name +
			" parses that: it may ignore the key, or refuse the whole server entry"
	default: // shapeEmpty
		res.detail = c.key + " is " + g.found + " — " + c.name + " loads NO plumb tools at all; " +
			"the server connects but nothing it offers is callable"
	}
	return res
}

// unknownAllowlistResult grades a list that is shaped correctly and still
// filters plumb to nothing, because not one of its entries names a tool plumb
// registers — a typo'd or wholly invented list. Same severity as a degenerate
// value: the observable outcome is identical (zero plumb tools), and the user
// cannot see it from the outside.
func unknownAllowlistResult(c leanClient, g allowlistGrade) checkResult {
	return checkResult{
		name: c.checkName(),
		ok:   true,
		warn: true,
		detail: fmt.Sprintf("%s lists %d name(s), none of which plumb registers (%s) — "+
			"%s loads NO plumb tools at all; the server connects but nothing it offers is callable",
			c.key, len(g.names), strings.Join(capNames(g.unknown, 3), ", "), c.name),
		fix: leanAllowlistFix(c),
	}
}

// staleAllowlistResult grades a list that is recognisably plumb's own
// snapshot but no longer equals the lean set — the allowlist's one documented
// failure mode, since it is written once and never refreshed.
//
// INFORMATIONAL (ok=true, warn=false, no fix), like the no-allowlist hint: the
// list still works, every tool in it is still callable, and the user may be
// pinning an older set deliberately. Drift is worth saying out loud, not worth a
// "!".
func staleAllowlistResult(c leanClient, g allowlistGrade) checkResult {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is a snapshot of an older lean set (%d name(s) listed, %d today)",
		c.key, len(g.names), len(tools.LeanToolNames()))
	if len(g.missing) > 0 {
		fmt.Fprintf(&b, "; missing: %s", strings.Join(capNames(g.missing, 3), ", "))
	}
	if len(g.unknown) > 0 {
		fmt.Fprintf(&b, "; no longer registered: %s", strings.Join(capNames(g.unknown, 3), ", "))
	}
	fmt.Fprintf(&b, " — re-run %s to refresh it", c.leanCmd())
	return checkResult{name: c.checkName(), ok: true, detail: b.String()}
}

func leanAllowlistFix(c leanClient) string {
	return fmt.Sprintf("run %s to write the %d-tool lean allowlist, "+
		"or delete the %s key to restore the full tool surface",
		c.leanCmd(), len(tools.LeanToolNames()), c.key)
}

// capNames renders at most n names, appending "+N more" for the rest, so a
// detail line cannot grow with the size of a hand-edited list. It caps a SLICE
// by element count and never touches a name it keeps — deliberately not spelt
// truncate*, which internal/arch reserves for string truncation (textfmt's rune
// and byte budgets); an allowlist entry saying "this one is different" is upkeep
// a name that is already different does not need.
func capNames(names []string, n int) []string {
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

// allowlistShape distinguishes the three ways an allowlist value can fail to be
// one. They are graded together but described separately: only the empty list
// definitely leaves the client with no tools (see kimiDegenerateAllowlistResult).
type allowlistShape int

const (
	shapeEmpty     allowlistShape = iota // [] or a list holding no tool name
	shapeNull                            // an explicit JSON null
	shapeWrongType                       // any non-list value
)

// allowlistGrade is a verdict plus the facts a message needs to be specific.
type allowlistGrade struct {
	verdict allowlistVerdict
	shape   allowlistShape // degenerate only: which way the value fails
	found   string         // degenerate only: the value in JSON vocabulary
	names   []string       // the usable (non-empty string) entries
	unknown []string       // entries naming no registered tool
	missing []string       // pinned names the list lacks
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
	if !isList {
		return degenerate(shapeOf(raw), configValueTypeName(raw))
	}
	if len(list) == 0 {
		return degenerate(shapeEmpty, "an empty list")
	}
	g := allowlistGrade{names: usableNames(list)}
	if len(g.names) == 0 {
		return degenerate(shapeEmpty, "a list holding no tool name")
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

func degenerate(shape allowlistShape, found string) allowlistGrade {
	return allowlistGrade{verdict: allowlistDegenerate, shape: shape, found: found}
}

// shapeOf classifies a non-list value: null is its own case because a client
// almost certainly reads it as "unset", where any other wrong type is anyone's
// guess.
func shapeOf(raw any) allowlistShape {
	if raw == nil {
		return shapeNull
	}
	return shapeWrongType
}

// configValueTypeName names a decoded value in the user's own vocabulary. The
// message is about a config file they wrote, so %T was the wrong alphabet
// entirely: it reported "not a list (float64)" or "map[string]interface {}" —
// Go type names for a language the reader is not writing in.
//
// It serves BOTH decoders, which is why the integer cases matter. JSON numbers
// all arrive as float64, but go-toml decodes `enabled_tools = 3` to an int64, so
// a TOML-only shape used to fall through to the unhelpful default. TOML's
// date-time values land as time.Time (offset date-times) or go-toml's Local*
// types; the first is named, the rest are rare enough to take the default
// honestly rather than be enumerated for their own sake.
func configValueTypeName(raw any) string {
	switch raw.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64, float32, json.Number,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return "a number"
	case string:
		return "a string"
	case time.Time:
		return "a date-time"
	case []any:
		return "a list"
	case map[string]any:
		// A TOML table reads as "an object" too: both name a keyed group, and the
		// sentence it lands in ("… is an object, not a list") is unambiguous in
		// either format.
		return "an object"
	default:
		return "a value plumb does not recognise"
	}
}
