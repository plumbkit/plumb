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

// TestSettings_RedactsSemanticsAPIKey verifies the GET /api/settings response
// never echoes the configured semantics.api_key — it is masked to the redacted
// sentinel instead. Regression test for the secret-leak finding (web-1).
func TestSettings_RedactsSemanticsAPIKey(t *testing.T) {
	cfg := config.Defaults()
	cfg.Semantics.APIKey = "sk-supersecret-1234"
	store := config.NewStore(cfg)

	srv := New(Deps{Store: store, StartedAt: time.Now()})
	srv.token = "tok"
	srv.addr = "127.0.0.1:8870"
	h := srv.routes()

	w := authedGet(h, "/api/settings")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "sk-supersecret-1234") {
		t.Fatal("GET /api/settings leaked the configured semantics.api_key in its response body")
	}

	var got settingsDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var row settingRowDTO
	for _, r := range got.Scopes[0].Rows {
		if r.Key == "semantics.api_key" {
			row = r
		}
	}
	if row.Key == "" {
		t.Fatal("semantics.api_key row missing from global settings")
	}
	if row.Value != redactedSecret {
		t.Errorf("semantics.api_key value = %v, want redacted sentinel %q", row.Value, redactedSecret)
	}
}

// TestSettingsSet_RedactedSentinelIsNoOp verifies that POSTing the mask sentinel
// back for a secret field does not overwrite the stored credential (the GET masks
// the value, so a blind re-submit must not clobber it).
func TestSettingsSet_RedactedSentinelIsNoOp(t *testing.T) {
	cfg := config.Defaults()
	cfg.Semantics.APIKey = "keep-me"
	store := config.NewStore(cfg)
	srv := New(Deps{Store: store, StartedAt: time.Now()})

	body := `{"scope":"global","key":"semantics.api_key","value":"` + redactedSecret + `"}`
	r := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	// Call the handler directly to exercise the guard without the auth middleware.
	srv.handleSettingsSet(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if store.Current().Semantics.APIKey != "keep-me" {
		t.Errorf("redacted sentinel overwrote stored api_key: got %q, want unchanged %q",
			store.Current().Semantics.APIKey, "keep-me")
	}
}
