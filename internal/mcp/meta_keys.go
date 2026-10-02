package mcp

// meta_keys.go — the `_meta` key vocabulary.
//
// MCP's `_meta` is the one extension point plumb uses to carry facts the base
// protocol has no field for: per-connection roots and session identity on the
// way in, and a structured error envelope on the way out. Every key is
// reverse-DNS namespaced per the MCP convention, and every one is
// forward-compatible by design — a client or daemon that does not know a key
// ignores it, so both ends can be upgraded independently.
//
// They live together because they are one vocabulary, and because a reader
// asking "what does plumb put in `_meta`?" should find the whole answer in one
// place rather than scattered through the server's plumbing.

// MetaAllowDirsKey is the MCP initialize-params `_meta` key under which
// `plumb serve` transports per-connection extra read-write roots (`--allow-dir`
// / PLUMB_ALLOWED_DIRS). It travels inside the captured initialize frame, so the
// resilient proxy's handshake replay re-applies it on every reconnect for free.
// Reverse-DNS namespaced per the MCP `_meta` convention.
const MetaAllowDirsKey = "dev.plumbkit/allow-dirs"

// MetaProxySessionKey is the MCP initialize-params `_meta` key under which
// `plumb serve` transports its stable per-proxy session ID. It is identical
// across every handshake replay, so the daemon can recognise a reconnected
// connection (after a daemon restart) as a continuation of the previous one and
// rehydrate its persisted state. Reverse-DNS namespaced per the MCP convention.
const MetaProxySessionKey = "dev.plumbkit/proxy-session-id"

// MetaProxyVersionKey is the MCP initialize-params `_meta` key under which
// `plumb serve` transports its OWN version — the binary the proxy is running,
// which is not the binary the daemon is running.
//
// The distinction has no obvious surface and has repeatedly been got wrong.
// `plumb restart` replaces the daemon; each session's proxy keeps the binary it
// was launched with until its client restarts. So a change in the proxy half
// (internal/cli/serve_proxy_*.go) can be merged, released, installed and
// running in the daemon while every attached session still executes the old
// code. Three sessions on one machine concluded a proxy-side fix was live when
// it was not, on a day when one of them had also cut the release.
//
// Carrying it here rather than annotating a tool result is what makes it exact:
// the value travels inside the captured initialize frame, so the handshake
// replay re-applies it on every reconnect, and the daemon can answer "which
// proxy is this?" from state it already holds — no frame to single out, no
// request id to correlate against a response that can outrun its own tracking.
// Reverse-DNS namespaced per the MCP convention.
const MetaProxyVersionKey = "dev.plumbkit/proxy-version"

// MetaSessionIDKey is the session_start-result `_meta` key under which the
// daemon echoes its plumb session ID, so the serve proxy can hold it and replay
// it in the initialize `_meta` after a daemon restart — the stable session ID
// across restarts (PLAN-296), distinct from the per-proxy MetaProxySessionKey.
const MetaSessionIDKey = "dev.plumbkit/session-id"

// MetaSessionIdentityKey is the initialize-RESULT `_meta` key under which the
// daemon states, authoritatively, which session the connection it just accepted
// belongs to: the internal plumb session ID, its current name, the revision
// ordering name changes, and the outcome of the recovery attempt.
//
// It exists because every earlier identity channel was gated on something
// incidental. MetaSessionIDKey rides a session_start RESULT, so a connection
// that never calls the tool never learns its ID; the workspace-argument gate
// narrowed it further still. The handshake, by contrast, is the one exchange
// every MCP connection performs exactly once, before any tool is served — so an
// identity delivered here is available to the proxy on first contact and on
// every reconnect, whatever the client goes on to call.
//
// The value is a JSON object, not a string, because a bare ID cannot say
// whether it was RECOVERED or freshly minted, and a proxy that cannot tell
// those apart cannot report the difference honestly. It deliberately never
// carries the proxy-session secret: this travels to the client, and the secret
// is the one credential that must not.
//
// A daemon that predates the key sends nothing and a proxy that predates it
// ignores the key, so either half upgrades independently.
const MetaSessionIdentityKey = "dev.plumbkit/session-identity"

// MetaDaemonInstanceKey is the initialize-RESULT `_meta` key under which the
// daemon reports an opaque marker unique to this daemon PROCESS — minted at
// start, constant for the process's life, and different for every start.
//
// It answers the one question a reconnecting proxy could not previously
// answer and yet was reporting on anyway: did the daemon process restart, or
// did this connection merely get dropped? Version equality cannot decide it (a
// restart onto the same build reports the same version, and an idle eviction
// reports the same version too), and the proxy has no other view of the daemon's
// lifetime. Absent the marker the honest answer is "unknown", which is what the
// reconnect note then says.
const MetaDaemonInstanceKey = "dev.plumbkit/daemon-instance"

