// Package xcodebsp inspects an Xcode workspace for SourceKit-LSP build-server
// configuration. It is deliberately side-effect free: generation belongs to
// the opt-in workflow, while doctor and MCP tools consume this shared model.
package xcodebsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	xcodeBuildServerName       = "xcode build server"
	xcodeBuildServerExecutable = "xcode-build-server"
)

// Marker identifies the Xcode container selected for build-server generation.
type Marker struct {
	Path string
	Flag string
}

// Selection is a marker plus the validated scheme that xcode-build-server needs.
type Selection struct {
	Marker Marker
	Scheme string
}

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

type listDocument struct {
	Project struct {
		Schemes []string `json:"schemes"`
	} `json:"project"`
	Workspace struct {
		Schemes []string `json:"schemes"`
	} `json:"workspace"`
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
	if err := validateBuildServer(document); err != nil {
		i.BuildServerErr = err
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

// Ambiguous reports when safe marker selection cannot choose a container.
func (i Inspection) Ambiguous() bool {
	_, err := i.SelectMarker()
	return err != nil
}

// SelectMarker applies documented Xcode precedence: one workspace wins,
// otherwise one project may be used. Multiple candidates of the selected kind
// are an actionable no-op; generation must never guess.
func (i Inspection) SelectMarker() (Marker, error) {
	switch {
	case len(i.Workspaces) == 1:
		return Marker{Path: i.Workspaces[0], Flag: "-workspace"}, nil
	case len(i.Workspaces) > 1:
		if len(i.Projects) == 1 {
			return Marker{Path: i.Projects[0], Flag: "-project"}, nil
		}
		return Marker{}, errors.New("multiple Xcode workspaces found")
	case len(i.Projects) == 1:
		return Marker{Path: i.Projects[0], Flag: "-project"}, nil
	case len(i.Projects) > 1:
		return Marker{}, errors.New("multiple Xcode projects found")
	default:
		return Marker{}, errors.New("no Xcode project or workspace found")
	}
}

// Schemes parses the matching xcodebuild JSON branch for marker.
func Schemes(marker Marker, data []byte) ([]string, error) {
	var document listDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parsing xcodebuild scheme list: %w", err)
	}
	var schemes []string
	switch marker.Flag {
	case "-project":
		schemes = document.Project.Schemes
	case "-workspace":
		schemes = document.Workspace.Schemes
	default:
		return nil, fmt.Errorf("unsupported Xcode marker flag %q", marker.Flag)
	}
	return uniqueSorted(schemes), nil
}

// SelectScheme validates an explicit scheme or selects the sole discovered one.
func SelectScheme(schemes []string, explicit string) (string, error) {
	schemes = uniqueSorted(schemes)
	if explicit != "" {
		if contains(schemes, explicit) {
			return explicit, nil
		}
		return "", fmt.Errorf("configured Xcode scheme %q was not found", explicit)
	}
	switch len(schemes) {
	case 0:
		return "", errors.New("no Xcode schemes found")
	case 1:
		return schemes[0], nil
	default:
		return "", fmt.Errorf("multiple Xcode schemes found: %s", strings.Join(schemes, ", "))
	}
}

// GenerateCommand returns a deterministic, copyable command. It intentionally
// omits a scheme because PR 1 must not run xcodebuild merely to improve a hint.
func (i Inspection) GenerateCommand() string {
	marker, err := i.SelectMarker()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("cd %s && xcode-build-server config %s %s", shellQuote(i.Root), marker.Flag, shellQuote(marker.Path))
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

func validateBuildServer(document buildServerDocument) error {
	if document.Name != xcodeBuildServerName {
		return fmt.Errorf("unexpected BSP server name %q", document.Name)
	}
	if len(document.Argv) == 0 || filepath.Base(document.Argv[0]) != xcodeBuildServerExecutable {
		return errors.New("buildServer.json does not invoke xcode-build-server")
	}
	if !contains(document.Languages, "swift") {
		return errors.New("buildServer.json does not support swift")
	}
	return nil
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
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
