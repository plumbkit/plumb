package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unicode"

	"github.com/plumbkit/plumb/internal/ignore"
)

// collectSearchPaths walks root and returns the scan candidates, plus the
// workspace-relative paths of any entries the walk withheld for resolving
// outside the boundary (see escapesBoundary).
func (t *SearchInFiles) collectSearchPaths(ctx context.Context, a searchInFilesArgs, root string) ([]searchPathPair, []string, error) {
	var withheld []string
	opts := walkOptions{
		root:          root,
		includeHidden: a.IncludeHidden,
		respectIgnore: true,
		boundary:      t.guard,
		onWithheld: func(abs string) {
			rel, err := filepath.Rel(root, abs)
			if err != nil {
				rel = filepath.Base(abs)
			}
			withheld = append(withheld, filepath.ToSlash(rel))
		},
	}
	globPrefix := globLiteralPrefix(a.Glob)
	var paths []searchPathPair
	walkErr := walk(ctx, opts, func(path string, d fs.DirEntry, _ int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return searchDirFilter(path, root, a, globPrefix)
		}
		if !searchFileFilter(path, d, a, root) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		paths = append(paths, searchPathPair{abs: path, rel: filepath.ToSlash(rel)})
		return nil
	})
	return paths, withheld, walkErr
}

// searchDirFilter returns fs.SkipDir when the directory can be pruned from the
// walk based on glob prefix or exclude patterns.
func searchDirFilter(path, root string, a searchInFilesArgs, globPrefix string) error {
	if path == root {
		return nil
	}
	if globPrefix == "" && len(a.Exclude) == 0 {
		return nil
	}
	rel, _ := filepath.Rel(root, path)
	relSlash := filepath.ToSlash(rel)
	if globPrefix != "" && !dirCompatibleWithPrefix(relSlash, globPrefix) {
		return fs.SkipDir
	}
	for _, excl := range a.Exclude {
		if m, _ := doubleStarMatchFile(excl, relSlash); m {
			return fs.SkipDir
		}
	}
	return nil
}

// searchFileFilter reports whether the file passes glob, exclude, and size
// filters and should be included in the scan candidate list.
func searchFileFilter(path string, d fs.DirEntry, a searchInFilesArgs, root string) bool {
	if a.Glob != "" {
		matched, err := filepath.Match(a.Glob, d.Name())
		if err != nil || !matched {
			rel, _ := filepath.Rel(root, path)
			if m, _ := doubleStarMatchFile(a.Glob, filepath.ToSlash(rel)); !m {
				return false
			}
		}
	}
	if len(a.Exclude) > 0 {
		rel, _ := filepath.Rel(root, path)
		relSlash := filepath.ToSlash(rel)
		for _, excl := range a.Exclude {
			if m, _ := doubleStarMatchFile(excl, relSlash); m {
				return false
			}
		}
	}
	if fi, err := d.Info(); err == nil && fi.Size() > a.MaxFileBytes {
		return false
	}
	return true
}

// searchScanFile opens, sniffs, and scans a single file for regex matches.
// Returns nil when the file is binary, unreadable, or has no hits and no
// oversized lines.
func searchScanFile(p searchPathPair, re *regexp.Regexp, contextLines int) *searchFileMatch {
	f, err := os.Open(p.abs)
	if err != nil {
		return nil
	}
	defer f.Close()

	sniff := make([]byte, binarySniffBytes)
	n, _ := f.Read(sniff)
	if bytes.IndexByte(sniff[:n], 0) >= 0 {
		return nil
	}

	lineNo := 1
	skippedLines := 0
	scanner := bufio.NewScanner(io.MultiReader(bytes.NewReader(sniff[:n]), f))
	scanner.Buffer(make([]byte, 64*1024), 2*searchMaxLineBytes)
	scanner.Split(makeSearchLineSplit(&skippedLines, &lineNo))

	if contextLines == 0 {
		return scanDirect(p, scanner, re, &lineNo, &skippedLines)
	}
	return scanWithContext(p, scanner, re, contextLines, &lineNo, &skippedLines)
}

func scanDirect(p searchPathPair, scanner *bufio.Scanner, re *regexp.Regexp, lineNo, skippedLines *int) *searchFileMatch {
	var hitNums []int
	var formatted []string
	for scanner.Scan() {
		data := scanner.Bytes()
		if re.Match(data) {
			hitNums = append(hitNums, *lineNo)
			formatted = append(formatted, fmt.Sprintf("  %d:> %s", *lineNo, strings.TrimRight(string(data), "\r")))
		}
		*lineNo++
	}
	if scanner.Err() != nil {
		*skippedLines++
	}
	return buildSearchFileMatch(p, formatted, hitNums, *skippedLines)
}

func scanWithContext(p searchPathPair, scanner *bufio.Scanner, re *regexp.Regexp, contextLines int, lineNo, skippedLines *int) *searchFileMatch {
	var hitNums []int
	var formatted []string
	ring := make([]searchLine, contextLines)
	ringCount, ringHead := 0, 0
	postContextRemaining := 0
	lastEmitted := 0

	for scanner.Scan() {
		data := scanner.Bytes()
		if re.Match(data) {
			hitNums = append(hitNums, *lineNo)
			flushRing(ring, ringCount, ringHead, contextLines, &lastEmitted, &formatted)
			ringCount = 0
			emitMatchLine(*lineNo, data, &lastEmitted, &formatted)
			postContextRemaining = contextLines
		} else if postContextRemaining > 0 {
			emitContextLine(*lineNo, data, &lastEmitted, &formatted)
			postContextRemaining--
		} else {
			pushRing(ring, *lineNo, data, &ringHead, &ringCount, contextLines)
		}
		*lineNo++
	}
	if scanner.Err() != nil {
		*skippedLines++
	}
	return buildSearchFileMatch(p, formatted, hitNums, *skippedLines)
}

