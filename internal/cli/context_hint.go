package cli

// context_hint.go — the daemon half of plumb's advisory context hints (PLAN-462
// Slice B): the `context-hint` control command a lifecycle hook asks, and the
// service that answers it.
//
// A hint is a few lines of selectors, locations and provenance that a hook can
// put in front of an agent at the moment it is most likely to need them — after
// it names a file in its prompt, after a compaction, when a subagent starts. It
// is never source, documentation, memory or mail text, and asking for one never
// records a read: this service is built with no read tracker in reach, and
// neither is the collector it asks (tools.ContextHinter). The only thing it
// persists is the bounded observation ledger (internal/contexthints), which is
// also where the per-turn and per-agent byte allowances live, so that neither a
// compaction, an eviction nor a daemon restart can mint a fresh one.
//
// Every failure is silent to the agent and visible in the ledger: off, no live
// identity, no collector, nothing to say, over budget, or an error all answer
// with no text.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/plumbkit/plumb/internal/contexthints"
	"github.com/plumbkit/plumb/internal/tools"
)

const (
	ctrlContextHintCommand = "context-hint "
	// contextHintBudget is the whole daemon-side answer, inside the hook's own
	// 5 s timeout: a cold index or a slow collector costs the agent nothing.
	contextHintBudget = 300 * time.Millisecond
	// The gate-file allowances (PLAN-462 §Phase 3b).
	contextHintPerTurn  = 1024
	contextHintPerAgent = 8192
	// maxContextHintSelectors caps what one prompt may nominate. The adapter caps
	// first; the daemon caps again, because the control socket is the boundary.
	maxContextHintSelectors = 8
	maxContextHintRequest   = 8 << 10
	maxContextHintStates    = 1024
	maxHintField            = 200
)

// contextHintRequest is what a host adapter sends. Selectors are explicit
// tokens the adapter lifted from the prompt — the prompt itself never leaves
// the hook process.
type contextHintRequest struct {
	Host        string   `json:"host"`
	HostVersion string   `json:"host_version,omitempty"`
	Event       string   `json:"event"`
	Source      string   `json:"source,omitempty"`
	SessionID   string   `json:"session_id"`
	AgentID     string   `json:"agent_id,omitempty"`
	TurnID      string   `json:"turn_id,omitempty"`
	Selectors   []string `json:"selectors,omitempty"`
	// Off and On carry PLUMB_CONTEXT_HINTS from the hook's environment, which a
	// user's shell export reaches and the daemon's may not. Either outranks the
	// daemon's own setting; Off outranks On.
	Off bool `json:"off,omitempty"`
	On  bool `json:"on,omitempty"`
}

// contextHintReply is the answer: an outcome, and the text to show, which is
// empty for every outcome but emitted.
type contextHintReply struct {
	Outcome contexthints.Outcome `json:"outcome"`
	Text    string               `json:"text,omitempty"`
}

// subagentRule is what a starting subagent is told when its parent has no
// validated seeds to pass on: how to get context, not what the context is.
const subagentRule = "Plumb: before changing code you start from known files or symbols, call context_for_task with them; use read_symbol for one body. Hints name code, never quote it.\n"

const hintHeader = "Plumb context hint (selectors only; pull bodies with context_for_task or read_symbol):\n"

// hintRoot is where a hint's caller works. Inherited marks a subagent that has
// made no call yet: the root is the one its first call will be seeded with.
type hintRoot struct {
	Path      string
	Inherited bool
}

// inheritedRootNote labels a hint whose root a subagent has not used yet.
const inheritedRootNote = "root: inherited (child has made no call yet)\n"

// contextHintService answers context-hint. Every dependency is injected; there
// is deliberately no field through which it could reach a read tracker.
type contextHintService struct {
	hinter  tools.ContextHinter
	ledger  *contexthints.Store
	resolve func(external string) (hintRoot, bool)
	enabled func() bool
	now     func() time.Time
	limits  contexthints.Limits

	mu    sync.Mutex
	turns map[string]int                 // agent key → prompts seen, for hosts with no turn id
	last  map[string][]tools.ContextSeed // agent key → the seeds behind its last emitted hint
}

func newContextHintService(hinter tools.ContextHinter, ledger *contexthints.Store,
	resolve func(string) (hintRoot, bool), enabled func() bool,
) *contextHintService {
	return &contextHintService{
		hinter: hinter, ledger: ledger, resolve: resolve, enabled: enabled, now: time.Now,
		limits: contexthints.Limits{PerTurn: contextHintPerTurn, PerAgent: contextHintPerAgent},
		turns:  map[string]int{}, last: map[string][]tools.ContextSeed{},
	}
}

