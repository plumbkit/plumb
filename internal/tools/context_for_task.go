package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Budget and work limits for context_for_task. The numbers come from the
// PLAN-462 release shape; each is a hard limit, and a request that meets one is
// told so rather than silently trimmed.
const (
	contextDefaultMaxBytes = 12000
	contextMaxBytesCap     = 32000
	// ContextReserveBytes is held back from max_bytes for the text the connection
	// layer appends to a served result (mailbox preview, policy notes). The pack
	// renders into max_bytes minus this, so the served total stays within max_bytes;
	// the connection layer holds what it appends to this much (AppendRoom).
	ContextReserveBytes = 1024
	contextReserveBytes = ContextReserveBytes
	// contextMinMaxBytes leaves room for the header and an omission footer after
	// the reserve; a smaller budget could not carry even the truncation notice.
	contextMinMaxBytes = contextReserveBytes + 512
	contextMaxSeeds    = 8
	// contextMaxCandidates bounds the candidates shown for an ambiguous or
	// unmatched selector.
	contextMaxCandidates = 5
)

const (
	contextIntentUnderstand = "understand"
	contextIntentChange     = "change"
)

var contextSHA256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

var contextForTaskSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Seed files (not directories)."
    },
    "symbols": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Seed selectors: path#Recv.Method or a bare name."
    },
    "intent": {
      "type": "string",
      "enum": ["understand", "change"],
      "description": "Default understand."
    },
    "task": {
      "type": "string",
      "description": "Prose; re-ranks only, never seeds."
    },
    "within": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Narrow to these paths or globs; never widens."
    },
    "corpora": {
      "type": "array",
      "items": {"type": "string", "enum": ["code", "docs", "memory"]},
      "description": "Default all permitted."
    },
    "max_bytes": {
      "type": "integer",
      "description": "Whole-response budget (default 12000, cap 32000)."
    },
    "have": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "symbol": {"type": "string"},
          "content_sha256": {"type": "string"}
        },
        "required": ["symbol", "content_sha256"],
        "additionalProperties": false
      },
      "description": "Bodies you still hold."
    }
  },
  "additionalProperties": false
}`)

// ContextForTask is the context_for_task MCP tool: a bounded, seeds-first
// context pack. It is a thin orchestrator over ContextCollector (gathering),
// renderContext (presentation and packing) and the calling agent's read tracker
// (recording the bodies that were delivered).
//
// The tracker lives here and not on the collector on purpose: gathering a pack
// never records a read. Only a body that survived packing and was rendered is a
// read, and only the tool knows which those are.
//
// Concurrency: Execute is safe for concurrent use.
type ContextForTask struct {
	collector *ContextCollector
	tracker   *ReadTracker
	readsFor  func(ctx context.Context) *ReadTracker // PLAN-286: per-agent resolver; overrides tracker
}

// NewContextForTask returns the tool over collector. A nil collector is valid
// for schema-only use (the catalogue tests); Execute then refuses.
func NewContextForTask(collector *ContextCollector) *ContextForTask {
	return &ContextForTask{collector: collector}
}

// WithReads wires the connection-level ReadTracker, the fallback when no
// per-agent resolver answers.
func (t *ContextForTask) WithReads(tracker *ReadTracker) *ContextForTask {
	t.tracker = tracker
	return t
}

// WithReadsFor wires a per-call ReadTracker resolver (PLAN-286): on a shared
// connection each logical agent records its reads against its own tracker, so one
// agent's delivered body never satisfies another's strict-mode edit.
func (t *ContextForTask) WithReadsFor(fn func(ctx context.Context) *ReadTracker) *ContextForTask {
	t.readsFor = fn
	return t
}

// readTracker resolves the ReadTracker for this call.
func (t *ContextForTask) readTracker(ctx context.Context) *ReadTracker {
	if t.readsFor != nil {
		if r := t.readsFor(ctx); r != nil {
			return r
		}
	}
	return t.tracker
}

// ReadDeps implements readRecordingTool (see read_deps.go). A pack records reads,
// so strict mode's edit gate depends on this wiring; it has no WriteTracker leg
// and no edit-lane hint, so those report not-applicable rather than a wiring gap.
func (t *ContextForTask) ReadDeps() (tracker, readsFor, writes, client bool) {
	return t.tracker != nil, t.readsFor != nil, writesNotApplicable, clientNotApplicable
}

// SensitiveWired reports whether the collector holds the sensitive-path decision.
// Without it a body reached by expansion would never be withheld, so the daemon's
// registration test pins it, as ReadDeps pins the tracker.
func (t *ContextForTask) SensitiveWired() bool {
	return t.collector != nil && t.collector.sensitive != nil
}

// contextForTaskName is the tool's registered name.
const contextForTaskName = "context_for_task"

// AppendRoom is the most bytes the connection layer may append to tool's result, and
// false for a tool whose result carries no such limit. context_for_task tells its
// caller that max_bytes bounds the WHOLE response, so what is appended to it (a
// mailbox preview, a policy notice) must fit the reserve the pack left.
func AppendRoom(tool string) (room int, bounded bool) {
	if tool == contextForTaskName {
		return ContextReserveBytes, true
	}
	return 0, false
}

func (*ContextForTask) Name() string                 { return contextForTaskName }
func (*ContextForTask) InputSchema() json.RawMessage { return contextForTaskSchema }
func (*ContextForTask) Description() string {
	return "Experimental, read-only context pack that starts from explicit seeds: at least one file or symbol is required, and task prose only re-ranks, never seeds. " +
		"Returns seeds, complete symbol bodies with an edit guard, related code, affected tests, memory and doc constraints, gaps and next calls; ambiguous or missing selectors are reported with candidates, never guessed. " +
		"Files must be files (a directory is scope: use within); symbols are path#Selector or a bare selector, code only (documents come through corpora). " +
		"Relative paths resolve against the pinned workspace; an unpinned call is refused. " +
		"max_bytes bounds the WHOLE response and includes a 1024-byte reserve for appended connection notes; above the cap it is clamped and disclosed. " +
		"No LLM, network or mutation. To find where to start, use workspace_search."
}

// contextHave is one body the caller still holds: the declaration it names and
// the content_sha256 of the exact body (context_ack.go).
type contextHave struct {
	Symbol        string `json:"symbol"`
	ContentSHA256 string `json:"content_sha256"`
}

// contextRequest is the parsed, normalised argument set.
type contextRequest struct {
	Files    []string      `json:"files"`
	Symbols  []string      `json:"symbols"`
	Intent   string        `json:"intent"`
	Task     string        `json:"task"`
	Within   []string      `json:"within"`
	Corpora  []string      `json:"corpora"`
	MaxBytes int           `json:"max_bytes"`
	Have     []contextHave `json:"have"`

	// clampedFrom is the max_bytes the caller asked for when it exceeded the cap,
	// so the response can disclose the clamp; zero when nothing was clamped.
	clampedFrom int
}

func (t *ContextForTask) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	req, err := parseContextRequest(raw)
	if err != nil {
		return "", err
	}
	if err := req.normalise(); err != nil {
		return "", err
	}
	if t.collector == nil {
		return "", errors.New("context_for_task: not wired to a collector")
	}
	pack, err := t.collector.Collect(ctx, req)
	if err != nil {
		return "", err
	}
	rendered := renderContext(pack)
	t.recordDelivered(ctx, pack, rendered.Delivered)
	return rendered.Text, nil
}

// recordDelivered records a read for each file whose body was actually rendered,
// once per file, with the version of that file the body was sliced from. A body
// that was omitted, degraded to a status line or handed off records nothing: the
// agent has not seen it, so an edit must not be allowed to rely on it.
func (t *ContextForTask) recordDelivered(ctx context.Context, pack contextPack, delivered []int) {
	reads := t.readTracker(ctx)
	for _, i := range delivered {
		f := pack.Files[i]
		reads.Record(f.Abs, f.MTime, f.SHA)
	}
}

func parseContextRequest(raw json.RawMessage) (contextRequest, error) {
	var req contextRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return req, badArgument(fmt.Errorf("context_for_task: invalid arguments: %w", err))
		}
	}
	return req, nil
}

// normalise validates the request and fills its defaults. Every refusal here
// is a fixable argument, so each is classified as one.
func (r *contextRequest) normalise() error {
	if err := r.checkSeeds(); err != nil {
		return err
	}
	if err := r.normaliseIntent(); err != nil {
		return err
	}
	if err := r.normaliseCorpora(); err != nil {
		return err
	}
	if err := r.normaliseBudget(); err != nil {
		return err
	}
	return r.checkHave()
}

func (r *contextRequest) checkSeeds() error {
	for i, f := range r.Files {
		if strings.TrimSpace(f) == "" {
			return badArgument(fmt.Errorf("context_for_task: files[%d] is empty", i))
		}
	}
	for i, s := range r.Symbols {
		if strings.TrimSpace(s) == "" {
			return badArgument(fmt.Errorf("context_for_task: symbols[%d] is empty", i))
		}
	}
	if len(r.Files)+len(r.Symbols) == 0 {
		return badArgument(errors.New("context_for_task: at least one file or symbol is required; " +
			"the pack grows from explicit seeds, and task prose alone only re-ranks them. " +
			"To find where to start, use workspace_search, then pass what it finds as files or symbols"))
	}
	return nil
}

func (r *contextRequest) normaliseIntent() error {
	switch r.Intent {
	case "":
		r.Intent = contextIntentUnderstand
	case contextIntentUnderstand, contextIntentChange:
	default:
		return badArgument(fmt.Errorf("context_for_task: intent must be %q or %q, got %q",
			contextIntentUnderstand, contextIntentChange, r.Intent))
	}
	return nil
}

// normaliseCorpora validates the allowlist and drops duplicates, keeping the
// caller's order.
func (r *contextRequest) normaliseCorpora() error {
	seen := map[string]bool{}
	var out []string
	for _, c := range r.Corpora {
		switch c {
		case corpusCode, corpusDocs, corpusMemory:
		default:
			return badArgument(fmt.Errorf("context_for_task: corpora entry %q is not one of code, docs, memory", c))
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	r.Corpora = out
	return nil
}

// normaliseBudget applies the default, clamps an over-cap value (remembering
// the request so the clamp is disclosed) and refuses one too small to carry the
// reserve plus a usable pack.
func (r *contextRequest) normaliseBudget() error {
	switch {
	case r.MaxBytes == 0:
		r.MaxBytes = contextDefaultMaxBytes
	case r.MaxBytes < contextMinMaxBytes:
		return badArgument(fmt.Errorf("context_for_task: max_bytes %d is below the minimum %d "+
			"(%d bytes are reserved for connection notes)", r.MaxBytes, contextMinMaxBytes, contextReserveBytes))
	case r.MaxBytes > contextMaxBytesCap:
		r.clampedFrom = r.MaxBytes
		r.MaxBytes = contextMaxBytesCap
	}
	return nil
}

// checkHave validates the shape of each acknowledgement and normalises its hash.
// A malformed entry is refused so callers learn the contract early; how many are
// honoured is the collector's business (contextMaxHave).
func (r *contextRequest) checkHave() error {
	for i := range r.Have {
		h := &r.Have[i]
		h.ContentSHA256 = strings.ToLower(strings.TrimSpace(h.ContentSHA256))
		if strings.TrimSpace(h.Symbol) == "" {
			return badArgument(fmt.Errorf("context_for_task: have[%d].symbol is empty", i))
		}
		if !contextSHA256Re.MatchString(h.ContentSHA256) {
			return badArgument(fmt.Errorf("context_for_task: have[%d].content_sha256 must be 64 hex characters", i))
		}
	}
	return nil
}
