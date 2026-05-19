package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/golimpio/plumb/internal/fsguard"
)

// Discovery scans a project directory and reports what plumb can infer about
// it: build system, test layout, and likely entry points. Used by
// `plumb init --discover` to seed .plumb/context.md and starter memories.
type Discovery struct {
	Root          string
	BuildSystems  []string // e.g. "Go modules", "npm", "Cargo"
	Languages     []string // e.g. "Go", "TypeScript"
	TestDirs      []string // e.g. "internal/.../foo_test.go", "tests/"
	EntryPoints   []string // e.g. "cmd/myapp/main.go", "src/index.ts"
	HasMakefile   bool
	HasDockerfile bool
	HasCI         bool // .github/workflows/, .gitlab-ci.yml, etc.
	HasReadme     bool
	GitRemote     string // origin URL if a git repo
}

// Discover walks root non-recursively at the top level, then targeted depths
// for each build system to keep cost bounded. Returns a populated Discovery.
//
// refuseHomeRoots gates the walk through fsguard: when set and running on
// macOS, Discover refuses to crawl $HOME or one of its protected subdirs
// (Desktop, Documents, Downloads, Pictures, Music, Movies, Public, iCloud
// Drive). The caller is expected to source this from walk.refuse_home_roots
// in the resolved config.
func Discover(root string, refuseHomeRoots bool) (*Discovery, error) {
	if skip, reason := fsguard.RefuseWalk(root, refuseHomeRoots); skip {
		return nil, fmt.Errorf("refusing to discover %s: %s — use a project subdirectory, or set walk.refuse_home_roots=false", root, reason)
	}
	d := &Discovery{Root: root}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", root, err)
	}
	langs := map[string]struct{}{}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "go.mod":
			d.BuildSystems = append(d.BuildSystems, "Go modules")
			langs["Go"] = struct{}{}
		case name == "package.json":
			d.BuildSystems = append(d.BuildSystems, "npm/yarn/pnpm")
			langs["JavaScript/TypeScript"] = struct{}{}
		case name == "Cargo.toml":
			d.BuildSystems = append(d.BuildSystems, "Cargo")
			langs["Rust"] = struct{}{}
		case name == "pyproject.toml" || name == "requirements.txt" || name == "setup.py":
			d.BuildSystems = append(d.BuildSystems, "Python")
			langs["Python"] = struct{}{}
		case name == "pom.xml":
			d.BuildSystems = append(d.BuildSystems, "Maven")
			langs["Java"] = struct{}{}
		case name == "build.gradle" || name == "build.gradle.kts":
			d.BuildSystems = append(d.BuildSystems, "Gradle")
			langs["Java/Kotlin"] = struct{}{}
		case name == "Gemfile":
			d.BuildSystems = append(d.BuildSystems, "Bundler")
			langs["Ruby"] = struct{}{}
		case name == "Makefile" || name == "makefile":
			d.HasMakefile = true
		case name == "Dockerfile":
			d.HasDockerfile = true
		case strings.EqualFold(name, "README.md"), strings.EqualFold(name, "README.rst"), strings.EqualFold(name, "README"):
			d.HasReadme = true
		case name == ".github":
			if info, err := os.Stat(filepath.Join(root, name, "workflows")); err == nil && info.IsDir() {
				d.HasCI = true
			}
		case name == ".gitlab-ci.yml", name == ".circleci", name == ".travis.yml":
			d.HasCI = true
		}
	}
	for l := range langs {
		d.Languages = append(d.Languages, l)
	}
	sort.Strings(d.Languages)
	sort.Strings(d.BuildSystems)

	d.EntryPoints = findEntryPoints(root)
	d.TestDirs = findTestDirs(root)
	d.GitRemote = readGitRemote(root)
	return d, nil
}

// findEntryPoints does a bounded walk for common entry-point files.
func findEntryPoints(root string) []string {
	var out []string
	maxDepth := 4
	candidates := map[string]bool{
		"main.go": true, "main.py": true, "main.rs": true,
		"index.ts": true, "index.js": true, "index.tsx": true,
		"App.tsx": true, "app.py": true, "manage.py": true,
		"server.go": true, "server.ts": true,
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "node_modules" || name == ".git" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if depth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if candidates[d.Name()] {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// findTestDirs reports common test layout markers.
func findTestDirs(root string) []string {
	var out []string
	for _, p := range []string{"tests", "test", "__tests__", "spec"} {
		if info, err := os.Stat(filepath.Join(root, p)); err == nil && info.IsDir() {
			out = append(out, p+"/")
		}
	}
	// For Go: scan up to depth 3 for *_test.go files.
	hasGoTest := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || hasGoTest {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "node_modules" || name == ".git" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if strings.Count(rel, string(filepath.Separator)) > 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			hasGoTest = true
		}
		return nil
	})
	if hasGoTest {
		out = append(out, "Go *_test.go (co-located)")
	}
	return out
}

func readGitRemote(root string) string {
	cfg, err := os.ReadFile(filepath.Join(root, ".git", "config"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(cfg), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "url = "); ok {
			return after
		}
	}
	return ""
}

// renderDiscoveryContext builds the markdown body for .plumb/context.md from
// a Discovery report.
func renderDiscoveryContext(d *Discovery) string {
	var sb strings.Builder
	sb.WriteString("# Project Context\n\n")
	sb.WriteString("Auto-generated by `plumb init --discover`. Edit freely — plumb does not regenerate this file.\n\n")

	sb.WriteString("## Overview\n\n")
	sb.WriteString("<!-- Describe what this project does and who uses it. -->\n\n")

	sb.WriteString("## Detected stack\n\n")
	if len(d.Languages) > 0 {
		fmt.Fprintf(&sb, "- Languages: %s\n", strings.Join(d.Languages, ", "))
	}
	if len(d.BuildSystems) > 0 {
		fmt.Fprintf(&sb, "- Build: %s\n", strings.Join(d.BuildSystems, ", "))
	}
	if d.HasMakefile {
		sb.WriteString("- Makefile present (run `make` for targets)\n")
	}
	if d.HasDockerfile {
		sb.WriteString("- Dockerfile present\n")
	}
	if d.HasCI {
		sb.WriteString("- CI workflows present\n")
	}
	if d.GitRemote != "" {
		fmt.Fprintf(&sb, "- Git remote: `%s`\n", d.GitRemote)
	}
	sb.WriteString("\n")

	if len(d.EntryPoints) > 0 {
		sb.WriteString("## Entry points\n\n")
		for _, p := range d.EntryPoints {
			fmt.Fprintf(&sb, "- `%s`\n", p)
		}
		sb.WriteString("\n")
	}

	if len(d.TestDirs) > 0 {
		sb.WriteString("## Test layout\n\n")
		for _, p := range d.TestDirs {
			fmt.Fprintf(&sb, "- %s\n", p)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Architecture\n\n")
	sb.WriteString("<!-- Key design decisions, module boundaries, invariants. -->\n\n")

	sb.WriteString("## Conventions\n\n")
	sb.WriteString("<!-- Naming, formatting, testing patterns. -->\n\n")

	sb.WriteString("## Known gotchas\n\n")
	sb.WriteString("<!-- Footguns and non-obvious behaviour. -->\n")

	return sb.String()
}
