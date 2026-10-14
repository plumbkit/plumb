package daemons

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"
)

func TestIsPlumbDaemonArgv(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"bare", []string{"plumb", "daemon"}, true},
		{"absolute path", []string{"/opt/homebrew/bin/plumb", "daemon"}, true},
		{"relative path", []string{"./plumb", "daemon"}, true},
		{"extra arguments", []string{"plumb", "daemon", "--verbose"}, true},
		{"serve is not a daemon", []string{"plumb", "serve"}, false},
		{"debug daemons is not a daemon", []string{"plumb", "debug", "daemons"}, false},
		{"a grep of the words is not a daemon", []string{"grep", "plumb daemon"}, false},
		{"a renamed binary is not matched", []string{"/tmp/plumb-0.24.1", "daemon"}, false},
		{"argv[0] only", []string{"plumb"}, false},
		{"no arguments", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPlumbDaemonArgv(tt.args); got != tt.want {
				t.Errorf("isPlumbDaemonArgv(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestStatFields(t *testing.T) {
	// A realistic line: comm holds spaces and a closing parenthesis, which is
	// exactly why the header is matched to the LAST ") ".
	line := "1234 (plumb daemon (x)) S 1 1234 1234 0 -1 4194560 60 0 0 0 1 2 0 0 20 0 1 0 987654 100 200 " +
		strings.Repeat("0 ", 30)

	fields, err := statFields(1234, line)
	if err != nil {
		t.Fatalf("statFields: %v", err)
	}
	if len(fields) <= statStartField {
		t.Fatalf("only %d fields, want more than %d", len(fields), statStartField)
	}
	if got := fields[statFlagsField]; got != "4194560" {
		t.Errorf("flags field = %q, want %q", got, "4194560")
	}
	if got := fields[statStartField]; got != "987654" {
		t.Errorf("starttime field = %q, want %q", got, "987654")
	}
	if got := fields[1]; got != "1" {
		t.Errorf("ppid field = %q, want %q", got, "1")
	}

	tests := []struct {
		name string
		line string
	}{
		{"no header", "S 1 2 3"},
		{"empty", ""},
		{"another pid", "9999 (plumb) S 1 2 3"},
		{"truncated after the header", "1234 (plumb) S 1 2 3"},
		{"closing paren without the space", "1234 (plumb)S 1 2 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := statFields(1234, tt.line); err == nil {
				t.Errorf("statFields(%q) = nil error, want a refusal", tt.line)
			}
		})
	}
}

func TestParseProcNetUnix(t *testing.T) {
	data := strings.Join([]string{
		"Num       RefCount Protocol Flags    Type St Inode Path",
		"0000: 00000002 00000000 00010000 0001 01 54321 /run/plumb/plumb.sock",
		"0000: 00000002 00000000 00010000 0001 01 98765 /run/other.sock",
		"0000: 00000002 00000000 00010000 0002 01 11111",
		"",
	}, "\n")

	if ino, ok := parseProcNetUnix(data, "/run/plumb/plumb.sock"); !ok || ino != 54321 {
		t.Errorf("parseProcNetUnix = %d, %v; want 54321, true", ino, ok)
	}
	// A socket file whose entry is not in the table is not bound by anything:
	// "not found" is an answer, not a failure.
	if ino, ok := parseProcNetUnix(data, "/run/plumb/gone.sock"); ok || ino != 0 {
		t.Errorf("parseProcNetUnix for an unbound path = %d, %v; want 0, false", ino, ok)
	}
	// The header names a path-like token in field 0, and an unnamed socket has
	// no path at all: neither may be read as a match.
	if _, ok := parseProcNetUnix(data, "Num"); ok {
		t.Error("the header line must not match a path")
	}
}

func TestForkedNotYetExec(t *testing.T) {
	// The bit that stops a daemon's not-yet-exec'd child from being counted as a
	// second daemon. Both directions, because a rule that always answers false
	// would pass a test that only ever saw an exec'd process.
	if !forkedNotYetExec(pfForkNoExec) {
		t.Error("a process carrying PF_FORKNOEXEC must be skipped")
	}
	if forkedNotYetExec(0x4004) {
		t.Error("an exec'd process (flags 0x4004) must not be skipped")
	}
	if forkedNotYetExec(0) {
		t.Error("flags 0 must not be skipped")
	}
}

func TestHasExecd(t *testing.T) {
	// The macOS counterpart, with the values measured on this machine: a running
	// process reports 0x00004004, its own fork-without-exec child 0x00000004.
	if !hasExecd(0x00004004) {
		t.Error("a process carrying P_EXEC must be considered exec'd")
	}
	if hasExecd(0x00000004) {
		t.Error("a forked child (0x4) must not be considered exec'd")
	}
}

