package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
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
	return results
}

func checkLSPBinary(lang string, lspCfg config.LSPConfig) checkResult {
	path, lookErr := exec.LookPath(lspCfg.Command)
	if lookErr != nil {
		if lspCfg.Enabled {
			return checkResult{
				name:   lang,
				ok:     false,
				detail: lspCfg.Command + " not found on PATH",
				fix:    "install " + lspCfg.Command + " and ensure it is on your PATH",
			}
		}
		return checkResult{
			name:   lang,
			ok:     true,
			detail: lspCfg.Command + " not installed — optional, enable in config.toml to activate",
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
	if !lspCfg.Enabled {
		detail += "  (disabled — enable in config.toml to activate)"
	}
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
