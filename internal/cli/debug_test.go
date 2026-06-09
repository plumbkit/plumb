package cli

import (
	"bufio"
	"io"
	"math"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

// sendCtrlFull dials ln, sends cmd, and returns the full (multi-line) response.
func sendCtrlFull(t *testing.T, ln net.Listener, cmd string) string {
	t.Helper()
	conn, err := net.Dial(ln.Addr().Network(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

func TestRenderLSPStatus(t *testing.T) {
	if got := renderLSPStatus(""); got != "no active language servers\n" {
		t.Fatalf("empty reply: got %q", got)
	}
	// go: active with pid+rss (566231040 B ≈ 540 MB, idle 12s);
	// java: hibernated with empty pid/rss (idle 1500s = 25m0s).
	resp := "go\t/x\tactive\t1234\t566231040\t12\njava\t/y\thibernated\t\t\t1500\n"
	out := renderLSPStatus(resp)
	for _, want := range []string{"LANGUAGE", "go", "/x", "active", "1234", "540 MB", "12s", "java", "hibernated", "25m0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestHandleCtrlConn_LSPStatus(t *testing.T) {
	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", ctrlHandlers{lspStatus: func() string {
		return "go\t/x\tactive\t1\t100\t5\n"
	}})
	resp := sendCtrlFull(t, ln, "lsp-status")
	if !strings.Contains(resp, "go\t/x\tactive") {
		t.Fatalf("lsp-status reply: got %q", resp)
	}
}

func TestHandleCtrlConn_LSPStatusNilFn(t *testing.T) {
	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", ctrlHandlers{})
	if resp := sendCtrlFull(t, ln, "lsp-status"); resp != "" {
		t.Fatalf("nil lspStatus should yield empty reply, got %q", resp)
	}
}

func TestHandleCtrlConn_MemStats(t *testing.T) {
	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", ctrlHandlers{})

	resp := sendCtrlFull(t, ln, "mem-stats")
	for _, want := range []string{"HeapAlloc", "HeapInuse", "HeapSys", "HeapReleased", "NumGC", "Goroutines"} {
		if !strings.Contains(resp, want) {
			t.Errorf("mem-stats response missing %q:\n%s", want, resp)
		}
	}
}

func TestHandleCtrlConn_HeapProfile(t *testing.T) {
	// Redirect the cache dir so the profile lands in a temp dir, not the real one.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", ctrlHandlers{})

	// Read only the first line: heap-profile replies with a single path line.
	conn, err := net.Dial(ln.Addr().Network(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("heap-profile\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	path := strings.TrimSpace(line)
	if strings.HasPrefix(path, "error:") {
		t.Fatalf("heap-profile returned error: %s", path)
	}
	if !strings.HasSuffix(path, ".pprof") {
		t.Fatalf("heap-profile path = %q, want a .pprof file", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat heap profile %q: %v", path, err)
	}
	if info.Size() == 0 {
		t.Fatalf("heap profile %q is empty", path)
	}
}

func TestHandleCtrlConn_GoroutineStacks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", ctrlHandlers{})

	// goroutine-stacks replies with a single path line.
	conn, err := net.Dial(ln.Addr().Network(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("goroutine-stacks\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	path := strings.TrimSpace(line)
	if strings.HasPrefix(path, "error:") {
		t.Fatalf("goroutine-stacks returned error: %s", path)
	}
	if !strings.HasSuffix(path, ".txt") {
		t.Fatalf("goroutine-stacks path = %q, want a .txt file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stacks dump %q: %v", path, err)
	}
	// debug=2 output starts with "goroutine profile: total N" and contains
	// per-goroutine "goroutine <id> [<state>]" headers.
	if !strings.Contains(string(data), "goroutine") {
		t.Fatalf("stacks dump %q missing goroutine stacks:\n%s", path, data)
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1024", 1024, false},
		{"512KB", 512 * 1024, false},
		{"512KiB", 512 * 1024, false},
		{"2MB", 2 * 1024 * 1024, false},
		{"1500MiB", 1500 * 1024 * 1024, false},
		{"4GB", 4 * 1024 * 1024 * 1024, false},
		{" 2 GiB ", 2 * 1024 * 1024 * 1024, false},
		{"100b", 100, false},
		{"", 0, true},
		{"abc", 0, true},
		{"12xy", 0, true},
		{"-5MB", 0, true},
	}
	for _, tc := range tests {
		got, err := parseByteSize(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseByteSize(%q) = %d, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseByteSize(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestApplyParseMemoryBudget(t *testing.T) {
	const envKey = "GOT_PARSE_MEMORY_BUDGET_MB"

	t.Run("default applied when unset", func(t *testing.T) {
		t.Setenv(envKey, "") // ensure a known starting point for LookupEnv via Unsetenv below
		if err := os.Unsetenv(envKey); err != nil {
			t.Fatalf("unsetenv: %v", err)
		}
		applyParseMemoryBudget()
		if got := os.Getenv(envKey); got != defaultParseMemoryBudgetMB {
			t.Errorf("budget = %q, want default %q", got, defaultParseMemoryBudgetMB)
		}
	})

	t.Run("operator value respected", func(t *testing.T) {
		t.Setenv(envKey, "256")
		applyParseMemoryBudget()
		if got := os.Getenv(envKey); got != "256" {
			t.Errorf("budget = %q, want operator value 256 (must not be overwritten)", got)
		}
	})
}

func TestApplyMemoryLimit(t *testing.T) {
	// Snapshot the current limit (SetMemoryLimit(-1) reports without changing) and
	// restore it so this test does not leak a global mutation into others.
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	cases := []struct {
		raw  string
		want int64
	}{
		{"", defaultDaemonMemLimit},
		{"1500MiB", 1500 * 1024 * 1024},
		{"off", math.MaxInt64},
		{"garbage", defaultDaemonMemLimit}, // malformed falls back to default
	}
	for _, tc := range cases {
		applyMemoryLimit(tc.raw)
		got := debug.SetMemoryLimit(-1)
		if got != tc.want {
			t.Errorf("applyMemoryLimit(%q): limit = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
