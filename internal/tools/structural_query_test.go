package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

const structuralFixture = `package demo

import "context"

// DocumentedExport is documented.
func DocumentedExport() {}

func UndocumentedExport() {}

// UsesCtx references its context parameter.
func UsesCtx(ctx context.Context) error {
	_ = ctx.Err()
	return nil
}

// IgnoresCtx never references its context parameter.
func IgnoresCtx(ctx context.Context) error {
	return nil
}

// BigFunc is deliberately long for the long-functions check.
func BigFunc() {
	a := 0
	a++
	a++
	a++
	a++
	a++
	a++
	a++
	a++
	a++
	_ = a
}
`

// openStructuralFixture writes the fixture, opens a topology store, and waits
// for the symbols to be indexed.
func openStructuralFixture(t *testing.T) (*topology.Store, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "demo.go"), []byte(structuralFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo.go")); len(n) >= 5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return s, ws
}

func runStructural(t *testing.T, s *topology.Store, ws string, args map[string]any) string {
	t.Helper()
	tool := NewStructuralQuery(func() *topology.Store { return s }, func() string { return ws })
	raw, _ := json.Marshal(args)
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute(%v): %v", args, err)
	}
	return out
}

func TestStructuralQuery_UndocumentedExports(t *testing.T) {
	s, ws := openStructuralFixture(t)
	out := runStructural(t, s, ws, map[string]any{"query": "undocumented-exports"})
	if !strings.Contains(out, "UndocumentedExport") {
		t.Errorf("UndocumentedExport should be flagged; got:\n%s", out)
	}
	if strings.Contains(out, "DocumentedExport") {
		t.Errorf("DocumentedExport has a doc comment and must not be flagged; got:\n%s", out)
	}
}

func TestStructuralQuery_LongFunctions(t *testing.T) {
	s, ws := openStructuralFixture(t)
	out := runStructural(t, s, ws, map[string]any{"query": "long-functions", "min_lines": 10})
	if !strings.Contains(out, "BigFunc") {
		t.Errorf("BigFunc should be flagged as long; got:\n%s", out)
	}
	if strings.Contains(out, "UndocumentedExport") {
		t.Errorf("a one-line function must not be flagged as long; got:\n%s", out)
	}
}

func TestStructuralQuery_UnusedContext(t *testing.T) {
	s, ws := openStructuralFixture(t)
	out := runStructural(t, s, ws, map[string]any{"query": "unused-context"})
	if !strings.Contains(out, "IgnoresCtx") {
		t.Errorf("IgnoresCtx never references ctx and should be flagged; got:\n%s", out)
	}
	if strings.Contains(out, "UsesCtx") {
		t.Errorf("UsesCtx references ctx and must not be flagged; got:\n%s", out)
	}
}

func TestStructuralQuery_NilStore(t *testing.T) {
	tool := NewStructuralQuery(func() *topology.Store { return nil }, func() string { return "" })
	raw, _ := json.Marshal(map[string]any{"query": "long-functions"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(out), "topology") {
		t.Errorf("nil store should return the topology-disabled message; got: %q", out)
	}
}

func TestParseStructuralQueryArgs(t *testing.T) {
	if _, err := parseStructuralQueryArgs(json.RawMessage(`{"query":"bogus"}`)); err == nil {
		t.Error("unknown query should be rejected")
	}
	a, err := parseStructuralQueryArgs(json.RawMessage(`{"query":"long-functions"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.MinLines != 80 || a.Limit != 50 {
		t.Errorf("defaults not applied: min_lines=%d limit=%d", a.MinLines, a.Limit)
	}
}

func TestCtxParamName(t *testing.T) {
	cases := []struct{ sig, want string }{
		{"func F(ctx context.Context) error", "ctx"},
		{"func (s *Store) Do(c context.Context, x int) error", "c"},
		{"func G(a, b context.Context)", ""}, // grouped — ambiguous, skipped
		{"func H(_ context.Context)", "_"},
		{"func NoCtx(x int) error", ""},
	}
	for _, tc := range cases {
		if got := ctxParamName(tc.sig); got != tc.want {
			t.Errorf("ctxParamName(%q) = %q, want %q", tc.sig, got, tc.want)
		}
	}
}

func TestIsExported(t *testing.T) {
	cases := []struct {
		name, lang string
		want       bool
	}{
		{"Foo", "go", true},
		{"foo", "go", false},
		{"public_thing", "python", true},
		{"_private", "python", false},
	}
	for _, tc := range cases {
		if got := isExported(tc.name, tc.lang); got != tc.want {
			t.Errorf("isExported(%q, %q) = %v, want %v", tc.name, tc.lang, got, tc.want)
		}
	}
}
