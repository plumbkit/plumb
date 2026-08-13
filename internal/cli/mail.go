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
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// mail.go — `plumb mail`: does a session have agent-to-agent messages waiting?
//
// It exists for ONE caller shape: a client-side hook, at the moment an agent
// finishes its turn, deciding whether to keep that turn going. Plumb's mailbox
// delivers by polling, and an agent idling on its human makes no tool calls, so
// nothing plumb does server-side reaches it. Only the client can, and the client
// needs a way to ask from outside any session. Before this, a hook had to open
// collab.db and write the delivery predicate itself.
//
// It narrows that window; it does not close it. An agent already sitting idle
// has had its end-of-turn hook run and allow, and nothing fires again until its
// human speaks — so mail arriving after the turn ends still waits. Do not build
// on this as if it were delivery.
//
// TWO RULES SHAPE EVERYTHING BELOW.
//
// It never claims. The caller is not the recipient: marking a row delivered on
// its behalf would spend the exactly-once guarantee on an agent that never saw
// the text. The handle is mode=ro (collab.OpenReadOnly) so this is enforced by
// the driver rather than by the author's care.
//
// It reveals no message CONTENT. A hook asks "is there something", never "what
// is it", so the answer is the resolved session name, its workspace root, a
// count, and the ages of the waiting messages — and nothing about any message
// beyond that it exists and when it arrived. No bodies, no senders, no
// conversation ids.
//
// Be exact about what that boundary is and is not. It is not confidentiality
// about which sessions exist or where they are pinned: the workspace root is an
// absolute path in the output, and --session / --external-id will answer for any
// live session on the machine, including one in another project. That is the
// same surface `plumb sessions` already prints to anyone who can run it, and the
// command is not a permission boundary. What it holds is the message content
// itself, for two reasons either of which is sufficient. A session NAME is not
// an identity — names come from a small pool, an ended session frees its name,
// and rename_session lets a session pick one — so content keyed on a name would
// be readable by anyone who guesses one. And a body is another agent's free
// text: printing it here pipes it straight into whatever consumes this output, a
// hook's feedback string, which is an injection channel into the very agent the
// mailbox renders these as unverified claims for. The body has somewhere better
// to go — it stays unclaimed and arrives through a real delivery path, in the
// recipient's context, correctly labelled.

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

Intended for a client-side hook that keeps a turn going when mail is waiting:
plumb's mailbox delivers by polling, so an agent waiting on its human never
learns that a peer wrote to it. This answers "is there something" from outside
the session. It narrows that window rather than closing it — an agent already
idle is not reachable at all.

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
  --workspace    a directory inside a session's workspace; the nearest enclosing
                 root wins, and several sessions on that root is an error

Exit status is 0 whether or not mail is waiting, and non-zero only on error, so
"has mail" is never confused with "the check failed". Read the count.

Scope: the workspace mailbox only. Cross-project messages live in a
daemon-level store behind the recipient's [collab] cross_project opt-in (off by
default) and are not counted. Notes addressed to "next" are excluded too — they
go to whichever session claims first.`,
	Args:        cobra.NoArgs,
	RunE:        runMail,
	Annotations: map[string]string{annoSkipLogo: "true"},
}

func init() {
	mailCmd.Flags().StringVar(&mailFlagSession, "session", "", "live session name to check")
	mailCmd.Flags().StringVar(&mailFlagExternalID, "external-id", "", "session_id the session passed to session_start")
	mailCmd.Flags().StringVar(&mailFlagWorkspace, "workspace", "", "a directory inside a session's workspace; nearest enclosing root wins")
	mailCmd.Flags().BoolVar(&mailFlagJSON, "json", false, "emit a JSON object instead of a sentence")
}

// mailReport is the whole disclosure surface: the resolved session, its
// workspace root, how many messages wait, and how old each is. Adding a field
// here is a decision about what a caller outside the session may learn — see the
// file comment, and note that no field describes any individual message beyond
// its existence and age.
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
	if selector == "workspace" {
		return matchByWorkspace(all, value)
	}
	var out []session.Info
	for _, s := range all {
		if s.Folder == "" {
			continue
		}
		hit := (selector == "session" && s.Name == value) ||
			(selector == "external-id" && s.ExternalID == value)
		if hit {
			out = append(out, s)
		}
	}
	return out
}

// matchByWorkspace resolves --workspace the way plumb itself resolves one: a
// directory belongs to the session whose root CONTAINS it, nearest root first.
//
// Exact equality was the original implementation and it was wrong in the case
// this selector exists for. Plumb acquires a workspace by walking UP to a root
// marker, so a session pinned to /repo serves every directory beneath it, and
// the hook that passes its `cwd` is routinely somewhere like
// /repo/internal/cli. Comparing the two literally answered "no live session
// matches" for a session that was live and pinned exactly there — and because
// every failure path in the recipe's hook allows the stop, the result was a
// wake hook that silently never fired and never said why.
//
// Containment goes through paths.WorkspaceRel, which already tries both the
// literal and the canonical spelling of each side, so the symlink asymmetry
// between a recorded (canonicalised) root and a typed argument is handled there
// rather than re-derived here.
//
// NEAREST wins. Nested workspaces are ordinary — a superproject with a
// submodule, a repository with a nested module — and both roots contain the
// argument. Returning both would report ambiguity and refuse, so a hook in the
// inner project would stop working the moment someone attached a session to the
// outer one. The deepest root is the one plumb's own upward walk would have
// found. Genuine ambiguity — several sessions sharing that same deepest root —
// still refuses, which is the case where guessing would wake the wrong agent.
func matchByWorkspace(all []session.Info, value string) []session.Info {
	abs, err := filepath.Abs(value)
	if err != nil {
		return nil
	}
	best := make([]session.Info, 0, 1) // one session on the nearest root is the normal case
	bestDepth := -1
	for _, s := range all {
		if s.Folder == "" {
			continue
		}
		if _, inside := paths.WorkspaceRel(s.Folder, abs); !inside {
			continue
		}
		switch depth := len(filepath.Clean(s.Folder)); {
		case depth > bestDepth:
			// A nearer root discards everything matched so far, reusing the buffer.
			best, bestDepth = append(best[:0], s), depth
		case depth == bestDepth:
			best = append(best, s)
		}
	}
	return best
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
// It reads the WORKSPACE mailbox only. A cross-project message lands in the
// daemon-level store, which a recipient reads only when its own project sets
// [collab] cross_project, and resolving that opt-in means resolving the
// workspace's config — work this probe does not do. So a cross-project message
// is never counted here and will not wake anyone. cross_project is off by
// default, which is what keeps that a footnote rather than a hole.
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
