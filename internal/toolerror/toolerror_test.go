package toolerror

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"testing"
)

func TestAllKinds_SortedCompleteAndValid(t *testing.T) {
	kinds := AllKinds()
	if len(kinds) != 13 {
		t.Fatalf("AllKinds() returned %d kinds, want 13 (add the new kind to allKinds and update this pin)", len(kinds))
	}
	if !slices.IsSorted(kinds) {
		t.Errorf("AllKinds() is not sorted: %v", kinds)
	}
	for _, k := range kinds {
		if !k.Valid() {
			t.Errorf("AllKinds() contains %q, which Valid() rejects", k)
		}
	}
	// A fresh copy each call: mutating the result must not disturb the package.
	kinds[0] = "clobbered"
	if AllKinds()[0] == "clobbered" {
		t.Error("AllKinds() returns the package's own slice; callers can corrupt it")
	}
}

func TestKindValid(t *testing.T) {
	tests := []struct {
		name string
		kind Kind
		want bool
	}{
		{"declared kind", KindDirtyFile, true},
		{"internal is declared", KindInternal, true},
		{"git_command_failed is the deliberate 13th", KindGitCommandFailed, true},
		{"empty is not a kind", Kind(""), false},
		{"unknown label", Kind("something_else"), false},
		{"near miss on case", Kind("Dirty_File"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.kind.Valid(); got != tt.want {
				t.Errorf("Kind(%q).Valid() = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

func TestAllRemediationClasses_SortedAndDefaulted(t *testing.T) {
	classes := AllRemediationClasses()
	if !slices.IsSorted(classes) {
		t.Errorf("AllRemediationClasses() is not sorted: %v", classes)
	}
	for _, c := range classes {
		if defaultReasons[c] == "" {
			t.Errorf("remediation class %q has no default reason", c)
		}
	}
	if len(defaultReasons) != len(classes) {
		t.Errorf("defaultReasons has %d entries but %d classes are declared", len(defaultReasons), len(classes))
	}
	for _, c := range classes {
		if !c.Valid() {
			t.Errorf("AllRemediationClasses() contains %q, which Valid() rejects", c)
		}
	}
}

func TestRemediationClassValid(t *testing.T) {
	tests := []struct {
		name  string
		class RemediationClass
		want  bool
	}{
		{"declared class", ClassPassDirtyOk, true},
		{"none is declared", ClassNone, true},
		{"empty is the absence of a remediation, not one", RemediationClass(""), false},
		{"unknown label", RemediationClass("do_a_barrel_roll"), false},
		{"near miss on case", RemediationClass("Pass_Dirty_Ok"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.class.Valid(); got != tt.want {
				t.Errorf("RemediationClass(%q).Valid() = %v, want %v", tt.class, got, tt.want)
			}
		})
	}
}

func TestError_TextIsByteIdenticalToCause(t *testing.T) {
	tests := []struct {
		name  string
		cause error
	}{
		{"plain sentence", errors.New("edit_file: %q has uncommitted changes")},
		{"multi-line with trailing detail", errors.New("write_file: stale\n  expected: a\n  current:  b")},
		{"formatted with a wrapped cause", fmt.Errorf("read_file: stat: %w", fs.ErrNotExist)},
		{"empty message", errors.New("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Wrap(tt.cause, KindDirtyFile, ClassPassDirtyOk)
			if got, want := e.Error(), tt.cause.Error(); got != want {
				t.Errorf("Error() = %q, want the cause verbatim %q", got, want)
			}
		})
	}
}

func TestError_NilSafe(t *testing.T) {
	var e *Error
	if got := e.Error(); got != "" {
		t.Errorf("nil *Error.Error() = %q, want empty", got)
	}
	if got := e.Unwrap(); got != nil {
		t.Errorf("nil *Error.Unwrap() = %v, want nil", got)
	}
	if got := e.WithOp("x"); got != nil {
		t.Errorf("nil *Error.WithOp() = %v, want nil", got)
	}
	// A nil cause must not panic; it is a programming mistake, not a crash.
	if got := Wrap(nil, KindInternal, ClassNone).Error(); got != "" {
		t.Errorf("Error() with a nil cause = %q, want empty", got)
	}
}

func TestErrorsIsAndAs_PassThroughToCause(t *testing.T) {
	sentinel := errors.New("the underlying cause")
	wrapped := fmt.Errorf("write_file: %w", sentinel)
	classified := Wrap(wrapped, KindUnreadOrStale, ClassReRead)

	if !errors.Is(classified, sentinel) {
		t.Error("errors.Is could not reach the sentinel through the classification")
	}
	// And through a further wrap applied on top of the classification.
	outer := fmt.Errorf("transaction_apply: op[0]: %w", classified)
	if !errors.Is(outer, sentinel) {
		t.Error("errors.Is could not reach the sentinel through an outer wrap")
	}

	var pathErr *fs.PathError
	inner := &fs.PathError{Op: "stat", Path: "/nope", Err: fs.ErrNotExist}
	if !errors.As(Wrap(inner, KindInternal, ClassNone), &pathErr) {
		t.Error("errors.As could not reach the concrete cause through the classification")
	}
	if pathErr.Path != "/nope" {
		t.Errorf("errors.As recovered the wrong value: %+v", pathErr)
	}
}

func TestClassify(t *testing.T) {
	classified := Wrap(errors.New("boom"), KindRateLimited, ClassRetryAfterWait)

	tests := []struct {
		name     string
		err      error
		wantOK   bool
		wantKind Kind
	}{
		{"nil", nil, false, ""},
		{"unclassified", errors.New("plain"), false, ""},
		{"classified", classified, true, KindRateLimited},
		{"classified beneath an outer wrap", fmt.Errorf("outer: %w", classified), true, KindRateLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Classify(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("Classify() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				if got != nil {
					t.Errorf("Classify() returned %v alongside ok=false", got)
				}
				return
			}
			if got.Kind != tt.wantKind {
				t.Errorf("Classify().Kind = %q, want %q", got.Kind, tt.wantKind)
			}
		})
	}
}

func TestKindOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Kind
	}{
		{"nil is the empty kind", nil, ""},
		{"unclassified falls back to internal", errors.New("mystery"), KindInternal},
		{"wrapped unclassified still internal", fmt.Errorf("ctx: %w", errors.New("mystery")), KindInternal},
		{"classified reports its kind", Wrap(errors.New("x"), KindGitPolicy, ClassEnablePolicy), KindGitPolicy},
		{
			"classified under a wrap still reports its kind",
			fmt.Errorf("ctx: %w", Wrap(errors.New("x"), KindLSPTimeout, ClassRetryWhenReady)),
			KindLSPTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KindOf(tt.err); got != tt.want {
				t.Errorf("KindOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRemediationDefaults(t *testing.T) {
	tests := []struct {
		name       string
		build      func() *Error
		wantClass  RemediationClass
		wantTool   string
		wantReason string
		wantRetry  bool
	}{
		{
			name:       "class-only wrap takes the default reason and the class's retryability",
			build:      func() *Error { return Wrap(errors.New("x"), KindDirtyFile, ClassPassDirtyOk) },
			wantClass:  ClassPassDirtyOk,
			wantReason: defaultReasons[ClassPassDirtyOk],
			wantRetry:  true,
		},
		{
			name: "WithTool applies, reason still defaulted",
			build: func() *Error {
				return Wrap(errors.New("x"), KindUnreadOrStale, ClassReRead, WithTool("read_file"))
			},
			wantClass:  ClassReRead,
			wantTool:   "read_file",
			wantReason: defaultReasons[ClassReRead],
			wantRetry:  true,
		},
		{
			name: "WithReason overrides the default",
			build: func() *Error {
				return Wrap(errors.New("x"), KindWorkspaceBoundary, ClassRepinWorkspace, WithReason("Pin it."))
			},
			wantClass:  ClassRepinWorkspace,
			wantReason: "Pin it.",
			wantRetry:  true,
		},
		{
			name: "New keeps an explicit Remediation verbatim",
			build: func() *Error {
				return New(KindWorkspaceBoundary, errors.New("x"), Remediation{
					Class:  ClassRepinWorkspace,
					Tool:   "session_start",
					Reason: "Re-pin, then retry.",
				})
			},
			wantClass:  ClassRepinWorkspace,
			wantTool:   "session_start",
			wantReason: "Re-pin, then retry.",
			wantRetry:  true,
		},
		{
			name: "an unknown class gets no invented prose",
			build: func() *Error {
				return Wrap(errors.New("x"), KindInternal, RemediationClass("not_a_class"))
			},
			wantClass:  RemediationClass("not_a_class"),
			wantReason: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := tt.build()
			if e.Remediation.Class != tt.wantClass {
				t.Errorf("Class = %q, want %q", e.Remediation.Class, tt.wantClass)
			}
			if e.Remediation.Tool != tt.wantTool {
				t.Errorf("Tool = %q, want %q", e.Remediation.Tool, tt.wantTool)
			}
			if e.Remediation.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", e.Remediation.Reason, tt.wantReason)
			}
			if e.Retryable() != tt.wantRetry {
				t.Errorf("Retryable = %v, want %v", e.Retryable(), tt.wantRetry)
			}
		})
	}
}

func TestZeroOptionDefaults(t *testing.T) {
	e := Wrap(errors.New("x"), KindGitPolicy, ClassEnablePolicy)
	if e.Op != "" {
		t.Errorf("Op = %q, want empty by default", e.Op)
	}
	if e.Details != nil {
		t.Errorf("Details = %v, want nil by default", e.Details)
	}
}

// TestEveryRemediationClassHasARetryability is the guard that makes derivation
// safe: a new class added without an entry would read false from the map, which
// looks conservative but silently tells clients "nothing you can do" about a
// remedy the agent could perform.
func TestEveryRemediationClassHasARetryability(t *testing.T) {
	for _, c := range AllRemediationClasses() {
		if _, ok := retryableByClass[c]; !ok {
			t.Errorf("remediation class %q has no retryability entry", c)
		}
	}
	if len(retryableByClass) != len(AllRemediationClasses()) {
		t.Errorf("retryableByClass has %d entries but %d classes are declared",
			len(retryableByClass), len(AllRemediationClasses()))
	}
}

// TestRetryableIsDerivedFromClass pins the table itself, and that Error just
// reports it — the incoherence this replaced was the same class carrying
// opposite flags at two seams.
func TestRetryableIsDerivedFromClass(t *testing.T) {
	tests := []struct {
		class RemediationClass
		want  bool
	}{
		{ClassReRead, true},
		{ClassRetryAfterWait, true},
		{ClassPassDirtyOk, true},
		{ClassPassConfirm, true},
		{ClassPassForce, true},
		{ClassFixArguments, true},
		{ClassRepinWorkspace, true},
		{ClassRetryWhenReady, true},
		{ClassEnablePolicy, false},
		{ClassInspectOutput, false},
		{ClassNone, false},
		{RemediationClass("not_a_class"), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			if got := tt.class.Retryable(); got != tt.want {
				t.Errorf("%q.Retryable() = %v, want %v", tt.class, got, tt.want)
			}
			e := Wrap(errors.New("x"), KindInternal, tt.class)
			if e.Retryable() != tt.want {
				t.Errorf("Error.Retryable = %v, want the class's %v", e.Retryable(), tt.want)
			}
		})
	}
}

func TestWithDetail(t *testing.T) {
	e := Wrap(errors.New("x"), KindGitCommandFailed, ClassInspectOutput,
		WithDetail("exit_code", "1"),
		WithDetail("subcommand", "commit"),
	)
	want := map[string]string{"exit_code": "1", "subcommand": "commit"}
	if len(e.Details) != len(want) {
		t.Fatalf("Details = %v, want %v", e.Details, want)
	}
	for k, v := range want {
		if e.Details[k] != v {
			t.Errorf("Details[%q] = %q, want %q", k, e.Details[k], v)
		}
	}
}

func TestWithOp_CopiesRatherThanMutates(t *testing.T) {
	original := Wrap(errors.New("boom"), KindClientTimeout, ClassRetryAfterWait)
	named := original.WithOp("read_file")

	if original.Op != "" {
		t.Errorf("WithOp mutated the original: Op = %q", original.Op)
	}
	if named.Op != "read_file" {
		t.Errorf("copy Op = %q, want read_file", named.Op)
	}
	if named.Error() != original.Error() {
		t.Errorf("copy text = %q, want %q", named.Error(), original.Error())
	}
	if named.Kind != original.Kind || named.Retryable() != original.Retryable() {
		t.Errorf("copy lost classification: %+v vs %+v", named, original)
	}
}
