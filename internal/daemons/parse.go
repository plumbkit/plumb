package daemons

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// The parsers below are pure: they take the bytes a platform's probe read and
// return what those bytes mean. They live in an untagged file on purpose, so a
// test run on any platform covers every one of them — the arithmetic of a
// /proc/<pid>/stat header is where a census goes quietly wrong, and "only the
// Linux CI job runs that test" is not a good enough place for it.

// The two platform markers that separate a process which has exec'd from one
// which has only forked. They matter for the same reason on both platforms and
// are defined here, untagged, so the rules can be tested wherever the census
// runs; the numbers themselves are named by the kernels (PF_FORKNOEXEC in
// /proc/<pid>/stat field 9; P_EXEC in XNU's ExternProc.P_flag).
const (
	// pfForkNoExec is PF_FORKNOEXEC (0x40). A forked child keeps its parent's
	// argv until it execs, so without this a daemon starting gopls or git looks
	// like a second daemon.
	pfForkNoExec = 0x40
	// pExec is XNU's P_EXEC: "this image has been exec'd". Verified rather than
	// trusted — on macOS arm64 a running process reports P_flag 0x00004004 while
	// its own fork-without-exec child reports 0x00000004, so 0x4000 is exactly the
	// bit that separates them.
	pExec = 0x4000
)

// forkedNotYetExec reports whether a /proc/<pid>/stat flags value carries
// PF_FORKNOEXEC: the process has forked but not exec'd, so its argv still
// belongs to its parent and it must not be counted as a daemon. It lives here,
// untagged, so the rule itself is covered by a test on every platform rather
// than only where /proc exists.
func forkedNotYetExec(flags uint64) bool { return flags&pfForkNoExec != 0 }

// hasExecd reports whether an ExternProc.P_flag value carries XNU's P_EXEC: the
// macOS counterpart of the test above, and here for the same reason. It takes
// the field's own signed type so no conversion is needed at the call site.
func hasExecd(flag int32) bool { return flag&pExec != 0 }

// isPlumbDaemonArgv reports whether these arguments are a plumb daemon's, by the
// process's OWN argv: argv[0]'s basename must be exactly `plumb` and argv[1]
// exactly `daemon`. A substring match is not enough — `grep plumb daemon`, a
// shell wrapper, and `plumb debug daemons` itself all contain the words, and
// matching them would make the census report daemons that are not there.
func isPlumbDaemonArgv(args []string) bool {
	if len(args) < 2 {
		return false
	}
	return filepath.Base(args[0]) == "plumb" && args[1] == "daemon"
}

// statFlagsField and statStartField are the 0-based indexes of those fields in
// the slice statFields returns, which starts at the field after comm: the kernel
// numbers state as field 3 (so flags is 9 and starttime 22).
const (
	statFlagsField = 6
	statStartField = 19
)

// statFields splits /proc/<pid>/stat after its header and checks that the header
// names this pid. comm can hold spaces and parentheses, so the header ends at
// the LAST ") " on the line; a line without that header, or naming another pid,
// is not trusted — a census that guessed here would be wrong about which
// processes are daemons.
func statFields(pid int, s string) ([]string, error) {
	open := strings.IndexByte(s, '(')
	shut := strings.LastIndex(s, ") ")
	if open < 0 || shut < 0 || shut < open {
		return nil, errors.New("no `pid (comm) ` header")
	}
	if got, err := strconv.Atoi(strings.TrimSpace(s[:open])); err != nil || got != pid {
		return nil, fmt.Errorf("header names pid %q, not %d", strings.TrimSpace(s[:open]), pid)
	}
	fields := strings.Fields(s[shut+2:])
	if len(fields) <= statStartField {
		return nil, fmt.Errorf("only %d fields after the header, need %d", len(fields), statStartField+1)
	}
	return fields, nil
}

