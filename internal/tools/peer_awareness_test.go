package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestJoinTopologyAnnotation(t *testing.T) {
	cases := []struct {
		name string
		pkg  string
		syms []string
		want string
	}{
		{"pkg and syms", "tools", []string{"RateLimiter", "Allow"}, "package tools · RateLimiter, Allow"},
		{"pkg only", "tools", nil, "package tools"},
		{"syms only", "", []string{"Foo", "Bar"}, "Foo, Bar"},
		{"neither", "", nil, ""},
		{"caps extra", "p", []string{"a", "b", "c", "d", "e"}, "package p · a, b, c (+2 more)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinTopologyAnnotation(tc.pkg, tc.syms); got != tc.want {
				t.Errorf("joinTopologyAnnotation(%q, %v) = %q, want %q", tc.pkg, tc.syms, got, tc.want)
			}
		})
	}
}

func TestFileTopologyAnnotation_NilStore(t *testing.T) {
	if got := fileTopologyAnnotation(context.Background(), nil, "/ws/a.go"); got != "" {
		t.Errorf("nil store must yield no annotation, got %q", got)
	}
}

func TestWriteToolNames_CopyIsIndependent(t *testing.T) {
	a := WriteToolNames()
	if len(a) == 0 {
		t.Fatal("WriteToolNames returned empty")
	}
	a[0] = "MUTATED"
	if WriteToolNames()[0] == "MUTATED" {
		t.Error("WriteToolNames must return a copy, not the backing slice")
	}
}

func TestFileFromToolInput(t *testing.T) {
	if got := FileFromToolInput(`{"file_path":"/ws/x.go"}`); got != "/ws/x.go" {
		t.Errorf("file_path extraction = %q", got)
	}
	if got := FileFromToolInput(`{"tool":"git"}`); got != "" {
		t.Errorf("non-path input should yield empty, got %q", got)
	}
}

// TestFormatWorkspaceSessions_Annotation asserts a topology annotation is
// appended to a recent-write line when present, and omitted (bare path) when the
// annotations map is nil — the peer_awareness-off / no-topology fallback.
func TestFormatWorkspaceSessions_Annotation(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{ID: "self-1", Name: "me", Folder: "/ws", LastSeenAt: now},
		{ID: "peer-2", Name: "swift-falcon", Folder: "/ws", LastSeenAt: now},
	}
	writes := []stats.RecentCall{
		{
			SessionName: "swift-falcon", Tool: "edit_file", CalledAt: now.Add(-3 * time.Minute),
			InputJSON: `{"file_path":"/ws/internal/tools/ratelimit.go"}`,
		},
	}
	annot := map[string]string{"/ws/internal/tools/ratelimit.go": "package tools · RateLimiter"}

	withAnnot := formatWorkspaceSessions("/ws", "self-1", peers, writes, annot, now)
	if !strings.Contains(withAnnot, "[package tools · RateLimiter]") {
		t.Errorf("annotated output missing the topology label:\n%s", withAnnot)
	}
	if !strings.Contains(withAnnot, "internal/tools/ratelimit.go") {
		t.Errorf("output missing the relative path:\n%s", withAnnot)
	}

	bare := formatWorkspaceSessions("/ws", "self-1", peers, writes, nil, now)
	if strings.Contains(bare, "[package") {
		t.Errorf("nil annotations must render bare paths:\n%s", bare)
	}
}

func TestClampToBytes(t *testing.T) {
	if got := clampToBytes("hello", 0); got != "hello" {
		t.Errorf("budget 0 disables clamp, got %q", got)
	}
	if got := clampToBytes("hello", 100); got != "hello" {
		t.Errorf("fitting string unchanged, got %q", got)
	}
	got := clampToBytes("hello world", 8)
	if len([]byte(got)) > 8 {
		t.Errorf("clamped %q exceeds 8 bytes", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clamped string should end with ellipsis, got %q", got)
	}
	// Multi-byte: never split a rune.
	multi := clampToBytes(strings.Repeat("é", 10), 7) // 'é' = 2 bytes; ellipsis 3 bytes
	if !utf8ValidString(multi) {
		t.Errorf("clamp split a multi-byte rune: %q", multi)
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