func flushRing(ring []searchLine, ringCount, ringHead, contextLines int, lastEmitted *int, formatted *[]string) {
	if ringCount <= 0 {
		return
	}
	startIdx := (ringHead - ringCount + contextLines) % contextLines
	for k := range ringCount {
		item := ring[(startIdx+k)%contextLines]
		if item.number > *lastEmitted {
			*formatted = append(*formatted, fmt.Sprintf("  %d:  %s", item.number, strings.TrimRight(string(item.data), "\r")))
			*lastEmitted = item.number
		}
	}
}

func emitMatchLine(lineNo int, data []byte, lastEmitted *int, formatted *[]string) {
	if lineNo > *lastEmitted {
		trimmed := strings.TrimRight(string(data), "\r")
		*formatted = append(*formatted, fmt.Sprintf("  %d:> %s", lineNo, trimmed))
		*lastEmitted = lineNo
	}
}

func emitContextLine(lineNo int, data []byte, lastEmitted *int, formatted *[]string) {
	if lineNo > *lastEmitted {
		trimmed := strings.TrimRight(string(data), "\r")
		*formatted = append(*formatted, fmt.Sprintf("  %d:  %s", lineNo, trimmed))
		*lastEmitted = lineNo
	}
}

func pushRing(ring []searchLine, lineNo int, data []byte, ringHead, ringCount *int, contextLines int) {
	buf := ring[*ringHead].data
	if cap(buf) >= len(data) {
		buf = buf[:len(data)]
	} else {
		buf = make([]byte, len(data))
	}
	copy(buf, data)
	ring[*ringHead] = searchLine{number: lineNo, data: buf}
	*ringHead = (*ringHead + 1) % contextLines
	if *ringCount < contextLines {
		*ringCount++
	}
}

func buildSearchFileMatch(p searchPathPair, formatted []string, hitNums []int, skippedLines int) *searchFileMatch {
	if len(hitNums) == 0 {
		if skippedLines > 0 {
			return &searchFileMatch{relPath: p.rel, skippedLines: skippedLines}
		}
		return nil
	}
	return &searchFileMatch{
		relPath:      p.rel,
		absPath:      p.abs,
		lines:        formatted,
		hitLineNums:  hitNums,
		hits:         len(hitNums),
		skippedLines: skippedLines,
	}
}

// makeSearchLineSplit returns a bufio.SplitFunc that strips CRLF line endings
// and skips lines exceeding searchMaxLineBytes in-place (increments *skippedLines
// and *lineNo for each skipped line) rather than aborting the scan.
func makeSearchLineSplit(skippedLines, lineNo *int) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line := trimCRSuffix(data[:i])
			if len(line) > searchMaxLineBytes {
				*skippedLines++
				*lineNo++
				return i + 1, nil, nil
			}
			return i + 1, line, nil
		}
		if atEOF {
			line := trimCRSuffix(data)
			if len(line) > searchMaxLineBytes {
				*skippedLines++
				return len(data), nil, nil
			}
			return len(data), line, nil
		}
		return 0, nil, nil
	}
}

// trimCRSuffix removes a trailing \r byte.
func trimCRSuffix(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

func (t *SearchInFiles) runParallelScan(ctx context.Context, paths []searchPathPair, a searchInFilesArgs, re *regexp.Regexp) ([]*searchFileMatch, int, int, bool) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := max(1, min(runtime.NumCPU(), len(paths)))
	pathsCh := make(chan searchPathPair)
	resultsCh := make(chan *searchFileMatch, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for p := range pathsCh {
				if wctx.Err() != nil {
					continue
				}
				if r := searchScanFile(p, re, a.ContextLines); r != nil {
					select {
					case resultsCh <- r:
					case <-wctx.Done():
						return
					}
				}
			}
		})
	}

	go func() {
		defer close(pathsCh)
		for _, p := range paths {
			select {
			case <-wctx.Done():
				return
			case pathsCh <- p:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var results []*searchFileMatch
	totalLines, totalSkipped := 0, 0
	truncated := false
	for r := range resultsCh {
		totalSkipped += r.skippedLines
		if r.hits == 0 {
			continue
		}
		if truncated {
			continue // drain so workers can exit
		}
		results = append(results, r)
		totalLines += r.hits
		if totalLines >= a.MaxResults {
			truncated = true
			cancel()
		}
	}
	return results, totalLines, totalSkipped, truncated
}

type searchLine struct {
	number int
	data   []byte
}

// allLower reports whether s contains no uppercase Unicode letters (smart-case).
func allLower(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// doubleStarMatchFile matches a glob that may contain ** — and, since
// filepath.Match has no brace syntax of its own, brace alternation — against a
// slash-separated path. The path matches when ANY expanded alternative matches.
func doubleStarMatchFile(glob, path string) (bool, error) {
	alts, err := expandBraces(glob)
	if err != nil {
		return false, err
	}
	for _, alt := range alts {
		m, matchErr := doubleStarMatchOne(alt, path)
		if matchErr != nil {
			return false, matchErr
		}
		if m {
			return true, nil
		}
	}
	return false, nil
}

// doubleStarMatchOne matches a single brace-free glob against a path.
func doubleStarMatchOne(glob, path string) (bool, error) {
	// Try base name first for simple globs.
	base := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		base = path[idx+1:]
	}
	if m, err := filepath.Match(glob, base); m || err != nil {
		return m, err
	}
	return ignore.DoubleStarMatch(glob, path), nil
}
