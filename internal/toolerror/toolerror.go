// Package toolerror gives plumb's tool failures a stable, machine-readable
// classification WITHOUT changing a single byte of the human-readable text an
// agent already reads.
//
// A plumb refusal is usually a good sentence — "%q has uncommitted changes;
// review and commit first, or pass dirty_ok: true" tells a competent reader
// exactly what to do. What it does not do is tell a PROGRAM anything: every
// refusal, every language-server timeout, and every genuine internal fault
// arrive as one undifferentiated `error`, so a client cannot tell a
// wait-and-retry from a fix-your-arguments from a this-will-never-work, and
// plumb itself cannot count them apart. This package adds that classification
// as a side-car on the existing error, never as a replacement:
// Error.Error() returns the wrapped cause's text verbatim — no prefix, no
// decoration — so byte-stability of every existing message is preserved by
// construction, and errors.Is/errors.As continue to reach the cause.
//
// # Kinds
//
// Kind is a closed, low-cardinality set. Twelve of the thirteen map one-to-one
// onto a distinct refusal seam. The thirteenth, KindGitCommandFailed, is a
// deliberate split from KindGitPolicy, and the distinction is the recovery
// path, not the subsystem: KindGitPolicy means PLUMB refused — a tier is
// disabled in configuration, a confirmation was not given, a protected branch
// was targeted — and the remedy is a policy or argument change on plumb's side.
// KindGitCommandFailed means plumb ran git and the child exited non-zero — a
// failing pre-commit hook, a merge conflict, a rejected push — where plumb has
// no objection at all and the remedy lies entirely in the repository. Folding
// the two would tell a client "adjust plumb's git policy" for a failing hook,
// which is the wrong advice in the one case where the captured output already
// holds the answer.
//
// # Retryable
//
// Retryable means: THE CALLING AGENT CAN MAKE THIS SAME OPERATION SUCCEED BY
// ACTING ON THE REMEDIATION ITSELF, WITH NO OUT-OF-BAND HUMAN ACTION.
//
// It is DERIVED, never set per call site: RemediationClass.Retryable is the one
// table, and Error.Retryable merely reports it. So re_read, retry_after_wait,
// pass_dirty_ok, pass_confirm, pass_force, fix_arguments, repin_workspace and
// retry_when_ready are all retryable — each names something the agent can do
// and then re-issue. enable_policy is not: a human must edit configuration
// first. inspect_output and none are not: nothing the agent does on plumb's
// side changes the outcome.
//
// Deriving it is the point. When the class and the flag were independent they
// drifted immediately — the same remediation class shipped with opposite flags
// at two seams — and a client keying on the flag then gets a different answer
// for the same remedy. If a seam appears to need a retryability its class does
// not imply, the class is wrong; change the class, not the flag.
//
// It NEVER means a client may automatically replay the call. Most plumb
// failures that carry Retryable=true guard a NON-IDEMPOTENT mutation — a write
// refused because the file changed under the caller is retryable only once the
// caller has re-read and reconciled; replaying the identical write would
// perform exactly the clobber the guard exists to prevent. A client that treats
// Retryable as "safe to replay" has inverted the field's meaning.
//
// # Concurrency
//
// An *Error is immutable once constructed and is safe to share across
// goroutines. WithOp returns a shallow copy rather than mutating in place;
// Details is treated as read-only after construction and is shared by that
// copy, so callers must not write to a Details map they have handed to New or
// Wrap.
package toolerror

import (
	"errors"
	"slices"
)

// Kind is the machine-readable class of a tool failure. The set is closed and
// deliberately small: it exists to be counted, matched, and switched on, so a
// new kind is a considered addition rather than a free-form label.
type Kind string

