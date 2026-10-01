package stats

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// Call holds one tool invocation record.
type Call struct {
	SessionID     string
	SessionName   string // human-readable name, e.g. "swift-falcon"
	Workspace     string // absolute path to the project root
	Tool          string
	CalledAt      time.Time
	DurationMs    int64
	InputBytes    int
	OutputBytes   int
	Success       bool
	ErrorMsg      string
	InputJSON     string // raw JSON args as sent to the tool (capped at 64 KiB)
	OutputText    string // full tool output (capped at 64 KiB)
	ClientName    string // MCP clientInfo.name (e.g. "claude-code")
	ClientVersion string // MCP clientInfo.version

	// Savings accounting (tokens-saved redesign). Populated at write time by the
	// scorer in the cli layer; SavingsModelVersion records which model produced
	// the figures (0 = unscored/legacy). TokensSaved is the headline total;
	// CapabilityTokens + EfficiencyTokens are the honest two-axis split.
	TokensSaved         int
	SavingsModelVersion int
	CapabilityTokens    int
	EfficiencyTokens    int

	// Purpose is the optional human-readable session purpose tag (e.g.
	// "deploy-fix"), set via session_start. Empty when unset.
	Purpose string

	// LogicalAgent is the logical-agent id the call carried (per-call _meta or
	// the argument-carried stamp), when the connection is shared by several
	// agents. Empty when the call declared none — which is also what every
	// pre-v19 row reads back as. Render it with AgentLabel; never infer an
	// agent from a blank.
	LogicalAgent string

	// CallID is the tools/call id; joins history.changes.call_id; '' predates linking.
	CallID string

	// Failure classification, mirroring the `_meta` envelope the same call put on
	// the wire. Both are stamped from ONE classification made at the MCP dispatch
	// boundary, so the recorded row and the client's copy can never disagree.
	//
	// A blank ErrorKind means "plumb makes no structured claim about this
	// failure" — the same thing the envelope's absence means — and it is also
	// what every pre-v14 row reads back as. Nothing infers a kind from ErrorMsg
	// prose, so an unclassified failure stays honestly unclassified rather than
	// being folded into KindInternal.
	//
	// ErrorRetryable is derived from RemediationClass at write time (see
	// toolerror.RemediationClass.Retryable) — stored so a query can count
	// retryable failures without re-deriving, never set independently.
	ErrorKind        toolerror.Kind
	ErrorRetryable   bool
	RemediationClass toolerror.RemediationClass
}

// maxStoredBytes caps the size of input_json and output_text stored per call.
// Large tool outputs (e.g. search_in_files on a big repo) are truncated to
// keep the DB compact. 64 KiB is generous for debugging purposes.
const maxStoredBytes = 64 * 1024

func capString(s string) string {
	if len(s) > maxStoredBytes {
		return s[:maxStoredBytes]
	}
	return s
}

