// Package xcodebsp inspects an Xcode workspace for SourceKit-LSP build-server
// configuration. It is deliberately side-effect free: generation belongs to
// the opt-in workflow, while doctor and MCP tools consume this shared model.
package xcodebsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Inspection describes the Xcode/BSP state at a workspace root.
type Inspection struct {
	Root            string
	Projects        []string
	Workspaces      []string
	SwiftPM         bool
	BuildServerPath string
	BuildServerOK   bool
	BuildServerErr  error
}

type buildServerDocument struct {
	Name      string   `json:"name"`
	Argv      []string `json:"argv"`
	Languages []string `json:"languages"`
}

// Inspect reads only root-level project markers and buildServer.json.
func Inspect(root string) Inspection {
	i := Inspection{Root: root, BuildServerPath: filepath.Join(root, "buildServer.json")}
	entries, err := os.ReadDir(root)
	if err != nil {
		return i
	}
	for _, entry := range entries {
		switch {
		case entry.Name() == "Package.swift":
			i.SwiftPM = true
		case entry.IsDir() && strings.HasSuffix(entry.Name(), ".xcodeproj"):
			i.Projects = append(i.Projects, filepath.Join(root, entry.Name()))
		case entry.IsDir() && strings.HasSuffix(entry.Name(), ".xcworkspace"):
			i.Workspaces = append(i.Workspaces, filepath.Join(root, entry.Name()))
		}
	}
	sort.Strings(i.Projects)
	sort.Strings(i.Workspaces)
	b, err := os.ReadFile(i.BuildServerPath)
	if os.IsNotExist(err) {
		return i
	}
	if err != nil {
		i.BuildServerErr = err
		return i
	}
	var document buildServerDocument
	if err := json.Unmarshal(b, &document); err != nil {
		i.BuildServerErr = fmt.Errorf("invalid JSON: %w", err)
		return i
	}
	if document.Name == "" || len(document.Argv) == 0 || !contains(document.Languages, "swift") {
		i.BuildServerErr = fmt.Errorf("missing required BSP name, argv, or swift language")
		return i
	}
	i.BuildServerOK = true
	return i
}

// IsBareXcode reports an Xcode project that cannot rely on SwiftPM metadata.
func (i Inspection) IsBareXcode() bool {
	return !i.SwiftPM && (len(i.Projects) > 0 || len(i.Workspaces) > 0)
}

// NeedsGuidance reports a bare Xcode project without a usable BSP file.
func (i Inspection) NeedsGuidance() bool { return i.IsBareXcode() && !i.BuildServerOK }

// Ambiguous reports when a safe command would require guessing a marker.
func (i Inspection) Ambiguous() bool { return len(i.Projects)+len(i.Workspaces) > 1 }

// GenerateCommand returns a deterministic, copyable command. Workspaces take
// precedence because they include their member projects and dependency graph.
func (i Inspection) GenerateCommand() string {
	if i.Ambiguous() {
		return ""
	}
	flag, target := "", ""
	if len(i.Workspaces) > 0 {
		flag, target = "-workspace", i.Workspaces[0]
	} else if len(i.Projects) > 0 {
		flag, target = "-project", i.Projects[0]
	}
	if target == "" {
		return ""
	}
	return fmt.Sprintf("cd %s && xcode-build-server config %s %s", shellQuote(i.Root), flag, shellQuote(target))
}

// Hint explains why semantic results may be empty and how to configure BSP.
func (i Inspection) Hint() string {
	if !i.NeedsGuidance() {
		return ""
	}
	if i.Ambiguous() {
		return "\n\nXcode note: multiple .xcodeproj/.xcworkspace markers were found and buildServer.json is not usable. Choose the intended marker and run `xcode-build-server config -project <path>` or `xcode-build-server config -workspace <path>`, then restart the Plumb session."
	}
	return "\n\nXcode note: this workspace has no valid buildServer.json, so SourceKit-LSP may return incomplete or empty semantic results. Install xcode-build-server, then run:\n  " + i.GenerateCommand() + "\nBuild the selected scheme once in Xcode so compile flags are present in the build log, then restart the Plumb session."
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
