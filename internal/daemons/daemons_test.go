package daemons

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeProbe builds a probe from literals, so the census is exercised without a
// process table, a lock file or a socket. Every field left nil is a question
// this fake cannot answer, which is a state the census must handle anyway.
func fakeProbe(procs []Daemon, unknown []string, holders map[string][]int, owners map[string][]int) probe {
	return probe{
		processes: func(int) ([]Daemon, []string) { return procs, unknown },
		owners: func(path string, candidates []int) ([]int, error) {
			source, ok := holders[path]
			if !ok {
				source, ok = owners[path]
			}
			if !ok {
				return nil, nil
			}
			if source == nil {
				return nil, errors.New("unreadable")
			}
			var out []int
			for _, pid := range source {
				for _, c := range candidates {
					if c == pid {
						out = append(out, pid)
						break
					}
				}
			}
			return out, nil
		},
	}
}

const (
	authority = "/data/plumb/daemon.authority.lock"
	socket    = "/run/plumb/plumb.sock"
)

func TestCensus_MergesEverySource(t *testing.T) {
	procs := []Daemon{
		{PID: 100, PPID: 1, Start: time.Unix(1000, 0), Executable: "/usr/local/bin/plumb", Argv: "plumb daemon", Identity: IdentityArgv},
		{PID: 200, PPID: 100, Executable: "/usr/local/bin/plumb", Argv: "plumb daemon", Identity: IdentityArgv},
		{PID: 300, Executable: "/usr/local/bin/plumb", Argv: "plumb daemon", Identity: IdentityArgv},
	}
	// 300 holds the authority; 100 owns the socket. Neither fact is discoverable on
	// its own — the census finds daemons by argv and then asks which of them holds
	// what — so the two answers must land on the right rows.
	p := fakeProbe(procs, nil, map[string][]int{authority: {300}}, map[string][]int{socket: {100}})
	r := census(p, Options{Authority: authority, Sockets: []string{socket}})

	if len(r.Daemons) != 3 {
		t.Fatalf("census found %d processes, want 3: %+v", len(r.Daemons), r.Daemons)
	}
	byPID := map[int]Daemon{}
	for _, d := range r.Daemons {
		byPID[d.PID] = d
	}
	if d := byPID[300]; !d.HoldsAuthority || d.Identity != IdentityAuthority {
		t.Errorf("pid 300 = %+v, want the authority holder", d)
	}
	if d := byPID[100]; !includesSocket(d, socket) || d.Identity != IdentitySocket {
		t.Errorf("pid 100 = %+v, want the socket owner", d)
	}
	if d := byPID[200]; d.HoldsAuthority || d.OwnsSocket != nil {
		t.Errorf("pid 200 = %+v, want neither", d)
	}
	if r.Daemons[0].PID != 100 || r.Daemons[1].PID != 200 || r.Daemons[2].PID != 300 {
		t.Errorf("census did not order by pid: %+v", r.Daemons)
	}
	if len(r.Unknown) != 0 {
		t.Errorf("unexpected unknown reasons: %v", r.Unknown)
	}
}

func TestCensus_IgnoresAHolderOutsideTheCandidates(t *testing.T) {
	// Ownership is asked only about processes the census already found, because a
	// scan of every descriptor on the machine is far too expensive for something
	// an agent triggers routinely. The cost is stated rather than hidden: a
	// process that holds plumb's files while looking like nothing at all is not
	// discovered by Linux's probe. (Darwin's lsof answers for all processes, so it
	// can and does attribute such a process.)
	procs := []Daemon{{PID: 100, Argv: "plumb daemon", Identity: IdentityArgv, HoldsAuthority: true}}
	p := fakeProbe(procs, nil, map[string][]int{authority: {100, 999}}, nil)
	r := census(p, Options{Authority: authority})
	if len(r.Daemons) != 1 || r.Daemons[0].PID != 100 {
		t.Fatalf("census = %+v, want only the argv candidate", r.Daemons)
	}
}

