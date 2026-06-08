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

func TestHandleCtrlConn_MemStats(t *testing.T) {
	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", nil, nil, nil)

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
	go serveControlSocket(ln, "info", "text", nil, nil, nil)

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
