package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// TestInjectAllowDirs_RoundTrip proves the allow-dirs the proxy folds into the
// initialize frame's _meta are exactly what the daemon-side parser reads back,
// and that the rest of the frame (jsonrpc/id/method/clientInfo) is preserved.
func TestInjectAllowDirs_RoundTrip(t *testing.T) {
	t.Parallel()
	const frame = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"acme","version":"3.2"},"capabilities":{}}}`
	dirs := []string{"/work/extra", "/srv/data"}

	out := injectAllowDirs([]byte(frame), dirs)

	// The envelope must survive untouched.
	e := parseEnvelope(out)
	if e.Method != "initialize" || idKey(e.ID) != "1" {
		t.Fatalf("envelope mangled: method=%q id=%q", e.Method, idKey(e.ID))
	}
	// clientInfo must still be readable by the existing parser path.
	var p struct {
		Params struct {
			ClientInfo struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("unmarshal augmented frame: %v", err)
	}
	if p.Params.ClientInfo.Name != "acme" {
		t.Fatalf("clientInfo lost: %q", p.Params.ClientInfo.Name)
	}

	// The injected _meta key must carry the exact dirs the daemon will read back
	// (the daemon-side parser is covered by mcp's own TestAllowDirsFromParams).
	var meta struct {
		Params struct {
			Meta map[string][]string `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		t.Fatal(err)
	}
	got := meta.Params.Meta[mcp.MetaAllowDirsKey]
	if len(got) != len(dirs) || got[0] != dirs[0] || got[1] != dirs[1] {
		t.Fatalf("round-trip mismatch: got %v want %v", got, dirs)
	}
}

// TestInjectAllowDirs_EmptyNoChange guarantees zero behaviour change when no
// allow-dir is granted: the frame is returned byte-for-byte.
func TestInjectAllowDirs_EmptyNoChange(t *testing.T) {
	t.Parallel()
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if got := injectAllowDirs(frame, nil); string(got) != string(frame) {
		t.Fatalf("empty allow-dirs must not change the frame:\n got %s\nwant %s", got, frame)
	}
}

// TestInjectAllowDirs_PreservesExistingMeta ensures an existing _meta entry is
// kept alongside the injected key.
func TestInjectAllowDirs_PreservesExistingMeta(t *testing.T) {
	t.Parallel()
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"keep":"me"}}}`)
	out := injectAllowDirs(frame, []string{"/x"})
	var got struct {
		Params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Params.Meta["keep"]; !ok {
		t.Fatalf("existing _meta key dropped: %v", got.Params.Meta)
	}
	if _, ok := got.Params.Meta[mcp.MetaAllowDirsKey]; !ok {
		t.Fatalf("allow-dirs key not injected: %v", got.Params.Meta)
	}
}

// TestInjectAllowDirs_MalformedFrameUnchanged proves the injector never
// corrupts a frame it cannot parse — it returns it verbatim.
func TestInjectAllowDirs_MalformedFrameUnchanged(t *testing.T) {
	t.Parallel()
	bad := []byte(`not json`)
	if got := injectAllowDirs(bad, []string{"/x"}); string(got) != string(bad) {
		t.Fatalf("malformed frame must pass through unchanged: %s", got)
	}
}

func TestResolveAllowDirs(t *testing.T) {
	t.Setenv("PLUMB_ALLOWDIR_TESTVAR", "/expanded")
	cwd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}

	got := resolveAllowDirs(
		[]string{"  ", "$PLUMB_ALLOWDIR_TESTVAR/sub", "rel/path"},
		"",
	)
	want := []string{"/expanded/sub", filepath.Join(cwd, "rel/path")}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestResolveAllowDirs_EnvListSeparator(t *testing.T) {
	env := "/a" + string(filepath.ListSeparator) + "/b"
	got := resolveAllowDirs(nil, env)
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("env split failed: %v", got)
	}
}

func TestResolveAllowDirs_EmptyIsNil(t *testing.T) {
	if got := resolveAllowDirs(nil, ""); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

// TestAllowDir_SurvivesHandshakeReplay is the load-bearing end-to-end check: the
// allow-dir must reach the daemon on the INITIAL connect AND again after a
// transparent reconnect, because the proxy replays the captured initialize
// frame. Both daemons (the one that crashes and its replacement) must observe
// the _meta-carried grant.
func TestAllowDir_SurvivesHandshakeReplay(t *testing.T) {
	t.Parallel()
	const grant = "/granted/dir"

	carriesGrant := func(frame []byte) bool {
		var p struct {
			Params struct {
				Meta map[string][]string `json:"_meta"`
			} `json:"params"`
		}
		if err := json.Unmarshal(frame, &p); err != nil {
			return false
		}
		dirs := p.Params.Meta[mcp.MetaAllowDirsKey]
		return len(dirs) == 1 && dirs[0] == grant
	}

	initialInit := make(chan bool, 4)
	replInit := make(chan bool, 4)

	_, initialProxySide := newPipeDaemon(func(m *mockDaemon) {
		m.crashOnTool = true
		m.onInit = func(f []byte) { initialInit <- carriesGrant(f) }
	})
	h := startProxy(t, initialProxySide, 0, 0, grant)

	_, replProxySide := newPipeDaemon(func(m *mockDaemon) {
		m.onInit = func(f []byte) { replInit <- carriesGrant(f) }
	})
	h.dialQueue <- replProxySide

	h.start()
	h.handshake()

	// Initial connect must have transported the grant.
	select {
	case ok := <-initialInit:
		if !ok {
			t.Fatal("initial daemon did not receive the allow-dir in _meta")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for initial initialize")
	}

	// Force a crash + reconnect; the proxy replays the captured (augmented) frame.
	h.write(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`)
	if frame := h.read(10 * time.Second); !strings.Contains(frame, `"id":5`) {
		t.Fatalf("expected synthesised error for in-flight id 5, got %q", frame)
	}

	select {
	case ok := <-replInit:
		if !ok {
			t.Fatal("replacement daemon did not receive the allow-dir on handshake replay")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for replayed initialize")
	}
	_ = h.clientIn.Close()
}