func hintAgentKey(sessionID, agentID string) string { return sessionID + "\x00" + agentID }

// hintExternal is the identity the daemon knows the caller by: the
// conversation, or `<conversation>/<agent>` for a subagent, as the Claude Code
// identity hook stamps it.
func hintExternal(sessionID, agentID string) string {
	if agentID == "" {
		return sessionID
	}
	return sessionID + "/" + agentID
}

// window returns the turn window this request is charged to. A host turn id
// wins. Otherwise each UserPromptSubmit opens a new window, and anything before
// the first prompt (SessionStart, SubagentStart) counts toward window 1. Stop
// is not used: it is skipped when a turn is interrupted.
func (c *contextHintService) window(req contextHintRequest) string {
	if req.TurnID != "" {
		return "t:" + req.TurnID
	}
	key := hintAgentKey(req.SessionID, req.AgentID)
	c.mu.Lock()
	defer c.mu.Unlock()
	seq := c.turns[key]
	if req.Event == "UserPromptSubmit" {
		if _, ok := c.turns[key]; ok || len(c.turns) < maxContextHintStates {
			seq++
			c.turns[key] = seq
		}
	}
	return fmt.Sprintf("w:%d", max(seq, 1))
}

func (c *contextHintService) remember(key string, seeds []tools.ContextSeed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.last[key]; ok || len(c.last) < maxContextHintStates {
		c.last[key] = seeds
	}
}

func (c *contextHintService) recall(key string) []tools.ContextSeed {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last[key]
}

// serve answers one request and records it. It never returns an error: every
// failure is an outcome with no text.
func (c *contextHintService) serve(ctx context.Context, req contextHintRequest) contextHintReply {
	start := c.now()
	turn := c.window(req)
	d := c.decide(ctx, req, turn)
	outcome, detail, text, seeds := d.outcome, d.detail, d.text, d.seeds
	_ = c.ledger.Record(contexthints.Observation{
		At: start, Workspace: c.ledgerRoot(req), SessionID: req.SessionID, AgentID: req.AgentID,
		Host: req.Host, HostVersion: req.HostVersion, Event: req.Event, Source: req.Source,
		Seeds: seedStrings(seeds), Emitted: emittedIf(outcome, d.emitted), EmittedBytes: len(text), Duration: c.now().Sub(start),
		Outcome: outcome, Detail: detail,
	})
	return contextHintReply{Outcome: outcome, Text: text}
}

func (c *contextHintService) ledgerRoot(req contextHintRequest) string {
	if req.SessionID == "" || c.resolve == nil {
		return ""
	}
	root, _ := c.resolve(hintExternal(req.SessionID, req.AgentID))
	return root.Path
}

// hintDecision is what one request came to. text is set only for a hint that is
// still to be charged (outcome unset) or was (outcome emitted); every other
// outcome is constructed without it, which is what keeps a refused or failed
// hint from reaching the agent — there is no later step that strips it.
type hintDecision struct {
	outcome contexthints.Outcome
	detail  string
	text    string
	seeds   []tools.ContextSeed
	emitted []string // the selectors the rendered text names
}

// emittedIf returns the named selectors only for a hint that was emitted: a
// refused or failed one named nothing to the agent.
func emittedIf(o contexthints.Outcome, sels []string) []string {
	if o != contexthints.OutcomeEmitted {
		return nil
	}
	return sels
}

func noopHint(detail string) hintDecision {
	return hintDecision{outcome: contexthints.OutcomeNoop, detail: detail}
}

func (c *contextHintService) decide(ctx context.Context, req contextHintRequest, turn string) hintDecision {
	root, refused := c.admit(req)
	if refused != "" {
		return noopHint(refused)
	}
	seeds, ruleOnly, detail := c.seedsFor(req, root.Path)
	d := c.compose(ctx, req, root, seeds, ruleOnly, detail)
	if d.text == "" {
		return d
	}
	return c.charge(req, turn, d)
}

// admit resolves the caller's root, or names why there is none to hint for.
func (c *contextHintService) admit(req contextHintRequest) (root hintRoot, refused string) {
	switch {
	case req.Off, !req.On && (c.enabled == nil || !c.enabled()):
		return hintRoot{}, "off"
	case req.SessionID == "":
		return hintRoot{}, "no-session"
	case c.resolve == nil:
		return hintRoot{}, "no-root"
	}
	root, ok := c.resolve(hintExternal(req.SessionID, req.AgentID))
	if !ok || root.Path == "" {
		return hintRoot{}, "no-root"
	}
	return root, ""
}

