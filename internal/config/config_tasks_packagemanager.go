package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// config_tasks_packagemanager.go resolves which JavaScript package manager a
// workspace uses,
// so the shipped `typescript` task defaults name the runner the project actually
// declared instead of assuming npm.
//
// This is not a relaxation of the "never guess an uninstalled tool" rule that
// keeps most default slots empty — it is the opposite. The shipped defaults were
// ALREADY a guess: `npm run build` was assumed for every JS/TS workspace, and on
// a pnpm or yarn project it is frequently wrong rather than merely unhelpful
// (pnpm's non-flat node_modules and `workspace:*` dependencies are not something
// npm can resolve), so the first thing an agent met on such a project was a
// failing build command it had not configured. A lockfile or a corepack
// `packageManager` field is the project STATING its runner; reading it is
// evidence, not a guess.
//
// The file is NOT named config_tasks_js.go: Go reads a trailing _js as an
// implicit GOOS=js build constraint, so that name silently excludes the file
// from every build except wasm — it compiles, and the symbols are simply absent.
//
// Concurrency: all functions here are pure apart from reading files, and are
// safe for concurrent use.

// jsPackageManager is a JavaScript package manager plumb can name in a default
// command.
type jsPackageManager struct {
	Name  string
	Build string
	Test  string
}

// jsPackageManagers are the runners recognised, with the commands used for them.
//
// Every entry spells `run` explicitly, npm included where it already did for
// build. bun is the reason it matters: `bun test` invokes bun's OWN test runner
// rather than the project's test script, so a project whose `test` script wraps
// vitest or jest would silently have a different runner executed against it.
// Using `run` everywhere means one rule rather than a per-runner exception.
var jsPackageManagers = map[string]jsPackageManager{
	"npm":  {Name: "npm", Build: "npm run build", Test: "npm test"},
	"pnpm": {Name: "pnpm", Build: "pnpm run build", Test: "pnpm run test"},
	"yarn": {Name: "yarn", Build: "yarn run build", Test: "yarn run test"},
	"bun":  {Name: "bun", Build: "bun run build", Test: "bun run test"},
}

// jsLockfiles maps a lockfile name to the manager it belongs to. Order is fixed
// by jsLockfileOrder, not by map iteration.
var jsLockfiles = map[string]string{
	"pnpm-lock.yaml":      "pnpm",
	"yarn.lock":           "yarn",
	"bun.lockb":           "bun",
	"bun.lock":            "bun",
	"package-lock.json":   "npm",
	"npm-shrinkwrap.json": "npm",
}

// jsLockfileOrder is the precedence used when a workspace carries more than one
// lockfile, which is common in a repository that has migrated between managers
// and left the old file behind. The non-npm managers come first deliberately: an
// abandoned `package-lock.json` is the usual leftover, so treating it as decisive
// would reintroduce exactly the wrong-runner bug this resolves.
var jsLockfileOrder = []string{
	"pnpm-lock.yaml", "yarn.lock", "bun.lockb", "bun.lock",
	"package-lock.json", "npm-shrinkwrap.json",
}

// detectJSPackageManager reports the manager a workspace declares, and whether
// anything declared it at all. The corepack `packageManager` field in
// package.json wins over a lockfile: it is an explicit statement by the project,
// whereas a lockfile can be a leftover.
func detectJSPackageManager(root string) (jsPackageManager, bool) {
	if name, ok := packageManagerField(root); ok {
		if pm, known := jsPackageManagers[name]; known {
			return pm, true
		}
	}
	for _, file := range jsLockfileOrder {
		if _, err := os.Stat(filepath.Join(root, file)); err == nil {
			return jsPackageManagers[jsLockfiles[file]], true
		}
	}
	return jsPackageManager{}, false
}

// packageManagerField reads the corepack `packageManager` field from
// package.json ("pnpm@9.1.0" → "pnpm"). A missing file, unreadable file, absent
// field or unparseable value is simply "not declared" — this informs a default
// and must never fail a config load.
func packageManagerField(root string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, "package.json")) //nolint:gosec // G304: root is a resolved workspace root, the filename is a literal
	if err != nil {
		return "", false
	}
	var pkg struct {
		PackageManager string `json:"packageManager"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", false
	}
	name, _, _ := strings.Cut(strings.TrimSpace(pkg.PackageManager), "@")
	if name == "" {
		return "", false
	}
	return strings.ToLower(name), true
}

// applyJSPackageManagerDefaults rewrites the typescript build/test slots to the
// manager the workspace declares.
//
// It rewrites ONLY a slot still holding the shipped default, compared byte for
// byte. A value the user, their global config or the project set is left exactly
// as written — including one that names npm deliberately — so this can never
// override a command someone chose. That check is also what keeps provenance
// honest: only a "default" command is ever replaced, and what replaces it is
// still a default.
func applyJSPackageManagerDefaults(tasks map[string]TasksConfig, root string) map[string]TasksConfig {
	if tasks == nil || root == "" {
		return tasks
	}
	tc, present := tasks["typescript"]
	if !present {
		return tasks
	}
	shipped := defaultTasks()["typescript"]
	if tc.Build != shipped.Build && tc.Test != shipped.Test {
		return tasks // both chosen by someone; nothing to do
	}
	pm, declared := detectJSPackageManager(root)
	if !declared || pm.Name == "npm" {
		return tasks // npm is already what the shipped defaults say
	}
	if tc.Build == shipped.Build {
		tc.Build = pm.Build
	}
	if tc.Test == shipped.Test {
		tc.Test = pm.Test
	}
	tasks["typescript"] = tc
	return tasks
}
