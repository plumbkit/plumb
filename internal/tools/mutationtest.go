package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// mutationtest.go is the mutation_test MCP tool: it verifies that a test suite
// actually ASSERTS what it appears to assert, by applying EXPLICIT mutants
// (caller-supplied old_string → new_string edits), checking each one still
// compiles, running a scoped test set, classifying the result, and restoring
// the file.
//
// THE FALSE KILL IS THE FAILURE MODE THIS TOOL EXISTS TO PREVENT. A mutant that
// never applied, or applied but did not compile, makes the test command fail —
// and a hand-rolled sed/`go test` harness reports that failure as a kill,
// "proving" an assertion that was never exercised. So the compile gate is
// mandatory and non-negotiable, and a mutant that fails to apply or fails to
// compile is classified `invalid` and can never be reported as `killed`.
//
// Filename note: this file is deliberately NOT mutation_test.go — Go would
// compile a _test.go suffix as a test file.
//
// Concurrency: Execute is safe for concurrent use. Each run holds the shared
// mutationRunLock (one mutation run per daemon) and, per mutant, the per-path
// lock every write tool uses, so no other plumb write can interleave with the
// mutate → verify → restore cycle.

// mutationRunLock serialises mutation runs across the whole daemon process.
//
// PROCESS-GLOBAL BY DESIGN, like pathLocks in file_write_helpers.go, and for
// the same reason: the state being protected is the machine, not a session.
// Tool instances are per-connection, so a field on MutationTest would let two
// agents mutate the same working tree at once and read each other's breakage as
// their own result — a mutant reported `survived` only because a peer's mutant
// was the thing failing the suite. A mutation run also monopolises the build
// and test toolchain (compiler cache, test binaries, ports), so serialising
// daemon-wide is correct rather than merely convenient.
//
// TryLock, not Lock: queueing behind a suite that may run for minutes is worse
// than an immediate, explicit refusal the agent can act on.
var mutationRunLock sync.Mutex

// MutationOutcome classifies one mutant's run. The three values are exhaustive
// and deliberately few; the WHY of an invalid result lives in its reason.
type MutationOutcome string

const (
	// MutationKilled — the mutant applied, compiled, and the test command
	// failed. This is the only outcome that proves an assertion is real.
	MutationKilled MutationOutcome = "killed"
	// MutationSurvived — the mutant applied, compiled, and the tests still
	// passed. The dangerous one: the assertions covering that code are vacuous.
	MutationSurvived MutationOutcome = "survived"
	// MutationInvalid — the mutant did not apply, or applied but did not
	// compile, or the test run timed out. NOT a kill; it proves nothing.
	MutationInvalid MutationOutcome = "invalid"
)

// Invalid-outcome reasons, rendered verbatim in the report.
const (
	reasonNotApplied     = "old_string not found — nothing was mutated"
	reasonAmbiguous      = "old_string is ambiguous — nothing was mutated"
	reasonCompileFailed  = "the mutant does not compile"
	reasonCompileTimeout = "the compile step timed out"
	reasonTestTimeout    = "the test step timed out"
	// The two unrunnable reasons are separate from a non-zero exit on purpose:
	// a command that never started proves nothing about the mutant, and in the
	// TEST slot a bare non-zero exit would otherwise read as a kill.
	reasonCompileUnrunnable = "the compile command could not be started"
	reasonTestUnrunnable    = "the test command could not be started"
)

const (
	// maxMutants bounds one request. Each mutant costs a full compile + test
	// cycle, so a large batch is a runaway, not a convenience.
	maxMutants = 20
	// maxMutationStepSeconds bounds a caller-supplied per-step timeout.
	maxMutationStepSeconds = 3600
	// mutationExcerptLines is how much of a step's output the report quotes.
	mutationExcerptLines = 12
)

// MutationTest is the mutation_test MCP tool.
type MutationTest struct {
	deps    WriteDeps
	resolve TaskResolverFn
}