// MetaWorkspaceKey is the MCP initialize-params `_meta` key under which
// `plumb serve` transports its explicit workspace pre-pin — the --workspace
// flag or PLUMB_WORKSPACE env var. There is deliberately no serve-cwd fallback:
// cwd is not intent (an MCP client spawns serve from its own launcher's
// directory), so a serve started with neither sends no key at all and starts
// unattached, with session_start as the sole workspace-pin authority. Unlike a
// client-reported root the key is not authoritative: the daemon consults it only
// after every stronger signal (a session_start-origin pin, client roots, a
// roots-origin pin) has failed, and always validates it through workspace
// detection before attaching. Identical across every handshake replay.
// Reverse-DNS namespaced per the MCP `_meta` convention.
const MetaWorkspaceKey = "dev.plumbkit/workspace"

// MetaPinnedWorkspaceKey is the MCP initialize-params `_meta` key under which
// `plumb serve` replays the workspace the caller last chose with an explicit
// `session_start(workspace=…)` call.
//
// Unlike MetaWorkspaceKey — the --workspace/PLUMB_WORKSPACE pre-pin, which still
// never overrode a session_start declaration — this is AUTHORITATIVE: it is the
// same declaration of intent as the live tool call that
// produced it, merely re-delivered to a daemon that restarted underneath the
// connection. It therefore outranks a client-reported root, which only says
// where the client happened to start. The proxy injects it at replay time (the
// pin is learned after the handshake), never on the first connect, so a session
// that never re-pins sends a byte-identical frame.
//
// An older daemon simply ignores the unknown key. Reverse-DNS namespaced per the
// MCP `_meta` convention.
const MetaPinnedWorkspaceKey = "dev.plumbkit/pinned-workspace"

// MetaResolvedWorkspaceKey is the tools/call result `_meta` key under which the
// daemon echoes the CANONICAL workspace root it actually pinned for a
// session_start(workspace=…) call — the resolved Detect/Synthesise root, not
// the caller's raw spelling. The serve proxy commits this spelling as the pin
// it replays after a restart (falling back to the raw argument against a daemon
// that predates the key), so the replayed pin is always a resolved root: one
// the restore path can verify verbatim, never an alias that would shadow the
// same project under two spellings or a subdirectory that re-resolves against
// state the proxy knows nothing about. Reverse-DNS namespaced per the MCP
// `_meta` convention.
const MetaResolvedWorkspaceKey = "dev.plumbkit/resolved-workspace"

// MetaPinScopeKey is the tools/call result `_meta` key under which the daemon
// says WHICH pin a session_start(workspace=…) call moved: "agent" (the calling
// agent's own shard on a shared connection) or "connection" (the connection's
// pin). It rides every successful session_start that carried a workspace.
//
// It exists for the serve proxy's replay pin. That pin is the CONNECTION's: it
// is replayed on reconnect as a session_start-level connection pin, which is
// sticky and outranks the client's roots. A call that moved only one agent's
// shard says nothing about where the connection should come back, so for
// "agent" the proxy records no replay pin, and does so before it reads
// MetaResolvedWorkspaceKey. The daemon still sends that key for "agent", naming
// the connection's root, because a proxy that predates this key cannot read the
// scope and would otherwise fall back to the call's raw argument, the agent's
// worktree, and replay it as the connection's pin. A daemon that predates the key
// sends nothing, which a proxy must read as "connection" — the behaviour before
// the key existed. Reverse-DNS namespaced per the MCP `_meta` convention.
const MetaPinScopeKey = "dev.plumbkit/pin-scope"

// PinScopeAgent and PinScopeConnection are the values of MetaPinScopeKey.
// tools.PinScope is defined from them, so session_start's report and the `_meta`
// the proxy reads cannot name the two scopes differently.
const (
	PinScopeAgent      = "agent"
	PinScopeConnection = "connection"
)

// MetaAlwaysLoadKey is the per-tool `tools/list` `_meta` key Claude Code reads to
// exempt a tool from MCP tool-search deferral: a tool advertised with
// `_meta["anthropic/alwaysLoad"] = true` is loaded into the client's context at
// session start instead of being deferred behind a ToolSearch round-trip.
// Unlike plumb's own `dev.plumbkit/…` keys, this one deliberately carries
// Anthropic's namespace because it is a client-recognised key, not a
// plumb-private one. Clients that predate the convention ignore the unknown
// `_meta` field, so emitting it is forward-compatible.
const MetaAlwaysLoadKey = "anthropic/alwaysLoad"

