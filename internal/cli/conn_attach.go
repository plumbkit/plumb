package cli

// conn_attach.go — workspace attach, re-pin, and language detection.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools/txlog"
)

// attachWorkspace resolves rootURI to a project root, acquires the shared
// language server if needed, and updates the session record.
func (s *connSession) attachWorkspace(ctx context.Context, rootURI string) {
	folder := strings.TrimPrefix(rootURI, "file://")
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
		s.startQualityRunner(v, folder)
		s.startTopologyIndexer(v, folder)
		v.policy = s.buildPathPolicy(v)
		s.warmDepRoots(language)
		recoverWorkspaceTxlog(folder, txlog.Scan)
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
func (s *connSession) attachSynthetic(_ context.Context, root string) {
	s.mutate(func(v *sessionView) {
		if v.acquiredRoot != "" {
			return
		}
		v.acquiredRoot = root
		s.startQualityRunner(v, root)
		s.startTopologyIndexer(v, root)
		v.policy = s.buildPathPolicy(v)
		recoverWorkspaceTxlog(root, txlog.Scan)
		cn, cv := v.clientName, v.clientVersion
		session.Patch(s.sessID, func(info *session.Info) {
			info.Folder = root
			info.Language = LanguageNone
			info.DetectedLanguage = detectAnyLanguageAt(root, s.store.Current())
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

// repinWorkspace deliberately switches the connection to a different workspace.
// Unlike attachWorkspace (idempotent, first-wins — the safe default for
// auto-resolution), this is driven only by an explicit session_start workspace
// argument: an unambiguous declaration of intent. It tears down the previous
// workspace's per-session subsystems (quality runner, topology store, LSP
// routing) and re-attaches the new root, so a connection reused across
// conversations (e.g. Claude Desktop) is no longer permanently welded to the
// first project it touched. The ad-hoc boundary guard on other path tools is
// unaffected — only this deliberate bootstrap call re-pins.
//
// folder may be any absolute path inside the target project. It is resolved to
// a workspace root via pool.Detect; when no marker is found the folder itself
// becomes the workspace (SynthesiseRoot), so an explicit pin always succeeds.
// Returns the resolved root.
//
// langOverride, when a non-empty active language, forces the primary language
// instead of the detected one — for an ambiguous project (e.g. an Xcode app with
// no SwiftPM Package.swift) where the agent knows the language detection cannot
// infer. An unknown or inactive override is ignored (detection wins), so a typo
// or an uninstalled server never breaks the pin.
func (s *connSession) repinWorkspace(ctx context.Context, folder, langOverride string) (string, error) {
	folder = strings.TrimPrefix(folder, "file://")
	if folder == "" || folder == "/" {
		return "", fmt.Errorf("repin: empty workspace path %q", folder)
	}
	root, language, err := s.pool.Detect(folder)
	if err != nil {
		// No .plumb/marker/.git found — the folder itself becomes the workspace.
		root = s.pool.SynthesiseRoot(folder)
		language = LanguageNone
	}
	if langOverride != "" && s.pool.hasActiveLanguage(langOverride) {
		language = langOverride
	}
	if s.attachOrRepinTo(ctx, root, language) {
		s.applyProjectConfig(root)
	}
	return root, nil
}

// onRootsChanged applies a client's updated workspace roots (the
// notifications/roots/list_changed path). On the first attach it pins the root,
// like OnInit. When the connection is already pinned and the client reports a
// different root — an editor that genuinely switched folders — it re-pins to
// follow the switch, closing the same "welded connection" gap that the
// session_start re-pin fixed for clients that never report roots (Claude
// Desktop). An empty or unchanged root is left alone: repinWorkspace no-ops when
// the resolved root matches the current pin, so a spurious notification (or a
// roots/list the client cannot satisfy) never tears the workspace down.
func (s *connSession) onRootsChanged(ctx context.Context, rootURI string) {
	if s.view().acquiredRoot == "" {
		s.attachWorkspace(ctx, rootURI)
		s.applyProjectConfig(s.workspace())
		return
	}
	folder := strings.TrimPrefix(rootURI, "file://")
	if folder == "" || folder == "/" {
		return // client reported no usable root — keep the current pin
	}
	if _, err := s.repinWorkspace(ctx, folder, ""); err != nil {
		s.log().Warn("daemon: roots-changed re-pin failed", "to", folder, "err", err)
	}
}

// attachOrRepinTo points the connection at root, tearing down any previous
// workspace's per-session subsystems first so the start* helpers (which no-op
// when already started) re-create them for the new root. Returns true when the
// root actually changed (false on a no-op re-pin to the same root). language is
// the LSP language for root, or LanguageNone. The whole teardown-and-reattach
// runs under the one mutation lane so readers never see a half-switched view.
func (s *connSession) attachOrRepinTo(ctx context.Context, root, language string) bool {
	changed := false
	s.mutate(func(v *sessionView) {
		prev := v.acquiredRoot
		// No-op only when neither the root NOR the primary language changes; a
		// same-root language switch (a forced primary via session_start) must still
		// re-acquire the new server.
		if root == prev && language == v.acquiredLanguage {
			return
		}
		changed = true
		// The pinned LS reference (if any) for the workspace we are leaving;
		// released at the end once the new root is acquired, so the pool can reclaim
		// the old server after its idle grace if no other session holds it.
		prevRef := v.lsRefRoot
		prevRefLang := v.lsRefLang
		v.lsRefRoot = ""
		v.lsRefLang = ""
		if v.qualityRunner != nil {
			v.qualityRunner.Stop()
			v.qualityRunner = nil
		}
		v.topologyStore = nil // pool stores are daemon-lifetime and shared; just re-Acquire
		// Per-session read/write tracking is workspace-relative: plumb has read and
		// written nothing in the new project yet, so the dirty-guard and strict-mode
		// read check must start clean rather than inherit the old root's paths.
		s.readTracker.Reset()
		s.writeTracker.Reset()
		s.undoStore.Reset()
		s.clearHintSeen()

		lang, adapter, discovered, adapters := s.resolvePrimaryLSP(ctx, v, root, language, true)
		language = lang
		// Acquire-before-release: the new root is pinned above before we drop the
		// old one, so even a re-pin back to a recently-left root never races teardown.
		if prevRef != "" {
			s.pool.release(prevRef, prevRefLang)
		}
		detectedLanguage := detectedLabel(root, language, discovered, s.store.Current())
		v.discoveredLangs = distinctLanguages(discovered)
		v.acquiredRoot = root
		v.acquiredLanguage = language
		v.lastCfgMtime = time.Time{}
		s.startQualityRunner(v, root)
		s.startTopologyIndexer(v, root)
		v.policy = s.buildPathPolicy(v)
		s.warmDepRoots(language)
		recoverWorkspaceTxlog(root, txlog.Scan)
		cn, cv := v.clientName, v.clientVersion
		session.Patch(s.sessID, func(info *session.Info) {
			info.Folder = root
			info.Language = language
			info.DetectedLanguage = detectedLanguage
			info.Adapter = adapter
			info.Adapters = adapters
			info.Synthetic = false
			info.Health = ""
			info.HealthMessage = ""
			if cn != "" {
				info.ClientName = cn
				info.ClientVersion = cv
			}
		})
		s.log().Info("daemon: session re-pinned", "from", prev, "to", root, "language", language, "adapter", adapter)
	})
	return changed
}

// rootFromClient calls roots/list on the MCP client and resolves the first
// root URI to a workspace path via pool.Detect.
func (s *connSession) rootFromClient(ctx context.Context) string {
	s.requestMu.RLock()
	req := s.clientRequest
	s.requestMu.RUnlock()
	if req == nil {
		return ""
	}
	uri := rootFromRoots(ctx, req)
	if uri == "" {
		return ""
	}
	folder := strings.TrimPrefix(uri, "file://")
	if folder == "" || folder == "/" {
		return ""
	}
	root, _, err := s.pool.Detect(folder)
	if err != nil {
		return folder
	}
	return root
}

// onBeforeTool resolves the workspace root from the tool arguments when the
// session has no primary workspace yet. Applies auto-attach and auto-attach-
// persist when configured.
func (s *connSession) onBeforeTool(toolCtx context.Context, _ string, args json.RawMessage) {
	if s.view().acquiredRoot != "" {
		return
	}
	seedPath := seedPathFromArgs(args)
	if seedPath == "" {
		return
	}
	startDir := seedPath
	if info, err := os.Stat(seedPath); err != nil || !info.IsDir() {
		startDir = filepath.Dir(seedPath)
	}
	root, _, err := s.pool.Detect(startDir)
	if err != nil {
		if !s.store.Current().Workspace.AutoAttach {
			s.log().Warn("daemon: cannot determine workspace root", "seed", "file://"+seedPath, "err", err)
			return
		}
		synthRoot := s.pool.SynthesiseRoot(startDir)
		s.attachSynthetic(toolCtx, synthRoot)
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
	s.attachWorkspace(toolCtx, "file://"+root)
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
	if sameDirAs(folder, homeFileInfo()) {
		s.sessionProxy.setDiscovered(folder, nil)
		return LanguageNone, "", nil, nil
	}
	discovered = s.pool.discoverChildLanguages(folder, s.store.Current().Workspace.ChildScanDepth)
	if len(discovered) == 0 {
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
// invalidator, the secondary-activation hook, and the pinned LS reference (kept
// at the entry's OWN root — a discovered child root may sit below the workspace
// — for release symmetry on detach). repin uses resetPrimary (switch) instead
// of setPrimary (first-wins).
func (s *connSession) bindPrimary(v *sessionView, root, language string, e *poolEntry, repin bool) {
	if repin {
		s.sessionProxy.resetPrimary(root, language, e.proxy)
		s.sessionInv.resetPrimary(root, language, e.inv)
	} else {
		s.sessionProxy.setPrimary(root, language, e.proxy)
		s.sessionInv.setPrimary(root, language, e.inv)
	}
	s.sessionProxy.setActivateHook(s.appendActiveAdapter)
	v.lsRefRoot = root
	v.lsRefLang = language
}

// detectedLabel computes the session's DetectedLanguage display string: the
// comma-joined discovered languages for a monorepo root, else the primary
// language, else a best-effort marker scan when nothing attached.
func detectedLabel(folder, language string, discovered []discoveredRoot, cfg config.Config) string {
	switch {
	case len(discovered) > 0:
		return discoveredLabel(discovered)
	case language == LanguageNone:
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

func adapterForLanguage(language string) string {
	switch language {
	case "go":
		return "gopls"
	case "python":
		return "pyright"
	case "java":
		return "jdtls"
	case "rust":
		return "rust-analyzer"
	case "swift":
		return "sourcekit-lsp"
	case "zig":
		return "zls"
	case "typescript", "javascript":
		return "typescript-language-server"
	case "kotlin":
		return "kotlin-language-server"
	case "html":
		return "vscode-html-language-server"
	default:
		return ""
	}
}

func detectAnyLanguageAt(dir string, cfg config.Config) string {
	langs := make([]string, 0, len(cfg.LSP))
	for name, lspCfg := range cfg.LSP {
		if len(lspCfg.RootMarkers) > 0 {
			langs = append(langs, name)
		}
	}
	sort.Slice(langs, func(i, j int) bool {
		if langs[i] == "go" {
			return true
		}
		if langs[j] == "go" {
			return false
		}
		return langs[i] < langs[j]
	})
	homeInfo := homeFileInfo()
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		// Stop at $HOME, mirroring the pool's Detect/detectLanguageAt walks: a stray
		// marker in the home directory (e.g. a global ~/package.json) must not be
		// reported as the detected language for a workspace beneath it.
		if sameDirAs(d, homeInfo) {
			return ""
		}
		for _, name := range langs {
			for _, marker := range cfg.LSP[name].RootMarkers {
				if markerPresent(d, marker) {
					return name
				}
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
	}
}
