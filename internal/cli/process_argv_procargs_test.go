package cli

import (
	"encoding/binary"
	"reflect"
	"testing"
)

// procArgs2 builds a kern.procargs2 buffer as darwin lays it out: argc, the exec
// path padded so path, terminator and padding fill a multiple of 8, the arguments,
// then the environment.
func procArgs2(exe string, args ...string) []byte {
	buf := binary.LittleEndian.AppendUint32(nil, uint32(len(args))) //nolint:gosec // G115: a test fixture's small argc
	buf = append(buf, exe...)
	for range 8 - len(exe)%8 {
		buf = append(buf, 0)
	}
	for _, a := range args {
		buf = append(buf, a...)
		buf = append(buf, 0)
	}
	return append(buf, "PATH=/usr/bin\x00"...)
}

func TestArgvFromProcArgs(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want []string
		ok   bool
	}{
		{"a daemon", procArgs2("/opt/homebrew/bin/plumb", "/opt/homebrew/bin/plumb", "daemon"), []string{"/opt/homebrew/bin/plumb", "daemon"}, true},
		{"a path with a space", procArgs2("/Users/a b/plumb", "/Users/a b/plumb", "daemon"), []string{"/Users/a b/plumb", "daemon"}, true},
		{"an exec path filling its 8 bytes exactly", procArgs2("/x/plumb", "plumb", "daemon"), []string{"plumb", "daemon"}, true},
		// The PLAN-552 review's case: an empty argv[0] is not padding. Read as
		// padding, this argv became ["plumb", "daemon", "PATH=/usr/bin"].
		{"an empty argv[0]", procArgs2("/x/plumbd", "", "plumb", "daemon"), []string{"", "plumb", "daemon"}, true},
		{"argc beyond the buffer is cannot-read", binary.LittleEndian.AppendUint32(nil, 3), nil, false},
		{"a truncated header is cannot-read", []byte{1, 0}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := argvFromProcArgs(c.buf)
			if ok != c.ok || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argvFromProcArgs = %q, %v; want %q, %v", got, ok, c.want, c.ok)
			}
		})
	}
	if argv, _ := argvFromProcArgs(procArgs2("/x/plumbd", "", "plumb", "daemon")); processIsPlumbDaemon(argv) {
		t.Fatalf("an empty-argv[0] process reads as a plumb daemon: %q", argv)
	}
}
