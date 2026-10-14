package cli

import (
	"bytes"
	"encoding/binary"
)

// parseProcArgs2 decodes darwin's kern.procargs2 buffer: a native-endian int32
// argc, the executable path, NUL padding, then argc NUL-terminated arguments
// (the environment follows and is ignored). It lives in an untagged file so
// every platform's test run pins the decoding, not only darwin's.
//
// Every darwin target plumb ships (arm64, amd64) is little-endian. A malformed
// or truncated buffer answers false: an argv that cannot be read is never
// guessed at, because a guessed "yes" here is a signal sent to a stranger.
func parseProcArgs2(buf []byte) ([]string, bool) {
	if len(buf) < 4 {
		return nil, false
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	rest := buf[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 || argc <= 0 {
		return nil, false
	}
	rest = rest[end:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	argv := make([]string, 0, min(argc, 64))
	for len(argv) < argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, false
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	return argv, true
}
