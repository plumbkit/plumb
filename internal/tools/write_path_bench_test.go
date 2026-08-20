package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Benchmarks for edit_file's write path. There were none: neither safeWrite, nor
// computeEditScript — the one component whose cost is superlinear in its input
// and which runs on EVERY edit — nor the post-write diagnostics path was
// measured, despite `[edits] fsync = false` being documented as existing "for
// benchmarks". These exist so a latency claim about this path can be checked
// rather than argued.

func benchLines(n int, prefix string) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("%s line %d with some representative content", prefix, i)
	}
	return lines
}

// BenchmarkComputeEditScript_SmallEdit is the common case: a large file, a tiny
// change. Myers terminates as soon as it finds the path, so this should stay
// cheap regardless of file size.
func BenchmarkComputeEditScript_SmallEdit(b *testing.B) {
	for _, size := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("lines=%d", size), func(b *testing.B) {
			before := benchLines(size, "a")
			after := append([]string(nil), before...)
			after[size/2] = "changed line"
			b.ResetTimer()
			for b.Loop() {
				_ = computeEditScript(before, after)
			}
		})
	}
}

// BenchmarkComputeEditScript_LargeDistance is the pathological case the bound
// exists for: every line differs, so the true edit distance is O(n+m) and the
// unbounded algorithm allocates O(D²) ints. Compare across sizes to see the
// bound take effect past maxMyersDistance.
func BenchmarkComputeEditScript_LargeDistance(b *testing.B) {
	for _, size := range []int{100, 1000, 3000} {
		b.Run(fmt.Sprintf("lines=%d", size), func(b *testing.B) {
			before := benchLines(size, "old")
			after := benchLines(size, "new")
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				_ = computeEditScript(before, after)
			}
		})
	}
}

// BenchmarkSafeWrite measures the durable write itself — temp file, fsync,
// rename, parent-dir fsync — which is the irreducible floor under any edit.
func BenchmarkSafeWrite(b *testing.B) {
	for _, size := range []int{1 << 10, 64 << 10, 512 << 10} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			dir := b.TempDir()
			path := filepath.Join(dir, "target.txt")
			data := []byte(strings.Repeat("x", size))
			if err := os.WriteFile(path, data, 0o644); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				if _, err := safeWrite(path, data, 0o644); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSummariseEditScript isolates the summary render from the Myers pass,
// since show_write_diff=false skips the RENDER but not the script computation —
// the summary needs the same script, which is why the diff cost cannot be
// configured away.
func BenchmarkSummariseEditScript(b *testing.B) {
	before := benchLines(2000, "a")
	after := append([]string(nil), before...)
	for i := 0; i < len(after); i += 50 {
		after[i] = "changed " + after[i]
	}
	script := computeEditScript(before, after)
	b.ResetTimer()
	for b.Loop() {
		_ = summariseEditScript(script)
	}
}
