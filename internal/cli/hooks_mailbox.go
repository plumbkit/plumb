package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

const (
	ctrlMailboxCommand = "mailbox-probe "
	hookMailboxBudget  = 300 * time.Millisecond
	maxStopMailStates  = 512
)

type hookMailboxRequest struct {
	SessionID string `json:"session_id"`
	Stop      bool   `json:"stop,omitempty"`
}

type hookMailboxReply struct {
	Report mailReport `json:"report"`
	Notify bool       `json:"notify"`
}

// probeHookMailbox asks the daemon that serves check_messages. It never opens
// SQLite or falls back to offline mail inspection; any uncertainty allows Stop.
func probeHookMailbox(sessionID string, stop bool) (hookMailboxReply, bool) {
	if strings.TrimSpace(sessionID) == "" {
		return hookMailboxReply{}, false
	}
	request, err := json.Marshal(hookMailboxRequest{SessionID: sessionID, Stop: stop})
	if err != nil {
		return hookMailboxReply{}, false
	}
	line, _, err := askDaemonCtrl(ctrlMailboxCommand+string(request), time.Now().Add(hookMailboxBudget))
	if err != nil {
		return hookMailboxReply{}, false
	}
	payload, ok := strings.CutPrefix(strings.TrimSpace(line), "ok ")
	var reply hookMailboxReply
	if !ok || json.Unmarshal([]byte(payload), &reply) != nil || reply.Report.Count < 0 {
		return hookMailboxReply{}, false
	}
	return reply, true
}

func hookStopMailReport(sessionID, _ string) (mailReport, bool) {
	reply, ok := probeHookMailbox(sessionID, true)
	return reply.Report, ok && reply.Notify
}

func handleMailboxCommand(out io.Writer, line string, h ctrlHandlers) bool {
	payload, matched := strings.CutPrefix(line, ctrlMailboxCommand)
	if !matched {
		return false
	}
	var request hookMailboxRequest
	if len(payload) > 4096 || json.Unmarshal([]byte(payload), &request) != nil || request.SessionID == "" || h.mailbox == nil {
		fmt.Fprintln(out, "error: mailbox probe unavailable")
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookMailboxBudget)
	defer cancel()
	reply, err := h.mailbox(ctx, request)
	if err != nil {
		// The control response carries no database paths, bodies or error details.
		fmt.Fprintln(out, "error: mailbox probe unavailable")
		return true
	}
	fmt.Fprint(out, "ok ")
	_ = json.NewEncoder(out).Encode(reply)
	return true
}

// hookInbox resolves only an established live owner or a known logical-agent
// shard. A hook cannot create a recipient, acquire predecessors or claim mail.
func (s *connSession) hookInbox(external string) (tools.Inbox, bool) {
	if s.ctx.Err() != nil || external == "" {
		return tools.Inbox{}, false
	}
	if !s.ownsConnectionID(external) {
		s.shardsMu.Lock()
		sh := s.shards[external]
		s.shardsMu.Unlock()
		if sh == nil {
			return tools.Inbox{}, false
		}
		sh.mu.RLock()
		registered := sh.rosterID != ""
		sh.mu.RUnlock()
		if !registered {
			return tools.Inbox{}, false
		}
	}
	ctx := mcp.WithLogicalAgent(s.ctx, external)
	inbox := s.knownInboxFor(ctx)
	inbox.WorkspaceReader = func() (*collab.Store, error) { return s.collabPool.probeResult(inbox.Root) }
	inbox.GlobalReader = s.collabPool.probeGlobalResult
	return inbox, inbox.Self != "" && inbox.SelfID != "" && inbox.Root != ""
}

// mailboxProbe resolves exactly one live recipient, then observes the daemon's
// pooled handles with delivery's policy and predicate. CWD/name guesses are not
// routing evidence. The response is metadata only and never claims a note.
func (r *connRegistry) mailboxProbe(ctx context.Context, request hookMailboxRequest) (hookMailboxReply, error) {
	r.mu.Lock()
	var resolvers []func(string) (tools.Inbox, bool)
	for _, h := range r.conns {
		if h.mailboxInbox != nil {
			resolvers = append(resolvers, h.mailboxInbox)
		}
	}
	r.mu.Unlock()
	var inbox tools.Inbox
	matches := 0
	for _, resolve := range resolvers {
		if candidate, ok := resolve(request.SessionID); ok {
			inbox = candidate
			matches++
		}
	}
	if matches != 1 {
		return hookMailboxReply{}, errors.New("mailbox recipient unavailable or ambiguous")
	}
	key := request.SessionID + "\x00" + inbox.Root + "\x00" + inbox.SelfID
	if request.Stop {
		unlock, err := r.lockStopMailbox(ctx, key)
		if err != nil {
			return hookMailboxReply{}, err
		}
		defer unlock()
	}
	snapshot, err := inbox.Snapshot(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return hookMailboxReply{}, err
	}
	reply := hookMailboxReply{Report: mailReport{
		Session: inbox.Self, Workspace: inbox.Root, Count: snapshot.Count,
	}}
	if request.Stop && snapshot.Count > 0 {
		reply.Notify = r.shouldNotifyMailbox(key, snapshot.Fingerprint)
	}
	return reply, nil
}

// stopMailboxState holds one recipient's gate and notice fingerprint. Map access
// and fingerprint updates use registry.mu; its gate orders snapshot/decision
// pairs without coupling healthy recipients to another recipient's slow query.
type stopMailboxState struct {
	gate        chan struct{}
	fingerprint string
	notified    bool
}

func (r *connRegistry) stopMailboxStateFor(key string) *stopMailboxState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopMailSeen == nil {
		r.stopMailSeen = make(map[string]*stopMailboxState)
	}
	state := r.stopMailSeen[key]
	if state == nil && len(r.stopMailSeen) < maxStopMailStates {
		state = &stopMailboxState{gate: make(chan struct{}, 1)}
		r.stopMailSeen[key] = state
	}
	return state
}

// lockStopMailbox serialises only this recipient's snapshot and notification
// decision. Policy resolution precedes the gate; a waiting hook keeps its
// original deadline. Saturation fails open rather than adding unbounded gates.
func (r *connRegistry) lockStopMailbox(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state := r.stopMailboxStateFor(key)
	if state == nil {
		return nil, errors.New("mailbox probe state unavailable")
	}
	select {
	case state.gate <- struct{}{}:
		return func() { <-state.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// One notice per unchanged eligible row set, even when the explicit check fails
// or is ignored. New arrivals can notify again. This is bounded in-memory hook
// state, not a delivery watermark; saturation allows completion.
func (r *connRegistry) shouldNotifyMailbox(key, fingerprint string) bool {
	state := r.stopMailboxStateFor(key)
	if state == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	notify := !state.notified || state.fingerprint != fingerprint
	state.fingerprint, state.notified = fingerprint, true
	return notify
}
