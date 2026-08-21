package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var readMultipleFilesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "paths": {
      "type": "array",
      "description": "Absolute paths, file:// URIs, or workspace-relative paths of files to read.",
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
type ReadMultipleFiles struct {
	guard BoundaryGuard
	ws    WorkspaceFn // may be nil; anchors workspace-relative entries in paths
}

func NewReadMultipleFiles() *ReadMultipleFiles { return &ReadMultipleFiles{} }

func (t *ReadMultipleFiles) WithBoundary(guard BoundaryGuard) *ReadMultipleFiles {
	t.guard = guard
	return t
}

// WithWorkspace wires the pinned-workspace accessor so relative entries in
// paths resolve against the workspace root, matching read_file. Nil-safe.
func (t *ReadMultipleFiles) WithWorkspace(ws WorkspaceFn) *ReadMultipleFiles {
	t.ws = ws
	return t
}

func (*ReadMultipleFiles) Name() string                 { return "read_multiple_files" }
func (*ReadMultipleFiles) InputSchema() json.RawMessage { return readMultipleFilesSchema }
func (*ReadMultipleFiles) Description() string {
	return "Read up to 20 files in a single call. Each file's content is returned " +
		"under a '### <path>' heading, followed by that file's own read_file header " +
		"(mtime, sha256, line and byte counts) so it can be edited without re-reading. " +
		"Errors for individual " +
		"files are reported inline — one unreadable file doesn't block the others. " +
		"Accepts absolute paths, file:// URIs, or workspace-relative paths. Binary files are detected and skipped. " +
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
		return "", errors.New("read_multiple_files: paths must not be empty")
	}
	if len(a.Paths) > 20 {
		return "", fmt.Errorf("read_multiple_files: at most 20 paths per call, got %d", len(a.Paths))
	}

	type result struct {
		content string
		err     error
	}
	results := make([]result, len(a.Paths))
	reader := (&ReadFile{}).WithBoundary(t.guard).WithWorkspace(t.ws)

	sem := make(chan struct{}, readMultipleFilesParallelism)
	var wg sync.WaitGroup
	for i, p := range a.Paths {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			raw, _ := json.Marshal(map[string]string{"file_path": p})
			out, err := reader.Execute(ctx, raw)
			results[i] = result{content: out, err: err}
		}()
	}
	wg.Wait()

	// No separator rule. It used to be strings.Repeat("─", 60) — and U+2500 is 3
	// bytes in UTF-8, so each rule cost 180 bytes. On a three-file read that was
	// 543 bytes, 17% of the entire response, spent on decoration.
	//
	// The "### " heading is the boundary marker, and it is sufficient precisely
	// because of the line-number gutter below it: file CONTENT containing a
	// markdown heading renders as "  1\t### Subsection", indented and numbered,
	// while a real boundary starts at column 0. That is what the rule was
	// disambiguating, and the gutter already does it for free.
	//
	// The byte count is gone too, and its removal is a correctness fix rather than
	// a saving: it printed len(r.content), the length of read_file's RENDERED
	// output — header and gutters included — not the size of the file. A 677-byte
	// file was announced as "933 bytes", one line above its own header stating
	// chars=675 baseline=677. Three numbers, and the prominent one meant nothing.
	// The provenance line carries the real figures.
	var sb strings.Builder
	for i, p := range a.Paths {
		if i > 0 {
			sb.WriteString("\n")
		}
		r := results[i]
		if r.err != nil {
			fmt.Fprintf(&sb, "### %s\n### ERROR: %s\n", p, r.err.Error())
			continue
		}
		fmt.Fprintf(&sb, "### %s\n", p)
		sb.WriteString(r.content)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}
