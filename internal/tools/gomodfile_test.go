package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gomodfile_test.go pins the go.work / go.mod reader to the go command's own
// grammar. Every "ok" row is a file go accepts and every error row one go rejects
// (each checked with `go list -m` against the go.work, go 1.26 — the one marked
// exception aside); the reader must agree in BOTH directions, because a
// disagreement either way flips the GOWORK decision.

func TestGoWorkUses(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		want    []string // relative to the go.work's directory; nil with wantErr
		wantErr bool
	}{
		{name: "single use", src: "go 1.21\n\nuse ./a\n", want: []string{"a"}},
		{name: "use block", src: "go 1.21\n\nuse (\n\t./a\n\t./b\n)\n", want: []string{"a", "b"}},
		// go's lexer ends a bare word at "(", so `use(` opens a block exactly as
		// `use (` does. A reader splitting on whitespace alone missed every entry.
		{name: "use( with no space", src: "go 1.21\n\nuse(\n\t./main\n\t./dep\n)\n", want: []string{"main", "dep"}},
		{name: "comments everywhere", src: "// top\ngo 1.21 // v\nuse ( // block\n\t./a // one\n\t// ./not\n)\n", want: []string{"a"}},
		{name: "quoted path", src: "use \"./with space\"\n", want: []string{"with space"}},
		{name: "quoted path with an escape", src: "use \"./a\\u0062\"\n", want: []string{"ab"}},
		{name: "empty block", src: "use ()\nuse ./a\n", want: []string{"a"}},
		{name: "CRLF line endings", src: "go 1.21\r\n\r\nuse (\r\n\t./a\r\n)\r\n", want: []string{"a"}},
		{name: "absolute path kept", src: "use /abs/mod\n", want: []string{"/abs/mod"}},
		{name: "path cleaned", src: "use ./a/../b/\n", want: []string{"b"}},
		{name: "a path inside a replace block is not a use", src: "replace (\n\texample.com/x v1.0.0 => ./y\n)\nuse ./a\n", want: []string{"a"}},
		{name: "godebug block skipped", src: "godebug (\n\tdefault=go1.21\n)\nuse ./a\n", want: []string{"a"}},
		// The one deliberate difference: go 1.26 rejects a directive it does not
		// know, but a NEWER go's directive must not make plumb read a file that
		// toolchain accepts as broken, so it is skipped rather than refused.
		{name: "unknown directive skipped", src: "futuredirective x y\nuse ./a\n", want: []string{"a"}},
		{name: "no use at all", src: "go 1.21\n", want: nil},
		// go rejects every one of these; a reader that accepted them would call a
		// module "listed" in a workspace go cannot load at all.
		{name: "backquoted path", src: "use `./m`\n", wantErr: true},
		{name: "quote inside a bare word", src: "use ./it's\n", wantErr: true},
		{name: "two operands", src: "use ./a ./b\n", wantErr: true},
		{name: "no operand", src: "use\n", wantErr: true},
		{name: "block-style comment", src: "/* x */\nuse ./a\n", wantErr: true},
		{name: "unterminated string", src: "use \"./a\n", wantErr: true},
		{name: "unterminated block", src: "use (\n\t./a\n", wantErr: true},
		{name: "text after the closing paren", src: "use (\n\t./a\n) x\n", wantErr: true},
		{name: "a bad quoted escape", src: "use \"./a\\q\"\n", wantErr: true},
		{name: "non-printable byte", src: "use ./a\x01\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			work := filepath.Join(dir, "go.work")
			if err := os.WriteFile(work, []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := goWorkUses(work)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("goWorkUses accepted a go.work go rejects; got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("goWorkUses rejected a go.work go accepts: %v", err)
			}
			var want []string
			for _, w := range tc.want {
				if filepath.IsAbs(w) {
					want = append(want, w)
				} else {
					want = append(want, filepath.Join(dir, w))
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("goWorkUses = %q, want %q", got, want)
			}
		})
	}
}

func TestGoModulePath(t *testing.T) {
	cases := []struct {
		name, src, want string
		ok              bool
	}{
		{"plain", "module example.com/m\n\ngo 1.21\n", "example.com/m", true},
		{"quoted", "module \"example.com/m\"\n", "example.com/m", true},
		{"with a deprecation comment", "// Deprecated: use v2\nmodule example.com/m // x\n", "example.com/m", true},
		{"retract brackets do not confuse it", "module example.com/m\nretract [v1.0.0, v1.1.0]\n", "example.com/m", true},
		{"no module line", "go 1.21\n", "", false},
		{"two module lines", "module a\nmodule b\n", "", false},
		{"unparseable", "module \"a\n", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod := filepath.Join(t.TempDir(), "go.mod")
			if err := os.WriteFile(mod, []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok := goModulePath(mod)
			if got != tc.want || ok != tc.ok {
				t.Errorf("goModulePath = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestReadGoConfigFile_NeverReadsWithoutBound is the daemon-safety half: a go.work
// committed as a link to a device or a FIFO, or simply enormous, must fail FAST.
// Before, one `git status` on such a clone read /dev/zero until the shared daemon
// died of memory exhaustion, and a FIFO parked it forever.
func TestReadGoConfigFile_NeverReadsWithoutBound(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{}
	if _, err := os.Stat("/dev/zero"); err == nil {
		link := filepath.Join(dir, "zero")
		if err := os.Symlink("/dev/zero", link); err != nil {
			t.Fatal(err)
		}
		cases["a link to /dev/zero"] = link
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err == nil {
		cases["a FIFO no one writes to"] = fifo
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", goConfigReadLimit+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cases["a file over the limit"] = big
	cases["a directory"] = dir
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { _, err := readGoConfigFile(path); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Errorf("readGoConfigFile(%s) succeeded; it must refuse anything but a small regular file", path)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("readGoConfigFile(%s) is still reading after 5s — the daemon would hang (or exhaust memory) here", path)
			}
		})
	}
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte(strings.Repeat("x", goConfigReadLimit)), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readGoConfigFile(ok); err != nil || len(data) != goConfigReadLimit {
		t.Errorf("a regular file at exactly the limit must be read whole: len=%d err=%v", len(data), err)
	}
}
