package tools

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// legacyFormatHitLines is the frozen pre-optimization formatting oracle.
// In the original implementation, `i == h` was checked against the outer loop's
// hit index `h`. As a consequence, if hit B fell inside hit A's context window,
// hit B was rendered with the context prefix ("  ") rather than the match
// prefix ("> ") because `shown[i]` blocked subsequent re-emission.
func legacyFormatHitLines(lines []searchLine, hitLineIdxs []int, contextLines int) []string {
	var formatted []string
	shown := make(map[int]bool)
	for _, h := range hitLineIdxs {
		lo := max(0, h-contextLines)
		hi := min(len(lines)-1, h+contextLines)
		for i := lo; i <= hi; i++ {
			if shown[i] {
				continue
			}
			shown[i] = true
			prefix := "  "
			if i == h {
				prefix = "> "
			}
			formatted = append(formatted,
				fmt.Sprintf("  %d:%s%s", lines[i].number, prefix, strings.TrimRight(string(lines[i].data), "\r")))
		}
	}
	return formatted
}

// legacySearchScanFile is the frozen pre-optimization whole-file-buffering
// scan preserved for differential testing to guarantee zero behavioural drift.
func legacySearchScanFile(p searchPathPair, re *regexp.Regexp, contextLines int) *searchFileMatch {
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

	var hitLineIdxs []int
	var lines []searchLine
	lineNo := 1
	skippedLines := 0

	scanner := bufio.NewScanner(io.MultiReader(bytes.NewReader(sniff[:n]), f))
	scanner.Buffer(make([]byte, 64*1024), 2*searchMaxLineBytes)
	scanner.Split(makeSearchLineSplit(&skippedLines, &lineNo))

	for scanner.Scan() {
		data := scanner.Bytes()
		cp := make([]byte, len(data))
		copy(cp, data)
		idx := len(lines)
		lines = append(lines, searchLine{number: lineNo, data: cp})
		if re.Match(cp) {
			hitLineIdxs = append(hitLineIdxs, idx)
		}
		lineNo++
	}
	if scanner.Err() != nil {
		skippedLines++
	}

	if len(hitLineIdxs) == 0 {
		if skippedLines > 0 {
			return &searchFileMatch{relPath: p.rel, skippedLines: skippedLines}
		}
		return nil
	}

	hitNums := make([]int, len(hitLineIdxs))
	for i, h := range hitLineIdxs {
		hitNums[i] = lines[h].number
	}
	return &searchFileMatch{
		relPath:      p.rel,
		absPath:      p.abs,
		lines:        legacyFormatHitLines(lines, hitLineIdxs, contextLines),
		hitLineNums:  hitNums,
		hits:         len(hitLineIdxs),
		skippedLines: skippedLines,
	}
}

func TestSearchInFiles_DifferentialScanParity(t *testing.T) {
	cases := []struct {
		name         string
		content      string
		pattern      string
		contextLines int
	}{
		{
			name:         "empty file",
			content:      "",
			pattern:      "needle",
			contextLines: 0,
		},
		{
			name:         "empty file with context",
			content:      "",
			pattern:      "needle",
			contextLines: 3,
		},
		{
			name:         "no match context 0",
			content:      "line1\nline2\nline3\n",
			pattern:      "needle",
			contextLines: 0,
		},
		{
			name:         "no match context 2",
			content:      "line1\nline2\nline3\n",
			pattern:      "needle",
			contextLines: 2,
		},
		{
			name:         "single match context 0",
			content:      "alpha\nbeta\nneedle\ngamma\ndelta\n",
			pattern:      "needle",
			contextLines: 0,
		},
		{
			name:         "single match context 1",
			content:      "alpha\nbeta\nneedle\ngamma\ndelta\n",
			pattern:      "needle",
			contextLines: 1,
		},
		{
			name:         "single match context 3 (exceeds file boundary)",
			content:      "alpha\nbeta\nneedle\ngamma\n",
			pattern:      "needle",
			contextLines: 3,
		},
		{
			name:         "match at first line context 2",
			content:      "needle\nline2\nline3\nline4\n",
			pattern:      "needle",
			contextLines: 2,
		},
		{
			name:         "match at last line context 2",
			content:      "line1\nline2\nline3\nneedle\n",
			pattern:      "needle",
			contextLines: 2,
		},
		{
			name:         "match at last line no trailing newline",
			content:      "line1\nline2\nline3\nneedle",
			pattern:      "needle",
			contextLines: 1,
		},
		{
			name:         "adjacent matches context 0",
			content:      "one\nneedle 1\nneedle 2\nfour\n",
			pattern:      "needle",
			contextLines: 0,
		},
		{
			name:         "separated clusters context 1",
			content:      "line 1\nneedle A\nline 3\nline 4\nline 5\nline 6\nneedle B\nline 8\n",
			pattern:      "needle",
			contextLines: 1,
		},
		{
			name:         "crlf endings context 2",
			content:      "line 1\r\nline 2\r\nneedle\r\nline 4\r\nline 5\r\n",
			pattern:      "needle",
			contextLines: 2,
		},
		{
			name:         "binary file ignored",
			content:      "binary \x00 file with needle",
			pattern:      "needle",
			contextLines: 1,
		},
		{
			name:         "oversized line split skip with match",
			content:      strings.Repeat("x", searchMaxLineBytes+1) + "\nneedle after long line\n",
			pattern:      "needle",
			contextLines: 1,
		},
		{
			name:         "oversized line scanner abort",
			content:      strings.Repeat("x", 2*searchMaxLineBytes+100),
			pattern:      "needle",
			contextLines: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fpath := filepath.Join(dir, "sample.txt")
			if err := os.WriteFile(fpath, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			p := searchPathPair{abs: fpath, rel: "sample.txt"}
			re := regexp.MustCompile(tc.pattern)

			legacyRes := legacySearchScanFile(p, re, tc.contextLines)
			newRes := searchScanFile(p, re, tc.contextLines)

			if legacyRes == nil && newRes == nil {
				return
			}
			if (legacyRes == nil) != (newRes == nil) {
				t.Fatalf("nil mismatch: legacy=%v, new=%v", legacyRes, newRes)
			}

			if legacyRes.hits != newRes.hits {
				t.Errorf("hits mismatch: legacy=%d, new=%d", legacyRes.hits, newRes.hits)
			}
			if legacyRes.skippedLines != newRes.skippedLines {
				t.Errorf("skippedLines mismatch: legacy=%d, new=%d", legacyRes.skippedLines, newRes.skippedLines)
			}
			if !reflect.DeepEqual(legacyRes.hitLineNums, newRes.hitLineNums) {
				t.Errorf("hitLineNums mismatch:\nlegacy: %v\nnew:    %v", legacyRes.hitLineNums, newRes.hitLineNums)
			}
			if !reflect.DeepEqual(legacyRes.lines, newRes.lines) {
				t.Errorf("lines mismatch:\nlegacy:\n%s\nnew:\n%s",
					strings.Join(legacyRes.lines, "\n"),
					strings.Join(newRes.lines, "\n"))
			}
		})
	}
}

