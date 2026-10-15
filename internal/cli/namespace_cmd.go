package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/sessionstate"
)

// This file is the admin surface of PLAN-520's namespaces: the control-socket
// command the daemon answers (`namespace <json>`) and the `plumb namespace`
// CLI that sends it. The registry itself is namespaces.go.

// namespaceRequest is one `namespace` control command.
type namespaceRequest struct {
	Op       string `json:"op"` // create | renew | release | list
	TTL      string `json:"ttl,omitempty"`
	OwnerPID int    `json:"owner_pid,omitempty"`
	Token    string `json:"token,omitempty"`
}

// namespaceInfo is a namespace as the CLI shows it. The token is never part of
// it: it is returned once, by create.
type namespaceInfo struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	TTL       string    `json:"ttl"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	OwnerPID  int       `json:"owner_pid,omitempty"`
}

type namespaceReply struct {
	Error      string          `json:"error,omitempty"`
	Token      string          `json:"token,omitempty"`
	Namespace  *namespaceInfo  `json:"namespace,omitempty"`
	Namespaces []namespaceInfo `json:"namespaces,omitempty"`
}

func toNamespaceInfo(ns sessionstate.Namespace) namespaceInfo {
	return namespaceInfo{ID: ns.ID, State: ns.State, TTL: ns.TTL.String(), CreatedAt: ns.CreatedAt, ExpiresAt: ns.ExpiresAt, OwnerPID: ns.OwnerPID}
}

// handleCtrl answers one control request. nil-safe: a daemon without a registry
// answers every request with errNoNamespaceStore.
func (r *namespaceRegistry) handleCtrl(req namespaceRequest) namespaceReply {
	reply := func(ns sessionstate.Namespace, err error) namespaceReply {
		if err != nil {
			return namespaceReply{Error: err.Error()}
		}
		info := toNamespaceInfo(ns)
		return namespaceReply{Namespace: &info}
	}
	switch req.Op {
	case "create":
		var ttl time.Duration
		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil {
				return namespaceReply{Error: fmt.Sprintf("bad TTL %q: %v", req.TTL, err)}
			}
			ttl = d
		}
		token, ns, err := r.create(ttl, req.OwnerPID)
		out := reply(ns, err)
		out.Token = token
		return out
	case "renew":
		return reply(r.renew(req.Token))
	case "release":
		return reply(r.release(req.Token))
	case "list":
		all, err := r.list()
		if err != nil {
			return namespaceReply{Error: err.Error()}
		}
		out := namespaceReply{Namespaces: []namespaceInfo{}}
		for _, ns := range all {
			out.Namespaces = append(out.Namespaces, toNamespaceInfo(ns))
		}
		return out
	default:
		return namespaceReply{Error: fmt.Sprintf("unknown namespace operation %q", req.Op)}
	}
}

// handleNamespaceCommand serves `namespace <json>` on the control socket.
func handleNamespaceCommand(conn net.Conn, line string, h ctrlHandlers) bool {
	payload, ok := strings.CutPrefix(line, "namespace ")
	if !ok {
		return false
	}
	var reply namespaceReply
	var req namespaceRequest
	switch {
	case h.namespace == nil:
		reply.Error = errNoNamespaceStore.Error()
	case json.Unmarshal([]byte(payload), &req) != nil:
		reply.Error = "malformed namespace request"
	default:
		reply = h.namespace(req)
	}
	data, _ := json.Marshal(reply)
	fmt.Fprintf(conn, "%s\n", data)
	return true
}

// askNamespace sends one request to the running daemon.
func askNamespace(req namespaceRequest) (namespaceReply, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return namespaceReply{}, err
	}
	raw, err := dialDaemonCtrlFull("namespace " + string(payload))
	if err != nil {
		return namespaceReply{}, err
	}
	var reply namespaceReply
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &reply); err != nil {
		return namespaceReply{}, fmt.Errorf("the daemon does not support namespaces (reply %q); update and restart it", strings.TrimSpace(raw))
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

var (
	namespaceFlagTTL      time.Duration
	namespaceFlagOwnerPID int
	namespaceFlagJSON     bool
)

var namespaceCmd = &cobra.Command{
	Use:   "namespace",
	Short: "Create and manage namespaces: private slices of the daemon for an end-to-end run",
	Long: `A namespace isolates an end-to-end run inside the one daemon: its sessions,
names, pins, read tracking, resume credentials, mail and stats are invisible to
everyone else, and released or expired together. Connect with
"plumb serve --namespace <token>" (or PLUMB_NAMESPACE).`,
}

var namespaceCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a namespace and print its token",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		owner := namespaceFlagOwnerPID
		if owner < 0 {
			owner = os.Getppid()
		}
		ttl := ""
		if namespaceFlagTTL != 0 {
			ttl = namespaceFlagTTL.String()
		}
		reply, err := askNamespace(namespaceRequest{Op: "create", TTL: ttl, OwnerPID: owner})
		if err != nil {
			return err
		}
		if namespaceFlagJSON {
			return printJSON(reply)
		}
		fmt.Printf("token: %s\nid:    %s\nexpires: %s (renewed by use; release with `plumb namespace release <token>`)\n",
			reply.Token, reply.Namespace.ID, reply.Namespace.ExpiresAt.Format(time.RFC3339))
		return nil
	},
}

func namespaceTokenCmd(op, short string) *cobra.Command {
	return &cobra.Command{
		Use:   op + " [token]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			token := os.Getenv("PLUMB_NAMESPACE")
			if len(args) == 1 {
				token = args[0]
			}
			if token == "" {
				return errors.New("no namespace token: pass it, or set PLUMB_NAMESPACE")
			}
			reply, err := askNamespace(namespaceRequest{Op: op, Token: token})
			if err != nil {
				return err
			}
			if namespaceFlagJSON {
				return printJSON(reply)
			}
			fmt.Printf("%s: %s (%s, expires %s)\n", op, reply.Namespace.ID, reply.Namespace.State, reply.Namespace.ExpiresAt.Format(time.RFC3339))
			return nil
		},
	}
}

var namespaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List namespaces",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		reply, err := askNamespace(namespaceRequest{Op: "list"})
		if err != nil {
			return err
		}
		if namespaceFlagJSON {
			return printJSON(reply.Namespaces)
		}
		if len(reply.Namespaces) == 0 {
			fmt.Println("no namespaces")
			return nil
		}
		for _, ns := range reply.Namespaces {
			fmt.Printf("%s  %-8s  ttl %-6s  expires %s  owner %d\n", ns.ID, ns.State, ns.TTL, ns.ExpiresAt.Format(time.RFC3339), ns.OwnerPID)
		}
		return nil
	},
}

func printJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func init() {
	namespaceCmd.PersistentFlags().BoolVar(&namespaceFlagJSON, "json", false, "print JSON")
	namespaceCreateCmd.Flags().DurationVar(&namespaceFlagTTL, "ttl", 0, "lifetime, renewed by every call (default 2h)")
	namespaceCreateCmd.Flags().IntVar(&namespaceFlagOwnerPID, "owner-pid", -1, "the owning process, a liveness hint (default: this command's parent; 0 for none)")
	namespaceCmd.AddCommand(namespaceCreateCmd,
		namespaceTokenCmd("renew", "Renew a namespace for another TTL"),
		namespaceTokenCmd("release", "Release a namespace: refuse its token now, purge its state after a 30 s drain"),
		namespaceListCmd)
}
