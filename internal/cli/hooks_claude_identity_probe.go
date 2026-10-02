package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// The identity hook's control-socket probe: how it asks the running daemon its
// version and the identity keys it lifts. The cache and the stamp decision that
// consume the answer are in hooks_claude_identity.go.

// askDaemonCtrl makes one ask of the daemon's control socket — the daemon
// answers a single command per connection — and returns the reply line.
// connected says whether the dial itself succeeded: a failure before it means
// there is nothing to talk to (retrying is pointless), a failure after it means
// the daemon was there and did not answer in time.
//
// Each phase has its own limit (identityProbeDialTimeout, then
// identityProbeReplyTimeout) and both are capped by deadline, the probe's
// overall budget, so a second ask cannot run past it.
//
// The failure is CLASSIFIED rather than flattened, because the kinds have
// opposite remedies and a third has neither. No socket (or one refusing
// connections) is a daemon that is not running: it starts on the next `plumb
// serve` and stamping resumes by itself. Anything else — a permission error on
// the socket, a dial timeout against a wedged listener, a short read —
// observed no answer at all, and naming either remedy would be a guess dressed
// as a fact.
func askDaemonCtrl(command string, deadline time.Time) (reply string, connected bool, err error) {
	dial := min(identityProbeDialTimeout, time.Until(deadline))
	if dial <= 0 {
		return "", false, errIdentityProbeBudget
	}
	conn, err := net.DialTimeout("unix", daemonCtrlSocketPath(), dial)
	if err != nil {
		return "", false, classifyDialError(err)
	}
	defer conn.Close()
	limit := time.Now().Add(identityProbeReplyTimeout)
	if limit.After(deadline) {
		limit = deadline
	}
	_ = conn.SetDeadline(limit)
	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		return "", true, fmt.Errorf("sending %q to the daemon: %w", command, err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", true, fmt.Errorf("reading the daemon's reply to %q: %w", command, err)
	}
	return line, true, nil
}

// probeDaemonVersion asks the running daemon its version (`version`, added with
// the channel — an older daemon answers with an unknown-command error, which
// reads as "too old"). An answer that is not a version is a daemon that
// predates the channel: it needs `plumb restart`.
func probeDaemonVersion(deadline time.Time) (string, error) {
	line, _, err := askDaemonCtrl("version", deadline)
	if err != nil {
		return "", err
	}
	return parseDaemonVersionReply(line)
}

// probeDaemonIdentity asks the daemon which identity argument keys it lifts
// and which version it is. A current daemon answers both to one `identity-keys`
// ask, so the common probe is a single dial: two dials doubled the chance that
// a loaded machine lost one of them, and either loss used to cost the stamp.
//
// A daemon whose `identity-keys` reply carries no version (it predates the
// field) or does not know the command at all (it predates the command, which
// reads as "not the declared key" — the safe answer, since such a daemon would
// reject it) is asked its version separately. So is a daemon that was
// connected to but did not answer identity-keys in time: the version is still
// worth having, because it is what keeps the stamp off a daemon that predates
// the channel, and the record is then marked uncertain — the safe key for this
// call, nothing cached — rather than guessed.
func probeDaemonIdentity() (identityProbeRecord, error) {
	return probeDaemonIdentityBy(time.Now().Add(identityProbeBudget), false)
}

// probeDaemonIdentityBy is probeDaemonIdentity against a caller-chosen
// deadline. haveRecord says a cached record for this daemon instance exists;
// a daemon that was connected to but did not answer identity-keys then costs
// nothing more — the record already answers, and a second ask of a wedged
// daemon only added its own timeout to every call.
func probeDaemonIdentityBy(deadline time.Time, haveRecord bool) (identityProbeRecord, error) {
	reply, connected, err := askDaemonCtrl(ctrlIdentityKeysCommand, deadline)
	if err != nil && !connected {
		return identityProbeRecord{}, err
	}
	answer := parseIdentityKeysReply(reply)
	if err == nil && answer.version != "" {
		return identityProbeRecord{DaemonVersion: answer.version, DeclaredKey: answer.declared, HookProof: answer.proof}, nil
	}
	if err != nil && haveRecord {
		return identityProbeRecord{}, err
	}
	version, verr := probeDaemonVersion(deadline)
	if verr != nil {
		return identityProbeRecord{}, verr
	}
	return identityProbeRecord{DaemonVersion: version, DeclaredKey: answer.declared, HookProof: answer.proof, uncertain: err != nil}, nil
}

// identityKeysAnswer is a parsed `identity-keys` reply.
type identityKeysAnswer struct {
	// declared: the daemon lists mcp.ArgLogicalAgentDeclaredKey.
	declared bool
	// proof: the daemon lists mcp.ArgHookProofKey, so it drops the proof a serve that
	// predates it forwards.
	proof bool
	// version is the daemon's own version when the reply carries it, so one ask
	// answers both questions; empty from a daemon that predates the field.
	version string
}

// ctrlIdentityKeysVersionField prefixes the daemon's version among the keys of
// an `identity-keys` reply. A key never contains `=`, and an older hook reads
// the reply only for the declared key, so the extra token is invisible to it.
const ctrlIdentityKeysVersionField = "version="

// parseIdentityKeysReply parses `ok <key> <key>... [version=<version>]`. Any
// other reply — an older daemon's `error: unknown command` included — is the
// empty answer: no declared key, no version.
func parseIdentityKeysReply(line string) identityKeysAnswer {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ok ")
	if !ok {
		return identityKeysAnswer{}
	}
	var answer identityKeysAnswer
	for _, field := range strings.Fields(rest) {
		switch version, isVersion := strings.CutPrefix(field, ctrlIdentityKeysVersionField); {
		case isVersion:
			answer.version = version
		case field == mcp.ArgLogicalAgentDeclaredKey:
			answer.declared = true
		case field == mcp.ArgHookProofKey:
			answer.proof = true
		}
	}
	return answer
}

// classifyDialError separates "there is no daemon" from "there is something
// there and plumb could not talk to it". Only a missing socket file and a
// refused connection mean the daemon is down; a permission error, a dial
// timeout against a wedged listener and anything else observed no such thing,
// and the caller must not be told to wait for a start that already happened.
func classifyDialError(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return errDaemonNotRunning
	}
	return fmt.Errorf("dialling the daemon control socket: %w", err)
}

// parseDaemonVersionReply accepts the `version` command's `ok <version>` line
// and treats anything else — an `error: unknown command` from an older daemon
// included — as no answer.
func parseDaemonVersionReply(line string) (string, error) {
	line = strings.TrimSpace(line)
	if v, ok := strings.CutPrefix(line, "ok "); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), nil
	}
	return "", errDaemonVersionUnknown
}

// The two ways a probe fails are told apart because their remedies differ:
// a daemon that is not running starts on the next `plumb serve` and stamping
// resumes by itself, while one that answers but predates the channel needs
// `plumb restart`.
var (
	errDaemonNotRunning     = errors.New("no daemon is listening on the control socket")
	errDaemonVersionUnknown = errors.New("the daemon did not report a version, so it predates the identity channel")
	// errIdentityProbeBudget: the probe used its whole time budget before it
	// could make another ask. Neither of the above — nothing was observed.
	errIdentityProbeBudget = errors.New("the daemon probe ran out of time")
)
