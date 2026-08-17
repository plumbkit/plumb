package cli

// conn_attach.go — workspace attach, re-pin, and language detection.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools/txlog"
)

// attachWorkspace resolves rootURI to a project root, acquires the shared
// language server if needed, and updates the session record. This entry point
// attaches from the client's reported root, so the resulting pin records
// PinSourceRoots — a real signal, but a weaker one than a workspace the caller
// named themselves.
func (s *connSession) attachWorkspace(ctx context.Context, rootURI string) {
	s.attachWorkspacePin(ctx, rootURI, sessionstate.PinSourceRoots)
}

// attachWorkspacePin is attachWorkspace with control over the origin recorded
// for the pin. PinSourceUnknown means "do not persist": an auto-attach seeded
// from an incidental tool path (onBeforeTool) must never become the sticky
// target, so a reconnect restores the workspace the caller actually chose rather
// than whatever file it touched first. See persistPin.
func (s *connSession) attachWorkspacePin(ctx context.Context, rootURI string, origin sessionstate.PinSource) {
	s.attachWorkspacePinFrom(ctx, rootURI, origin, pinTriggerLive)
}

// attachWorkspacePinFrom is attachWorkspacePin with the pin trigger made
// explicit, so a pin restored on reconnect is not logged as a live one.
func (s *connSession) attachWorkspacePinFrom(ctx context.Context, rootURI string, origin sessionstate.PinSource, trigger pinTrigger) {
	folder := paths.URIToPath(rootURI)
	if folder == "" || folder == "/" {
		return
	}
	projectRoot, language, err := s.pool.Detect(folder)
	if err != nil {
		slog.Info("daemon: no project root found — deferring to first tool call", "folder", folder)
		return
	}
	if projectRoot != folder {
		folder = projectRoot
	}

	// Containment guard (issue #306): a DETECTED wide root — a marker placed
	// at a directory that is or CONTAINS a home directory — still needs a
	// declaration before it may be pinned.
	if err := undeclaredWideRootErr(folder, origin); err != nil {
		s.log().Warn("daemon: refusing to pin a wide workspace root", "root", folder, "origin", string(origin), "reason", err)
		return
	}

	s.mutate(func(v *sessionView) {
		if v.acquiredRoot != "" {
			return
		}
		lang, adapter, discovered, adapters := s.resolvePrimaryLSP(ctx, v, folder, language, false)
		language = lang
		detectedLanguage := detectedLabel(folder, language, discovered, s.store.Current())
		v.discoveredLangs = distinctLanguages(discovered)
		v.acquiredRoot = folder
		v.acquiredLanguage = language
		recordPinProvenance(v, origin, trigger, "")
		// Rehydrate strict-mode reads for this root (after a daemon restart) and
		// persist the pin, both scoped to the proxy session ID. No-ops when
		// persistence is off or this is not a serve-proxy connection.
		s.rehydrateReads(v.proxySessionID, folder, v.session.PersistState)
		s.persistPin(v.proxySessionID, folder, language, v.session.PersistState, origin)
		s.startQualityRunner(v, folder)
		s.startTopologyIndexer(v, folder)
		v.policy = s.buildPathPolicy(v)
		s.warmDepRoots(language)
		recoverWorkspaceTxlog(folder, func(ws string) { txlog.Scan(ws, s.daemonStartedAt, txlogReplayGuard(v.policy)) })
		cn, cv := v.clientName, v.clientVersion
		session.Patch(s.sessID, func(info *session.Info) {
			info.Folder = folder
			info.Language = language
			info.DetectedLanguage = detectedLanguage
			info.Adapter = adapter
			info.Adapters = adapters
			if cn != "" {
				info.ClientName = cn
				info.ClientVersion = cv
			}
		})
		s.log().Info("daemon: session attached", "root", folder, "language", language, "adapter", adapter)
	})
}