const (
	// KindInvalidArguments is a malformed, unknown, or unusable call argument —
	// including an unknown tool name and an unresolvable parameter alias.
	KindInvalidArguments Kind = "invalid_arguments"
	// KindUnreadOrStale is a read-before-write or optimistic-concurrency
	// refusal: the caller never read the file, or read a version that has since
	// changed on disk.
	KindUnreadOrStale Kind = "unread_or_stale"
	// KindDirtyFile is the uncommitted-changes guard on a write.
	KindDirtyFile Kind = "dirty_file"
	// KindWorkspaceBoundary is a path outside the connection's allowed roots,
	// or a path-bearing call on a connection with nothing pinned.
	KindWorkspaceBoundary Kind = "workspace_boundary"
	// KindRateLimited is the per-session write-rate budget refusing an operation.
	KindRateLimited Kind = "rate_limited"
	// KindGitPolicy is plumb refusing a git operation by policy: a disabled
	// tier, a missing confirmation, or a protected-branch/ad-hoc-remote guard.
	KindGitPolicy Kind = "git_policy"
	// KindGitCommandFailed is a git child process exiting non-zero. plumb
	// permitted the operation; git itself declined. See the package doc for why
	// this is not KindGitPolicy.
	KindGitCommandFailed Kind = "git_command_failed"
	// KindConcurrentRefMove is the cross-session ref-movement guard: HEAD or the
	// branch moved since this session last observed it, or expected_head did not
	// match.
	KindConcurrentRefMove Kind = "concurrent_ref_move"
	// KindLSPUnavailable is a language server that cannot answer yet — still
	// warming, or not running.
	KindLSPUnavailable Kind = "lsp_unavailable"
	// KindLSPTimeout is a language server that did not respond within the
	// operation's deadline.
	KindLSPTimeout Kind = "lsp_timeout"
	// KindDaemonTransport is a daemon-side transport or lifecycle condition —
	// shutting down, draining, connection torn down.
	KindDaemonTransport Kind = "daemon_transport"
	// KindClientTimeout is plumb abandoning a call at its own per-tool execution
	// deadline before the client's timeout could fire.
	KindClientTimeout Kind = "client_timeout"
	// KindInternal is an unclassified fault. It is the answer KindOf gives for
	// any non-nil error that carries no classification, so it is never a claim
	// about the failure — only an admission that nothing more specific is known.
	KindInternal Kind = "internal"
)

// allKinds lists every Kind in sorted order. Sorted by construction rather than
// at call time so AllKinds is allocation-cheap and its order is pinned by a test
// rather than by a sort call nobody reads.
var allKinds = []Kind{
	KindClientTimeout,
	KindConcurrentRefMove,
	KindDaemonTransport,
	KindDirtyFile,
	KindGitCommandFailed,
	KindGitPolicy,
	KindInternal,
	KindInvalidArguments,
	KindLSPTimeout,
	KindLSPUnavailable,
	KindRateLimited,
	KindUnreadOrStale,
	KindWorkspaceBoundary,
}

// AllKinds returns every declared Kind in a stable sorted order. The returned
// slice is a fresh copy, so a caller cannot disturb the package's own list.
func AllKinds() []Kind { return slices.Clone(allKinds) }

// Valid reports whether k is one of the declared kinds. The empty Kind is not
// valid — it is KindOf's answer for a nil error, not a class of failure.
func (k Kind) Valid() bool { return slices.Contains(allKinds, k) }

// RemediationClass names WHAT the caller must do, in a form a program can
// branch on. It is deliberately coarser than the error text: the sentence says
// which file and which flag, the class says only "pass dirty_ok".
type RemediationClass string

const (
	// ClassReRead — the caller's view of the file is stale; read it again.
	ClassReRead RemediationClass = "re_read"
	// ClassRetryAfterWait — a transient condition; the same call works later.
	ClassRetryAfterWait RemediationClass = "retry_after_wait"
	// ClassPassDirtyOk — commit the file, or opt in with dirty_ok.
	ClassPassDirtyOk RemediationClass = "pass_dirty_ok"
	// ClassPassConfirm — acknowledge the risk with confirm: true.
	ClassPassConfirm RemediationClass = "pass_confirm"
	// ClassPassForce — override a protective refusal with force: true.
	ClassPassForce RemediationClass = "pass_force"
	// ClassEnablePolicy — the operation is switched off in configuration.
	ClassEnablePolicy RemediationClass = "enable_policy"
	// ClassFixArguments — the call itself is wrong; change it.
	ClassFixArguments RemediationClass = "fix_arguments"
	// ClassRepinWorkspace — pin this connection to the right project first.
	ClassRepinWorkspace RemediationClass = "repin_workspace"
	// ClassRetryWhenReady — a subsystem is still warming; retry once it is up.
	ClassRetryWhenReady RemediationClass = "retry_when_ready"
	// ClassInspectOutput — the answer is in the captured output, not in a flag.
	ClassInspectOutput RemediationClass = "inspect_output"
	// ClassNone — nothing the caller can pass will make this call succeed.
	ClassNone RemediationClass = "none"
)

