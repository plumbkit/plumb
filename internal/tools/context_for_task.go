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
	// contextReserveBytes is held back from max_bytes for the text the connection
	// layer appends to a served result (mailbox preview, policy notes). The pack
	// renders into max_bytes minus this, so the served total stays within max_bytes.
	contextReserveBytes = 1024
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
// context pack. It is a thin orchestrator over ContextCollector (gathering) and
// renderContextPack (presentation).
//
// Concurrency: Execute is safe for concurrent use.
type ContextForTask struct {
	collector *ContextCollector
}

// NewContextForTask returns the tool over collector. A nil collector is valid
// for schema-only use (the catalogue tests); Execute then refuses.
func NewContextForTask(collector *ContextCollector) *ContextForTask {
	return &ContextForTask{collector: collector}
}

func (*ContextForTask) Name() string                 { return "context_for_task" }
func (*ContextForTask) InputSchema() json.RawMessage { return contextForTaskSchema }
func (*ContextForTask) Description() string {
	return "Experimental, read-only context pack that starts from explicit seeds: at least one file or symbol is required, and task prose only re-ranks, never seeds. " +
		"Returns the resolved seeds, gaps and concrete next calls; ambiguous or missing selectors are reported with candidates, never guessed. " +
		"Files must be files (a directory is scope: use within); symbols are path#Selector or a bare selector, code only (documents come through corpora). " +
		"Relative paths resolve against the pinned workspace; an unpinned call is refused. " +
		"max_bytes bounds the WHOLE response and includes a 1024-byte reserve for appended connection notes; above the cap it is clamped and disclosed. " +
		"No LLM, network or mutation. To find where to start, use workspace_search."
}

// contextHave is one body the caller still holds. A1 validates its shape only.
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
	return renderContextPack(pack), nil
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

// checkHave validates the shape of each acknowledgement. Behaviour arrives with
// the packer; until then a malformed entry is still refused so callers learn
// the contract early.
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
