package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// CoCallRule pins "a function that calls any of Triggers must ALSO call
// Requires" within Scope — the shape CallRule (only X may call this) and
// DelegationRule (a literal ⇒ a call) cannot express. Allowed keys are
// "<pkg>.<Recv>.<Func>" for methods and "<pkg>.<Func>" for functions, so one
// exemption never covers every method sharing a name.
type CoCallRule struct {
	Name     string
	Scope    []string // package directories relative to the module root; subdirectories included
	Triggers []string // "os.Remove" (qualified) or "safeWrite" (bare, same package)
	Requires string   // method or function name, any receiver
	Why      string
	Allowed  map[string]string
}

// CoCallRules is checked by TestCoCallRules.
var CoCallRules = []CoCallRule{{
	Name:  "write-history",
	Scope: []string{"internal/tools", "internal/cli"},
	Triggers: []string{
		"safeWrite", "safeWriteSibling", "os.WriteFile", "os.Create", "os.OpenFile",
		"os.Truncate", "os.Remove", "os.RemoveAll", "os.Rename", "fsync.AtomicWrite", "fsync.AtomicWriteFunc",
		"memory.WriteIndexedWithOptions", "memory.WriteWithOptions", "memory.DeleteIndexed", "memory.Delete",
		"memory.WriteGenerated", "memory.PruneGeneratedEpisodic",
		"config.AgentApplyBatch",
	},
	Requires: "recordHistory",
	Why: "every file write plumb makes on a user's behalf is recorded in history.db (spec 2026-10-01 " +
		"write-diff-history §6): call recordHistory (WriteDeps, historySink, txlog.RestoreSink) after the " +
		"write lands and before the per-path lock is released — or add an Allowed entry saying why this " +
		"write is not a user-visible change",
	//nolint:gosec // G101: false positive on map keys resembling credentials
	Allowed: map[string]string{
		// Primitive file-writing implementations whose callers record history.
		"internal/tools.safeWrite":        "primitive temp-file staging and atomic rename; callers record history",
		"internal/tools.safeWriteSibling": "primitive temp-file staging and atomic rename; callers record history",

		// Internal and test helpers.
		"internal/tools.applyTextEditsToFile": "called only from tests (it lives in edit_apply.go beside the production path); tools apply edits through applyWorkspaceEditDetailed, which records",
		"internal/tools.readGoConfigFile":     "read-only open of go.mod/go.work config file, not a write",

		// Generated memories: plumb writes them itself and prunes them to a
		// retention cap. Spec §2 leaves episodic memories out; a shared finding is
		// the same kind of entry in the same pruned pool.
		"internal/tools.ShareFindings.run":                      "generated memory in plumb's auto-pruned pool (an agent-shared finding), plus that pool's prune; out of scope with episodic memories (spec §2)",
		"internal/cli.connSession.writeGeneratedEpisodicMemory": "episodic memory and its pool's prune: out of scope (spec §2)",

		// Mutants: temporary modifications reverted within the test call.
		"internal/tools.MutationTest.runOne":        "temporary mutant restored within the call (owner decision)",
		"internal/tools.MutationTest.restore":       "temporary mutant restored within the call (owner decision)",
		"internal/tools.MutationTest.restoreFailed": "temporary mutant restored within the call (owner decision)",

		// A clean-clone merge preview (PLAN-454): plumb builds a throwaway bare
		// repository beside the real one, points its object store at the real one with
		// an alternates file, and deletes the whole directory again before returning.
		// Nothing the user owns is written, so there is no user-visible change to
		// record in history.db — the same reasoning as the mutants above.
		"internal/tools.writeAlternateRepo": "writes the alternates file of a throwaway preview clone, removed in the same call",
		"internal/tools.Git.runCleanClone":  "removes that throwaway preview clone; no user file is touched",

		// mutation_test's crash journal (PLAN-459). The entry lives under the daemon's
		// own state dir, and the sweep's write is plumb UNDOING its own temporary
		// mutation: the mutant was never a user edit, so neither the mutation nor its
		// reversal is a user-visible change to record.
		"internal/tools.journalMutant":            "journal entry for a mutant in flight, under the daemon's state dir",
		"internal/tools.checkMutantJournalUsable": "writability probe for the journal directory: creates a temp file under the daemon's state dir and removes it again; not user content",
		"internal/tools.clearMutantJournal":       "removes that entry once the file is verifiably back",
		"internal/tools.sweepOne":                 "restores a source file a killed mutation_test left mutated; plumb undoing its own temporary change",
		"internal/tools.SweepMutantJournal":       "removes the journal entries it has resolved, under the daemon's state dir",

		// Git lock sidecars, not user content.
		"internal/tools.clearGitLockOwner":  "git lock sidecar file, not user content",
		"internal/tools.reapStaleGitLock":   "git lock sidecar file, not user content",
		"internal/tools.recordGitLockOwner": "git lock sidecar file, not user content",

		// Transaction-log internal bookkeeping.
		"internal/tools/txlog.atomicWriteManifest": "write-ahead-log bookkeeping under .plumb/tx-log",
		"internal/tools/txlog.Begin":               "write-ahead-log bookkeeping under .plumb/tx-log",
		"internal/tools/txlog.Log.Commit":          "write-ahead-log bookkeeping under .plumb/tx-log",
		"internal/tools/txlog.Log.Record":          "write-ahead-log bookkeeping under .plumb/tx-log",
		"internal/tools/txlog.Log.Rollback":        "write-ahead-log bookkeeping under .plumb/tx-log; transaction_apply records tx_rollback",
		"internal/tools/txlog.ScanRecording":       "write-ahead-log bookkeeping under .plumb/tx-log; crash-recovery restores recorded via RestoreSink",

		// CLI daemon runtime and bookkeeping files (PID, socket, profiler dumps, wake locks).
		"internal/cli.acquireDaemonLock":           "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.acquireSpawnLock":            "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.acquireWakeLockWith":         "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.claudeStopHook":              "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.ensureResumeProofKey":        "the identity hook and serve's own per-user proof key under the state directory (removes its temporary file), not an agent file write",
		"internal/cli.findAllDaemonPIDs":           "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.handleHeapProfile":           "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.handleStacksProfile":         "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.publishDaemonPID":            "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.recordWake":                  "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.resumeStore.prune":           "serve proxy's own resume-credential store under the state directory, not an agent file write",
		"internal/cli.resumeStore.put":             "serve proxy's own resume-credential store under the state directory, not an agent file write",
		"internal/cli.runDaemon":                   "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.startDaemonProcess":          "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.stopByPID":                   "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.sweepWakeDir":                "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.wakeLock.release":            "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.workspacePool.pruneStateDir": "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.writeIdentityProbe":          "daemon/CLI bookkeeping, not an agent file write",
		"internal/cli.writeWakeStamp":              "daemon/CLI bookkeeping, not an agent file write",

		// User-run CLI setup/init/skill management commands (not an agent's tool call).
		"internal/cli.backupFileTo":           "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.backupSkillDir":         "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.cleanupSkillBackups":    "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.installSkill":           "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.installSkillReferences": "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.removeHooksAt":          "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.removeOwnedFile":        "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.removePlumbSkills":      "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.replaceForcedSkill":     "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.runInit":                "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.saveSkillManifest":      "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.writeConflictProposal":  "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.writeDSHPatch":          "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.writeJSON":              "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.writeTOML":              "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
		"internal/cli.writeYAML":              "daemon/CLI bookkeeping or explicit user-run setup, not an agent file write",
	},
}}