// defaultReasons supplies the sentence used when a call site names a class but
// no bespoke reason. Every class has one, so a Remediation is never emitted
// with a class the client can read and a reason it cannot.
var defaultReasons = map[RemediationClass]string{ //nolint:gosec // G101: the ClassPass* keys make gosec's credential heuristic fire on "pass"; every value here is agent-facing guidance prose, and the identifiers are the remediation vocabulary ("pass dirty_ok"), not a password
	ClassReRead:         "Re-read the file, then retry against the version you have just seen.",
	ClassRetryAfterWait: "Wait for the transient condition to clear, then retry.",
	ClassPassDirtyOk:    "Review and commit the file's existing changes, or retry with dirty_ok: true.",
	ClassPassConfirm:    "Re-check the current state, then retry with confirm: true.",
	ClassPassForce:      "Retry with force: true once you are satisfied the override is intended.",
	ClassEnablePolicy:   "The operation is disabled by configuration; enable it, then retry.",
	ClassFixArguments:   "Correct the call's arguments and retry.",
	ClassRepinWorkspace: "Pin this connection to the project you meant to work in, then retry.",
	ClassRetryWhenReady: "Retry once the language server has finished warming.",
	ClassInspectOutput:  "Read the captured output in the message; the cause is reported there.",
	ClassNone:           "This operation is refused by design; no argument or setting will permit it.",
}

// retryableByClass is the single source of truth for Error.Retryable. The
// split is "can the calling agent alone act on this remedy and re-issue?":
// enable_policy needs a human to edit configuration, and inspect_output/none
// have no plumb-side remedy at all, so those three are the only false entries.
//
// A class with no entry here reads as false, which is why
// TestEveryRemediationClassHasARetryability asserts the map is total — a new
// class must state its answer rather than inherit the safe-looking one.
var retryableByClass = map[RemediationClass]bool{
	ClassReRead:         true,
	ClassRetryAfterWait: true,
	ClassPassDirtyOk:    true,
	ClassPassConfirm:    true,
	ClassPassForce:      true,
	ClassFixArguments:   true,
	ClassRepinWorkspace: true, // the agent itself calls session_start
	ClassRetryWhenReady: true,
	ClassEnablePolicy:   false, // a human must edit configuration
	ClassInspectOutput:  false, // nothing plumb-side changes the outcome
	ClassNone:           false,
}

// Retryable reports whether acting on this remediation lets the calling agent
// re-issue the operation successfully, unaided. See the package doc.
func (c RemediationClass) Retryable() bool { return retryableByClass[c] }

// allRemediationClasses lists every RemediationClass in sorted order, on the
// same terms as allKinds.
var allRemediationClasses = []RemediationClass{
	ClassEnablePolicy,
	ClassFixArguments,
	ClassInspectOutput,
	ClassNone,
	ClassPassConfirm,
	ClassPassDirtyOk,
	ClassPassForce,
	ClassReRead,
	ClassRepinWorkspace,
	ClassRetryAfterWait,
	ClassRetryWhenReady,
}

// AllRemediationClasses returns every declared RemediationClass in a stable
// sorted order, as a fresh copy.
func AllRemediationClasses() []RemediationClass {
	return slices.Clone(allRemediationClasses)
}

// Remediation is the structured "what do I do about it" half of a classified
// failure. Tool names the plumb tool the caller should reach for, when one
// applies (read_file for a stale read, session_start for a boundary refusal);
// it is empty when the remedy is an argument or a setting rather than another
// call. Reason is a short readable sentence, defaulted per class.
type Remediation struct {
	Class  RemediationClass
	Tool   string
	Reason string
}

