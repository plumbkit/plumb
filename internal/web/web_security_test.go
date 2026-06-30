package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSameOrigin_AcceptsLoopbackEquivalents verifies the CSRF guard treats any
// loopback host on the bound port as same-origin (so the UI opened via
// http://localhost:<port> works) while a foreign origin or wrong port is
// rejected. Regression test for web-3.
func TestSameOrigin_AcceptsLoopbackEquivalents(t *testing.T) {
	s, _ := newTestServer(t) // addr 127.0.0.1:8870
	cases := []struct {
		origin string
		want   bool
	}{
		{"http://127.0.0.1:8870", true},
		{"http://localhost:8870", true},
		{"http://[::1]:8870", true},
		{"http://127.0.0.1:9999", false},
		{"http://evil.example:8870", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/api/settings", nil)
		r.Header.Set("Origin", c.origin)
		if got := s.sameOrigin(r); got != c.want {
			t.Errorf("origin %q: sameOrigin = %v, want %v", c.origin, got, c.want)
		}
	}
}

// TestSettingsSet_RejectsUnknownWorkspaceScope verifies a project-scope settings
// write to a path that is not a currently-attached workspace is refused, so it
// cannot drop a .plumb/config.toml at an arbitrary location. Regression for
// web-2.
func TestSettingsSet_RejectsUnknownWorkspaceScope(t *testing.T) {
	s := &Server{}
	body := `{"scope":"/tmp/definitely-not-an-active-workspace","key":"edits.strict","value":true}`
	r := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSettingsSet(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown workspace scope") {
		t.Errorf("body = %q, want an unknown-scope error", w.Body.String())
	}
}

// TestReadEndpoints_RejectUnknownWorkspace is the read-side counterpart to
// TestSettingsSet_RejectsUnknownWorkspaceScope: handleTopology / handleMemoryList
// must not read an arbitrary on-disk path's index or memories via an unvalidated
// ?workspace= param. With the requested path not an active workspace, the read is
// refused with 400 rather than resolved.
func TestReadEndpoints_RejectUnknownWorkspace(t *testing.T) {
	s := &Server{}
	const bad = "/tmp/definitely-not-an-active-workspace"
	cases := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"topology", s.handleTopology},
		{"memoryList", s.handleMemoryList},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/x?workspace="+bad, nil)
			w := httptest.NewRecorder()
			tc.handler(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (an unknown workspace must be rejected, not read)", w.Code)
			}
			if !strings.Contains(w.Body.String(), "unknown workspace") {
				t.Errorf("body = %q, want an unknown-workspace error", w.Body.String())
			}
		})
	}
}
