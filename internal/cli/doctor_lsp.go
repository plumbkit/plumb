package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/monitor"
	"github.com/plumbkit/plumb/internal/xcodebsp"
)

// checkLSPs reports install and runtime status for all configured language servers.
// Enabled languages with a missing binary are failures. Disabled languages are
// informational regardless of install status.
// For the Java language server, a separate Java runtime check is always included.
func checkLSPs(ws string) []checkResult {
	cfg, err := config.Load()
	if err != nil {
		return []checkResult{{
			name:   "config",
			ok:     false,
			detail: err.Error(),
			fix:    "fix global config at " + contractConfigPath(config.GlobalConfigPath()),
		}}
	}
	if ws != "" {
		if merged, err := config.LoadProject(cfg, ws); err == nil {
			cfg = merged
		}
	}

	// Stable order: go first, then alphabetical.
	order := []string{"go"}
	for lang := range cfg.LSP {
		if lang != "go" {
			order = append(order, lang)
		}
	}

	var results []checkResult
	for _, lang := range order {
		lspCfg, ok := cfg.LSP[lang]
		if !ok || lspCfg.Command == "" {
			continue
		}
		results = append(results, checkLSPBinary(lang, lspCfg))
		switch lang {
		case "java":
			results = append(results, checkJavaRuntime())
		case "swift":
			results = append(results, checkSwiftToolchain())
		}
	}
	results = append(results, checkActiveLSPProcesses()...)
	if ws != "" {
		results = append(results, checkXcodeBuildServer(ws)...)
	}
	return results
}

func checkXcodeBuildServer(ws string) []checkResult {
	i := xcodebsp.Inspect(ws)
	if !i.IsBareXcode() {
		return nil
	}
	if i.BuildServerErr != nil {
		return []checkResult{{
			name: "Xcode build server", ok: false,
			detail: contractConfigPath(i.BuildServerPath) + " — " + i.BuildServerErr.Error(),
			fix:    "regenerate it with: " + i.GenerateCommand(),
		}}
	}
	if i.BuildServerOK {
		return []checkResult{{name: "Xcode build server", ok: true, detail: contractConfigPath(i.BuildServerPath) + "  (configured)"}}
	}
	if i.Ambiguous() {
		return []checkResult{{
			name: "Xcode build server", ok: true, warn: true,
			detail: "buildServer.json absent and multiple Xcode project/workspace markers were found",
			fix:    "choose a marker and run xcode-build-server config with -project or -workspace",
		}}
	}
	if _, err := exec.LookPath("xcode-build-server"); err != nil {
		return []checkResult{{
			name: "Xcode build server", ok: true, warn: true,
			detail: "buildServer.json absent; xcode-build-server not installed (optional)",
			fix:    "install xcode-build-server, then run: " + i.GenerateCommand(),
		}}
	}
	return []checkResult{{
		name: "Xcode build server", ok: true, warn: true,
		detail: "buildServer.json absent; SourceKit-LSP semantic results may be incomplete",
		fix:    "run: " + i.GenerateCommand(),
	}}
}

// checkActiveLSPProcesses queries the running daemon for its live language
// servers and reports each one's process state, PID, and resident memory — so
// `plumb doctor` shows the resource footprint of heavyweight servers (jdtls) and
// confirms idle ones have hibernated. Returns nil when the daemon is down (the
// Daemon section already reports that) or when no servers are pooled.
func checkActiveLSPProcesses() []checkResult {
	resp, err := dialDaemonCtrlFull("lsp-status")
	if err != nil {
		return nil
	}
	resp = strings.TrimRight(resp, "\n")
	if resp == "" {
		return nil
	}
	var results []checkResult
	for _, line := range strings.Split(resp, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			continue
		}
		results = append(results, checkResult{
			name:   f[0] + " (live)",
			ok:     true,
			detail: formatActiveLSPDetail(f[1], f[2], f[3], f[4]),
		})
	}
	return results
}

// formatActiveLSPDetail renders the detail line for one live server from its
// raw lsp-status fields (root, state, pid, rss_bytes).
func formatActiveLSPDetail(root, state, pid, rss string) string {
	detail := root + "  " + state
	if pid != "" {
		detail += "  pid " + pid
	}
	if rss != "" {
		if n, err := strconv.ParseUint(rss, 10, 64); err == nil {
			detail += "  " + monitor.FormatBytes(n)
		}
	}
	return detail
}

