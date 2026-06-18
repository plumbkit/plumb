// Package web hosts plumb's opt-in, loopback-only web UI inside the daemon
// process. It is never bound until `plumb web` asks (over the control socket),
// always listens on 127.0.0.1 only, and gates every request behind a per-start
// token. The HTTP layer reuses plumb's existing read paths (internal/stats,
// internal/monitor, internal/session, internal/topology, internal/memory) and
// the live config store, so it never reimplements domain logic — handlers are
// thin parse → read → present orchestrators, mirroring the MCP tool pattern.
//
// Concurrency: a Server is safe for concurrent use. Start/Stop are guarded by a
// mutex; the running *http.Server and token are read under that lock. Handlers
// run concurrently on the net/http pool and touch only read-only stores and the
// concurrency-safe config.Store.
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// Deps are the read handles and paths the web Server needs. They are all
// daemon-owned singletons resolved once at daemon start; the Server keeps them
// for the daemon's lifetime. Only Store is a write path (config + theme writes
// reload through it).
type Deps struct {
	// Store is the live global config store. Read for the configured port and
	// theme; written by the settings/theme endpoints, which then Reload it.
	Store *config.Store
	// MetricsPath is the daemon metrics snapshot file (monitor.SnapshotPath()).
	MetricsPath string
	// LogPath is the daemon log file, tailed by the logs SSE endpoint.
	LogPath string
	// StartedAt is the daemon start time, for the dashboard uptime figure.
	StartedAt time.Time
}

// Server owns the loopback HTTP listener and its lifecycle. It is constructed
// once at daemon start (unbound) and Start/Stop'd on demand over the control
// socket.
type Server struct {
	deps Deps

	mu    sync.Mutex
	http  *http.Server
	ln    net.Listener
	token string
	addr  string // "127.0.0.1:<port>" once bound
}

// New constructs an unbound Server. It does not listen until Start is called.
func New(deps Deps) *Server {
	return &Server{deps: deps}
}

// Start binds the loopback listener (if not already bound) and returns the URL
// a browser should open, including the one-shot token query parameter. Calling
// Start while already running returns the existing URL — `plumb web` run twice
// just re-prints the address. The port is taken from config unless portOverride
// is non-zero.
func (s *Server) Start(portOverride int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.http != nil {
		return s.urlLocked(), nil
	}

	port := portOverride
	if port == 0 {
		port = s.deps.Store.Current().Web.Port
	}
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("invalid web port %d (expected 1–65535)", port)
	}

	tok, err := mintToken()
	if err != nil {
		return "", fmt.Errorf("minting web token: %w", err)
	}

	// Loopback bind only — never a routable address.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", fmt.Errorf("binding web listener on 127.0.0.1:%d: %w", port, err)
	}

	s.token = tok
	s.ln = ln
	s.addr = ln.Addr().String()
	if err := writeTokenFile(tok, s.addr); err != nil {
		slog.Warn("web: could not persist token file", "err", err)
	}

	srv := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.http = srv

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("web: server stopped", "err", err)
		}
	}()

	slog.Info("web: listening", "addr", s.addr)
	return s.urlLocked(), nil
}

// Status reports whether the web server is bound and, if so, its URL.
func (s *Server) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http == nil {
		return "stopped"
	}
	return "running " + s.urlLocked()
}

// Stop gracefully shuts the listener down. It is safe to call when not running.
func (s *Server) Stop() error {
	s.mu.Lock()
	srv := s.http
	s.http = nil
	s.ln = nil
	s.token = ""
	addr := s.addr
	s.addr = ""
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	removeTokenFile()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutting down web server: %w", err)
	}
	slog.Info("web: stopped", "addr", addr)
	return nil
}

// Close stops the server during daemon shutdown. It mirrors Stop but never errs
// to the caller — it is wired as a defer.
func (s *Server) Close() {
	_ = s.Stop()
}

// urlLocked builds the browser URL with the token query param. Caller holds mu.
func (s *Server) urlLocked() string {
	return fmt.Sprintf("http://%s/?t=%s", s.addr, s.token)
}