// CoCallSite represents one function inspected by a CoCallRule.
type CoCallSite struct {
	Key         string
	Pos         string
	Triggered   bool
	Satisfied   bool
	TriggerName string
}

// FuncDeclKey returns "<pkg>.<Recv>.<Name>" for methods and "<pkg>.<Name>" for functions.
func FuncDeclKey(pkg string, fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return pkg + "." + fn.Name.Name
	}
	expr := fn.Recv.List[0].Type
	for {
		star, ok := expr.(*ast.StarExpr)
		if !ok {
			break
		}
		expr = star.X
	}
	recvName := ""
	if ident, ok := expr.(*ast.Ident); ok {
		recvName = ident.Name
	} else if index, ok := expr.(*ast.IndexExpr); ok {
		if ident, ok := index.X.(*ast.Ident); ok {
			recvName = ident.Name
		}
	}
	if recvName != "" {
		return pkg + "." + recvName + "." + fn.Name.Name
	}
	return pkg + "." + fn.Name.Name
}

func isSatisfiedCall(call *ast.CallExpr, required string) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name == required
	}
	if ident, ok := call.Fun.(*ast.Ident); ok {
		return ident.Name == required
	}
	return false
}

func triggerCall(call *ast.CallExpr, triggers []string) (string, bool) {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if ident, ok := sel.X.(*ast.Ident); ok {
			qName := ident.Name + "." + sel.Sel.Name
			if slices.Contains(triggers, qName) {
				return qName, true
			}
		}
		return "", false
	}
	if ident, ok := call.Fun.(*ast.Ident); ok {
		if slices.Contains(triggers, ident.Name) {
			return ident.Name, true
		}
	}
	return "", false
}

// InspectFuncDecl inspects a function or method declaration against a CoCallRule.
//
// Each trigger needs its OWN Requires call later in the body: a Requires call
// settles the most recent trigger still waiting (in source order), and the
// function is satisfied when none is left waiting. So a function with two
// writes and one record fails, which a "does the body call Requires at all"
// test let through. The match is syntactic: it cannot tell that one write needs
// two records (rename_file's delete-of-destination plus rename rows), so those
// remain pinned by the write-site tests, not by this rule.
func InspectFuncDecl(fset *token.FileSet, pkg, relPath string, fn *ast.FuncDecl, rule CoCallRule) CoCallSite {
	site := CoCallSite{
		Key: FuncDeclKey(pkg, fn),
	}
	if fn.Body == nil {
		return site
	}
	type waiting struct {
		name string
		pos  token.Pos
	}
	var pending []waiting
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isSatisfiedCall(call, rule.Requires) && len(pending) > 0 {
			pending = pending[:len(pending)-1]
		}
		if name, ok := triggerCall(call, rule.Triggers); ok {
			site.Triggered = true
			pending = append(pending, waiting{name, call.Pos()})
		}
		return true
	})
	site.Satisfied = site.Triggered && len(pending) == 0
	if len(pending) > 0 {
		site.TriggerName = pending[0].name
		site.Pos = relPath + ":" + strconv.Itoa(fset.Position(pending[0].pos).Line)
	}
	return site
}

// WalkCoCallSites walks all production .go files under rule.Scope and inspects their functions.
func WalkCoCallSites(root string, rule CoCallRule) ([]CoCallSite, error) {
	var sites []CoCallSite
	fset := token.NewFileSet()

	for _, scope := range rule.Scope {
		base := filepath.Join(root, scope)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			rel, rerr := filepath.Rel(root, filepath.Dir(path))
			if rerr != nil {
				return rerr
			}
			pkg := filepath.ToSlash(rel)
			relFile, _ := filepath.Rel(root, path)

			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name == nil {
					continue
				}
				s := InspectFuncDecl(fset, pkg, filepath.ToSlash(relFile), fn, rule)
				sites = append(sites, s)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return sites, nil
}
