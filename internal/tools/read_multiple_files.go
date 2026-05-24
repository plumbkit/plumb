package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

var readMultipleFilesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "paths": {
      "type": "array",
      "description": "Absolute paths or file:// URIs of files to read.",
      "items": { "type": "string" },
      "minItems": 1,
      "maxItems": 20
    }
  },
  "required": ["paths"],
  "additionalProperties": false
}`)

// ReadMultipleFiles reads up to 20 files in a single call, returning each
// file's content separated by a clear header. Errors for individual files are
// reported inline rather than failing the whole call, so a single unreadable
// file doesn't block the others.
//
// Concurrency: Execute is safe for concurrent use.
type ReadMultipleFiles struct{}

func NewReadMultipleFiles() *ReadMultipleFiles { return &ReadMultipleFiles{} }

func (*ReadMultipleFiles) Name() string                 { return "read_multiple_files" }
func (*ReadMultipleFiles) InputSchema() json.RawMessage { return readMultipleFilesSchema }
func (*ReadMultipleFiles) Description() string {
	return "Read up to 20 files in a single call. Each file's content is returned " +
		"with a clear header showing the path and byte count. Errors for individual " +
		"files are reported inline — one unreadable file doesn't block the others. " +
		"Accepts absolute paths or file:// URIs. Binary files are detected and skipped. " +
		"Each file is subject to the same 200 KiB cap as read_file."
}

type readMultipleFilesArgs struct {
	Paths []string `json:"paths"`
}

// readMultipleFilesParallelism caps simultaneous file reads. 8 is a good
// balance: enough to hide latency from cold-cache reads on rotational media,
// low enough not to thrash an SSD's queue depth or exhaust open-fd limits.
const readMultipleFilesParallelism = 8

func (t *ReadMultipleFiles) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a readMultipleFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("read_multiple_files: invalid arguments: %w", err)
	}
	if len(a.Paths) == 0 {
		return "", fmt.Errorf("read_multiple_files: paths must not be empty")
	}
	if len(a.Paths) > 20 {
		return "", fmt.Errorf("read_multiple_files: at most 20 paths per call, got %d", len(a.Paths))
	}

	type result struct {
		content string
		err     error
	}
	results := make([]result, len(a.Paths))
	reader := &ReadFile{}

	sem := make(chan struct{}, readMultipleFilesParallelism)
	var wg sync.WaitGroup
	for i, p := range a.Paths {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			raw, _ := json.Marshal(map[string]string{"path": p})
			out, err := reader.Execute(ctx, raw)
			results[i] = result{content: out, err: err}
		}()
	}
	wg.Wait()

	var sb strings.Builder
	sep := strings.Repeat("─", 60)
	for i, p := range a.Paths {
		if i > 0 {
			sb.WriteString("\n")
		}
		r := results[i]
		if r.err != nil {
			fmt.Fprintf(&sb, "%s\n### %s\n### ERROR: %s\n", sep, p, r.err.Error())
			continue
		}
		fmt.Fprintf(&sb, "%s\n### %s (%d bytes)\n\n", sep, p, len(r.content))
		sb.WriteString(r.content)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}
