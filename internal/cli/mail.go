package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// mail.go — `plumb mail`: does a session have agent-to-agent messages waiting?
//
// It exists for ONE caller shape: a client-side hook deciding whether to wake an
// agent that has finished its turn. Plumb's mailbox delivers by polling, and an
// agent idling on its human makes no tool calls, so nothing plumb does
// server-side can reach it. Only the client can, and the client needs a way to
// ask the question from outside any session. Before this, a hook had to open
// collab.db and write the delivery predicate itself.
//
// TWO RULES SHAPE EVERYTHING BELOW.
//
// It never claims. The caller is not the recipient: marking a row delivered on
// its behalf would spend the exactly-once guarantee on an agent that never saw
// the text. The handle is mode=ro (collab.OpenReadOnly) so this is enforced by
// the driver rather than by the author's care.
//
// It reveals only whether, and how stale. A hook asks "is there something",
// never "what is it", so the answer is a count and the ages of the waiting
// messages — no bodies, no senders, no conversation ids. Three reasons, and any
// one of them is sufficient. A session NAME is not an identity: names come from
// a small pool, an ended session frees its name, and rename_session lets a
// session pick one, so anyone who can run this command could otherwise read a
// peer's mail by guessing what it is called. A message body is another agent's
// free text, and printing it here would pipe it straight into whatever consumes
// this output — a hook's feedback string — which is an injection channel into
// the very agent the mailbox renders these as unverified claims for. And the
// body has somewhere better to go: it stays unclaimed, and arrives through a
// real delivery path, in the recipient's context, correctly labelled.

var (
	mailFlagSession    string
	mailFlagExternalID string
	mailFlagWorkspace  string
	mailFlagJSON       bool
)

var mailCmd = &cobra.Command{
	Use:   "mail",
	Short: "Report whether a session has unread agent-to-agent messages",
	Long: `Report how many messages are waiting for a plumb session, without reading
or consuming them.

Intended for a client-side hook that wakes an idle agent: plumb's mailbox
delivers by polling, so an agent waiting on its human never learns that a peer
wrote to it. This answers "is there something" from outside the session.

It is strictly read-only — the messages stay undelivered and reach the agent
through ` + "`check_messages`" + ` as usual — and reports only a count and the ages
of what is waiting. Bodies, senders and conversation ids are deliberately not
reported: a session name is not proof of identity, and a peer's text belongs in
the recipient's context, not in a hook's output.

Name exactly one selector:

  --session      a live session's name, as shown by ` + "`plumb sessions`" + `
  --external-id  the value a session passed to session_start's session_id
                 (a client's own conversation id — the reliable selector for a
                 hook, which knows its conversation but not the session name)
  --workspace    a directory, when exactly one live session is pinned there

Exit status is 0 whether or not mail is waiting, and non-zero only on error, so
"has mail" is never confused with "the check failed". Read the count.`,
	Args:        cobra.NoArgs,
	RunE:        runMail,
	Annotations: map[string]string{annoSkipLogo: "true"},
}

func init() {
	mailCmd.Flags().StringVar(&mailFlagSession, "session", "", "live session name to check")
	mailCmd.Flags().StringVar(&mailFlagExternalID, "external-id", "", "session_id the session passed to session_start")
	mailCmd.Flags().StringVar(&mailFlagWorkspace, "workspace", "", "workspace root with exactly one live session")
	mailCmd.Flags().BoolVar(&mailFlagJSON, "json", false, "emit a JSON object instead of a sentence")
}

// mailReport is the whole disclosure surface. Adding a field here is a decision
// about what a caller outside the session may learn; see the file comment.
type mailReport struct {
	Session   string `json:"session"`
	Workspace string `json:"workspace"`
	Count     int    `json:"count"`
	// AgesSeconds is one entry per waiting message, oldest first, so a hook can
	// apply a staleness rule (a note minutes from expiry may not be worth
	// interrupting for) without being told anything about content.
	AgesSeconds []int `json:"ages_seconds"`
}

func runMail(_ *cobra.Command, _ []string) error {
	info, err := resolveMailSession()
	if err != nil {
		return err
	}
	ages, err := mailWaiting(info.Folder, info.Name)
	if err != nil {
		return err
	}
	report := mailReport{
		Session:     info.Name,
		Workspace:   info.Folder,
		Count:       len(ages),
		AgesSeconds: ages,
	}
	if mailFlagJSON {
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	fmt.Println(mailSentence(report))
	return nil
}

// resolveMailSession finds the one live session the selector names. Ambiguity is
// an error rather than a guess: this drives whether an agent is interrupted, and
// waking the wrong one is worse than waking none.
func resolveMailSession() (session.Info, error) {
	selector, value, err := mailSelector()
	if err != nil {
		return session.Info{}, err
	}
	all, err := session.List()
	if err != nil {
		return session.Info{}, fmt.Errorf("listing sessions: %w", err)
	}
	matches := matchSessions(all, selector, value)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return session.Info{}, fmt.Errorf("no live session matches --%s %q (see `plumb sessions`)", selector, value)
	default:
		return session.Info{}, fmt.Errorf(
			"%d live sessions match --%s %q (%s) — select one with --session or --external-id",
			len(matches), selector, value, strings.Join(mailNames(matches), ", "))
	}
}