// compose asks the collector and renders what it says. A subagent start that
// yields no line still gets the tool-choice rule.
func (c *contextHintService) compose(ctx context.Context, req contextHintRequest, root hintRoot,
	seeds []tools.ContextSeed, ruleOnly bool, detail string,
) hintDecision {
	switch {
	case len(seeds) == 0 && ruleOnly, len(seeds) > 0 && c.hinter == nil && ruleOnly:
		return hintDecision{text: subagentRule, seeds: seeds}
	case len(seeds) == 0:
		return noopHint(detail)
	case c.hinter == nil:
		return noopHint("no-collector")
	}
	res, err := c.hinter.Hint(ctx, tools.HintRequest{
		Workspace: root.Path, Agent: hintExternal(req.SessionID, req.AgentID), Seeds: seeds,
		MaxBytes: c.limits.PerTurn, Deadline: c.now().Add(contextHintBudget),
	})
	if err != nil {
		return hintDecision{outcome: contexthints.OutcomeError, detail: "collector", seeds: seeds}
	}
	note := ""
	if root.Inherited {
		note = inheritedRootNote
	}
	if text, shown := renderContextHint(res, c.limits.PerTurn, note); text != "" {
		return hintDecision{text: text, seeds: seeds, emitted: lineSelectors(res.Lines[:shown])}
	}
	if ruleOnly {
		return hintDecision{text: subagentRule, seeds: seeds}
	}
	return hintDecision{outcome: contexthints.OutcomeNoop, detail: "no-lines", seeds: seeds}
}

// charge spends the hint's bytes from the caller's allowances, and emits it only
// when both can take it.
func (c *contextHintService) charge(req contextHintRequest, turn string, d hintDecision) hintDecision {
	granted, err := c.ledger.Spend(req.SessionID, req.AgentID, turn, len(d.text), c.limits, c.now())
	switch {
	case err != nil:
		return hintDecision{outcome: contexthints.OutcomeError, detail: "ledger", seeds: d.seeds}
	case !granted:
		return hintDecision{outcome: contexthints.OutcomeThrottled, detail: "budget", seeds: d.seeds}
	}
	if len(d.seeds) > 0 {
		c.remember(hintAgentKey(req.SessionID, req.AgentID), d.seeds)
	}
	d.outcome = contexthints.OutcomeEmitted
	return d
}

// seedsFor picks the seeds an event may hint from. ruleOnly marks a subagent
// start, which gets the tool-choice rule even with nothing to name.
func (c *contextHintService) seedsFor(req contextHintRequest, root string) (seeds []tools.ContextSeed, ruleOnly bool, detail string) {
	switch req.Event {
	case "UserPromptSubmit":
		seeds = promptSeeds(root, req.Selectors)
		return seeds, false, "no-seeds"
	case "SessionStart":
		switch req.Source {
		case "resume", "compact":
			return revalidateSeeds(root, c.recall(hintAgentKey(req.SessionID, req.AgentID))), false, "no-seeds"
		}
		return nil, false, "startup"
	case "SubagentStart":
		// The parent's validated seeds, re-checked against the CHILD's own root:
		// a parent's path outside it is dropped, never inherited.
		return revalidateSeeds(root, c.recall(hintAgentKey(req.SessionID, ""))), true, ""
	}
	return nil, false, "event"
}

var hintSymbolRe = regexp.MustCompile(`^[A-Za-z_(*][A-Za-z0-9_.()*/\[\]]*$`)

// promptSeeds turns the selectors a prompt nominated into seeds the workspace
// can stand behind: a path that names an existing regular file inside root, or
// a symbol selector. Anything else — a path that escapes root, another root's
// absolute path, a missing file, prose — is dropped, not guessed at.
func promptSeeds(root string, selectors []string) []tools.ContextSeed {
	var out []tools.ContextSeed
	seen := map[tools.ContextSeed]bool{}
	for i, s := range selectors {
		if i == maxContextHintSelectors {
			break
		}
		s = strings.TrimSpace(s)
		if s == "" || len(s) > maxHintField || strings.ContainsFunc(s, unicode.IsSpace) {
			continue
		}
		seed, ok := pathSeed(root, s)
		if !ok && hintSymbolRe.MatchString(s) && !strings.Contains(s, "/") {
			seed, ok = tools.ContextSeed{Symbol: s}, true
		}
		if ok && !seen[seed] {
			seen[seed] = true
			out = append(out, seed)
		}
	}
	return out
}