// parseProcNetUnix returns the kernel inode of the unix socket bound at path,
// from the text of /proc/net/unix. A line is
//
//	0000: 00000002 00000000 00010000 0001 01 12345 /run/plumb/plumb.sock
//
// so the inode is field 6 and the bound path field 7. A socket that exists as a
// file but is not bound (or is abstract, whose path starts with @) has no
// matching line, which is not an error: nothing is listening on it.
func parseProcNetUnix(data, path string) (uint64, bool) {
	for line := range strings.SplitSeq(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[0] == "Num" || f[7] != path {
			continue
		}
		ino, err := strconv.ParseUint(f[6], 10, 64)
		if err != nil {
			continue
		}
		return ino, true
	}
	return 0, false
}

// parseProcArgs decodes one process's KERN_PROCARGS2 payload: a 4-byte argc, the
// exec path, padding NULs, then argc NUL-terminated arguments. The padding is
// why the argument scan resumes at the first non-NUL byte rather than straight
// after the path.
//
// Anything that does not parse is an error, never a partial answer: a caller
// that cannot read a daemon's argv must say so rather than report a daemon with
// no arguments.
func parseProcArgs(raw []byte) (exe string, args []string, err error) {
	if len(raw) < 4 {
		return "", nil, errors.New("shorter than its own argc field")
	}
	argc := binary.LittleEndian.Uint32(raw[:4])
	if argc == 0 || argc > 4096 {
		return "", nil, fmt.Errorf("implausible argc %d", argc)
	}
	rest := raw[4:]
	end := indexByte(rest, 0)
	if end < 0 {
		return "", nil, errors.New("no NUL-terminated exec path")
	}
	exe = string(rest[:end])
	rest = rest[end:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for range argc {
		end := indexByte(rest, 0)
		if end < 0 {
			// The kernel does not promise a trailing NUL on the last argument.
			end = len(rest)
		}
		args = append(args, string(rest[:end]))
		if end >= len(rest) {
			break
		}
		rest = rest[end+1:]
	}
	return exe, args, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// psLineMentionsPlumb reports whether a `ps` command line is worth confirming
// with the process's own arguments. It is deliberately looser than the argv
// match: on darwin ps is only a cheap PREFILTER, because its joined command line
// cannot be split into arguments when a path contains a space and because the
// kernel's own argv is the authority (kern.procargs2). Anything this lets
// through is confirmed or refused by that second read.
func psLineMentionsPlumb(command string) bool { return strings.Contains(command, "plumb") }

// lockHeldBy reports whether /proc/locks shows a flock on the file whose
// "major:minor:inode" identity is want. Only existence is asked here, never
// identity: /proc/locks names the process that ACQUIRED a lock, which after
// #649's fd-3 handover is the launcher rather than the daemon that holds it.
// "Somebody holds it" is exactly what the kernel can say, and that is what this
// answers.
func lockHeldBy(data, want string) bool {
	for line := range strings.SplitSeq(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 || f[1] != "FLOCK" {
			continue
		}
		if f[5] == want {
			return true
		}
	}
	return false
}

// parseLsofOpeners maps each wanted path to the pids that have it open, from the
// output of `lsof -w -F pn`. The field groups are one record per line: `p<pid>`
// starts a process, and every `n<name>` after it is a file that process holds.
//
// A name is matched under both its literal and its resolved spelling, because
// lsof reports what the kernel holds: on macOS that is /private/var/... for a
// path spelled /var/....
func parseLsofOpeners(out string, wanted map[string]bool) map[string][]int {
	// Every wanted path is indexed under both spellings, so it does not matter
	// which side of the comparison carries the resolution: the caller holds the
	// lexical path (it built the path itself) and lsof holds the kernel's, and
	// either of them can be the resolved one.
	index := make(map[string]string, len(wanted)*2)
	for p := range wanted {
		index[p] = p
		if r := paths.Canonical(p); r != p {
			index[r] = p
		}
	}
	owners := make(map[string][]int)
	pid := 0
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'n':
			name := line[1:]
			if pid <= 0 {
				continue
			}
			key, ok := index[name]
			if ok {
				owners[key] = append(owners[key], pid)
			}
		}
	}
	return owners
}
