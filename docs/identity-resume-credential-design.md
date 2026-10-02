# Resume credentials for serve-replacement identity continuity

*Status: implemented (#591 daemon, #594 proxy). The daemon mints, discloses, accepts,
rotates and revokes as specified, and deals in a credential only with a proxy that
announces it consumes and strips the key (section 3). `plumb serve` is that proxy: it
announces, strips the key from every frame it forwards, stores the credential per
conversation and presents it from a replacement serve. The proxy session ID stays the
stronger authority and the one a surviving serve uses; the resume credential is what a
REPLACEMENT serve has. It extends the vocabulary of [threat-model.md](threat-model.md)
and must not contradict it — where this design narrows a guarantee the threat model
states, it says so in [Residual risks](#9-residual-risks).*

plumb restores a reconnected connection's identity — its internal session ID,
its name, its mailbox binding — from a durable record selected by the proxy
session ID: a 122-bit secret each `plumb serve` process generates for itself at
startup (`newProxySessionID`, `internal/cli/serve.go`) and replays only inside
its own initialize handshake. That credential's lifetime is the serve PROCESS.
A client restart, an agent relaunch, or a machine reboot mints a new one, and
the strongest continuity path dies with the old process even though the same
human conversation continues. Today the replacement recovers the NAME alone,
through the external-conversation-ID claim; the internal session ID, the mail
bound to it, and every thread membership it carried do not come back.

This document designs the fix: a client-owned resume credential the daemon
discloses at identity establishment and a replacement serve replays, which
proves the same fact the proxy credential proves — "I am the process entrusted
with this conversation" — and therefore buys the same privileges. It also
evaluates the trusted-host alternative and rejects it, specifies the
credential's lifecycle, and states plainly what the design does not defend
against.

## 1. Problem statement

### What restores today, and what does not

Identity restoration is a single operation
(`restoreIdentity`, `internal/cli/conn_restore.go`) with one authorisation
argument: the durable record in `session_state.db` is selected by the proxy
session ID, and presenting that ID is evidence of being the same serve process.
A plumb session ID or a session NAME authorises nothing — both are disclosed to
clients, so presenting either is a claim, never proof. Under a live proxy
credential the restore is complete: the internal session ID is adopted, the
name comes back with it, and mail bound to the ID is readable again because the
session reads its own mailbox under its own ID.

The credential does not survive the process that minted it. When the serve
process is replaced — the machine rebooted, the client host restarted the MCP
server, the agent session was relaunched — the new serve presents a fresh proxy
secret that selects no record. Continuity then rests entirely on the external
linkage: the conversation re-declares itself with `session_start({session_id})`,
and the resume-by-external-ID path hands back the NAME its predecessor answered
to. The internal session ID does not come back —
`TestSmoke_ServeReplacementResumesByName`
(`cmd/smoke/identity_recovery_test.go`) asserts exactly that, on the grounds
that only the proxy credential may restore an ID and no credential survived.
That assertion is correct about authority and still describes the gap: the
identity that comes back is a fork with an inherited name.

The costs of the name-only resume are concrete:

- **Mail strands.** Messages bound to the predecessor's session ID sit behind
  an ID nobody holds. The degraded mailbox-inheritance grant
  (`inheritSessionID`, `internal/cli/conn_persist.go`) reaches some of them —
  gated on the successor actually holding the predecessor's name — but it is a
  single-predecessor, name-conditioned approximation of "this is the same
  conversation".
- **Threads sever.** Thread membership is ID-bound. When both parties' IDs
  change across a reboot, every reply is refused with "not one of yours", and
  the conversation's history is orphaned behind IDs that will never answer.
- **A second identity row is written per resume.** Records are never deleted,
  so each name-only resume adds a row claiming the same name and linkage under
  a new proxy key — the residue the reservation system then has to reason
  around forever.
- **The legibility gap.** The "— resumed" packet disclosed neither the new
  internal ID nor the dead binding, so an agent learned it had lost its
  identity only when a tool refused it.

### The September 2026 incident class

The reboot of 2026-09-06 exercised the whole failure at once: the daemon and
every `plumb serve` proxy died together, so no proxy credential survived
anywhere. Zero sessions resumed an internal ID. Five of six pre-reboot names
resumed by external-ID claim (the sixth was unresumable because its durable
row's linkage column was blank while its session JSON knew the conversation —
the JSON/DB split `repairBlankLinkage` now heals). The mail store held nothing
recoverable. Both working threads refused writes with "not one of yours". Every
resume wrote a second identity row. This is the failure class this design
closes: not a crash bug, but the predicted behaviour of name-only continuity
(H2 in the identity-continuity review) when the process boundary is crossed.

### Why the external-ID claim cannot be stretched to cover it

The obvious repair — let the external conversation ID authorise ID adoption
too — was reviewed and rejected (decision D1 of the identity-continuity
review). The external ID is client-supplied and client-visible; whoever can
name a conversation would inherit its ID, its mail, and its thread seats. The
name reservation system already lives with that weakness for NAMES, as a
deliberate residual, because a name is a shallow asset. An internal session ID
is not: it is what mail BINDING and thread membership authenticate against.
Upgrading a claim into that authority is the one move the identity model
cannot make.

## 2. Threat model

This section extends `threat-model.md`; its actors, boundaries and honesty
rules carry over unchanged.

### Assumed attacker capabilities

The relevant attacker is the semi-trusted agent/model of A3, working through
what the client host exposes. Assumed: it can read anything the CLIENT side
puts where it can reach — tool results, packet text, anything a client logs,
including `_meta` on frames some clients log verbatim; it can name any
session; it can present any client-visible identifier (session ID, name,
external conversation ID) as a claim. Not assumed: read access to the serve
proxy's or the daemon's PROCESS MEMORY, and read access to on-disk state
outside the workspace — the path policy keeps workspace reads inside the
pinned root, and a hostile process already running as the user can read
`session_state.db` directly, which is the standing same-user boundary the
threat model declines to defend (A6), not a property any credential here can
change.

Against that attacker, the existing structure holds: the proxy session ID is
the authority because it is never disclosed — never written to a session file,
a log line, or a tool result — so the model-side attacker has never seen one.
Session IDs and names are claims because clients see them. The resume design
must not disturb that split, and it does not: the proxy credential keeps its
rank as the sole authority, and the new credential is ADDITIVE — a second way
to prove the same fact, held to the same disclosure discipline and bought with
a weaker, honestly-priced mechanism.

### What a client-stored secret changes

The resume credential is a genuine widening of the trust surface, and pricing
it honestly is the point of this section. The proxy credential is generated
and consumed inside the serve process and never crosses the wire clientward:
its exposure set is the process memory of one local process. The resume
credential cannot live under that discipline, because its whole job is to
outlive the process that first held it: the daemon must DISCLOSE it (one
hop across the initialize result `_meta`), the proxy must PERSIST it (on its
own disk, per conversation), and a replacement proxy must PRESENT it (one hop
back across a request `_meta`). Each hop is exposure the proxy credential
never has.

Three consequences follow, and the design is shaped by all three:

1. **It is copyable client-side.** "Persisted client-side" means "copyable
   client-side". Any future leak of client-visible material — a logged
   initialize result, a captured frame — yields a reusable bearer secret.
   The design therefore treats revocation and fencing as core requirements,
   not hardening: a credential that cannot be invalidated is a credential
   that will eventually be copied.
2. **Its grant must have a low ceiling.** The credential authorises identity
   continuity for ONE conversation — ID adoption, mailbox binding, thread
   membership — and nothing else. It never widens the workspace pin, never
   opens a git tier, never runs a command; all of that remains bounded by the
   path policy and the configuration trust boundary regardless of how the
   connection proved its identity. A stolen credential lets an attacker SPEAK
   AS one conversation — read its mail, sit in its threads. That is a real
   harm and this design does not minimise it; it bounds it, and it arranges
   that the theft is loud rather than silent (rotation plus replay refusal,
   section 5).
3. **It must be conversation-bound, not client-wide.** One secret per
   conversation, so a leak cannot impersonate a different conversation on the
   same client host, and the blast radius of any single disclosure is one
   identity.

### Why the disclosure channel is acceptable

The v0 sketch flagged the disclosure question: some clients log initialize
results, and the secret rides one. The design accepts the disclosure
deliberately. The `_meta` channel is already the restricted lane for exactly
this class of fact — the identity snapshot (`dev.plumbkit/session-identity`)
and the daemon-process marker (`dev.plumbkit/daemon-instance`) travel there,
read by the proxy's fail-safe frame readers
(`internal/cli/serve_proxy_identity.go`) and not by the model. But those readers
only READ: the proxy forwards every daemon frame to its client verbatim, `_meta`
included, and Claude Code persists a tool result's `_meta` to its on-disk
transcripts (`mcpMeta._meta`), where a model with file tools can read it. A
credential in a forwarded frame is therefore in the A3 attack surface for the
life of its generation, and the daemon must not put one there. So disclosure is
gated on the proxy announcing that it consumes and strips the key (section 3,
"Establishment and disclosure"): a proxy that has not announced it is disclosed
nothing, and the key a consuming proxy reads out of a frame never reaches its
client. The model-visible packet text never carries a credential either, and the
leak scan that enforces that today extends to the new token and to every frame
the proxy forwards, `_meta` included (section 7). Rotation bounds the life of
any one disclosed value to the next accepted resume, which is the most that can
be promised of a bearer secret that must be stored to work at all. The
alternative — never disclosing — is the status quo, and the status quo is the
incident class above.

## 3. Design A — the client-owned resume credential

### Generated, never derived

The v0 sketch's first instinct — derive the secret as
`HKDF(proxy_session_id, conversation_external_id)` — is recorded here only to
reject it on the page: both inputs are client-visible claims, and deriving a
secret from claims launders a claim into a proof. Every value the client can
see is worthless as key material. The daemon GENERATES a fresh 128-bit secret
from `crypto/rand` — the same source as `newProxySessionID` — at identity
establishment. It is never derived from the proxy credential, the external ID,
the name, or any combination of them.

The token is self-describing and versioned for scanning: `rsk1-` followed by
22 base64url characters (128 bits, unpadded). A distinct shape matters twice —
the credential-leak scan can match it exactly rather than guessing, and
`internal/redact` can learn the shape for the paths A5 covers.

### Establishment and disclosure

**Only to a proxy that strips the key.** The daemon mints, discloses and accepts
a credential only on a connection whose proxy announced, in its initialize
params `_meta`, that it consumes the credential and removes the key from every
frame before forwarding it to its client:
`dev.plumbkit/resume-credential-consumer: 1` (the number 1; anything else is not
an announcement). The key travels inside the captured initialize frame, so the
handshake replay re-announces it on every reconnect. A connection without the
announcement — a direct MCP client, and every proxy built before the strip —
is minted nothing, disclosed nothing, in initialize or in any tool result, and
its presentations prove nothing: the feature is inert for it, exactly as before.
Every secret the daemon ever discloses originates in the mint at the end of an
initialize (and the C3 convergence points) or in an accepted presentation's
successor, and both check the announcement.

At the end of an initialize exchange that established or restored identity
under a proxy credential — the outcomes `established` and `restored` — the
daemon mints a credential for the identity's record and discloses it once, in
the initialize result `_meta`, under a new reverse-DNS key
(`dev.plumbkit/resume-credential`, `internal/mcp/meta_keys.go`). It is a
sibling key to the identity snapshot, not a field inside it, so the per-key
fail-safe rule ("absence of the key is not evidence of anything") applies to it
independently. A proxy that predates the key would NOT ignore it: it would
forward it, which is why the announcement above, and not the key's being a
sibling, is what makes a mixed-version pair safe (section 6).

The same trigger covers a connection whose identity converged on the bounded
degraded-recovery retry (C3): the retry flips the outcome to restored with the
proxy credential proven, which is the same fact an initialize restore
asserts, so the mint-and-disclose step rides the convergence point too.
Without this, the flakiest class — a connection that degraded under a restart
storm and healed in place — stays credential-less until its next reconnect,
which is systematic, not incidental. The C3 ordering note applies: the retry
sets the outcome before the heal commit, so anything reading the outcome
after convergence reads it settled.

The daemon stores only a SHA-256 hash. A 128-bit random secret has no
dictionary, so the hash is not reversible in any sense that matters, and a
dump of the session-state store discloses nothing usable. Disclosure is
gated on the recovery outcome precisely: a connection whose recovery was
`degraded` — running under a temporary stand-in identity — never receives a
credential, because issuing one to a connection that is not provably the
recorded identity would hand the fork bug a credential of its own. An
ordinary MCP client (no proxy credential) and a session with
`[session] persist_state` off receive nothing, exactly as today.

### Proxy-side storage

The proxy persists the secret per conversation, under its own state — never in
the workspace, never in anything a tool can address. The store entry is
`{external conversation ID, daemon scope, secret, generation}`, written when
the daemon discloses and keyed to the conversation once `session_start` links
one. File mode 0600. The daemon scope binds the entry to the daemon whose
store issued it, so a proxy does not present a secret to an unrelated daemon
that cannot meaningfully evaluate it.

As built (`internal/cli/serve_resume*.go`): the store is a directory under the
proxy's state directory (`<state dir>/serve/resume-credentials/`, 0700), one file per
conversation named by a hash of (scope, conversation) so a client-supplied conversation ID
never becomes a path, each written atomically (`internal/fsync.AtomicWrite`) at 0600 and
tightened first if a looser file was already there. Several `plumb serve` processes on one
host share it without a lock because no two write the same file unless they carry the same
conversation. The scope is a hash of the canonical path of the daemon's session-state
database, the one fact a proxy and its daemon share without being told: two daemons that
share a database share a scope. The generation is a counter of the credentials this store
has held for the conversation; the daemon never discloses its own. The proxy holds the
credential in memory from the moment it is disclosed and writes it once the first
successful `session_start` that names a conversation links one (first link wins; a later
relink, which the daemon answers by revoking, is not followed), then again at every
later disclosure: the successor after an accepted resume, a re-mint after a daemon
restart, a late C3 disclosure. The strip is the last step of `writeClient`, the one place
every byte bound for the client passes, and removes the key from every JSON object in a
frame at any depth; a frame it cannot parse has any token redacted. The proxy is the only
presenter: a credential a client put in a request `_meta` is removed from every
`session_start`.

### Presentation

Two cases, and they must not be confused:

- **Daemon restart, same serve process.** The proxy survives and replays its
  captured initialize; the proxy credential selects the record and the full
  restore runs as today. The resume credential is held in memory and NOT
  presented — the proxy credential has already proved everything the resume
  credential could, and presenting it would only rotate it pointlessly. (The
  proxy presents only while the daemon's last word on the connection is
  `established`, so a restored connection never does.)
- **Serve replacement.** The new process reads its on-disk store at startup,
  but it cannot know which conversation it carries until the client names
  one. So presentation is deferred to the first `session_start` that names a
  conversation with a stored entry: the proxy attaches the credential to that
  request's `_meta`. The per-call request `_meta` channel already exists —
  `dev.plumbkit/logical-agent` rides it today
  (`internal/mcp/server_handlers.go`) — so this is an established path, not a
  new transport.

This is a deliberate refinement of the v0 sketch's "replays it in every
initialize", and the reason belongs in the design rather than in a footnote:
a fresh serve that presented at initialize would have to choose WHICH stored
secret to present, and its store may hold several conversations (one client
host, several agents). Presenting all candidates hands every stored secret to
whatever process answers; presenting one unselected risks proving conversation
A's identity onto connection B. Deferring to the call in which the client
itself names the conversation makes the presentation self-selecting: the
named conversation selects the entry, and the credential proves the caller is
the process entrusted with exactly that conversation. A harness that CAN
identify the conversation at spawn (an environment hint naming the
conversation) may present at initialize as an optimisation; it is not
required, and nothing else in the design depends on it.

### Acceptance: hash match escalates a name resume into a full restore

On the daemon side, the acceptance rule is a hash comparison against rows
linked to the named external conversation ID. A presented hash matching the
CURRENT generation of such a row authorises the same escalation the proxy
credential authorises: the conn_restore machinery runs from a second trigger,
resuming the internal session ID (`adoptStoredID`), restoring the name, and
letting mail and threads come back as consequences of the ID — mailbox
binding and thread membership are ID-bound, which is precisely why name-only
resume strands them and ID adoption repairs them.

The machinery is already shaped for this. `restoreIdentity` runs during
initialize before any workspace attach and depends on no attach state, so a
post-initialize trigger is architecturally admissible; its ordering rules
(identity before name, failures preserve the record, degraded never
converges) are stated as invariants of the operation, not of the initialize
timing. On acceptance the identity is re-recorded under the NEW proxy
credential — as the external-ID path does today, so the new serve's own later
daemon-restart reconnects work unchanged — and the successor is issued under
that new row. Acceptance consumes exactly the presented credential: it never
touches another identity's credential for the same conversation, because a
connection can reach a conversation it merely CLAIMED, and consuming its
siblings would let it revoke the credential of whoever really owns it.

A degraded outcome on the acceptance path — the name is held by a live
overlap, the store hiccups — behaves like every degraded restore: the record
is preserved, the outcome is reported, a later attempt retries. The claimed
generation is given back and no successor is issued, so the legitimate
claimant's credential stays valid for the retry.

### Grant equivalence

Hash adoption grants exactly what proxy-credential restoration grants, and
nothing the proxy credential does not: identity, mail, threads, name. Both
credentials prove one fact — "I am the process entrusted with this
conversation" — at different strengths: the proxy credential is
process-lifetime and never leaves the process; the resume credential is
conversation-lifetime and crosses the wire once per generation. The strength
difference is paid for in lifecycle machinery (rotation, fencing, revocation,
theft signalling), not in a weaker grant.

## 4. Design B — trusted-host linkage

The alternative: the harness or host (zcode, claude, an IDE launcher)
attests, out of band, that the new serve belongs to the same conversation as
the dead one, and the daemon upgrades continuity on the host's word.

Evaluated honestly, it fails as a primary mechanism, for reasons that are
structural rather than incidental:

- **No attestation channel exists.** Across clients plumb does not control,
  there is no out-of-band path by which a host could vouch for a serve
  process. Every channel that exists — initialize `_meta`, environment, argv,
  configuration — is settable by whatever process speaks at all, which makes
  each of them a claim-shaped authority: exactly what D1 excludes. A
  host-level "trust me" is the external-ID weakness promoted to ID authority.
- **The precedent is already in the threat model.** The replayed
  pinned-workspace key is read by the daemon as an unauthenticated claim
  precisely because it cannot tell a genuine proxy replay from any other
  client that simply set the key (#318); the containment guard exists because
  of it. Design B would take the one channel the daemon already refuses to
  trust with pin WIDTH and ask it to authenticate identity — a stronger
  privilege from a weaker premise.
- **It relocates trust rather than establishing it.** A6's mailbox binding
  rests on the distinction between presenting a secret and answering to a
  name. Design B erases the distinction at the layer where the name lives.

**What would have to be true for B to win.** The daemon would need a
verifier the client host cannot forge: a key the host holds but cannot
self-issue, or an MCP-spec origin attestation with a real trust chain. In a
shared-secret world, that mechanism converges with Design A — a host-held
per-conversation secret IS the resume credential with the storage moved
host-side, and the daemon's acceptance path (hash match escalates to full
restore) is unchanged. So A is not a dead end under B's premises; it is B's
mechanism, with the secret held where today's trust actually sits: with the
conversation's own process, attested by possession rather than by assertion.
If a future specification grows a verifiable attestation primitive, the
generation and custody halves of this design swap out; the acceptance,
lifecycle and fencing halves carry over.

## 5. Credential lifecycle

### Generation

Minted by the daemon from `crypto/rand`, 128 bits, at identity establishment
(first contact) and after every accepted resume (rotation, below). One
generation is live per conversation identity at any moment.

### Storage

Daemon side: the hash, generation number and state (`current` or
`superseded`) live beside the identity record — a small credential table keyed
by the identity row, written through `internal/sqlitex` and the store mutex,
retaining the immediately-previous generation for theft detection and pruning
older ones. Hash-only storage means a store dump leaks nothing. Proxy side:
the plaintext, per conversation, under the serve's own state directory, 0600,
scoped to the issuing daemon (section 3).

### Disclosure

Initialize result `_meta` on establishment and restoration; the acceptance
response's result `_meta` (the `session_start` result — `toolResultMeta`
already contributes `_meta` there) carries the successor after a rotation, so
a resume costs one round-trip and never leaves the claimant holding a dead
secret. Both disclosures are gated on the fully-proven outcomes only: never
`degraded`, never `unavailable`, never to a connection that presented no
proxy credential.

### Rotation on every accepted resume

An accepted hash resume invalidates the presented generation (and only that
one) and issues a successor in the same response. Rotation on every resume — not only on
suspected theft — is chosen deliberately: fencing needs an invalidation
EVENT (a claimed-newer owner must be able to prove newer-ness, and a
monotonically moving generation is that proof); theft detection needs a
superseded generation to exist (a replay can only be recognised if the old
value is retained and marked); and periodic rotation without an event would
be churn that breaks the long-outage case without buying anything. The cost
is one hash write and one `_meta` field per resume.

### Revocation

Three triggers, each riding authority that already exists:

- **Detach.** Any operation that clears or replaces the record's external
  linkage under a proxy credential revokes the credential with it —
  revocation is a consequence of the linkage write, so there is no detach
  path needing separate machinery.
- **Failed ownership.** A presented secret that matches NO retained
  generation for a conversation whose record EXISTS is never an honest race —
  an honest claimant presents either the current generation or the
  superseded one — so it counts. After a small bounded number of failures
  (three), the current generation is cleared and the credential is revoked:
  continuity falls back to name-only, and the log says why. Presentations to
  a daemon with no record for the conversation do not count (a re-homed
  machine is not an attack), and superseded presentations do not count
  toward revocation — they are answered, not punished.

  Named griefing vector, accepted with bounded harm: the request `_meta` is
  client-settable and conversation IDs are client-visible claims, so any
  connection can present three junk credentials against a VICTIM's
  conversation and clear its credential. The worst case is the status quo
  ante — the victim falls back to name-only continuity until it re-links —
  with one loud log line naming the attacker's connection. Hardening that
  would need an authority the client cannot forge, which is Design B's
  problem, not this counter's; the implementation card must carry the vector
  and this acceptance, not silently inherit the counter. It is the ONLY way a
  claim reaches another identity's credential: acceptance and rotation consume
  exactly the credential presented, so a connection that relinked itself to a
  victim's conversation cannot use them to revoke the victim's.
- **Supersession.** A losing claimant's credential is dead by definition:
  the generation moved. The winner holds the only live secret.

### Exclusive-owner fencing

First accepted resume wins, and the arbitration is atomic in the store: the
generation move is a single conditional UPDATE in the pattern
`RepairExternalID` already proves — match on the exact hash AND the current
state, act only if rows-affected says it landed. It is the FIRST step of an
acceptance, ahead of any adoption or durable write: a claimant that loses has
applied nothing and has nothing to undo, and a claimant that arrives after the
winner has claimed but before it has finished (adopted, recorded, issued its
successor) finds the generation already moved and is told so, however far the
winner has got. A claimant that wins and then cannot finish gives the
generation back (above). Two concurrent claimants presenting the same
current-generation secret therefore cannot both adopt: exactly one UPDATE
matches, and the loser is told — in its own `session_start` result — that it
was SUPERSEDED. When it loses the UPDATE itself the recovery outcome says so
too and the loser continues under a temporary identity under the standing
degraded rules (never converges, never persists); one that merely finds the
generation already moved is indistinguishable from a replay and is handled as
one. Either way the human sees a double-open immediately, which is the
entire point of telling rather than silently refusing. Killing the loser's
connection is deliberately NOT specified: it is a policy decision a review
can add, and telling is the part the fencing contract needs.

### Replay after rotation is a theft signal

A presented secret matching a SUPERSEDED generation — after the rotation
that a legitimate successor would have learned — is refused, and the refusal
is logged loudly at Warn with the conversation identified. Two causes
produce this shape and the log says both: a replaced process that never
learned its successor (a zombie serve re-presenting its old credential), or
a copied credential being replayed after the true owner resumed. The
outcomes are identical — refused, name-only fallback — because the daemon
cannot distinguish them and should not pretend to; what matters is that a
copied credential converts a silent impersonation into a visible event the
moment the legitimate owner next resumes. Rotation is what makes the theft
legible; the loud log is what makes it actionable.

### Lifetime: no TTL

The credential has no independent expiry. Identity records are never pruned
by age precisely because a serve that has been connected for a week is the
one that most needs its row, and the outage this design exists to survive —
a machine off for days — is exactly what a TTL would break. Staleness is
bounded by rotation (every accepted resume mints a fresh generation), abuse
is bounded by revocation, and the residual — a hash retained indefinitely
beside an identity nobody ever resumes — discloses nothing and grants
nothing.

## 6. Failure and rollback

### Lost or corrupt client secret

The most common failure is the most boring one: the proxy's state file is
missing, truncated, or from a conversation that never got a credential, so
the replacement has nothing to present. Presentation never happens, and the
resume falls back to today's external-ID name-only path — the incident-class
behaviour, no worse. The design's guarantee is precisely this: it can only
ADD continuity on top of the existing restore, never subtract from it. Every
path in the design terminates in either a full restore (new) or the
behaviour that ships today.

### Daemon-side failures

An unreadable or schema-mismatched credential table degrades to "no
credential recorded" for every row: matching fails closed, presentation
earns a "no record" answer that does not count toward revocation, and the
existing identity restore proceeds untouched. The identity record remains
the authority in every failure, exactly as `conn_restore.go` requires.

Four combinations, all safe by construction, and the safety rests on the
consumer announcement (section 3), not on the key being a sibling:

- **New daemon, old proxy.** An old proxy does not ignore the key: it forwards
  every daemon frame to its client verbatim, `_meta` included, and the client
  persists it. So the daemon never discloses to it. The old proxy sends no
  `resume-credential-consumer` announcement, so the daemon mints nothing,
  discloses nothing in initialize or in any tool result, and accepts no
  presentation; nothing is stored and the hash path never fires. Today's
  behaviour, verbatim. (A proxy that DOES send the announcement is one that
  reads the key out of every daemon frame and strips it before forwarding;
  `plumb serve` is one.)
- **Old daemon, new proxy.** No key is disclosed; the proxy stores nothing
  and presents nothing. Inert in the other direction.
- **Both new.** The proxy announces, the daemon discloses, the proxy strips the
  key from what it forwards, and the design operates as specified.
- **Both old.** Unchanged.

The store change is additive and forward-only (a new credential table; no
existing column is reinterpreted), and `sessionstate.stampVersion` already
refuses to stamp a database downward (A7), so an old binary against a new
store reads what it knows and ignores the rest. A new binary against an old
store finds no credential table and treats every identity as credential-less
until each is re-established — which is the migration:

### Migration path

No backfill and none is needed. Credentials appear organically: the first
post-upgrade handshake under a proxy credential is an `established` or
`restored` outcome, and the disclosure rides it. Every live identity gains a
credential the first time its serve reconnects after the upgrade; identities
that never reconnect are exactly the ones that cannot be replaced either.
Rollback is the mirror image: an older binary ignores the credential table,
and the proxy-side state files are inert without a daemon that understands
them. At no point does a half-upgraded pair behave differently from today
beyond the missing continuity that today already lacks.

## 7. Test matrix

The scenarios extend the identity-recovery smoke suite
(`cmd/smoke/identity_recovery_test.go`) and its existing harness — reboot-shaped
kills, packet parsing, `assertNoCredentialLeak` — per the review's D5
instruction to extend existing tests rather than rebuild them. Every scenario
below funnels its tool results, packets and CLI output through the leak scan.

1. **Full restore across a serve replacement.** Reboot-shaped kill of serve
   and daemon; the replacement presents the stored credential at
   `session_start`; the SAME internal session ID, name, mailbox binding and
   thread membership come back; the packet reports the full identity.
   The successor of `TestSmoke_ServeReplacementResumesByName`, whose
   assertion that no ID returns must be updated BY THIS DESIGN's
   implementation card — the assertion was correct while the proxy credential
   was the only authority, and the resume credential is the second one.
2. **Replay after rotation refused.** Perform one accepted resume
   (generation moves), then present the superseded generation from a third
   process: refused, name-only outcome, the loud Warn line asserted present,
   no ID adoption.
3. **Two concurrent claimants fenced.** Two replacement processes present the
   same current-generation secret in an interleaved order the test forces:
   exactly one adoption lands; the loser's result states it was superseded;
   the loser runs under a temporary identity and did not persist one.
4. **The secret is never in any tool result, packet, or log, and never in any
   frame the proxy forwards to its client.** Extends `assertNoCredentialLeak`
   with the `rsk1-` token shape alongside the UUID shape, and scans every raw
   frame the smoke client receives from the proxy (initialize results, every
   tool result, notifications, `_meta` included, since that is what a client
   persists), every tool output, every packet, and the daemon log file —
   including the rotation successor disclosure. A connection whose proxy never
   announced the consumer key is disclosed no credential at all (in-process
   test, with the announcing connection as the positive control).
5. **Degraded connections never learn the secret.** A degraded recovery
   outcome (predecessor overlap, unreadable store) shows no credential key in
   the initialize result `_meta`, and a degraded acceptance consumes no
   generation.
6. **Restart chains, three or more deep.** Replace the serve three times in
   sequence with a full restore at every hop and rotation at every
   acceptance; deliver a real ID-bound message across the chain and read it
   at the far end; the leak scan runs over the whole chain (D5's mail
   round-trip, hardened from name-only to full-identity).
7. **Non-serve and disabled clients.** An ordinary MCP client with no proxy
   credential, and a session with `[session] persist_state` off, never
   receive a credential; their behaviour is byte-identical to today.
8. **Detach revokes.** Clear the linkage under a proxy credential; a later
   replacement presenting the stored credential is refused with "revoked",
   and the fallback is name-only.
9. **Mixed-version.** An old-build proxy against a new daemon (and the
   reverse) leaves every existing smoke test green without modification —
   the strongest single signal that the feature is purely additive.

Unit-level, the fencing and theft rules live beside the store
(`internal/sessionstate`): the conditional UPDATE's rows-affected
arbitration under concurrency, superseded-matching versus no-match
accounting, and the revocation counter.

## 8. Open questions

The card's five, with what this design answers and what it leaves open.

1. **Is disclosure in initialize `_meta` acceptable given some clients log
   it?** Answered: yes, with the argument in section 2 — `_meta` is already
   the restricted lane for identity facts, the model-visible packet text
   never carries it, rotation bounds each disclosure's life, and refusing to
   disclose is the status quo this design exists to fix. What remains open is
   empirical: whether any widely-deployed client surfaces initialize `_meta`
   to the model, which is worth confirming per-client before the
   implementation ships, and which would tighten but not change the
   conclusion (the ceiling of a leaked credential is one conversation's
   identity, and rotation plus revocation apply regardless).
2. **Rotation on every resume, or only on suspected theft?** Answered:
   every accepted resume (section 5). Fencing needs an invalidation event to
   prove newer-ness, theft detection needs a superseded generation to
   recognise a replay, and "suspected theft" has no signal to rotate on that
   replay-after-rotation does not already produce.
3. **Does the credential need a TTL independent of identity retention?**
   Answered: no (section 5). A TTL would break the long-outage case the
   design exists for; staleness is bounded by rotation and abuse by
   revocation. This is the answer most worth a reviewer's second opinion,
   since it is a policy choice rather than a derived one.
4. **Interaction with C2's repair and with degraded restores.** Answered for
   degraded restores: they never learn a credential, never consume a
   generation, and never write one — the rule that only proven branches
   mutate the record extends to the credential table without exception,
   which is the original fork bug prevented in its new costume. For C2's
   conditional linkage repair: unaffected, and ordering is fixed — the
   repair runs from the fully-restored branch only, and a credential's
   establishment on a healed record follows the repair in the same
   transaction-free but outcome-gated sequence, so a healed row is disclosed
   a credential exactly when it would have been disclosed had the linkage
   never been blank. Extended for C3's bounded retry (the retry-converged
   connection): the same outcome-gated ordering holds — the retry classifies
   established/restored before any credential step — so the establishment
   trigger covers retry-converged connections on identical terms, and Q4's
   ordering claim is inherited, not new.
