package tokenise

import (
	"strings"
	"testing"
	"unicode"
)

// referenceSplitIdentifier is the previous multi-pass implementation, retained
// here as the equivalence oracle for the single-pass rewrite (#86). The
// single-pass SplitIdentifier must produce byte-identical output to this over
// every input.
func referenceSplitIdentifier(s string) string {
	if s == "" {
		return ""
	}
	s = strings.NewReplacer("_", " ", "-", " ", ".", " ", "/", " ").Replace(s)
	var buf strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && runes[i-1] != ' ' {
			lowerToUpper := unicode.IsUpper(r) && !unicode.IsUpper(runes[i-1])
			upperSeqToLower := unicode.IsUpper(r) && unicode.IsUpper(runes[i-1]) &&
				i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if lowerToUpper || upperSeqToLower {
				buf.WriteRune(' ')
			}
		}
		buf.WriteRune(unicode.ToLower(r))
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}

// TestSplitIdentifier_MatchesReference exhaustively proves the single-pass
// rewrite is behaviourally identical to the prior implementation. The recursive
// sweep covers every string up to length 4 over an alphabet that exercises
// lower/upper-acronym/digit runes and both separator classes (explicit `_`/`.`
// and whitespace) — exactly where camel/acronym boundaries meet separator
// collapsing and trimming. A hand-picked set adds the remaining separators
// (`-`/`/`), realistic identifiers, and multi-byte runes.
func TestSplitIdentifier_MatchesReference(t *testing.T) {
	alphabet := []rune{'a', 'B', 'c', '1', '_', '.', ' '}

	var sweep func(prefix string, depth int)
	sweep = func(prefix string, depth int) {
		if got, want := SplitIdentifier(prefix), referenceSplitIdentifier(prefix); got != want {
			t.Fatalf("SplitIdentifier(%q) = %q, reference = %q", prefix, got, want)
		}
		if depth == 0 {
			return
		}
		for _, r := range alphabet {
			sweep(prefix+string(r), depth-1)
		}
	}
	sweep("", 4)

	for _, s := range []string{
		"HTTPServer", "parseHTTPRequest", "XMLParser", "workspacePool",
		"HandleRequest", "getURLFromID", "ALLCAPS", "n1n2N3",
		"foo/bar/baz", "a__b", "-leading", "trailing-", "_us_", "a-b.c/d",
		"mixed_Case-and.dotsAndHTTP", "café", "ÉCole", "a\tb", "  spaced  out  ",
	} {
		if got, want := SplitIdentifier(s), referenceSplitIdentifier(s); got != want {
			t.Errorf("SplitIdentifier(%q) = %q, reference = %q", s, got, want)
		}
	}
}

const benchIdent = "parseHTTPRequestFromJSONBody_v2"

func BenchmarkSplitIdentifier(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = SplitIdentifier(benchIdent)
	}
}

func BenchmarkSplitIdentifierReference(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = referenceSplitIdentifier(benchIdent)
	}
}