// attachSynthetic records a synthetic workspace root when pool.Detect fails.
// origin distinguishes an explicit session_start workspace arg naming a
// markerless folder (sticky and persisted, like any other explicit pin — issue
// #182's contract must not depend on the folder having a .git or language
// marker) from an incidental tool-path seed (PinSourceUnknown: not sticky,
// never persisted). trigger separates a live seed from rehydratePin's restore
// of a persisted synthetic pin, so the provenance label reads restore:… .
//
// A root that is or CONTAINS a home directory is refused here for every
// origin but an explicit session_start (undeclaredWideRootErr, issue #306):
// SynthesiseRoot refuses a home-IDENTITY seed on its own, but a CONTAINER of
// home synthesises to itself untouched, and this is where the pin's origin is
// in scope.
func (s *connSession) attachSynthetic(_ context.Context, root string, origin sessionstate.PinSource, trigger pinTrigger) {
	if err := undeclaredWideRootErr(root, origin); err != nil {
		s.log().Warn("daemon: refusing to pin a wide synthetic workspace root", "root", root, "origin", string(origin), "reason", err)
		return
	}
	s.mutate(func(v *sessionView) {
		if v.acquiredRoot != "" {
			return
		}
		v.acquiredRoot = root
		recordPinProvenance(v, origin, trigger, "")
		s.rehydrateReads(v.proxySessionID, root, v.session.PersistState)
		s.persistPin(v.proxySessionID, root, LanguageNone, v.session.PersistState, origin)
		s.startQualityRunner(v, root)
		s.startTopologyIndexer(v, root)
		v.policy = s.buildPathPolicy(v)
		recoverWorkspaceTxlog(root, func(ws string) { txlog.Scan(ws, s.daemonStartedAt, txlogReplayGuard(v.policy)) })
		cn, cv := v.clientName, v.clientVersion
		session.Patch(s.sessID, func(info *session.Info) {
			info.Folder = root
			info.Language = LanguageNone
			// detectedLabel, not detectAnyLanguageAt directly, so a home root
			// records the LSP-skip note instead of silence (issue #316).
			info.DetectedLanguage = detectedLabel(root, LanguageNone, nil, s.store.Current())
			info.Adapter = ""
			info.Synthetic = true
			if cn != "" {
				info.ClientName = cn
				info.ClientVersion = cv
			}
		})
		s.log().Info("daemon: session auto-attached (synthetic)", "root", root)
	})
}

// rootFromClient calls roots/list on the MCP client and resolves the first
// root URI to a workspace path via pool.Detect. When the client reports no
// usable root (no request channel, no roots, or an unusable URI) it falls back
// to the serve-proxy cwd hint — Detect-validated, so a hint outside any project
// still yields "".
func (s *connSession) rootFromClient(ctx context.Context) string {
	s.requestMu.RLock()
	req := s.clientRequest
	s.requestMu.RUnlock()
	if req != nil {
		if folder := paths.URIToPath(rootFromRoots(ctx, req, s.log())); folder != "" && folder != "/" {
			root, _, err := s.pool.Detect(folder)
			if err != nil {
				// Detect found no marker, so canonicalise the folder here instead:
				// this value is compared against peers' session.Folder, which the
				// pool resolved, and a raw spelling would silently hide every peer
				// in an aliased project (issue #263).
				return paths.Canonical(folder)
			}
			return root
		}
	}
	return s.rootFromHint()
}

// explicitOrAutoAttach reports whether root synthesis is permitted for a
// directory the detector could not place: an explicit session_start workspace
// arg always pins (even a markerless directory), and auto_attach is the opt-in
// that lets an incidental tool path synthesise a root.
func explicitOrAutoAttach(explicit, autoAttach bool) bool {
	return explicit || autoAttach
}

