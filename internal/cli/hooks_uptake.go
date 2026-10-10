package cli

// hooks_uptake.go — `plumb hooks uptake`: whether advisory context hints are
// configured, trusted, invoked and consumed (PLAN-462 Slice B, B7).
//
// The four are reported separately on purpose. A hook in a settings file is not
// a hook the client trusts (Codex asks the user to review every new or changed
// handler), a trusted hook is not one that fired, and one that fired is not one
// that helped. Everything here is read-only: the ledger and stats.db are opened
// read-only, nothing is created, and nothing goes through the daemon.

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/contexthints"
	"github.com/plumbkit/plumb/internal/stats"
)

const (
	// uptakeWindow and uptakeMaxCalls bound the consumption proxy: the calls an
	// agent makes in the ten minutes after a hint, at most twenty of them.
	uptakeWindow   = 10 * time.Minute
	uptakeMaxCalls = 20
)

var (
	hooksUptakeSince time.Duration
	hooksUptakeJSON  bool
)

var hooksUptakeCmd = &cobra.Command{
	Use:   "uptake",
	Short: "Report whether advisory context hints are configured, trusted, invoked and consumed",
	Long: `Report advisory context-hint uptake per client, from plumb's bounded hint
ledger and its tool-call stats, both opened read-only.

  configured  the hint handlers present in the client's hooks file
  trusted     Codex only: whether /hooks approved them (Claude Code needs no trust step)
  invoked     hook requests the daemon recorded, by outcome and reason
  consumed    emitted hints a later plumb call by the same agent mentioned, within
              ten minutes — a proxy for use, not proof of comprehension; hints whose
              agent made no attributable call are counted as unattributed

Rows the ledger evicted (1000 rows / 1 MiB / 7 days per workspace) are counted, so a
missing row is never read as a no-op.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error { return runHooksUptake(os.Stdout) },
}

func init() {
	hooksUptakeCmd.Flags().DurationVar(&hooksUptakeSince, "since", 7*24*time.Hour, "report hook requests from this far back")
	hooksUptakeCmd.Flags().BoolVar(&hooksUptakeJSON, "json", false, "print the report as JSON")
	hooksCmd.AddCommand(hooksUptakeCmd)
}

// uptakeClient is one client's configured/trusted line.
type uptakeClient struct {
	Client     string `json:"client"`
	Configured string `json:"configured"`        // "2/2", "0/2"
	Trusted    string `json:"trusted,omitempty"` // Codex only
}

type uptakeReport struct {
	Since   time.Time                 `json:"since"`
	Clients []uptakeClient            `json:"clients"`
	Hosts   []contexthints.HostUptake `json:"hosts"`
	Evicted int                       `json:"evicted"`
}

func runHooksUptake(out *os.File) error {
	since := time.Now().Add(-hooksUptakeSince)
	ledger, err := contexthints.OpenReadOnly("")
	if err != nil {
		return err
	}
	defer ledger.Close()
	obs, err := ledger.Since(since)
	if err != nil {
		return err
	}
	evicted, err := ledger.EvictedTotal()
	if err != nil {
		return err
	}
	var calls contexthints.CallInputs
	if sdb, err := stats.OpenReadOnly(); err == nil && sdb != nil {
		defer sdb.Close()
		calls = sdb.InputsForAgent
	}
	u, err := contexthints.ComputeUptake(obs, since, calls, uptakeWindow, uptakeMaxCalls)
	if err != nil {
		return err
	}
	report := uptakeReport{Since: since, Clients: uptakeClients(), Hosts: u.Hosts, Evicted: evicted}
	if hooksUptakeJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	PrintLogo()
	fmt.Fprint(out, renderUptake(report))
	return nil
}

// uptakeClients reports, per client, how many hint handlers its hooks file holds
// and, for Codex, whether they are trusted.
func uptakeClients() []uptakeClient {
	plumbBin, _ := os.Executable()
	targets := hooksTargets()
	out := make([]uptakeClient, 0, len(targets))
	for _, t := range targets {
		c := uptakeClient{Client: t.use, Configured: "unknown"}
		if path, _, states, err := hookPlan(t, plumbBin); err == nil {
			installed, total := 0, 0
			for _, s := range states {
				if slices.Contains(contextHintEvents, s.entry.event) {
					total++
					if s.state == hookStateInstalled || s.state == hookStateStale {
						installed++
					}
				}
			}
			c.Configured = fmt.Sprintf("%d/%d", installed, total)
			if t.use == codexHooksTarget.use {
				c.Trusted = codexContextTrust(path)
			}
		}
		out = append(out, c)
	}
	return out
}

// codexContextTrust reads Codex's own record of which hooks the user approved:
// [hooks.state."<hooks.json>:<event>:<group>:<handler>"] trusted_hash = "…" in
// config.toml. It reports how many hint events carry an approval; it does not
// recompute Codex's hash, so an approval of an earlier version of the handler
// still counts here, and says so.
func codexContextTrust(hooksPath string) string {
	cfgPath, err := CodexConfigPath()
	if err != nil {
		return "unknown"
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return "unknown"
	}
	var cfg struct {
		Hooks struct {
			State map[string]map[string]any `toml:"state"`
		} `toml:"hooks"`
	}
	if toml.Unmarshal(data, &cfg) != nil {
		return "unknown"
	}
	trusted := 0
	for _, event := range contextHintEvents {
		prefix := hooksPath + ":" + snakeEvent(event) + ":"
		for key, v := range cfg.Hooks.State {
			if strings.HasPrefix(key, prefix) && v["trusted_hash"] != nil {
				trusted++
				break
			}
		}
	}
	return fmt.Sprintf("%d/%d (approval recorded; hash not re-verified)", trusted, len(contextHintEvents))
}

// snakeEvent spells a hook event the way Codex keys its trust state:
// UserPromptSubmit → user_prompt_submit.
func snakeEvent(event string) string {
	var b strings.Builder
	for i, r := range event {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func renderUptake(r uptakeReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Context-hint uptake since %s\n\n", r.Since.Format(time.RFC3339))
	for _, c := range r.Clients {
		fmt.Fprintf(&b, "  %-12s configured %s", c.Client, c.Configured)
		if c.Trusted != "" {
			fmt.Fprintf(&b, "   trusted %s", c.Trusted)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if len(r.Hosts) == 0 {
		b.WriteString("  No hint requests recorded in this window.\n")
	}
	for _, h := range r.Hosts {
		fmt.Fprintf(&b, "  %s: %d invoked, %d emitted (%d B), %d noop, %d throttled, %d error; %d agent(s)\n",
			h.Host, h.Invoked, h.ByOutcome[contexthints.OutcomeEmitted], h.EmittedBytes,
			h.ByOutcome[contexthints.OutcomeNoop], h.ByOutcome[contexthints.OutcomeThrottled],
			h.ByOutcome[contexthints.OutcomeError], h.Agents)
		fmt.Fprintf(&b, "    consumed %d, not consumed %d, unattributed %d (proxy: a later plumb call by the same agent named it)\n",
			h.Consumed, h.Unconsumed, h.Unattributed)
		keys := make([]string, 0, len(h.ByDetail))
		for k := range h.ByDetail {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "    %-28s %d\n", k, h.ByDetail[k])
		}
	}
	if r.Evicted > 0 {
		fmt.Fprintf(&b, "\n  %d older observation(s) were evicted from the ledger and are not counted above.\n", r.Evicted)
	}
	return b.String()
}