func TestCensus_UnidentifiedHolderIsUnknown(t *testing.T) {
	// The gap the peer ruling covers: ownership is asked only about candidates, so
	// a lock held by a process the census could not identify would otherwise read
	// as "no daemon holds the authority" — a violation report against an innocent
	// daemon. It must read as unknown instead, and the verdict must fail closed.
	procs := []Daemon{{PID: 100, Argv: "plumb daemon", Identity: IdentityArgv}}
	p := fakeProbe(procs, nil, nil, nil)
	p.lockHeld = func(path string) (bool, error) { return path == authority, nil }
	r := census(p, Options{Authority: authority})
	if len(r.Unknown) != 1 || !strings.Contains(r.Unknown[0], "could not identify") {
		t.Fatalf("unknown reasons = %v, want one naming an unidentified holder", r.Unknown)
	}
	if v := r.Verdict(); v.State != StateUnknown {
		t.Errorf("verdict = %v (%s), want UNKNOWN", v.State, v.Detail)
	}

	// An unheld lock is a plain "nobody holds the authority", which the one
	// confirmed daemon without it makes a violation.
	p.lockHeld = func(string) (bool, error) { return false, nil }
	r = census(p, Options{Authority: authority})
	if len(r.Unknown) != 0 {
		t.Fatalf("unknown reasons = %v, want none for an unheld lock", r.Unknown)
	}
	if v := r.Verdict(); v.State != StateViolation {
		t.Errorf("verdict = %v (%s), want VIOLATION", v.State, v.Detail)
	}

	// A source that cannot answer is unknown in its own right.
	p.lockHeld = func(string) (bool, error) { return false, errors.New("no /proc/locks") }
	if r := census(p, Options{Authority: authority}); len(r.Unknown) != 1 {
		t.Errorf("unknown reasons = %v, want one for the unreadable lock table", r.Unknown)
	}
}

func TestCensus_UnreadableSourcesBecomeUnknown(t *testing.T) {
	p := probe{
		processes: func(int) ([]Daemon, []string) { return nil, []string{"cannot read /proc/p/stat"} },
		owners:    func(string, []int) ([]int, error) { return nil, errors.New("no /proc/locks") },
	}
	r := census(p, Options{Authority: authority, Sockets: []string{socket}})
	if len(r.Unknown) != 3 {
		t.Fatalf("unknown reasons = %v, want one per unreadable question", r.Unknown)
	}
	if v := r.Verdict(); v.State != StateUnknown {
		t.Errorf("verdict = %v, want UNKNOWN: %s", v.State, v.Detail)
	}
}

func TestCensus_NoAuthorityNamedIsUnknown(t *testing.T) {
	r := census(fakeProbe(nil, nil, nil, nil), Options{})
	if len(r.Unknown) != 1 {
		t.Fatalf("unknown reasons = %v, want one for the unnamed authority", r.Unknown)
	}
	if v := r.Verdict(); v.State != StateUnknown {
		t.Errorf("verdict = %v, want UNKNOWN", v.State)
	}
}

func TestVerdict(t *testing.T) {
	ok := Daemon{PID: 100, Argv: "plumb daemon", Identity: IdentityArgv, HoldsAuthority: true}
	tests := []struct {
		name  string
		proc  []Daemon
		want  State
		pids  int
		holds int
	}{
		{"no daemon at all", nil, StateNone, 0, 0},
		{"exactly one, holding the authority", []Daemon{ok}, StateOK, 1, 100},
		{"two daemons", []Daemon{ok, {PID: 200, Argv: "plumb daemon", Identity: IdentityArgv}}, StateViolation, 2, 100},
		{
			"one daemon that lost the authority",
			[]Daemon{{PID: 200, Argv: "plumb daemon", Identity: IdentityArgv}},
			StateViolation, 1, 0,
		},
		{"an unconfirmed look-alike alone is not a daemon", []Daemon{{PID: 300, Identity: IdentityCandidate}}, StateNone, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Report{Daemons: tt.proc, Authority: authority}
			v := r.Verdict()
			if v.State != tt.want {
				t.Fatalf("state = %v (%s), want %v", v.State, v.Detail, tt.want)
			}
			if tt.want != StateNone && v.Confirmed != tt.pids {
				t.Errorf("confirmed = %d, want %d", v.Confirmed, tt.pids)
			}
			if tt.want != StateNone && v.AuthorityPID != tt.holds {
				t.Errorf("authority pid = %d, want %d", v.AuthorityPID, tt.holds)
			}
			if tt.want == StateViolation && v.Remedy == "" {
				t.Error("a violation must say what to do about it")
			}
			if tt.want == StateOK && v.Remedy != "" {
				t.Errorf("an OK verdict needs no remedy, got %q", v.Remedy)
			}
		})
	}
}