// TestSearchInFiles_OverlappingMatchesMarkAllHitsWithArrow demonstrates and pins
// the user-visible fix for overlapping/adjacent hits: in the legacy implementation,
// a hit within an earlier hit's context window was output with the context prefix
// "  " instead of the match indicator "> ". The streaming implementation correctly
// marks every hit line with "> ".
func TestSearchInFiles_OverlappingMatchesMarkAllHitsWithArrow(t *testing.T) {
	cases := []struct {
		name         string
		content      string
		pattern      string
		contextLines int
		wantLegacy   []string
		wantNew      []string
	}{
		{
			name:         "adjacent matches context 1",
			content:      "one\nneedle 1\nneedle 2\nfour\n",
			pattern:      "needle",
			contextLines: 1,
			wantLegacy: []string{
				"  1:  one",
				"  2:> needle 1",
				"  3:  needle 2", // Legacy defect: hit marked as context
				"  4:  four",
			},
			wantNew: []string{
				"  1:  one",
				"  2:> needle 1",
				"  3:> needle 2", // Fixed: hit marked with >
				"  4:  four",
			},
		},
		{
			name:         "adjacent matches context 2",
			content:      "one\ntwo\nneedle 1\nneedle 2\nfive\nsix\n",
			pattern:      "needle",
			contextLines: 2,
			wantLegacy: []string{
				"  1:  one",
				"  2:  two",
				"  3:> needle 1",
				"  4:  needle 2", // Legacy defect
				"  5:  five",
				"  6:  six",
			},
			wantNew: []string{
				"  1:  one",
				"  2:  two",
				"  3:> needle 1",
				"  4:> needle 2", // Fixed
				"  5:  five",
				"  6:  six",
			},
		},
		{
			name:         "overlapping windows context 2",
			content:      "line 1\nline 2\nneedle 1\nline 4\nneedle 2\nline 6\nline 7\n",
			pattern:      "needle",
			contextLines: 2,
			wantLegacy: []string{
				"  1:  line 1",
				"  2:  line 2",
				"  3:> needle 1",
				"  4:  line 4",
				"  5:  needle 2", // Legacy defect
				"  6:  line 6",
				"  7:  line 7",
			},
			wantNew: []string{
				"  1:  line 1",
				"  2:  line 2",
				"  3:> needle 1",
				"  4:  line 4",
				"  5:> needle 2", // Fixed
				"  6:  line 6",
				"  7:  line 7",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fpath := filepath.Join(dir, "sample.txt")
			if err := os.WriteFile(fpath, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			p := searchPathPair{abs: fpath, rel: "sample.txt"}
			re := regexp.MustCompile(tc.pattern)

			legacyRes := legacySearchScanFile(p, re, tc.contextLines)
			if !reflect.DeepEqual(legacyRes.lines, tc.wantLegacy) {
				t.Fatalf("legacy behaviour drifted:\ngot:  %v\nwant: %v", legacyRes.lines, tc.wantLegacy)
			}

			newRes := searchScanFile(p, re, tc.contextLines)
			if !reflect.DeepEqual(newRes.lines, tc.wantNew) {
				t.Fatalf("new behaviour does not match expected golden:\ngot:  %v\nwant: %v", newRes.lines, tc.wantNew)
			}
		})
	}
}

// TestSearchInFiles_ZeroRetentionNoMatches asserts that scanning a 1,000-line
// zero-match file allocates only bounded setup resources rather than buffering
// the entire file into memory.
func TestSearchInFiles_ZeroRetentionNoMatches(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "large_zero_match.txt")
	var b strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&b, "filler line %d with non-matching content to test memory retention\n", i)
	}
	if err := os.WriteFile(fpath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	p := searchPathPair{abs: fpath, rel: "large_zero_match.txt"}
	re := regexp.MustCompile("needle")

	res := searchScanFile(p, re, 0)
	if res != nil {
		t.Fatalf("expected nil result for zero-match file, got %+v", res)
	}

	allocs := testing.AllocsPerRun(10, func() {
		_ = searchScanFile(p, re, 0)
	})
	// In the zero-retention path, allocation is bounded by file open/read setup.
	// Whole-file buffering of 1,000 lines would exceed 1,000 allocations.
	if allocs > 50 {
		t.Errorf("allocations per run %f exceeded threshold of 50 for 1,000-line zero-match file", allocs)
	}
}