// onBeforeTool resolves the workspace root from the tool arguments when the
// session has no primary workspace yet. Applies auto-attach and auto-attach-
// persist when configured.
func (s *connSession) onBeforeTool(toolCtx context.Context, _ string, args json.RawMessage) {
	// Before the attached-already short circuit: an ALREADY attached session is
	// exactly the one whose primary can be stale after a live `enable-lsp`.
	// Generation-gated, so this is one atomic load on the steady-state path.
	s.refreshPrimaryIfStale(toolCtx)
	if s.view().acquiredRoot != "" {
		return
	}
	// A `workspace` arg is a deliberate pin (session_start); an incidental
	// file_path/path/uri is not. Only the former persists as the sticky target.
	//
	// The seed must BE the workspace argument, not merely accompany one.
	// seedPathFromArgs prefers uri/file_path/path/root OVER workspace, and tools
	// take both — relevant_memories and write_memory each accept a path AND a
	// workspace — so `{path: X, workspace: Y}` seeded X while presence alone
	// stamped it a deliberate declaration of X. That pinned a directory the
	// caller never named, made it STICKY, and persisted it, so the caller's real
	// session_start was then refused by the issue-#182 sticky guard and had to be
	// retried with force. Round 3 fixed one of this boolean's two consumers and
	// left this one; both now ask the same question.
	seedPath := seedPathFromArgs(args)
	origin := sessionstate.PinSourceUnknown
	explicit := seedIsWorkspaceArg(args, seedPath)
	if explicit {
		origin = sessionstate.PinSourceSessionStart
	}
	if !explicit {
		// On an unpinned connection, prefer restoring the last EXPLICIT pin over
		// seeding from whatever file a tool happens to touch — so reading a file in
		// another project by absolute path can never silently re-pin the connection
		// away from the workspace the caller actually chose.
		s.rehydratePin(toolCtx)
		// Next rung down: the serve-proxy cwd hint — still ahead of seeding from
		// whatever file this tool happens to touch, and Detect-validated, so it
		// can only land on a real project boundary.
		if s.view().acquiredRoot == "" {
			s.attachFromHint(toolCtx)
		}
		if s.view().acquiredRoot != "" {
			s.applyProjectConfig(s.workspace())
			s.startConfigWatcher()
			return
		}
	}
	if seedPath == "" {
		return
	}
	// A relative seed carries no location: os.Stat and pool.Detect would resolve it
	// against the daemon's working directory (inherited from whichever client
	// spawned the singleton daemon), so the connection could attach to — and then
	// write into — an unrelated project. Leave the session unattached; checkBoundary
	// refuses the call and tells the caller to pin a workspace.
	if !filepath.IsAbs(seedPath) {
		s.log().Warn("daemon: ignoring relative tool path as a workspace seed", "seed", seedPath)
		return
	}
	startDir := seedPath
	if info, err := os.Stat(seedPath); err != nil || !info.IsDir() {
		startDir = filepath.Dir(seedPath)
	}
	root, _, err := s.pool.Detect(startDir)
	if err != nil {
		// A deliberate session_start workspace arg always pins — even a markerless
		// directory — mirroring repinWorkspaceFrom's SynthesiseRoot fallback, so the
		// sticky contract is unconditional. auto_attach stays the opt-in that lets an
		// incidental tool path (file_path/path/uri) synthesise a root.
		if !explicitOrAutoAttach(explicit, s.store.Current().Workspace.AutoAttach) {
			s.log().Warn("daemon: cannot determine workspace root", "seed", "file://"+seedPath, "err", err)
			return
		}
		// Explicit only when the SEED IS the workspace argument — not merely when a
		// workspace key is present somewhere in the call.
		//
		// seedPathFromArgs prefers uri/file_path/path/root OVER workspace, so the
		// two routinely name different directories: relevant_memories and
		// write_memory both take a path AND a workspace. Keying on presence let
		// `{path: "~/.zshrc", workspace: "/some/project"}` seed $HOME while counting
		// as a deliberate declaration — laundering an incidental path into an
		// explicit $HOME pin that then PERSISTS as session_start and rehydrates on
		// every later reconnect, in the default configuration. Found by review, and
		// it is the same class this guard exists to close, one level up: the
		// declaration has to be about the directory being named.
		synthRoot := s.pool.SynthesiseRoot(startDir, explicit)
		if synthRoot == "" {
			s.log().Warn("daemon: refusing to synthesise a workspace at or above the home directory from an incidental tool path — call session_start({workspace}) to pin it deliberately", "seed", startDir)
			return
		}
		s.attachSynthetic(toolCtx, synthRoot, origin, pinTriggerLive)
		if s.store.Current().Workspace.AutoAttachPersist {
			go func() {
				if mkErr := materialisePlumbDir(synthRoot); mkErr != nil {
					s.log().Warn("daemon: failed to materialise .plumb/", "root", synthRoot, "err", mkErr)
					return
				}
				s.log().Info("daemon: materialised .plumb/ at synthetic root", "root", synthRoot)
			}()
		}
		s.applyProjectConfig(s.workspace())
		s.startConfigWatcher()
		return
	}
	s.attachWorkspacePin(toolCtx, "file://"+root, origin)
	s.applyProjectConfig(s.workspace())
	s.startConfigWatcher()
}

