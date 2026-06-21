package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// TestLogsStream_SurfacesRecentLinesNearEOF guards the log-tail seek fix (#64).
//
// The handler seeks ~8KiB before EOF and drops the partial leading line so the
// client sees recent context immediately. The previous code dropped that
// partial line on a THROWAWAY bufio.Reader. That reader's fill() pulled ~4KiB
// off the fd into its buffer (advancing the fd ~4KiB past the first newline)
// and was then discarded; the freshly-built follow reader started ~4KiB further
// in, so every line in the first half of the 8KiB tail window was SKIPPED —
// exactly the recent context the seek was meant to surface.
//
// This writes a log well over the 8KiB tail window with one numbered marker per
// line and asserts the streamed body has no ~4KiB hole near its start: the
// surfaced line numbers must be contiguous (no large jump), so a marker sitting
// in the middle of the tail window is present, not dropped.
func TestLogsStream_SurfacesRecentLinesNearEOF(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "daemon.log")

	var b strings.Builder
	// ~60 bytes per line; ~400 lines ≫ the 8KiB tail window, so ~130 lines fall
	// inside the tail and every one of them must be surfaced contiguously.
	const lineCount = 400
	for i := 0; i < lineCount; i++ {
		fmt.Fprintf(&b, "MARK-%04d daemon log line padding-padding-padding-pad\n", i)
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if fi, _ := os.Stat(logPath); fi.Size() <= 8*1024 {
		t.Fatalf("test log must exceed the 8KiB tail window, got %d bytes", fi.Size())
	}

	store := config.NewStore(config.Defaults())
	s := New(Deps{Store: store, LogPath: logPath, StartedAt: time.Now()})

	// A short-lived context lets the follow loop drain the tail and then return
	// when ctx.Done() fires, rather than blocking forever.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/stream/logs", nil).WithContext(ctx)
	s.handleLogsStream(w, r)

	marks := extractMarks(w.Body.String())
	if len(marks) == 0 {
		t.Fatal("no marker lines surfaced at all")
	}
	// The last line of the file must always be present.
	if marks[len(marks)-1] != lineCount-1 {
		t.Errorf("last surfaced marker = %d, want %d (final line missing)", marks[len(marks)-1], lineCount-1)
	}

	// The throwaway-reader bug starts the follow reader ~4KiB into the 8KiB tail
	// window, so the FIRST line surfaced after the dropped partial is roughly
	// halfway through the window — every earlier line in the window is lost. A
	// correct tail starts the follow reader right after the dropped partial, so
	// the first surfaced line sits near the start of the window. With ~54-byte
	// lines and a 21,600-byte file, the window opens at line ~249; the bug
	// would skip ahead to line ~325. Assert the first surfaced line is near the
	// window start (a generous bound well below the bug's ~325).
	first := marks[0]
	const windowStartUpperBound = 290 // window opens ~249; bug jumps to ~325
	if first > windowStartUpperBound {
		t.Errorf("first surfaced line = MARK-%04d; want a line near the tail-window start (<= %d). "+
			"A higher value means the throwaway-reader buffer fill skipped the recent lines the seek surfaces.",
			first, windowStartUpperBound)
	}

	// Contiguity among the surfaced lines: once the tail starts, no line is
	// missing through to EOF.
	for i := 1; i < len(marks); i++ {
		if marks[i] != marks[i-1]+1 {
			t.Fatalf("gap in surfaced lines: %d jumps to %d", marks[i-1], marks[i])
		}
	}
}

var markRE = regexp.MustCompile(`MARK-(\d{4})`)

func extractMarks(body string) []int {
	var out []int
	for _, m := range markRE.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