// MetaToolErrorKey is the `_meta` key under which a FAILED tools/call result
// carries plumb's structured error envelope: the failure's kind, whether it is
// retryable, the remediation, and any low-cardinality details. The key is
// present only when the error classifies, so a client can treat its absence as
// "plumb has nothing structured to say" rather than as a shape to parse.
//
// `_meta` is valid in the negotiated 2024-11-05 revision; the envelope is
// deliberately NOT emitted as `structuredContent`, which is a 2025-06-18 field.
// Reverse-DNS namespaced per the MCP `_meta` convention.
const MetaToolErrorKey = "dev.plumbkit/error"

// MetaLogicalAgentKey is the tools/call-params `_meta` key under which a client
// declares which LOGICAL AGENT issued the call. A multiplexing client runs
// several logical agents over one `plumb serve` connection; the daemon keys
// mutable state by this ID (alongside session_start's session_id) so one agent's
// pin/trackers/rate budget/undo state cannot be reset by a peer's call. A client
// that cannot supply one fails closed on state-changing calls (PLAN-286 §3).
// Reverse-DNS namespaced per the MCP `_meta` convention.
const MetaLogicalAgentKey = "dev.plumbkit/logical-agent"

// ArgLogicalAgentKey is the ARGUMENT-carried form of MetaLogicalAgentKey: the
// same reverse-DNS string, placed as a top-level key inside tools/call
// `arguments` instead of `_meta`. It exists for client runtimes that can rewrite
// a tool call's input but not its envelope — Claude Code's PreToolUse hook
// (`plumb hooks run-claude`) is the emitter — and it is stripped in
// handleToolsCall before the argument guard or any tool sees the arguments, so
// no tool schema declares it and no tool ever receives it. `_meta` outranks it
// when both are present. Reverse-DNS with a `/` cannot collide with a declared
// parameter or a parameter alias. The daemon cannot distinguish a stamp a
// runtime injected from one a model typed; the trust boundary stays the
// connection, exactly as for `_meta` (docs/threat-model.md).
const ArgLogicalAgentKey = MetaLogicalAgentKey

// ArgLogicalAgentDeclaredKey is the DECLARABLE spelling of the argument-carried
// identity, lifted exactly like ArgLogicalAgentKey. It exists because some
// hosts forward only the arguments a tool's inputSchema declares. Claude
// desktop's connector (clientInfo.name "local-agent-mode-<server>") applies the
// PreToolUse hook's updatedInput — a hook-added session_id reaches the daemon —
// but drops every undeclared key, so the reverse-DNS stamp never arrived and
// every Code-tab conversation shared one connection pin. The reverse-DNS key
// cannot be declared: the Anthropic API limits property names to
// ^[a-zA-Z0-9_.-]{1,64}$. For such a client the server advertises this key in
// every tool's schema (Server.DeclareIdentityArg); no tool declares it itself.
const ArgLogicalAgentDeclaredKey = "plumb_agent"

// MetaResumeCredentialKey is the `_meta` key of the resume credential
// (docs/identity-resume-credential-design.md), and it travels BOTH ways under the
// one name:
//
//   - Disclosed by the daemon, once per generation, as a string in an initialize
//     RESULT `_meta` (identity established or restored under a proxy credential), and
//     in a tools/call RESULT `_meta` for the two cases that have no initialize to ride:
//     the successor after an accepted resume, and a connection whose degraded
//     recovery converged on the bounded retry. It is a sibling of
//     MetaSessionIdentityKey, not a field inside it, so a proxy that predates it
//     ignores it wholesale and each key's fail-safe rule ("absence is not evidence of
//     anything") applies to it on its own.
//   - Presented by a `plumb serve` proxy, as a string in the `_meta` of a
//     `session_start` tools/call REQUEST that names a conversation it holds a stored
//     credential for. Only the request `_meta` is read: a value in the arguments is
//     the model's own and is ignored. Any other tool, and an initialize, ignores it.
//
// The value is a bearer secret (`rsk1-` and 22 base64url characters). It is never
// written to a tool result's text, a packet, a log line or the stats database;
// only its SHA-256 is stored. A daemon that predates the key sends nothing and
// ignores a presentation, and a proxy that predates it ignores the disclosure, so
// either half upgrades independently. Reverse-DNS namespaced per the MCP convention.
const MetaResumeCredentialKey = "dev.plumbkit/resume-credential" //nolint:gosec // G101: the NAME of a _meta key, not a credential
