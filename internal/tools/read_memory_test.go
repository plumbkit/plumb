package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/memory"
)

func TestReadMemory_ProvenanceFooterAndRedaction(t *testing.T) {
	ws := t.TempDir()
	// A generated memory whose body carries a secret must be redacted on disk and
	// show a provenance footer on read.
	prov := memory.Provenance{
		Confidence:    memory.ConfidenceGenerated,
		SourceSession: "swift-falcon",
		SourcePaths:   []string{"internal/auth/login.go"},
		CreatedAt:     time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
	}
	if err := memory.WriteGenerated(nil, ws, "last-session", "session summary",
		"touched login.go; api_key = sk-supersecretvalue", prov); err != nil {
		t.Fatalf("WriteGenerated: %v", err)
	}

	tool := NewReadMemory(func() string { return ws })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"last-session"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "sk-supersecretvalue") {
		t.Errorf("secret leaked into stored/returned memory:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED:secret]") {
		t.Errorf("expected redaction placeholder in body:\n%s", out)
	}
	if !strings.Contains(out, "[provenance] generated") {
		t.Errorf("expected provenance footer, got:\n%s", out)
	}
	if !strings.Contains(out, "swift-falcon") || !strings.Contains(out, "internal/auth/login.go") {
		t.Errorf("footer should carry session + touched path:\n%s", out)
	}
}

func TestReadMemory_UserMemoryHasNoFooter(t *testing.T) {
	ws := t.TempDir()
	if err := memory.Write(ws, "notes", "plain user note", "my notes"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tool := NewReadMemory(func() string { return ws })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"notes"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "[provenance]") {
		t.Errorf("user memory must not show a provenance footer:\n%s", out)
	}
}
