package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// newTestServer builds a Server backed by a default config store and a token, so
// handler tests can drive the routes without binding a socket.
func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	store := config.NewStore(config.Defaults())
	s := New(Deps{Store: store, StartedAt: time.Now()})
	s.token = "tok"
	s.addr = "127.0.0.1:8870"
	return s, s.routes()
}

func authedGet(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path+"?t=tok", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	h.ServeHTTP(w, r)
	return w
}

func TestSPA_ServesIndexAndFallback(t *testing.T) {
	_, h := newTestServer(t)

	// Root serves the placeholder index.html.
	w := authedGet(h, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "plumb web") {
		t.Error("index.html body missing expected content")
	}

	// Unknown SPA route falls back to index.html (client-side routing).
	w = authedGet(h, "/sessions")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /sessions status = %d, want 200 (SPA fallback)", w.Code)
	}
}

func TestThemeEndpoint_ReturnsPalette(t *testing.T) {
	_, h := newTestServer(t)
	w := authedGet(h, "/api/theme")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got themeDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "plumb" {
		t.Errorf("theme name = %q, want plumb", got.Name)
	}
	if got.Palette.Acc == "" {
		t.Error("palette accent is empty")
	}
	if len(got.Names) == 0 {
		t.Error("theme names list is empty")
	}
}

func TestSettingsEndpoint_HasGlobalScopeAndWebPort(t *testing.T) {
	_, h := newTestServer(t)
	w := authedGet(h, "/api/settings")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got settingsDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Scopes) == 0 || !got.Scopes[0].Global {
		t.Fatal("expected a Global scope first")
	}
	var found settingRowDTO
	for _, row := range got.Scopes[0].Rows {
		if row.Key == "web.port" {
			found = row
		}
	}
	if found.Key == "" {
		t.Fatal("web.port row missing from global settings")
	}
	if found.ReloadTier != "next-session" {
		t.Errorf("web.port reloadTier = %q, want next-session", found.ReloadTier)
	}
	// web.port is global-only — it must not be project-overridable.
	if projectOverridable("web.port") {
		t.Error("web.port should be global-only")
	}
}

func TestDashboardEndpoint_OK(t *testing.T) {
	_, h := newTestServer(t)
	w := authedGet(h, "/api/dashboard")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got dashboardDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.UptimeSeconds < 0 {
		t.Error("negative uptime")
	}
}

func TestFlattenConfig_ResolvesDottedKeys(t *testing.T) {
	flat := flattenConfig(config.Defaults())
	if flat["web.port"] != int64(8870) {
		t.Errorf("web.port = %v (%T), want 8870", flat["web.port"], flat["web.port"])
	}
	if flat["ui.theme"] != "plumb" {
		t.Errorf("ui.theme = %v, want plumb", flat["ui.theme"])
	}
}

func TestStartStop_BindsLoopback(t *testing.T) {
	store := config.NewStore(config.Defaults())
	s := New(Deps{Store: store, StartedAt: time.Now()})
	// Port 0 from config default would be 8870; override to 0 is invalid, so use a
	// high port unlikely to clash. If it is taken the test skips.
	url, err := s.Start(38871)
	if err != nil {
		t.Skipf("could not bind test port: %v", err)
	}
	defer func() { _ = s.Stop() }()

	if !strings.HasPrefix(url, "http://127.0.0.1:38871/?t=") {
		t.Errorf("url = %q, want loopback URL with token", url)
	}
	if !strings.Contains(s.Status(), "running") {
		t.Errorf("status = %q, want running", s.Status())
	}

	// A real request with the minted token succeeds.
	tok := strings.TrimPrefix(url, "http://127.0.0.1:38871/?t=")
	resp, err := http.Get("http://127.0.0.1:38871/api/theme?t=" + tok) //nolint:noctx,bodyclose
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authed request status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if s.Status() != "stopped" {
		t.Errorf("status after stop = %q, want stopped", s.Status())
	}
}
