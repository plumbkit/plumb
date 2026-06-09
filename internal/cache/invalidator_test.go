package cache_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
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

func TestInvalidator_WaitDiagnostics_AlreadyTracked(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	uri := "file:///p/main.go"
	want := protocol.Diagnostic{Severity: protocol.SevError, Message: "already here"}
	params, _ := json.Marshal(protocol.PublishDiagnosticsParams{URI: uri, Diagnostics: []protocol.Diagnostic{want}})
	inv.Handle(protocol.MethodPublishDiagnostics, params)

	diags, err := inv.WaitDiagnostics(context.Background(), uri)
	if err != nil {
		t.Fatalf("WaitDiagnostics: %v", err)
	}
	if len(diags) != 1 || diags[0].Message != want.Message {
		t.Fatalf("got %v, want %v", diags, want)
	}
}

func TestInvalidator_WaitDiagnostics_BlocksUntilPush(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	uri := "file:///p/other.go"
	want := protocol.Diagnostic{Severity: protocol.SevWarning, Message: "late arrival"}

	// Push diagnostics from a separate goroutine after a short delay.
	go func() {
		time.Sleep(20 * time.Millisecond)
		p, _ := json.Marshal(protocol.PublishDiagnosticsParams{URI: uri, Diagnostics: []protocol.Diagnostic{want}})
		inv.Handle(protocol.MethodPublishDiagnostics, p)
	}()

	diags, err := inv.WaitDiagnostics(context.Background(), uri)
	if err != nil {
		t.Fatalf("WaitDiagnostics: %v", err)
	}
	if len(diags) != 1 || diags[0].Message != want.Message {
		t.Fatalf("got %v, want %v", diags, want)
	}
}

func TestInvalidator_WaitDiagnostics_ContextCancelled(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := inv.WaitDiagnostics(ctx, "file:///p/never.go")
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

func TestInvalidator_Tracked(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	uri := "file:///p/main.go"
	if inv.Tracked(uri) {
		t.Fatal("expected Tracked=false before any notification")
	}

	params, _ := json.Marshal(protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: []protocol.Diagnostic{},
	})
	inv.Handle(protocol.MethodPublishDiagnostics, params)

	if !inv.Tracked(uri) {
		t.Fatal("expected Tracked=true after publishDiagnostics")
	}
	if inv.Tracked("file:///p/other.go") {
		t.Fatal("Tracked should return false for a URI that was never reported")
	}
}

func TestInvalidator_AllDiagnosticTimes(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	if len(inv.AllDiagnosticTimes()) != 0 {
		t.Fatal("expected empty AllDiagnosticTimes before any notifications")
	}

	before := time.Now()
	uri := "file:///p/main.go"
	params, _ := json.Marshal(protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "boom"}},
	})
	inv.Handle(protocol.MethodPublishDiagnostics, params)
	after := time.Now()

	times := inv.AllDiagnosticTimes()
	ts, ok := times[uri]
	if !ok {
		t.Fatal("expected timestamp entry after publishDiagnostics")
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("timestamp %v not within [%v, %v]", ts, before, after)
	}

	// A second notification for the same URI should update the timestamp.
	time.Sleep(2 * time.Millisecond)
	inv.Handle(protocol.MethodPublishDiagnostics, params)
	times2 := inv.AllDiagnosticTimes()
	if !times2[uri].After(ts) {
		t.Error("expected updated timestamp after second notification")
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