// pathSeed resolves s as a workspace file: relative to root, or absolute inside
// it. The result is root-relative and names an existing regular file.
func pathSeed(root, s string) (tools.ContextSeed, bool) {
	p := filepath.FromSlash(s)
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return tools.ContextSeed{}, false
	}
	info, err := os.Lstat(filepath.Join(root, rel))
	if err != nil || !info.Mode().IsRegular() {
		return tools.ContextSeed{}, false
	}
	return tools.ContextSeed{Path: filepath.ToSlash(rel)}, true
}

// revalidateSeeds keeps the seeds that still stand in root: paths that still
// name a regular file inside it, and symbol selectors (which the collector
// resolves against root itself).
func revalidateSeeds(root string, seeds []tools.ContextSeed) []tools.ContextSeed {
	var out []tools.ContextSeed
	for _, s := range seeds {
		if s.Path != "" {
			ps, ok := pathSeed(root, s.Path)
			if !ok {
				continue
			}
			s.Path = ps.Path
		}
		out = append(out, s)
	}
	return out
}

func seedStrings(seeds []tools.ContextSeed) []string {
	out := make([]string, 0, len(seeds))
	for _, s := range seeds {
		switch {
		case s.Path != "" && s.Symbol != "":
			out = append(out, s.Path+"::"+s.Symbol)
		case s.Path != "":
			out = append(out, s.Path)
		default:
			out = append(out, s.Symbol)
		}
	}
	return out
}

// hintField makes one value safe for a single hint line: no control
// characters, so a value can never start a line of its own, and bounded.
func hintField(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > maxHintField {
		// Cutting at a byte count can split a rune; drop the partial one.
		s = strings.ToValidUTF8(s[:maxHintField], "")
	}
	return s
}

// lineSelectors is what each hint line names: its selector, or its path when it
// has none.
func lineSelectors(lines []tools.HintLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if s := hintField(l.Selector); s != "" {
			out = append(out, s)
		} else if p := hintField(l.Path); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// renderContextHint renders a hint within maxBytes, keeping only whole lines,
// and reports how many of res.Lines it showed (always a prefix). note, when
// set, is a fixed label line under the header. A hint with no line that fits
// renders as "": a bare header names nothing.
func renderContextHint(res tools.HintResult, maxBytes int, note string) (string, int) {
	var b strings.Builder
	b.WriteString(hintHeader)
	b.WriteString(note)
	shown := 0
	for _, l := range res.Lines {
		line := "- " + hintField(l.Selector)
		if l.Path != "" {
			line += " — " + hintField(l.Path)
			if l.Line > 0 {
				line += fmt.Sprintf(":%d", l.Line)
			}
		}
		var tags []string
		for _, t := range []string{hintField(l.Kind), hintField(l.Provenance)} {
			if t != "" {
				tags = append(tags, t)
			}
		}
		if len(tags) > 0 {
			line += " (" + strings.Join(tags, ", ") + ")"
		}
		line += "\n"
		// Leave room for the omission line that may follow.
		if b.Len()+len(line)+len("(+0000 more)\n") > maxBytes {
			break
		}
		b.WriteString(line)
		shown++
	}
	if shown == 0 {
		return "", 0
	}
	if more := len(res.Lines) - shown + res.Omitted; more > 0 {
		fmt.Fprintf(&b, "(+%d more)\n", more)
	}
	return b.String(), shown
}

// handleContextHintCommand answers `context-hint <json>` on the control
// socket. The reply carries an outcome and, only when emitted, the hint text;
// never paths of databases, error details or candidates.
func handleContextHintCommand(out io.Writer, line string, h ctrlHandlers) bool {
	payload, matched := strings.CutPrefix(line, ctrlContextHintCommand)
	if !matched {
		return false
	}
	var req contextHintRequest
	if len(payload) > maxContextHintRequest || json.Unmarshal([]byte(payload), &req) != nil || h.contextHint == nil {
		fmt.Fprintln(out, "error: context hint unavailable")
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), contextHintBudget)
	defer cancel()
	reply := h.contextHint(ctx, req)
	fmt.Fprint(out, "ok ")
	_ = json.NewEncoder(out).Encode(reply)
	return true
}
