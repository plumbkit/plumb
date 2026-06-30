package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
)

// tokenCookie is the name of the HttpOnly cookie that carries the session token
// after the first token-bearing load.
const tokenCookie = "plumb_web_token"

// mintToken returns a fresh 256-bit hex token.
func mintToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// tokenFilePath is where the active token + address are mirrored (mode 0600) so
// a later `plumb web` invocation can re-print the URL without re-minting.
func tokenFilePath() string {
	return filepath.Join(config.CacheDir(), "plumb.web.token")
}

func writeTokenFile(token, addr string) error {
	dir := config.CacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(tokenFilePath(), []byte(addr+"\n"+token+"\n"), 0o600)
}

func removeTokenFile() {
	_ = os.Remove(tokenFilePath())
}

// authMiddleware enforces the security model on every request: loopback origin,
// a valid token (query param on first load, then HttpOnly cookie), and an
// Origin/Host CSRF check on state-changing methods.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "forbidden: loopback only", http.StatusForbidden)
			return
		}

		s.mu.Lock()
		want := s.token
		s.mu.Unlock()
		if want == "" {
			http.Error(w, "web server not ready", http.StatusServiceUnavailable)
			return
		}

		got := requestToken(r)
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}

		// First load carries the token in the query string; set it as an
		// HttpOnly cookie so subsequent same-origin requests authenticate without
		// leaking the token into the address bar history of every link.
		if r.URL.Query().Get("t") != "" {
			// Secure is intentionally omitted: the server is loopback-only plain
			// HTTP (no TLS on 127.0.0.1), and a Secure cookie would never be sent
			// back. HttpOnly + SameSite=Strict still apply.
			http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: loopback HTTP — Secure cookie would never be returned
				Name:     tokenCookie,
				Value:    want,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
		}

		// CSRF: state-changing requests must originate from our own loopback
		// address. A cross-site form post carries a foreign Origin (or none).
		if isStateChanging(r.Method) && !s.sameOrigin(r) {
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requestToken extracts the token from the query param (first load) or the
// cookie (subsequent requests).
func requestToken(r *http.Request) string {
	if t := r.URL.Query().Get("t"); t != "" {
		return t
	}
	if c, err := r.Cookie(tokenCookie); err == nil {
		return c.Value
	}
	return ""
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// sameOrigin verifies the request's Origin (or, absent that, its Host) matches
// the server's own loopback address on the bound port — the CSRF guard for
// write endpoints. Any loopback host (127.0.0.1, localhost, ::1) is accepted on
// that port, so opening the UI via http://localhost:<port> is not rejected as
// cross-origin while a genuine foreign origin still is.
func (s *Server) sameOrigin(r *http.Request) bool {
	s.mu.Lock()
	addr := s.addr
	s.mu.Unlock()
	_, wantPort, err := net.SplitHostPort(addr)
	if err != nil {
		wantPort = addr
	}
	matches := func(hostport string) bool {
		host, port, err := net.SplitHostPort(hostport)
		if err != nil {
			return hostport == addr
		}
		return port == wantPort && isLoopbackHost(host)
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		host := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
		return matches(host)
	}
	// No Origin header (some same-origin fetches omit it): fall back to Host.
	return matches(r.Host)
}

// isLoopbackHost reports whether host is a loopback name or IP (localhost,
// 127.0.0.0/8, ::1).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopback reports whether remoteAddr is a loopback peer. The listener already
// binds 127.0.0.1, so this is defence in depth.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