// NewMutationTest constructs the tool. resolve is the same stored-task resolver
// run_task uses (slot + optional {target}, trust-gated) — mutation_test never
// accepts an agent-supplied command line.
func NewMutationTest(deps WriteDeps, resolve TaskResolverFn) *MutationTest {
	return &MutationTest{deps: deps, resolve: resolve}
}

var mutationTestSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "mutants": {
      "type": "array",
      "minItems": 1,
      "maxItems": 20,
      "description": "The mutants to test, applied and restored ONE AT A TIME. Each is an exact-once str_replace in the style of edit_file.",
      "items": {
        "type": "object",
        "properties": {
          "file_path": {"type": "string", "description": "Absolute path, file:// URI, or workspace-relative path of the file to mutate."},
          "old_string": {"type": "string", "description": "Exact text to replace. Must appear EXACTLY ONCE in the file — zero or several occurrences is reported invalid (nothing is mutated), never a kill."},
          "new_string": {"type": "string", "description": "The mutation. Must differ from old_string. Empty string deletes the matched text — the classic 'delete the guard and see if anything fails' mutant."},
          "label": {"type": "string", "description": "Optional short name for this mutant, echoed in the report (e.g. \"drop the addressee check\")."}
        },
        "required": ["file_path", "old_string", "new_string"],
        "additionalProperties": false
      }
    },
    "test_task": {
      "type": "string",
      "enum": ["build", "lint", "test", "e2e", "verify"],
      "description": "Which stored [tasks.<lang>] slot runs the tests. Default \"test\"."
    },
    "test_target": {
      "type": "string",
      "description": "Optional value for the test command's {target} placeholder — the way to scope the run to the affected package or test instead of the whole suite (ask topology_affected which). ONLY usable when the stored test command actually contains a {target} token: it is refused outright otherwise, and the shipped defaults (Go's is 'go test ./...') do NOT have one, so on an unmodified config the whole suite runs per mutant and scoping means editing [tasks.<lang>].test first. One shell-safe argument ([A-Za-z0-9._/:@-])."
    },
    "compile_task": {
      "type": "string",
      "enum": ["build", "lint", "test", "e2e", "verify"],
      "description": "Which stored slot proves the mutant COMPILES before its tests are trusted. Default \"build\". It always runs unscoped (no {target}) — a whole-module compile catches breakage a scoped test never reaches. Cannot be disabled: without it a non-compiling mutant looks exactly like a kill."
    },
    "timeout_seconds": {
      "type": "integer",
      "minimum": 1,
      "maximum": 3600,
      "description": "Per-step timeout for the compile and test commands. Default 600."
    }
  },
  "required": ["mutants"],
  "additionalProperties": false
}`)

func (*MutationTest) Name() string                 { return "mutation_test" }
func (*MutationTest) InputSchema() json.RawMessage { return mutationTestSchema }

func (*MutationTest) Description() string {
	return "Mutation-test your own assertions: apply an explicit mutant, prove it still COMPILES, run a scoped test set, classify the result, and restore the file — the check that tells a real assertion from a vacuous one. " +
		"Takes explicit mutants only (file_path + exact-once old_string/new_string, like edit_file); it does not generate them. " +
		"Three outcomes: KILLED (mutant compiled and a test failed — the assertion is real), SURVIVED (mutant compiled and every test still passed — the assertion is VACUOUS, the finding that matters), and INVALID (the mutant did not apply, did not compile, could not be started, or the tests timed out — it proves nothing and is NEVER reported as a kill; that false kill is the whole reason the compile gate exists). " +
		"Scope the run with test_target, which fills the stored test command's {target} placeholder (topology_affected says which tests to name) — but only if the stored command HAS that placeholder; the shipped defaults do not, so out of the box the whole suite runs per mutant. " +
		"Commands are the stored, trust-gated [tasks.<lang>] slots run_task uses; you cannot pass a command line. " +
		"Restoration is guaranteed on every exit path (pass, fail, compile error, timeout, panic, cancellation): the pre-mutation bytes are snapshotted in memory, rewritten under the same per-path lock, and verified by SHA-256 before the run is reported clean. " +
		"It REFUSES to touch a file with uncommitted changes (untracked included), with no override — a clean file means `git checkout` can always recover it if the daemon dies mid-run, and that is the recovery story. " +
		"It also refuses to start unless the workspace BUILDS and its tests PASS unmutated: a kill means \"green before, red after\", so against an already-red suite every mutant would be reported killed for a reason that has nothing to do with it. The refusal names which of three things happened — the suite is red, the command timed out, or it could not be started at all — because only the first is about your code, and the other two prove nothing about the workspace. " +
		"One mutation run at a time per daemon; a second call is refused rather than queued."
}

type mutantSpec struct {
	Path  string `json:"file_path"`
	Old   string `json:"old_string"`
	New   string `json:"new_string"`
	Label string `json:"label"`
}

type mutationTestArgs struct {
	Mutants     []mutantSpec `json:"mutants"`
	TestTask    string       `json:"test_task"`
	TestTarget  string       `json:"test_target"`
	CompileTask string       `json:"compile_task"`
	TimeoutSecs int          `json:"timeout_seconds"`
}

// withDefaults returns a copy with the unset slots filled. A value receiver
// keeps every method on mutationTestArgs consistent (recvcheck), and the
// defaults stay stated in one readable place.
func (a mutationTestArgs) withDefaults() mutationTestArgs {
	if a.TestTask == "" {
		a.TestTask = "test"
	}
	if a.CompileTask == "" {
		a.CompileTask = "build"
	}
	if a.TimeoutSecs == 0 {
		a.TimeoutSecs = int(defaultTaskTimeout / time.Second)
	}
	return a
}

func (a mutationTestArgs) validate() error {
	if len(a.Mutants) == 0 {
		return errors.New("mutation_test: mutants is required (at least one {file_path, old_string, new_string})")
	}
	if len(a.Mutants) > maxMutants {
		return fmt.Errorf("mutation_test: %d mutants exceeds the limit of %d — each costs a full compile+test cycle", len(a.Mutants), maxMutants)
	}
	if !taskSlots[a.TestTask] {
		return fmt.Errorf("mutation_test: test_task must be one of build, lint, test, e2e, verify; got %q", a.TestTask)
	}
	if !taskSlots[a.CompileTask] {
		return fmt.Errorf("mutation_test: compile_task must be one of build, lint, test, e2e, verify; got %q", a.CompileTask)
	}
	if a.TestTarget != "" && !targetPattern.MatchString(a.TestTarget) {
		return fmt.Errorf("mutation_test: test_target %q is not a single shell-safe argument ([A-Za-z0-9._/:@-])", a.TestTarget)
	}
	if a.TimeoutSecs < 0 || a.TimeoutSecs > maxMutationStepSeconds {
		return fmt.Errorf("mutation_test: timeout_seconds must be between 1 and %d; got %d", maxMutationStepSeconds, a.TimeoutSecs)
	}
	return validateMutantSpecs(a.Mutants)
}

func validateMutantSpecs(specs []mutantSpec) error {
	for i, m := range specs {
		switch {
		case strings.TrimSpace(m.Path) == "":
			return fmt.Errorf("mutation_test: mutant %d: file_path is required", i+1)
		case m.Old == "":
			return fmt.Errorf("mutation_test: mutant %d: old_string is required (the exact text to mutate)", i+1)
		case m.Old == m.New:
			return fmt.Errorf("mutation_test: mutant %d: new_string equals old_string — that mutates nothing and would report a meaningless survival", i+1)
		}
	}
	return nil
}

// mutationPlan is the resolved, ready-to-run command pair for a whole run. Both
// commands are resolved BEFORE any file is touched, so a resolution failure (an
// unconfigured slot, an untrusted project command, a {target} the command has
// no placeholder for) can never leave a file mutated on disk.
type mutationPlan struct {
	compile TaskCommand
	test    TaskCommand
	target  string
	timeout time.Duration
}

func (t *MutationTest) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := parseMutationTestArgs(raw)
	if err != nil {
		return "", err
	}
	if err := args.validate(); err != nil {
		return "", err
	}
	plan, err := t.resolvePlan(args)
	if err != nil {
		return "", err
	}
	if !mutationRunLock.TryLock() {
		return "", errors.New("mutation_test: another mutation run is already in progress on this daemon — " +
			"concurrent runs would read each other's breakage as their own result. Wait for it to finish and retry")
	}
	defer mutationRunLock.Unlock()

	targets, warnings, err := t.preflight(ctx, args.Mutants)
	if err != nil {
		return "", err
	}
	if err := t.baseline(ctx, plan); err != nil {
		return "", err
	}
	results, restoreErr := t.runAll(ctx, targets, plan)
	report := formatMutationReport(args, plan, warnings, results)
	if restoreErr != nil {
		return "", fmt.Errorf("%w\n\nresults before the failure:\n%s", restoreErr, report)
	}
	return report, nil
}

func parseMutationTestArgs(raw json.RawMessage) (mutationTestArgs, error) {
	var a mutationTestArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("mutation_test: invalid arguments: %w", err)
	}
	return a.withDefaults(), nil
}

// resolvePlan resolves both stored commands up front. The compile command is
// resolved WITHOUT the target: {target} is honoured only in the test slot
// (config.TasksConfig), and a whole-module compile is the stronger check anyway
// — it catches breakage a package-scoped test run would never reach.
func (t *MutationTest) resolvePlan(a mutationTestArgs) (mutationPlan, error) {
	if t.resolve == nil {
		return mutationPlan{}, errors.New("mutation_test: task commands are not available for this session")
	}
	compile, err := t.resolve(a.CompileTask, "")
	if err != nil {
		return mutationPlan{}, fmt.Errorf("mutation_test: resolving the compile gate (%s): %w", a.CompileTask, err)
	}
	if len(compile.Steps) == 0 {
		return mutationPlan{}, fmt.Errorf("mutation_test: no %s command is configured for this workspace, so a mutant's compilation cannot be proven. "+
			"Without that proof a non-compiling mutant is indistinguishable from a kill, so the run is refused rather than reported unverifiably. "+
			"Configure [tasks.<lang>] %s, or point compile_task at a slot that does compile", a.CompileTask, a.CompileTask)
	}
	test, err := t.resolve(a.TestTask, a.TestTarget)
	if err != nil {
		return mutationPlan{}, fmt.Errorf("mutation_test: resolving the test command (%s): %w", a.TestTask, err)
	}
	if len(test.Steps) == 0 {
		return mutationPlan{}, fmt.Errorf("mutation_test: no %s command is configured for this workspace", a.TestTask)
	}
	return mutationPlan{
		compile: compile,
		test:    test,
		target:  a.TestTarget,
		timeout: time.Duration(a.TimeoutSecs) * time.Second,
	}, nil
}

// baseline runs the compile and test commands on the UNMUTATED tree and refuses
// the whole run unless both pass.
//
// Without it, `killed` does not mean what the tool says it means. A kill is
// "the suite passed before this change and fails after it", and only the second
// half was ever checked — so a suite that was ALREADY red (a peer's edit
// elsewhere in the tree, a pre-existing failure, an environment the tests need
// and cannot find) reports EVERY mutant killed, certifying assertions that were
// never exercised. That is the tool's own stated failure mode, arrived at from
// the workspace instead of from the mutant, and it is the likeliest one in
// practice: the dirty-file refusal covers the file being mutated, nothing else
// in the tree.
//
// It costs one extra compile+test cycle per RUN (not per mutant), which is the
// cheapest part of any run that has something to say. Refusing rather than
// warning matches how the tool treats its other two preconditions — a dirty
// file and a missing compile gate — because a verdict nothing stands behind is
// worse than no verdict.
//
// The refusal names WHICH of the three causes it hit (see stepFailure). It used
// to assert the workspace was red whatever happened, so a command that timed out
// or could not be started at all was reported as "the test command ALREADY
// fails … Get the suite green first" — a false diagnosis with unactionable
// advice attached, which is this tool's own defect class reached from the
// diagnostics side rather than the verdict side.
func (t *MutationTest) baseline(ctx context.Context, plan mutationPlan) error {
	if compile := t.runStep(ctx, plan.compile, plan.timeout); compile.failed() {
		return t.baselineError(plan, plan.compile, compile, roleCompile)
	}
	if test := t.runStep(ctx, plan.test, plan.timeout); test.failed() {
		return t.baselineError(plan, plan.test, test, roleTest)
	}
	return nil
}

// baselineRole is which half of the baseline failed. It is a type rather than a
// bare string because baselineError BRANCHES on it: a typo in a literal at one
// of the two call sites would compile and silently route a compile-gate failure
// into the red-suite text.
type baselineRole string

const (
	roleCompile baselineRole = "compile gate"
	roleTest    baselineRole = "test command"
)

// baselineError explains ONE failed baseline step, in the terms of what actually
// went wrong. Every branch ends "Nothing was mutated", because none of them got
// far enough to touch a file.
//
// None of them offers test_target either. A resolved command can never hold the
// {target} placeholder — substituteTarget replaces it when a target was given
// and refuses the command outright when one was not — so scoping advice keyed on
// the resolved argv could never fire, and the compile gate resolves without a
// target by construction, so scoping the test command would not shorten it
// anyway.
func (t *MutationTest) baselineError(plan mutationPlan, cmd TaskCommand, out stepOutcome, role baselineRole) error {
	where := t.runDirNote(cmd, out.step)
	switch out.failure() {
	case stepUnrunnable:
		// The command never launched, so it says NOTHING about this workspace —
		// telling the reader to fix a build or a suite would send them to repair
		// something that was never shown to be broken.
		return fmt.Errorf("mutation_test: the %s (%s) could NOT BE STARTED, so it never ran and proves nothing about this "+
			"workspace — the build and the suite may both be fine. Nothing was mutated. This is a tooling problem, not a code "+
			"problem: check that the command exists on the daemon's PATH and that %s is right.%s\n%s",
			role, cmd.Slot, slotSource(cmd.Slot), where, excerpt(out.output))
	case stepTimedOut:
		return fmt.Errorf("mutation_test: the %s (%s) TIMED OUT after %s, so it never returned a verdict — this says nothing "+
			"about whether the workspace is green. Nothing was mutated. Raise timeout_seconds (max %d) if the command "+
			"legitimately needs longer.%s\n%s",
			role, cmd.Slot, plan.timeout, maxMutationStepSeconds, where, excerpt(out.output))
	case stepExited, stepOK:
	}
	if role == roleCompile {
		return fmt.Errorf("mutation_test: the workspace does not pass its compile gate (%s) BEFORE any mutant was applied, "+
			"so no mutant's compilation could be attributed to the mutant. Nothing was mutated. Fix the build first.%s\n%s",
			cmd.Slot, where, excerpt(out.output))
	}
	return fmt.Errorf("mutation_test: the test command (%s) ALREADY fails before any mutant was applied, so every mutant "+
		"would be reported killed for a reason that has nothing to do with it — the false kill this tool exists to prevent. "+
		"Nothing was mutated. Get the suite green first.%s\n%s",
		cmd.Slot, where, excerpt(out.output))
}

// slotSource names the config key that actually holds a slot's command. verify
// stores none of its own — it is synthesised from build then test — so pointing
// the reader at [tasks.<lang>].verify sends them to a key the runner never reads.
func slotSource(slot string) string {
	if slot == "verify" {
		return "[tasks.<lang>].build and .test, which verify is built from"
	}
	return "[tasks.<lang>]." + slot
}

// runDirNote names the argv that FAILED — the step runStep stopped on — and the
// directory it ran in. A composite command runs several argvs, and naming the
// first one beside output produced by the second contradicts itself on screen.
//
// The directory is the load-bearing half. Task commands run from the WORKSPACE
// ROOT, which is not always a buildable directory — a repository whose root only
// holds a go.work, with the module in a subdirectory, fails `go build ./...`
// instantly while the tree itself compiles perfectly. Without the cwd on screen
// that reads as "your build is broken", and the reader goes looking for a
// compile error that does not exist.
func (t *MutationTest) runDirNote(cmd TaskCommand, step int) string {
	if len(cmd.Steps) == 0 {
		return ""
	}
	if step < 0 || step >= len(cmd.Steps) {
		step = 0
	}
	argv := strings.Join(cmd.Steps[step], " ")
	dir := ""
	if t.deps.WorkspaceFn != nil {
		dir = t.deps.WorkspaceFn()
	}
	if dir == "" {
		return fmt.Sprintf(" It ran `%s`.", argv)
	}
	return fmt.Sprintf(" It ran `%s` in %s — task commands run from the WORKSPACE ROOT, "+
		"which is not always the directory the command expects.", argv, dir)
}

// mutationTarget is one preflighted mutant: the resolved path plus the
// pre-mutation snapshot that restoration is built on.
type mutationTarget struct {
	spec     mutantSpec
	path     string
	display  string
	original []byte
	mode     os.FileMode
	sha      string
}

// preflight validates and snapshots EVERY mutant before any of them is applied.
//
// The split is deliberate. SAFETY preconditions (boundary, exists, regular
// file, readable, not dirty, write budget) are all-or-nothing: a request that
// fails one is refused whole, so a malformed batch never leaves a half-mutated
// tree. The MATCH precondition (old_string occurring exactly once) is NOT
// checked here — a mutant that does not apply is a legitimate per-mutant
// `invalid` result the report must show, not a reason to abandon the batch.
func (t *MutationTest) preflight(ctx context.Context, specs []mutantSpec) ([]mutationTarget, []string, error) {
	targets := make([]mutationTarget, 0, len(specs))
	var warnings []string
	unprotected := make(map[string]bool)
	for i, spec := range specs {
		tgt, err := t.preflightOne(ctx, spec)
		if err != nil {
			return nil, nil, fmt.Errorf("mutation_test: mutant %d (%s): %w", i+1, spec.Path, err)
		}
		if inRepo, _ := gitCleanliness(ctx, tgt.path); !inRepo && !unprotected[tgt.display] {
			unprotected[tgt.display] = true
			warnings = append(warnings, tgt.display+" is not in a git repository — if this daemon dies mid-run there is no `git checkout` to fall back on")
		}
		targets = append(targets, tgt)
	}
	return targets, warnings, nil
}

func (t *MutationTest) preflightOne(ctx context.Context, spec mutantSpec) (mutationTarget, error) {
	path := t.deps.resolvePath(spec.Path)
	if err := t.deps.checkBoundary(path); err != nil {
		return mutationTarget{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return mutationTarget{}, fmt.Errorf("cannot stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return mutationTarget{}, errors.New("not a regular file")
	}
	if inRepo, dirty := gitCleanliness(ctx, path); inRepo && dirty {
		return mutationTarget{}, errors.New("has uncommitted changes (untracked counts). " +
			"mutation_test refuses a dirty file with no override: a clean file is what makes `git checkout` a guaranteed recovery if restoration ever fails or the daemon dies mid-run. Commit or stash first")
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is resolved against the pinned workspace and passed the boundary guard above
	if err != nil {
		return mutationTarget{}, fmt.Errorf("cannot read: %w", err)
	}
	if !t.deps.Limiter.Allow() {
		return mutationTarget{}, rateLimitError("mutation_test", t.deps.Limiter)
	}
	return mutationTarget{
		spec:     spec,
		path:     path,
		display:  t.displayPath(path),
		original: data,
		mode:     info.Mode().Perm(),
		sha:      sha256OfString(string(data)),
	}, nil
}

// displayPath renders path relative to the session workspace for the report,
// falling back to the absolute path when no workspace is resolved.
func (t *MutationTest) displayPath(path string) string {
	if t.deps.WorkspaceFn == nil {
		return path
	}
	root := t.deps.WorkspaceFn()
	if root == "" {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}
