package cli

import (
	"encoding/binary"
	"reflect"
	"testing"
)

// procArgs2 builds a kern.procargs2 buffer the way darwin lays it out.
func procArgs2(argc int, exe string, args ...string) []byte {
	buf := binary.LittleEndian.AppendUint32(nil, uint32(argc)) //nolint:gosec // G115: a test fixture's small argc
	buf = append(buf, exe...)
	buf = append(buf, 0, 0, 0, 0) // the exec path's NUL and its padding
	for _, a := range args {
		buf = append(buf, a...)
		buf = append(buf, 0)
	}
	return append(buf, "PATH=/usr/bin\x00"...) // the environment follows
}

func TestParseProcArgs2(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want []string
		ok   bool
	}{
		{"a daemon", procArgs2(2, "/opt/homebrew/bin/plumb", "/opt/homebrew/bin/plumb", "daemon"), []string{"/opt/homebrew/bin/plumb", "daemon"}, true},
		// The case `ps` got wrong: a space inside one argument stays inside it.
		{"a path with a space", procArgs2(2, "/Users/a b/plumb", "/Users/a b/plumb", "daemon"), []string{"/Users/a b/plumb", "daemon"}, true},
		{"a quoted value is one argument", procArgs2(3, "/x/plumb", "plumb", "--msg", "hello daemon"), []string{"plumb", "--msg", "hello daemon"}, true},
		{"argc beyond the buffer", procArgs2(5, "/x/plumb", "plumb"), nil, false},
		{"argc zero", procArgs2(0, "/x/plumb"), nil, false},
		{"truncated header", []byte{1, 0}, nil, false},
		{"no exec path terminator", binary.LittleEndian.AppendUint32(nil, 1), nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseProcArgs2(c.buf)
			if ok != c.ok || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseProcArgs2 = %q, %v; want %q, %v", got, ok, c.want, c.ok)
			}
		})
	}
}
