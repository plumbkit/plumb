package jsonrpc

import (
	"bufio"
	"context"
	"io"
	"runtime"
	"testing"
	"time"
)

// TestConn_ContextCancel_AlwaysSendsCancelRequest_UnderLoad is the regression
// guard for the CI-only cancel-path deadlock: when a Call's request write finished
// and its ctx was cancelled in the same instant, sendCtx's select could pick
// ctx.Done() over the completed write and return ctx.Err() before the response-wait
// select ran — so Call returned without ever emitting $/cancelRequest, and a peer
// waiting for it blocked forever. It surfaced only under scheduling load (the select
// has to wake *after* cancel() for both cases to be ready), so this test pins
// GOMAXPROCS=1 to serialise scheduling and runs the cancel scenario many times,
// each read bounded by a timeout so a regression fails fast instead of hanging the
// suite. Against the pre-fix code it fails within the first handful of iterations.
func TestConn_ContextCancel_AlwaysSendsCancelRequest_UnderLoad(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1)) // serialise to widen the cancel-vs-send window

	for i := 0; i < 3000; i++ {
		pr, pw := io.Pipe()
		cr, cw := io.Pipe()
		conn := NewConn(pr, cw)
		br := bufio.NewReader(cr)

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- conn.Call(ctx, "noreply", nil, nil) }()

		req, err := readMessageTimeout(br, 2*time.Second)
		if err != nil {
			t.Fatalf("iteration %d: reading request frame: %v", i, err)
		}
		if req.Method != "noreply" {
			t.Fatalf("iteration %d: first frame method = %q, want noreply", i, req.Method)
		}

		cancel()

		got, err := readMessageTimeout(br, 2*time.Second)
		if err != nil {
			t.Fatalf("iteration %d: cancelRequest not emitted after ctx cancel: %v", i, err)
		}
		if got.Method != "$/cancelRequest" {
			t.Fatalf("iteration %d: second frame method = %q, want $/cancelRequest", i, got.Method)
		}

		if err := <-errCh; err == nil {
			t.Fatalf("iteration %d: expected Call to return an error after cancel", i)
		}
		_ = conn.Close()
		_ = pw.Close()
		_ = cw.Close()
	}
}

// readMessageTimeout reads one framed message but gives up after d, so a missing
// frame fails the test rather than blocking the suite to its 10-minute timeout.
func readMessageTimeout(br *bufio.Reader, d time.Duration) (wireMessage, error) {
	type res struct {
		m   wireMessage
		err error
	}
	ch := make(chan res, 1)
	go func() {
		m, err := readMessage(br)
		ch <- res{m, err}
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.m, r.err
	case <-timer.C:
		return wireMessage{}, context.DeadlineExceeded
	}
}
