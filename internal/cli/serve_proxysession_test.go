package cli

import (
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// initMeta decodes params._meta from an initialize frame for assertions.
func initMeta(t *testing.T, frame []byte) map[string]json.RawMessage {
	t.Helper()
	var got struct {
		Params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &got); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return got.Params.Meta
}

func TestInjectInitMetaBothKeys(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	out := injectInitMeta(frame, buildInitMeta([]string{"/a", "/b"}, "sess-xyz"))
	meta := initMeta(t, out)

	var dirs []string
	if err := json.Unmarshal(meta[mcp.MetaAllowDirsKey], &dirs); err != nil {
		t.Fatalf("allow-dirs: %v", err)
	}
	if len(dirs) != 2 || dirs[0] != "/a" || dirs[1] != "/b" {
		t.Fatalf("allow-dirs = %v", dirs)
	}
	var id string
	if err := json.Unmarshal(meta[mcp.MetaProxySessionKey], &id); err != nil {
		t.Fatalf("proxy id: %v", err)
	}
	if id != "sess-xyz" {
		t.Fatalf("proxy id = %q, want sess-xyz", id)
	}
}

func TestInjectInitMetaProxyIDOnly(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	out := injectInitMeta(frame, buildInitMeta(nil, "only-id"))
	meta := initMeta(t, out)
	if _, ok := meta[mcp.MetaAllowDirsKey]; ok {
		t.Fatal("allow-dirs key present when no dirs given")
	}
	var id string
	if err := json.Unmarshal(meta[mcp.MetaProxySessionKey], &id); err != nil || id != "only-id" {
		t.Fatalf("proxy id = %q err=%v", id, err)
	}
}

func TestBuildInitMetaEmptyIsNil(t *testing.T) {
	if m := buildInitMeta(nil, ""); m != nil {
		t.Fatalf("buildInitMeta(nil, \"\") = %v, want nil", m)
	}
}

func TestInjectInitMetaEmptyParityByteIdentical(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	// No dirs, no id ⇒ the meta map is nil ⇒ frame must be byte-identical.
	if got := injectInitMeta(frame, buildInitMeta(nil, "")); string(got) != string(frame) {
		t.Fatalf("empty meta changed the frame:\n got %s\nwant %s", got, frame)
	}
}

func TestInjectInitMetaPreservesExistingMeta(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"keep":"me"}}}`)
	out := injectInitMeta(frame, buildInitMeta(nil, "the-id"))
	meta := initMeta(t, out)
	var keep string
	if err := json.Unmarshal(meta["keep"], &keep); err != nil || keep != "me" {
		t.Fatalf("existing _meta key dropped: keep=%q err=%v", keep, err)
	}
	if _, ok := meta[mcp.MetaProxySessionKey]; !ok {
		t.Fatal("proxy id not added alongside existing _meta")
	}
}

func TestInjectInitMetaMalformedPassthrough(t *testing.T) {
	bad := []byte(`not json`)
	if got := injectInitMeta(bad, buildInitMeta(nil, "id")); string(got) != string(bad) {
		t.Fatalf("malformed frame must pass through unchanged: %s", got)
	}
}

func TestNewProxySessionIDFormat(t *testing.T) {
	id := newProxySessionID()
	// UUIDv4: 36 chars, 8-4-4-4-12 hex with version/variant nibbles.
	if len(id) != 36 {
		t.Fatalf("len(id) = %d (%q), want 36", len(id), id)
	}
	if id[14] != '4' {
		t.Fatalf("version nibble = %c, want 4 (%q)", id[14], id)
	}
	// Two calls must differ.
	if id2 := newProxySessionID(); id2 == id {
		t.Fatalf("two IDs collided: %q", id)
	}
}
