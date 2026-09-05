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