// withDefaults fills an empty Reason from the class table. An unrecognised
// class leaves Reason empty rather than inventing prose for a class this
// package does not know.
func (r Remediation) withDefaults() Remediation {
	if r.Reason == "" {
		r.Reason = defaultReasons[r.Class]
	}
	return r
}

// Error is a classified tool failure. It carries the classification alongside
// the original error rather than in place of it: Error() is the cause's text
// verbatim, and Unwrap exposes the cause so errors.Is and errors.As behave as
// though this wrapper were not there.
//
// Op is the tool the failure belongs to. Most call sites leave it empty and it
// is filled at the MCP dispatch boundary, which is the one place that reliably
// knows the resolved tool name.
//
// Details carries low-cardinality machine-readable facts (an exit code, a
// subcommand) — never raw output, and never anything unbounded. It is optional
// and usually nil.
type Error struct {
	Kind Kind
	Op   string
	// Retryable is derived from Remediation.Class at construction and is not
	// independently settable. See the package doc's Retryable section.
	Retryable   bool
	Remediation Remediation
	Details     map[string]string

	cause error
}

// Error returns EXACTLY the wrapped cause's text — no prefix, no suffix, no
// decoration. This is the hard contract of the package: adding a classification
// must never change a message an agent or a test already depends on.
func (e *Error) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

// Unwrap returns the wrapped cause so errors.Is and errors.As see straight
// through the classification to whatever the call site actually returned.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// WithOp returns a shallow copy carrying op as the operation name. It copies
// rather than mutating so the dispatch boundary can name an error it did not
// construct without racing whoever else holds it.
func (e *Error) WithOp(op string) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Op = op
	return &clone
}

// Option adjusts an Error at construction. Options never touch Retryable —
// that is derived from the remediation class.
type Option func(*Error)

// WithTool names the plumb tool the caller should reach for.
func WithTool(tool string) Option {
	return func(e *Error) { e.Remediation.Tool = tool }
}

// WithReason overrides the class's default remediation sentence.
func WithReason(reason string) Option {
	return func(e *Error) { e.Remediation.Reason = reason }
}

// WithDetail records one low-cardinality machine-readable fact. Do not pass
// file contents, captured output, or anything else unbounded.
func WithDetail(key, value string) Option {
	return func(e *Error) {
		if e.Details == nil {
			e.Details = make(map[string]string, 2)
		}
		e.Details[key] = value
	}
}

// New classifies cause with a fully-specified Remediation. Use it where the
// remedy needs its own wording or a named tool; Wrap is the shorthand for the
// common class-only case.
//
// cause should be non-nil: a nil cause yields an Error whose text is empty,
// which is a programming mistake rather than a supported shape.
func New(kind Kind, cause error, r Remediation, opts ...Option) *Error {
	e := &Error{Kind: kind, Remediation: r, cause: cause}
	for _, opt := range opts {
		opt(e)
	}
	e.Remediation = e.Remediation.withDefaults()
	e.Retryable = e.Remediation.Class.Retryable()
	return e
}

// Wrap classifies cause with a remediation that is just a class, letting the
// class's default sentence stand unless WithReason or WithTool says otherwise.
// This is the shape almost every refusal seam wants.
func Wrap(cause error, kind Kind, class RemediationClass, opts ...Option) *Error {
	return New(kind, cause, Remediation{Class: class}, opts...)
}

// Classify reports the classification carried by err, if any. It is
// errors.As-based, so it finds an *Error anywhere in the chain — including
// beneath a wrapper a caller added afterwards.
func Classify(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// KindOf reports err's kind: the classified kind when there is one,
// KindInternal for any other non-nil error, and the empty Kind for nil. The
// three-way answer lets a counter distinguish "succeeded", "failed in a way we
// understand", and "failed in a way nothing has classified yet".
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	if e, ok := Classify(err); ok {
		return e.Kind
	}
	return KindInternal
}