// appendActiveAdapter records a secondary language server as active for this
// session, so the sessions view lists every LSP the session is driving (like
// nvim's :LspInfo). Wired as routingProxy.onActivate; dedups and is a no-op for
// a language with no adapter. The primary is already recorded at attach time.
func (s *connSession) appendActiveAdapter(language string) {
	adp := adapterForLanguage(language)
	if adp == "" {
		return
	}
	session.Patch(s.sessID, func(in *session.Info) {
		if !slices.Contains(in.Adapters, adp) {
			in.Adapters = append(in.Adapters, adp)
		}
	})
}

// resolvePrimaryLSP acquires the language server for an attaching workspace and
// wires the session's routing proxy. For a root with its own detected language
// it acquires that as the primary. For a LanguageNone root it discovers child
// language roots (the monorepo case — core/build.zig + app/Package.swift under a
// bare .plumb/ root) and elects one as the connection primary; the rest attach
// lazily via routing and are surfaced for display + workspace_symbols fan-out.
//
// Returns the effective primary language (LanguageNone when nothing attached),
// the primary adapter name, the full discovered child set (nil for a normal
// single-language root), and the adapter list to seed the session record. repin
// selects resetPrimary (a deliberate workspace switch) over setPrimary (first
// attach). Must run inside the s.mutate lane: it writes v.lsRefRoot/lsRefLang.
func (s *connSession) resolvePrimaryLSP(ctx context.Context, v *sessionView, folder, language string, repin bool) (lang, adapter string, discovered []discoveredRoot, adapters []string) {
	// Wire the secondary-activation hook here, on every attach path, rather than
	// in bindPrimary: the LanguageNone exits below never reach bindPrimary, and
	// a primary-less session is exactly the one whose routed servers would
	// otherwise never reach the session record's Adapters list. Idempotent, and
	// noteActivated's workspace gate keeps it scoped to what serves this root.
	s.sessionProxy.setActivateHook(s.appendActiveAdapter)
	if language != LanguageNone {
		e, err := s.pool.acquireLang(ctx, folder, language, true)
		if err != nil {
			// LSP unavailable (binary not on PATH, crash, etc.) — degrade gracefully
			// rather than aborting. The workspace is still attached for filesystem
			// tools and stat tracking; LSP tools will surface their own errors.
			s.log().Error("daemon: acquire LS — attaching without LSP", "root", folder, "language", language, "err", err)
			s.sessionProxy.setDiscovered(folder, nil)
			return LanguageNone, "", nil, nil
		}
		s.bindPrimary(v, folder, language, e, repin)
		s.sessionProxy.setDiscovered(folder, nil)
		adp := adapterForLanguage(language)
		return language, adp, nil, adaptersFor(adp)
	}
	// LanguageNone: look for language roots in child subdirectories. Never scan
	// $HOME (a stray ~/.plumb must not trigger a full-home descent).
	if sameDirAs(folder, homeDirInfos()) {
		s.sessionProxy.setDiscovered(folder, nil)
		return LanguageNone, "", nil, nil
	}
	discovered = s.pool.discoverChildLanguages(folder, s.store.Current().Workspace.ChildScanDepth)
	if len(discovered) == 0 {
		// No strong-marker child roots. Last resort: content-sniff the root for a
		// language whose source files dominate (a .py repo with no manifest) and
		// attach it rooted at folder. Gated on the server being installed
		// (extLangAt → the effective p.langs set); a failed acquire degrades to
		// LanguageNone like any other.
		if sniffed := s.pool.extLangAt(folder); sniffed != "" {
			e, err := s.pool.acquireLang(ctx, folder, sniffed, true)
			if err == nil {
				s.bindPrimary(v, folder, sniffed, e, repin)
				s.sessionProxy.setDiscovered(folder, nil)
				adp := adapterForLanguage(sniffed)
				return sniffed, adp, nil, adaptersFor(adp)
			}
			s.log().Error("daemon: acquire sniffed language — attaching without LSP", "root", folder, "language", sniffed, "err", err)
		}
		s.sessionProxy.setDiscovered(folder, nil)
		return LanguageNone, "", nil, nil
	}
	primary := electPrimary(discovered)
	e, err := s.pool.acquireLang(ctx, primary.root, primary.language, true)
	if err != nil {
		// Surface the discovered languages for display even though the primary
		// server failed to start; the lazy routing path retries on first file.
		s.log().Error("daemon: acquire discovered primary — listing without LSP", "root", primary.root, "language", primary.language, "err", err)
		s.sessionProxy.setDiscovered(folder, discovered)
		return LanguageNone, "", discovered, adaptersForDiscovered(discovered)
	}
	s.bindPrimary(v, primary.root, primary.language, e, repin)
	s.sessionProxy.setDiscovered(folder, discovered)
	return primary.language, adapterForLanguage(primary.language), discovered, adaptersForDiscovered(discovered)
}

