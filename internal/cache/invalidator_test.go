package cache_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

func TestInvalidator_PublishDiagnostics(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	uri := "file:///p/main.py"
	c.Set(uri+":hover", "v", time.Minute)
	c.Set(uri+":def", "d", time.Minute)

	params, _ := json.Marshal(protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: []protocol.Diagnostic{},
	})
	inv.Handle(protocol.MethodPublishDiagnostics, params)

	if _, ok := c.Get(uri + ":hover"); ok {
		t.Fatal("expected eviction after publishDiagnostics")
	}
	if _, ok := c.Get(uri + ":def"); ok {
		t.Fatal("expected eviction after publishDiagnostics")
	}
}

func TestInvalidator_OtherMethod_noEviction(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	c.Set("key", "v", time.Minute)
	inv.Handle("window/logMessage", json.RawMessage(`{"message":"hi"}`))

	if _, ok := c.Get("key"); !ok {
		t.Fatal("unrelated notification should not evict cache")
	}
}

func TestInvalidator_MalformedParams_noEviction(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	c.Set("key", "v", time.Minute)
	inv.Handle(protocol.MethodPublishDiagnostics, json.RawMessage(`not-json`))

	if _, ok := c.Get("key"); !ok {
		t.Fatal("malformed params should not evict cache")
	}
}

func TestInvalidator_EmptyURI_noEviction(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	c.Set("key", "v", time.Minute)
	params, _ := json.Marshal(protocol.PublishDiagnosticsParams{URI: ""})
	inv.Handle(protocol.MethodPublishDiagnostics, params)

	if _, ok := c.Get("key"); !ok {
		t.Fatal("empty URI should not evict cache")
	}
}
