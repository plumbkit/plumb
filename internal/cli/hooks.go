package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const (
	codexSessionHookStatus = "Linking Plumb session"
	codexMailboxHookStatus = "Checking Plumb mailbox"
)

var hooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Install Plumb lifecycle hooks for supported clients",
}

var hooksInstallCmd = &cobra.Command{
	Use:   "install <client>",
	Short: "Install Plumb's opt-in Codex mailbox hooks",
	Long: `Install two opt-in hooks in Codex's user hooks.json: SessionStart states the
conversation ID needed by session_start, and Stop checks for unread Plumb mail.

The Stop hook only checks as Codex is ending the current turn. Codex background
hooks cannot wake an already-idle session, so this narrows the end-of-turn race;
it is not push delivery. Both hooks fail open: a missing mailbox, unavailable
daemon, or ambiguous session allows Codex to stop normally.

Codex requires an interactive trust review for non-managed command hooks. After
installation, use /hooks in Codex to inspect and trust the two Plumb entries.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] != "codex" {
			return fmt.Errorf("unsupported hooks client %q (supported: codex)", args[0])
		}
		return runInstallCodexHooks(cmd, args)
	},
}

var hooksRunCodexCmd = &cobra.Command{
	Use:    "run-codex",
	Short:  "Run a Codex lifecycle hook",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runCodexHook,
}

func init() {
	hooksCmd.AddCommand(hooksInstallCmd, hooksRunCodexCmd)
}

// runInstallCodexHooks installs only after Codex already has Plumb registered:
// linkage is otherwise context with no receiver, and the Stop hook could only
// emit an instruction for an unavailable tool surface.
func runInstallCodexHooks(_ *cobra.Command, _ []string) error {
	if !plumbRegisteredIn(codexTarget) {
		return errors.New("plumb is not registered in Codex — run `plumb setup codex` first")
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving Plumb binary path: %w", err)
	}
	path, err := codexHooksPath()
	if err != nil {
		return fmt.Errorf("locating Codex hooks config: %w", err)
	}
	changed, err := installCodexHooks(path, bin)
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("Installed Plumb Codex hooks in %s.\n", path)
	} else {
		fmt.Printf("Plumb Codex hooks are already current in %s.\n", path)
	}
	fmt.Println("Use /hooks in Codex to review and trust the two Plumb command hooks.")
	fmt.Println("The Stop hook checks only as a turn ends; it cannot wake an already-idle Codex session.")
	return nil
}

// codexHooksPath follows CodexConfigPath's CODEX_HOME precedence, but hooks have
// their own JSON file so one representation per config layer remains possible.
func codexHooksPath() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "hooks.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "hooks.json"), nil
}

// installCodexHooks merges only handlers carrying Plumb's stable statusMessage
// marker. Existing user groups, including other handlers on these events, remain
// untouched; re-running also refreshes the executable path after an upgrade.
func installCodexHooks(path, bin string) (bool, error) {
	cfg := map[string]any{}
	data, err := os.ReadFile(path)
	newFile := os.IsNotExist(err)
	if err != nil && !newFile {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	if !newFile && len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return false, fmt.Errorf("parsing %s as JSON: %w — will not overwrite", path, err)
		}
	}

	hooks, err := hookMap(cfg)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	command := strconv.Quote(bin) + " hooks run-codex"
	changed := upsertCodexHook(hooks, "SessionStart", codexSessionHookStatus, command, 5)
	changed = upsertCodexHook(hooks, "Stop", codexMailboxHookStatus, command, 5) || changed
	if !changed {
		return false, nil
	}
	cfg["hooks"] = hooks
	if !newFile {
		if err := backupFile(path); err != nil {
			return false, fmt.Errorf("backing up %s: %w", path, err)
		}
	}
	if err := writeJSON(path, cfg); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, nil
}

func hookMap(cfg map[string]any) (map[string]any, error) {
	if existing, ok := cfg["hooks"]; ok {
		hooks, ok := existing.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("hooks must be an object, got %T", existing)
		}
		return hooks, nil
	}
	return map[string]any{}, nil
}

func upsertCodexHook(hooks map[string]any, event, status, command string, timeout int) bool {
	want := map[string]any{
		"type":          "command",
		"command":       command,
		"timeout":       float64(timeout),
		"statusMessage": status,
	}
	existing, ok := hooks[event]
	if !ok {
		hooks[event] = []any{map[string]any{"hooks": []any{want}}}
		return true
	}
	groups, ok := existing.([]any)
	if !ok {
		return false
	}
	for _, groupAny := range groups {
		group, ok := groupAny.(map[string]any)
		if !ok {
			continue
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		for i, handlerAny := range handlers {
			handler, ok := handlerAny.(map[string]any)
			if !ok || handler["statusMessage"] != status {
				continue
			}
			if reflect.DeepEqual(handler, want) {
				return false
			}
			handlers[i] = want
			group["hooks"] = handlers
			return true
		}
	}
	hooks[event] = append(groups, map[string]any{"hooks": []any{want}})
	return true
}

type codexHookInput struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	Event          string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
}

func runCodexHook(_ *cobra.Command, _ []string) error {
	var input codexHookInput
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&input); err != nil {
		return nil // Hook failures must never strand a Codex turn.
	}
	output := codexHookResult(input, codexHookMailReport)
	if output == nil {
		return nil
	}
	return json.NewEncoder(os.Stdout).Encode(output)
}

// codexHookResult is deliberately pure apart from the supplied probe so the
// protocol shape and fail-open recursion guard stay testable without a daemon.
func codexHookResult(input codexHookInput, probe func(string, string) (mailReport, bool)) map[string]any {
	switch input.Event {
	case "SessionStart":
		if strings.TrimSpace(input.SessionID) == "" {
			return nil
		}
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "SessionStart",
			"additionalContext": fmt.Sprintf(
				"Plumb session linkage: this Codex conversation has id %s. Pass it as session_id on your first session_start call so Plumb mail can address this session.",
				strconv.Quote(input.SessionID)),
		}}
	case "Stop":
		if input.StopHookActive || probe == nil {
			return nil
		}
		report, ok := probe(input.SessionID, input.CWD)
		if !ok || report.Count == 0 {
			return nil
		}
		return map[string]any{
			"decision": "block",
			"reason":   fmt.Sprintf("You have %d unread Plumb message(s) from a peer agent. Call check_messages before finishing.", report.Count),
		}
	default:
		return nil
	}
}

// codexHookMailReport prefers the stable conversation ID and falls back to cwd
// only when it identifies exactly one live session. Every error is intentionally
// converted to no output: this hook is advisory and must fail open.
func codexHookMailReport(sessionID, cwd string) (mailReport, bool) {
	if id := strings.TrimSpace(sessionID); id != "" {
		if report, err := mailReportFor("external-id", id); err == nil {
			return report, true
		}
	}
	if dir := strings.TrimSpace(cwd); dir != "" {
		if report, err := mailReportFor("workspace", dir); err == nil {
			return report, true
		}
	}
	return mailReport{}, false
}