// bindPrimary wires an acquired primary entry into the session: routing proxy,
// invalidator, and the pinned LS reference (kept at the entry's OWN root — a
// discovered child root may sit below the workspace — for release symmetry on
// detach). repin uses resetPrimary (switch) instead of setPrimary (first-wins).
// The secondary-activation hook is wired by resolvePrimaryLSP on every path,
// including the LanguageNone ones that never arrive here.
func (s *connSession) bindPrimary(v *sessionView, root, language string, e *poolEntry, repin bool) {
	if repin {
		s.sessionProxy.resetPrimary(root, language, e.proxy)
		s.sessionInv.resetPrimary(root, language, e.inv)
	} else {
		s.sessionProxy.setPrimary(root, language, e.proxy)
		s.sessionInv.setPrimary(root, language, e.inv)
	}
	v.lsRefRoot = root
	v.lsRefLang = language
}

// homeLSPSkipNote is the DetectedLanguage-style label recorded when a session's
// workspace root IS the home directory: resolvePrimaryLSP skips all language
// discovery there by design, and before this label that skip was invisible —
// the pin succeeded (an explicit declaration, issue #182) with lang="none" and
// nothing naming the cause (issue #316).
const homeLSPSkipNote = "LSP skipped: the workspace root is the home directory"

// lspHomeSkipNote returns the orientation note naming why no language server
// is attached when the pinned workspace root IS the home directory —
// resolvePrimaryLSP skips all discovery there by design (a stray ~/.plumb must
// not trigger a full-home descent), and without this note the session_start
// identity block reported no language with nothing naming the cause (#316).
// Empty when a language IS attached (an explicit session_start language
// override can still acquire one at a home root), when no workspace is pinned,
// or when the root is not a home directory.
func (s *connSession) lspHomeSkipNote() string {
	if s.acquiredLanguageName() != "" {
		return ""
	}
	ws := s.workspace()
	if ws == "" || !sameDirAs(ws, homeDirInfos()) {
		return ""
	}
	return homeLSPSkipNote
}

// detectedLabel computes the session's DetectedLanguage display string: the
// comma-joined discovered languages for a monorepo root, else the primary
// language, else the LSP-skip note for a home root, else a best-effort marker
// scan when nothing attached.
func detectedLabel(folder, language string, discovered []discoveredRoot, cfg config.Config) string {
	switch {
	case len(discovered) > 0:
		return discoveredLabel(discovered)
	case language == LanguageNone:
		// Every detection walk stops at $HOME, so a home root would otherwise
		// always label "" — silent about why no server is attached. detectAnyLanguageAt
		// returns "" for a home folder by the same guard, so this branch changes
		// nothing for any other root.
		if sameDirAs(folder, homeDirInfos()) {
			return homeLSPSkipNote
		}
		return detectAnyLanguageAt(folder, cfg)
	default:
		return language
	}
}

// adaptersForDiscovered maps the discovered child languages to their adapter
// names, deduplicated and order-preserving, for the session's Adapters list.
func adaptersForDiscovered(ds []discoveredRoot) []string {
	var out []string
	for _, d := range ds {
		if adp := adapterForLanguage(d.language); adp != "" && !slices.Contains(out, adp) {
			out = append(out, adp)
		}
	}
	return out
}

// distinctLanguages returns the sorted, deduplicated set of languages across
// the discovered child roots (two subdirs naming the same language collapse to
// one). nil-safe — returns nil for an empty/nil slice (a single-language root).
func distinctLanguages(ds []discoveredRoot) []string {
	var langs []string
	for _, d := range ds {
		if !slices.Contains(langs, d.language) {
			langs = append(langs, d.language)
		}
	}
	sort.Strings(langs)
	return langs
}

// discoveredLabel joins the distinct discovered language names for the
// DetectedLanguage display string, e.g. "swift, zig".
func discoveredLabel(ds []discoveredRoot) string {
	return strings.Join(distinctLanguages(ds), ", ")
}

// adaptersFor seeds the active-adapter list with the primary adapter, or nil
// when the session attached without LSP.
func adaptersFor(adapter string) []string {
	if adapter == "" {
		return nil
	}
	return []string{adapter}
}