// insertCallSQL inserts one tool_calls row. Shared by Record and RecordBatch.
const insertCallSQL = `INSERT INTO tool_calls
	 (session_id, session_name, workspace, tool, called_at, duration_ms, input_bytes, output_bytes, success, error_msg, input_json, output_text, client_name, client_version, tokens_saved, savings_model_version, capability_tokens, efficiency_tokens, purpose, error_kind, error_retryable, remediation_class, logical_agent, call_id)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// validateCall reports the required-field error for c, or nil when storable.
// These three are the row's identity: without them it cannot be attributed to a
// workspace, a session, or a tool, and there is nothing to store.
func validateCall(c Call) error {
	switch {
	case c.Workspace == "":
		return errors.New("stats: workspace is required")
	case c.SessionID == "":
		return errors.New("stats: session_id is required")
	case c.Tool == "":
		return errors.New("stats: tool is required")
	}
	return nil
}

// normaliseCall blanks a classification that is not in toolerror's declared
// vocabulary, and reports what it dropped.
//
// error_kind and remediation_class are GROUP BY keys, so an invented label would
// quietly split a bucket in every failure report that ever reads the table — but
// the answer to that is to drop the LABEL, not the row. The classification is
// the optional part of a telemetry row; the duration, the savings, the tool and
// the client identity are not, and refusing the whole row over a bad label would
// trade a large certain loss for a small one.
//
// An undeclared KIND takes the whole classification with it, remedy and
// retryability included. A row with no kind makes no structured claim at all, so
// a row sitting in the `unclassified` bucket while still reporting a retryable
// call is not a half-truth, it is two truths: the CLI renders that bucket's
// retryability as unknown while the TUI sums the stored flag, and the two
// surfaces would then disagree about the same rows. An undeclared CLASS is
// narrower — the kind survives, because knowing WHAT went wrong is useful
// without knowing what to do — but the retryability derived from the class does
// not, since it would be a claim with nothing behind it.
//
// Nothing can trigger this today — every classified seam uses a declared
// constant — so it is a guard against a future typo, not a live path. That is
// exactly why it must not be silent: its callers log what it reports.
func normaliseCall(c Call) (Call, string) {
	badKind := c.ErrorKind != "" && !c.ErrorKind.Valid()
	badClass := c.RemediationClass != "" && !c.RemediationClass.Valid()
	// A blank kind is the normal shape for a successful call, not a fault, so a
	// class riding along with it clears silently — UNLESS that class is itself
	// undeclared, in which case the diagnostic below still needs to report it;
	// badClass above is computed on the original value, so this cannot mask it.
	if c.ErrorKind == "" && !badClass {
		c.RemediationClass = ""
		c.ErrorRetryable = false
	}
	if !badKind && !badClass {
		return c, ""
	}
	// Record both labels before either is cleared, so a row that is wrong twice
	// does not report only its first fault.
	var parts []string
	if badKind {
		parts = append(parts, "error_kind="+string(c.ErrorKind))
	}
	if badClass {
		parts = append(parts, "remediation_class="+string(c.RemediationClass))
	}
	if badKind {
		c.ErrorKind = ""
	}
	c.RemediationClass = ""
	c.ErrorRetryable = false
	return c, strings.Join(parts, " ")
}

// storableCall is the one gate every insert path goes through: it normalises the
// classification and then applies the required-field check, returning whatever
// label it dropped so the CALLER can decide how to report it. It does not log:
// RecordBatch runs it once per row on the single writer goroutine, and a
// per-row warning there would be the log spam the Writer's own drop accounting
// exists to avoid (see logDropped).
func storableCall(c Call) (Call, string, error) {
	c, dropped := normaliseCall(c)
	return c, dropped, validateCall(c)
}

// callArgs returns the positional bind arguments for insertCallSQL.
func callArgs(c Call) []any {
	success := 1
	if !c.Success {
		success = 0
	}
	retryable := 0
	if c.ErrorRetryable {
		retryable = 1
	}
	return []any{
		c.SessionID, c.SessionName, c.Workspace, c.Tool,
		c.CalledAt.UnixMilli(), c.DurationMs,
		c.InputBytes, c.OutputBytes,
		success, c.ErrorMsg,
		capString(c.InputJSON), capString(c.OutputText),
		c.ClientName, c.ClientVersion,
		c.TokensSaved, c.SavingsModelVersion, c.CapabilityTokens, c.EfficiencyTokens,
		c.Purpose,
		string(c.ErrorKind), retryable, string(c.RemediationClass),
		c.LogicalAgent,
		c.CallID,
	}
}

// Record inserts a call. Stats are best-effort, but the caller gets the
// insert error so the daemon can log storage failures.
func (d *DB) Record(c Call) error {
	if d == nil {
		return nil
	}
	c, dropped, err := storableCall(c)
	if err != nil {
		return err
	}
	if dropped != "" {
		slog.Warn("stats: dropped an undeclared failure classification; the row is stored without it",
			"tool", c.Tool, "dropped", dropped)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.db.Exec(insertCallSQL, callArgs(c)...); err != nil {
		return fmt.Errorf("stats: insert call: %w", err)
	}
	return nil
}

// RecordBatch inserts many calls in one transaction — a single fsync and one
// write-lock acquisition for the whole batch instead of per row, which is what
// keeps the writer off SQLITE_BUSY under load. Rows that fail validation are
// skipped and counted; a SQLite error rolls the whole transaction back.
func (d *DB) RecordBatch(calls []Call) (skipped int, err error) {
	if d == nil || len(calls) == 0 {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("stats: begin batch: %w", err)
	}
	stmt, err := tx.Prepare(insertCallSQL)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("stats: prepare batch: %w", err)
	}
	defer stmt.Close()
	// Counted across the batch and logged once, rather than per row: this runs on
	// the single writer goroutine, where a line per row is the spam the Writer's
	// own drop accounting is careful to avoid.
	demoted, example := 0, ""
	for _, c := range calls {
		c, dropped, err := storableCall(c)
		if err != nil {
			skipped++
			continue
		}
		if dropped != "" {
			demoted++
			example = dropped
		}
		if _, err := stmt.Exec(callArgs(c)...); err != nil {
			_ = tx.Rollback()
			return skipped, fmt.Errorf("stats: insert batch: %w", err)
		}
	}
	if demoted > 0 {
		slog.Warn("stats: dropped undeclared failure classifications; the rows are stored without them",
			"rows", demoted, "example", example)
	}
	if err := tx.Commit(); err != nil {
		return skipped, fmt.Errorf("stats: commit batch: %w", err)
	}
	return skipped, nil
}
