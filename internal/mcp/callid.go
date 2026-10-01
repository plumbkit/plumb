package mcp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"time"
)

// callIDCtxKey is the context key a tools/call's id travels under. Unexported,
// so only WithCallID can set it.
type callIDCtxKey struct{}

// WithCallID returns ctx carrying id as the current tools/call's id. An empty
// id leaves ctx unchanged.
func WithCallID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, callIDCtxKey{}, id)
}

// CallIDFromCtx returns the tools/call id handleToolsCall minted, or "" outside
// a call. It is the join key between history.changes and stats.tool_calls.
func CallIDFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(callIDCtxKey{}).(string)
	return v
}

// NewCallID returns a 26-character ULID: a 48-bit Unix-millisecond timestamp
// and 80 random bits in Crockford base32, so ids sort by time at millisecond
// resolution. The client's JSON-RPC id is not reused: clients choose and reuse
// it.
func NewCallID() string { return newULID(time.Now(), rand.Reader) }

func newULID(t time.Time, entropy io.Reader) string {
	var b [16]byte
	ms := uint64(t.UnixMilli()) //nolint:gosec // G115: Unix ms is positive for any real clock
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		// crypto/rand does not fail on supported platforms; keep an id anyway.
		binary.BigEndian.PutUint64(b[8:], uint64(t.UnixNano())) //nolint:gosec // G115: bit pattern only
	}
	return encodeCrockford(b)
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encodeCrockford writes the 128-bit value as 26 base32 digits (130 bits, the
// top two zero), most significant first.
func encodeCrockford(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