// mailSelector returns the single selector flag that was set, refusing zero or
// several. They are mutually exclusive because they answer the same question
// differently, and a call that set two would have to be resolved by precedence
// no caller could predict.
func mailSelector() (name, value string, err error) {
	set := make([][2]string, 0, 3)
	for _, f := range [][2]string{
		{"session", mailFlagSession},
		{"external-id", mailFlagExternalID},
		{"workspace", mailFlagWorkspace},
	} {
		if strings.TrimSpace(f[1]) != "" {
			set = append(set, f)
		}
	}
	switch len(set) {
	case 1:
		return set[0][0], strings.TrimSpace(set[0][1]), nil
	case 0:
		return "", "", errors.New("name a session: --session <name>, --external-id <id>, or --workspace <dir>")
	default:
		return "", "", fmt.Errorf("name exactly one selector, not %d", len(set))
	}
}

// matchSessions filters live sessions by the chosen selector. A session with no
// resolved folder is skipped throughout: its workspace is where a mailbox would
// live, so there is nothing to check for one.
func matchSessions(all []session.Info, selector, value string) []session.Info {
	var want []string
	if selector == "workspace" {
		want = workspaceArgSpellings(value)
		if len(want) == 0 {
			return nil
		}
	}
	var out []session.Info
	for _, s := range all {
		if s.Folder == "" {
			continue
		}
		var hit bool
		switch selector {
		case "session":
			hit = s.Name == value
		case "external-id":
			hit = s.ExternalID == value
		case "workspace":
			hit = slices.Contains(want, filepath.Clean(s.Folder))
		}
		if hit {
			out = append(out, s)
		}
	}
	return out
}

// workspaceArgSpellings returns the forms a --workspace argument may legitimately
// take when compared with a recorded session root: the absolute path, and its
// symlink-resolved form when those differ.
//
// A session's Folder arrives already canonicalised — internal/cli resolves
// symlinks once when it acquires the workspace (issue #263) — while this value
// is whatever a human or a hook typed. On macOS the difference is routine, since
// /tmp is a symlink to /private/tmp, and a lexical compare of the two spellings
// matches nothing while reading as "no session here" rather than as a path
// problem.
//
// Only the ARGUMENT is normalised, never the session's Folder. Re-resolving the
// stored root would re-litigate an invariant the acquisition site already
// guarantees and put a syscall per session behind every call — the same reasoning
// that keeps EvalSymlinks out of leave_note's sameWorkspace. Accepting either
// spelling of the untrusted side costs nothing and needs no such assumption.
func workspaceArgSpellings(value string) []string {
	abs, err := filepath.Abs(value)
	if err != nil {
		return nil
	}
	abs = filepath.Clean(abs)
	out := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		if resolved = filepath.Clean(resolved); resolved != abs {
			out = append(out, resolved)
		}
	}
	return out
}

func mailNames(matches []session.Info) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Name)
	}
	return out
}

// mailWaiting returns the age in seconds of every message waiting for name in
// the workspace's mailbox, oldest first.
//
// It goes through collab.PendingNotes, the same listing path workspace_sessions
// uses, rather than a query written here, so the delivery predicate stays
// defined in one place.
//
// That inherits PendingNotes' exclusion of notes addressed to "next", which is
// the right trade rather than a free one. A "next" note goes to whichever
// session claims it first, so counting it for every candidate would wake several
// agents for one message that all but one of them will lose the race for. The
// cost is real: a "next" note left while a session sits idle will not wake it,
// and it arrives whenever that session next makes a call of its own.
//
// An absent collab.db is not an error. It is created lazily on first use, so a
// workspace whose sessions have never exchanged a message simply has no mailbox
// and nothing is waiting.
func mailWaiting(workspace, name string) ([]int, error) {
	// Never nil: ages_seconds is a JSON array in the contract, and a nil slice
	// marshals as null. `jq '.ages_seconds | length'` errors on null, so a
	// consumer would break on precisely the common case — no mail at all.
	if workspace == "" || !collab.Exists(workspace) {
		return []int{}, nil
	}
	store, err := collab.OpenReadOnly(workspace)
	if err != nil {
		return nil, fmt.Errorf("opening the mailbox: %w", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now()
	rows, err := store.PendingNotes(ctx, name, workspace, now)
	if err != nil {
		return nil, fmt.Errorf("reading the mailbox: %w", err)
	}
	ages := make([]int, 0, len(rows))
	for _, r := range rows {
		// Clamped at zero: a note stamped in the future (clock skew between the
		// writer and this process) would otherwise report a negative age, which
		// reads as nonsense in a staleness rule.
		age := max(int(now.Sub(r.CreatedAt).Seconds()), 0)
		ages = append(ages, age)
	}
	slices.Sort(ages)
	slices.Reverse(ages) // oldest first: a larger age is an older message
	return ages, nil
}

// mailSentence renders the human form. It says the session name back so a
// caller that resolved by --external-id or --workspace can see which session it
// actually asked about.
func mailSentence(r mailReport) string {
	if r.Count == 0 {
		return fmt.Sprintf("No messages waiting for %s.", r.Session)
	}
	return fmt.Sprintf("%d %s waiting for %s (oldest %s, newest %s). Call check_messages in that session to read them.",
		r.Count, textfmt.Plural(r.Count, "message", "messages"), r.Session,
		mailAge(r.AgesSeconds[0]), mailAge(r.AgesSeconds[len(r.AgesSeconds)-1]))
}

func mailAge(seconds int) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", seconds)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