func TestVerdict_ViolationOutweighsUnknown(t *testing.T) {
	// Two daemons in front of you are a violation however much else could not
	// be read: reporting UNKNOWN here would hide the very thing this exists for.
	r := Report{
		Daemons: []Daemon{
			{PID: 100, Argv: "plumb daemon", Identity: IdentityArgv, HoldsAuthority: true},
			{PID: 200, Argv: "plumb daemon", Identity: IdentityArgv},
		},
		Authority: authority,
		Unknown:   []string{"cannot read the owner of /run/plumb/plumb.sock"},
	}
	if v := r.Verdict(); v.State != StateViolation {
		t.Errorf("state = %v, want VIOLATION (%s)", v.State, v.Detail)
	}
}

func TestVerdict_UnknownBeatsOK(t *testing.T) {
	// "Exactly one daemon" from a census that may have missed processes is not
	// evidence, so it must never be reported as OK.
	r := Report{
		Daemons:   []Daemon{{PID: 100, Argv: "plumb daemon", Identity: IdentityArgv, HoldsAuthority: true}},
		Authority: authority,
		Unknown:   []string{"cannot read /proc/7/stat for a process that still exists"},
	}
	if v := r.Verdict(); v.State != StateUnknown {
		t.Errorf("state = %v, want UNKNOWN (%s)", v.State, v.Detail)
	}
}

// TestVerdict_UnattributedSingleDaemonIsUnknown is the defence in depth: an
// absence of attribution is not evidence against a daemon. One confirmed daemon
// whose lock the census could not attribute reads as UNKNOWN — never as a
// violation carrying the remedy `plumb stop`, which is what a failing lsof used
// to produce against a daemon holding its authority perfectly well.
func TestVerdict_UnattributedSingleDaemonIsUnknown(t *testing.T) {
	procs := []Daemon{{PID: 100, Argv: "plumb daemon", Identity: IdentityArgv}}
	p := probe{
		processes: func(int) ([]Daemon, []string) { return procs, nil },
		// What a failing lsof looks like from here: the ownership question comes
		// back as an error instead of an answer.
		owners: func(string, []int) ([]int, error) {
			return nil, errors.New("lsof: exit status 1: lsof: status error on " + authority + ": Permission denied")
		},
	}
	r := census(p, Options{Authority: authority})
	v := r.Verdict()

	if v.State != StateUnknown {
		t.Fatalf("state = %v (%s), want UNKNOWN: a lock nobody could attribute must never be a violation", v.State, v.Detail)
	}
	if !strings.Contains(v.Detail, "could not confirm") {
		t.Errorf("detail = %q, want it to say the lock could not be confirmed", v.Detail)
	}
	if v.Confirmed != 1 {
		t.Errorf("confirmed = %d, want the daemon still counted", v.Confirmed)
	}
	if strings.Contains(v.Remedy, "plumb stop") {
		t.Errorf("remedy = %q, want no stop instruction: the census could not say this daemon is the wrong one", v.Remedy)
	}

	// The mirror, so the rule cannot be satisfied by never accusing anybody: when
	// the census DID ask and was told nobody holds the lock, one daemon without it
	// is exactly the E1 breach.
	p.owners = func(string, []int) ([]int, error) { return nil, nil }
	r = census(p, Options{Authority: authority})
	if v := r.Verdict(); v.State != StateViolation {
		t.Errorf("state = %v (%s), want VIOLATION for an answered `nobody holds it`", v.State, v.Detail)
	}
}

// includesSocket reports whether a socket path is in the list. It is a function,
// not a method, so Daemon has one receiver kind (pointer) throughout.
func includesSocket(d Daemon, path string) bool {
	for _, s := range d.OwnsSocket {
		if s == path {
			return true
		}
	}
	return false
}