5. **Test matrix.** Answered: section 7, extending the existing suite per D5.

Left explicitly open for the implementation review:

6. Whether a superseded claimant's connection should be closed by the daemon
   after being told, or left running degraded. The design tells and leaves
   the connection; closing is a policy with UX consequences that belongs to
   the implementation card.
7. Retention for the proxy-side credential store. No deletion or retention
   controls exist anywhere in plumb yet (threat-model known gap 4). The proxy store
   is capped at 256 entries, evicting the oldest by modification time, and an entry is
   replaced by the next credential disclosed for its conversation. The daemon does not
   yet tell a proxy that it refused a credential as superseded or revoked, so a proxy
   cannot delete a dead entry on that signal, and it keeps an entry whose presentation
   earned no successor because it cannot tell a refusal from a restore that could not
   finish (a live session still holding the predecessor's ID), which a later retry can
   still complete. A refused entry therefore stays until the cap evicts it.
8. Whether `plumb doctor` should surface credential state (per-conversation
   liveness, revocations, theft signals) once the feature ships; diagnostics
   for a security mechanism deserve their own pass.

## 9. Residual risks

Stated plainly, because an unclaimed property is not a guarantee.

- **The credential is strictly weaker than the proxy credential, and the
  design does not close that gap.** It crosses the wire once per generation
  and rests on disk between generations; the proxy credential does neither.
  A compromise of the client host — or of any channel the client exposes —
  yields a working, copyable bearer secret. What the design buys is a
  bounded grant, conversation-scoped, with rotation and revocation and a
  theft signal; it does not buy secrecy equal to the proxy credential's, and
  nothing in it should be cited as if it did.
- **Clients that log initialize results log the secret.** Rotation bounds the
  window; it does not close it. A client that pipes `_meta` to the model puts
  the credential inside the A3 attack surface for the life of that
  generation. Claude Code is one: it persists tool-result `_meta` to its
  transcripts. The proxy half is therefore REQUIRED to strip the key from every
  frame before forwarding it, and the daemon discloses only to a proxy that
  announced it does; a proxy that announces and does not strip would put the
  secret on disk, and nothing in the daemon can see that.
- **The identity-only ceiling still permits real harm.** A stolen credential
  lets an attacker read a conversation's mail and speak in its threads. Mail
  binding and thread membership are the assets the whole binding exists to
  protect; this design widens who can prove ownership of them from one
  process to "one process, or whoever holds one rotatable secret". The
  compensations are the fencing, the revocation triggers and the loud
  replay signal — none of which prevents a first use.
- **A same-user process reads everything.** The proxy's credential store and
  the daemon's `session_state.db` are equally readable by anything already
  running as the user. That is the standing boundary the threat model
  declines to defend, unchanged by this design — but it means the design's
  guarantees hold only against the model-side attacker, never against the
  host itself.
- **No retention controls.** The daemon retains superseded hash generations
  and the identity rows they hang off indefinitely; the proxy-side store has
  no deletion story. Neither discloses a usable secret, and both are
  instances of threat-model known gap 4 rather than new gaps — but the gap
  grows by two stores.
- **This document has had no independent security review.** It is an
  author's threat model of a new credential, which is the weakest kind — the
  posture threat-model gap 6 takes toward itself applies here with interest,
  because the asset is new. The implementation card must not ship ahead of
  the adversarial pass the board has already scheduled for this design.
