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
		"internal/tools.applyTextEditsToFile": "test-only helper for applying text edits without a session or history store",
		"internal/tools.readGoConfigFile":     "read-only open of go.mod/go.work config file, not a write",

		// Mutants: temporary modifications reverted within the test call.
		"internal/tools.MutationTest.runOne":        "temporary mutant restored within the call (owner decision)",
		"internal/tools.MutationTest.restore":       "temporary mutant restored within the call (owner decision)",
		"internal/tools.MutationTest.restoreFailed": "temporary mutant restored within the call (owner decision)",

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
func InspectFuncDecl(fset *token.FileSet, pkg, relPath string, fn *ast.FuncDecl, rule CoCallRule) CoCallSite {
	site := CoCallSite{
		Key: FuncDeclKey(pkg, fn),
	}
	if fn.Body == nil {
		return site
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isSatisfiedCall(call, rule.Requires) {
			site.Satisfied = true
		}
		if name, ok := triggerCall(call, rule.Triggers); ok {
			site.Triggered = true
			if site.TriggerName == "" {
				site.TriggerName = name
				site.Pos = relPath + ":" + strconv.Itoa(fset.Position(call.Pos()).Line)
			}
		}
		return true
	})
	if site.Pos == "" && site.Triggered {
		site.Pos = relPath + ":" + strconv.Itoa(fset.Position(fn.Pos()).Line)
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
