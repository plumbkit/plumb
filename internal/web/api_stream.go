package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// sseHeaders sets the standard Server-Sent Events response headers and returns
// the flusher, or false if the ResponseWriter cannot stream.
func sseHeaders(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, true
}

// handleMetricsStream pushes a daemon metrics snapshot every second over SSE,
// replacing the TUI's 2 s poll. It ends when the client disconnects.
func (s *Server) handleMetricsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := sseHeaders(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	send := func() bool {
		payload, err := json.Marshal(readMetricsDTO(s.deps.MetricsPath))
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

// handleLogsStream tails the daemon log file and pushes new lines over SSE. It
// seeks near the end of the file so the client gets recent context, then follows
// appends. It ends when the client disconnects.
func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := sseHeaders(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	f, err := os.Open(s.deps.LogPath) //nolint:gosec // G304: path is the daemon's own log file
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: log file unavailable\n\n")
		flusher.Flush()
		return
	}
	defer f.Close()

	if info, statErr := f.Stat(); statErr == nil {
		// Start a little before EOF so the client sees recent lines immediately.
		const tailBytes = 8 * 1024
		if info.Size() > tailBytes {
			_, _ = f.Seek(-tailBytes, 2)
			_, _ = bufio.NewReader(f).ReadString('\n') // discard the partial first line
		}
	}

	reader := bufio.NewReader(f)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			payload, _ := json.Marshal(line)
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", payload); werr != nil {
				return
			}
			flusher.Flush()
			continue
		}
		if err != nil { // EOF — wait for more, or client disconnect
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	}
}