func checkLSPBinary(lang string, lspCfg config.LSPConfig) checkResult {
	// Languages are enabled by default and activate automatically when installed.
	// Enabled is true unless the user explicitly set it false, so !Enabled here
	// reliably means "excluded by config".
	if !lspCfg.Enabled {
		return checkResult{
			name:   lang,
			ok:     true,
			detail: lspCfg.Command + " — disabled in config (enabled = false)",
		}
	}
	path, lookErr := exec.LookPath(lspCfg.Command)
	if lookErr != nil {
		// Not installed is not a problem: an absent server is simply dormant.
		return checkResult{
			name:   lang,
			ok:     true,
			detail: lspCfg.Command + " not installed — optional; install it to activate automatically",
		}
	}

	out, _ := commandOutputTimeout(path, "--version")
	version := strings.TrimSpace(string(out))
	detail := contractConfigPath(path)
	if version != "" {
		detail += "  " + version
	} else {
		detail += "  (version unknown)"
	}
	detail += "  (active)"
	return checkResult{name: lang, ok: true, detail: detail}
}

// checkSwiftToolchain checks that the Swift compiler is on PATH. sourcekit-lsp
// ships with the Swift toolchain (Xcode or a standalone swift.org build), so a
// missing `swift` binary means sourcekit-lsp is also unavailable.
func checkSwiftToolchain() checkResult {
	path, err := exec.LookPath("swift")
	if err != nil {
		return checkResult{
			name:   "swift toolchain",
			ok:     false,
			detail: "swift compiler not found on PATH",
			fix:    "install Xcode or the Swift toolchain from https://swift.org/download",
		}
	}
	out, err := commandOutputTimeout(path, "--version")
	if err != nil || len(out) == 0 {
		return checkResult{
			name:   "swift toolchain",
			ok:     true,
			detail: contractConfigPath(path) + "  (version unknown)",
		}
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return checkResult{
		name:   "swift toolchain",
		ok:     true,
		detail: contractConfigPath(path) + "\n" + firstLine,
	}
}

// checkJavaRuntime checks that a Java 21+ runtime is on PATH.
// This is always shown in the Language Servers section when java is configured,
// regardless of whether jdtls itself is installed, because a missing or outdated
// JVM is the most common reason jdtls fails to start.
func checkJavaRuntime() checkResult {
	path, err := exec.LookPath("java")
	if err != nil {
		return checkResult{
			name:   "java runtime",
			ok:     false,
			detail: "java not found on PATH",
			fix:    "install Java 21+ (SDKMAN: sdk install java 21-tem, or your OS package manager)",
		}
	}
	out, err := commandOutputTimeout(path, "--version")
	if err != nil || len(out) == 0 {
		return checkResult{
			name:   "java runtime",
			ok:     true,
			detail: contractConfigPath(path) + "  (version unknown)",
		}
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	major := parseJavaMajorVersion(firstLine)
	detail := contractConfigPath(path) + "\n" + firstLine
	if major > 0 && major < 21 {
		return checkResult{
			name:   "java runtime",
			ok:     false,
			detail: fmt.Sprintf("%s  (Java %d — 21+ required by jdtls)", contractConfigPath(path), major),
			fix:    "upgrade to Java 21+",
		}
	}
	return checkResult{name: "java runtime", ok: true, detail: detail}
}

// parseJavaMajorVersion extracts the major version integer from a java --version
// first line, e.g. "openjdk 21.0.3 ..." → 21, "java 17.0.1 ..." → 17.
func parseJavaMajorVersion(versionLine string) int {
	for f := range strings.FieldsSeq(versionLine) {
		f = strings.Trim(f, "\"")
		// Old-style "1.8.0_292" — major is the component after "1."
		if strings.HasPrefix(f, "1.") && strings.Count(f, ".") >= 2 {
			if n, err := strconv.Atoi(strings.SplitN(f[2:], ".", 2)[0]); err == nil {
				return n
			}
		}
		// Modern: "21.0.3", "17.0.1" — major is the first component
		if n, err := strconv.Atoi(strings.SplitN(f, ".", 2)[0]); err == nil && n >= 8 {
			return n
		}
	}
	return 0
}

func commandOutputTimeout(name string, arg ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, arg...).Output()
}