func TestPSLineMentionsPlumb(t *testing.T) {
	// Deliberately loose: ps is a prefilter on darwin, and the process's own
	// arguments are what decide. A shell quoting the words is the case that must
	// pass the prefilter and still not be counted.
	tests := []struct {
		line string
		want bool
	}{
		{"/opt/homebrew/bin/plumb daemon", true},
		{`/bin/sh -c sleep 5 # plumb daemon`, true},
		{"/usr/bin/gopls -mode=stdio", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := psLineMentionsPlumb(tt.line); got != tt.want {
			t.Errorf("psLineMentionsPlumb(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestLockHeldBy(t *testing.T) {
	data := strings.Join([]string{
		"1: POSIX  ADVISORY  WRITE 4321 00:54:7562196 0 EOF",
		"2: FLOCK  ADVISORY  WRITE 11 00:58:7566731 0 EOF",
		"3: FLOCK  ADVISORY  WRITE 22 00:58:9999999 0 EOF",
		"4: FLOCK",
		"",
	}, "\n")
	if !lockHeldBy(data, "00:58:7566731") {
		t.Error("a held flock must be reported as held")
	}
	if lockHeldBy(data, "00:58:1111111") {
		t.Error("an inode no line names must not be reported as held")
	}
	if lockHeldBy("", "00:58:7566731") {
		t.Error("an empty table must not report a lock as held")
	}
	if lockHeldBy(data, "00:54:7562196") {
		t.Error("a POSIX lock is a different mechanism and must not count")
	}
}

func TestParseLsofOpeners(t *testing.T) {
	// The shape of `lsof -w -F pn`: a p line per process, then one n line per open
	// file. Two processes hold the lock; one holds the socket.
	out := strings.Join([]string{
		"p100",
		"fcwd",
		"n/run/plumb/plumb.sock",
		"f5",
		"n/tmp/daemon.authority.lock",
		"p200",
		"f7",
		"n/tmp/daemon.authority.lock",
		"p300",
		"f9",
		"n/tmp/unrelated",
		"",
	}, "\n")
	wanted := map[string]bool{
		"/run/plumb/plumb.sock":       true,
		"/tmp/daemon.authority.lock":  true,
		"/tmp/another.authority.lock": true,
	}
	owners := parseLsofOpeners(out, wanted)
	if got := owners["/tmp/daemon.authority.lock"]; len(got) != 2 || got[0] != 100 || got[1] != 200 {
		t.Errorf("lock owners = %v, want [100 200]", got)
	}
	if got := owners["/run/plumb/plumb.sock"]; len(got) != 1 || got[0] != 100 {
		t.Errorf("socket owners = %v, want [100]", got)
	}
	if got := owners["/tmp/another.authority.lock"]; len(got) != 0 {
		t.Errorf("an unheld path = %v, want none", got)
	}

	// A name line before any p line has no process to attribute, and must be
	// dropped rather than counted against pid 0.
	if got := parseLsofOpeners("n/tmp/daemon.authority.lock", wanted)["/tmp/daemon.authority.lock"]; len(got) != 0 {
		t.Errorf("a name with no owning process = %v, want none", got)
	}
}

func TestParseLsofOpeners_MatchesAResolvedSpelling(t *testing.T) {
	// lsof reports what the kernel holds: on macOS a path spelled /var/... comes
	// back as /private/var/.... A census that only matched the literal spelling
	// would report nobody holding its own lock.
	dir := t.TempDir()
	lock := filepath.Join(dir, "daemon.authority.lock")
	// The file has to exist: EvalSymlinks is what resolves the spelling, and it
	// cannot resolve a path that is not there.
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatalf("creating %s: %v", lock, err)
	}
	resolved := paths.Canonical(lock)
	if resolved == lock {
		t.Skip("this temp directory has no symlinked spelling to distinguish")
	}
	out := "p42\n" + "n" + resolved + "\n"
	owners := parseLsofOpeners(out, map[string]bool{lock: true})
	if got := owners[lock]; len(got) != 1 || got[0] != 42 {
		t.Errorf("an owner reported under the resolved spelling must be attributed to the caller's key: %v", got)
	}
}

func TestParseProcArgs(t *testing.T) {
	build := func(argc uint32, parts ...string) []byte {
		var b []byte
		b = binary.LittleEndian.AppendUint32(b, argc)
		for _, p := range parts {
			b = append(b, p...)
			b = append(b, 0)
		}
		return b
	}

	exe, args, err := ParseProcArgs(build(2, "/opt/plumb/bin/plumb", "", "plumb", "daemon"))
	if err != nil {
		t.Fatalf("parseProcArgs: %v", err)
	}
	if exe != "/opt/plumb/bin/plumb" {
		t.Errorf("exe = %q, want the exec path", exe)
	}
	if len(args) != 2 || args[0] != "plumb" || args[1] != "daemon" {
		t.Errorf("args = %q, want [plumb daemon]", args)
	}

	// The last argument is not promised a trailing NUL.
	if _, args, err := ParseProcArgs(build(1, "/bin/plumb", "plumb")); err != nil {
		t.Errorf("parseProcArgs without a trailing NUL: %v", err)
	} else if len(args) != 1 || args[0] != "plumb" {
		t.Errorf("args = %q, want [plumb]", args)
	}

	tests := []struct {
		name string
		raw  []byte
	}{
		{"shorter than argc", []byte{1, 0, 0}},
		{"argc zero", build(0, "/bin/plumb")},
		{"argc implausible", build(99999, "/bin/plumb")},
		{"no NUL-terminated path", binary.LittleEndian.AppendUint32(nil, 1)},
		{"argc beyond the arguments", build(3, "/bin/plumb", "plumb")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := ParseProcArgs(tt.raw); err == nil {
				t.Errorf("ParseProcArgs(%v) = nil error, want a refusal", tt.raw)
			}
		})
	}
}

// TestParseProcArgs_AnEmptyArgv0IsNotPadding: the exec path's padding is exact, so
// a NUL beyond it is an empty argv[0] (`exec -a ”`), not more padding.
func TestParseProcArgs_AnEmptyArgv0IsNotPadding(t *testing.T) {
	exe := "/x/plumbd" // 9 bytes: padding is 7 NULs
	raw := binary.LittleEndian.AppendUint32(nil, 3)
	raw = append(raw, exe...)
	raw = append(raw, 0, 0, 0, 0, 0, 0, 0)
	raw = append(raw, "\x00plumb\x00daemon\x00PATH=/usr/bin\x00"...)
	_, args, err := ParseProcArgs(raw)
	if err != nil {
		t.Fatalf("ParseProcArgs: %v", err)
	}
	if want := []string{"", "plumb", "daemon"}; !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

// procArgsBuf lays a kern.procargs2 buffer out as darwin does: argc, the exec path
// padded so path, terminator and padding fill a multiple of 8, then the arguments.
func procArgsBuf(exe string, terminateLast bool, args ...string) []byte {
	raw := binary.LittleEndian.AppendUint32(nil, uint32(len(args))) //nolint:gosec // G115: a test fixture's small argc
	raw = append(raw, exe...)
	for range 8 - len(exe)%8 {
		raw = append(raw, 0)
	}
	for i, a := range args {
		raw = append(raw, a...)
		if i < len(args)-1 || terminateLast {
			raw = append(raw, 0)
		}
	}
	return raw
}

// TestParseProcArgs_PaddingAtEveryLength pins the padding rule across a full
// period of exec-path lengths, with an empty argv[0] that only exact padding can
// tell from padding: a rule off by any multiple of 8 reads the wrong argv here.
func TestParseProcArgs_PaddingAtEveryLength(t *testing.T) {
	for n := 1; n <= 24; n++ {
		exe := "/" + strings.Repeat("p", n-1)
		_, args, err := ParseProcArgs(procArgsBuf(exe, true, "", "plumb", "daemon"))
		if err != nil || !slices.Equal(args, []string{"", "plumb", "daemon"}) {
			t.Errorf("exec path of %d bytes: args = %q, err = %v", n, args, err)
		}
	}
}

func TestParseProcArgs_LastArgumentMayLackItsNUL(t *testing.T) {
	_, args, err := ParseProcArgs(procArgsBuf("/bin/plumb", false, "plumb", "daemon"))
	if err != nil || !slices.Equal(args, []string{"plumb", "daemon"}) {
		t.Fatalf("args = %q, err = %v", args, err)
	}
}

// TestParseProcArgs_RefusesMissingArguments: a buffer that ends exactly at an
// argument's NUL with arguments still owed is truncated — never an invented
// empty trailing argument.
func TestParseProcArgs_RefusesMissingArguments(t *testing.T) {
	raw := binary.LittleEndian.AppendUint32(nil, 3)
	raw = append(raw, "/bin/plumb\x00\x00\x00\x00\x00\x00plumb\x00daemon\x00"...)
	if _, args, err := ParseProcArgs(raw); err == nil {
		t.Fatalf("a 3-argument buffer holding 2 was read as %q", args)
	}
	// Cut mid-way through the last argument it does hold: the final count check.
	cut := binary.LittleEndian.AppendUint32(nil, 3)
	cut = append(cut, "/bin/plumb\x00\x00\x00\x00\x00\x00plumb\x00daemo"...)
	if _, args, err := ParseProcArgs(cut); err == nil {
		t.Fatalf("a 3-argument buffer cut inside its second argument was read as %q", args)
	}
}
