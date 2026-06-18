package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:54321": true,
		"[::1]:8080":      true,
		"192.168.1.5:80":  false,
		"10.0.0.1:443":    false,
	}
	for addr, want := range cases {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestRequestToken_QueryThenCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?t=abc", nil)
	if got := requestToken(r); got != "abc" {
		t.Fatalf("query token = %q, want abc", got)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	r2.AddCookie(&http.Cookie{Name: tokenCookie, Value: "xyz"})
	if got := requestToken(r2); got != "xyz" {
		t.Fatalf("cookie token = %q, want xyz", got)
	}
}

func TestAuthMiddleware_RejectsMissingToken(t *testing.T) {
	s := &Server{token: "secret", addr: "127.0.0.1:8870"}
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	// No token → 401.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", w.Code)
	}

	// Correct token in query → passes, and sets the cookie.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/dashboard?t=secret", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Fatalf("valid token: status = %d, want 418", w.Code)
	}
	if len(w.Result().Cookies()) == 0 {
		t.Error("valid token did not set the auth cookie")
	}
}

func TestAuthMiddleware_RejectsNonLoopback(t *testing.T) {
	s := &Server{token: "secret", addr: "127.0.0.1:8870"}
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/dashboard?t=secret", nil)
	r.RemoteAddr = "192.168.1.20:5000"
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback: status = %d, want 403", w.Code)
	}
}

func TestAuthMiddleware_CSRFOnPost(t *testing.T) {
	s := &Server{token: "secret", addr: "127.0.0.1:8870"}
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Cross-origin POST → 403.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/theme?t=secret", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("Origin", "http://evil.example")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: status = %d, want 403", w.Code)
	}

	// Same-origin POST → passes.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/api/theme?t=secret", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("Origin", "http://127.0.0.1:8870")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("same-origin POST: status = %d, want 200", w.Code)
	}
}

func TestMintToken_UniqueAndHex(t *testing.T) {
	a, err := mintToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := mintToken()
	if a == b {
		t.Fatal("mintToken returned identical tokens")
	}
	if len(a) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(a))
	}
}
