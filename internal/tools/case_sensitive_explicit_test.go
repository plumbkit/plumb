package tools

import (
	"testing"
)

// case_sensitive is a *bool so that "unset" (apply smart-case) is
// distinguishable from an explicit false (force case-insensitive). search_in_files
// and read_file used to collapse the two and then re-apply smart-case, so
// `case_sensitive: false` was a silent no-op for any pattern containing an
// uppercase letter — the search ran case-SENSITIVE and reported "No matches".
// find_replace and search_memories always honoured the explicit value; these
// tests pin all four on the same contract.
//
// The -i / ignore_case alias rewrites to exactly `case_sensitive: false`, so the
// bug turned an explicit "ignore case" request into a confident wrong answer.

func TestSmartCase_ExplicitFalseForcesInsensitive_SearchInFiles(t *testing.T) {
	// Uppercase pattern: smart-case would choose case-SENSITIVE.
	re, err := compileSearchRegex(searchInFilesArgs{Pattern: "FSYNC-BEFORE-ACK", CaseSensitive: boolPtr(false)})
	if err != nil {
		t.Fatalf("compileSearchRegex: %v", err)
	}
	if !re.MatchString(`the "fsync-before-ack" write contract`) {
		t.Error("case_sensitive:false must force an insensitive match on an uppercase pattern")
	}
}

// The full matrix: pattern case × parameter state × haystack case. Each row
// carries its own haystack, so the smart-case DEFAULT is exercised in both
// directions — a lowercase pattern matching an uppercase haystack (insensitive)
// and an uppercase pattern failing against a lowercase one (sensitive).
func TestSmartCase_SearchInFilesMatrix(t *testing.T) {
	tests := []struct {
		name      string
		pattern   string
		haystack  string
		cs        *bool
		wantMatch bool
	}{
		// Unset → smart-case. All-lowercase pattern is case-INSENSITIVE, so it
		// matches an uppercase haystack.
		{"unset + lowercase pattern → insensitive, matches UPPER haystack", "fsync", "FSYNC ENABLED", nil, true},
		{"unset + lowercase pattern matches lowercase haystack", "fsync", "fsync enabled", nil, true},
		// Unset + uppercase pattern → case-SENSITIVE.
		{"unset + uppercase pattern → sensitive, no match on lower", "FSYNC", "fsync enabled", nil, false},
		{"unset + uppercase pattern matches UPPER haystack", "FSYNC", "FSYNC ENABLED", nil, true},
		// Explicit false → insensitive regardless of pattern case (the fix).
		{"explicit false + uppercase pattern → insensitive", "FSYNC", "fsync enabled", boolPtr(false), true},
		{"explicit false + lowercase pattern → insensitive", "fsync", "FSYNC ENABLED", boolPtr(false), true},
		// Explicit true → sensitive regardless of pattern case.
		{"explicit true + lowercase pattern → sensitive", "fsync", "fsync enabled", boolPtr(true), true},
		{"explicit true + lowercase pattern → sensitive, no match on UPPER", "fsync", "FSYNC ENABLED", boolPtr(true), false},
		{"explicit true + uppercase pattern → sensitive, no match on lower", "FSYNC", "fsync enabled", boolPtr(true), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re, err := compileSearchRegex(searchInFilesArgs{Pattern: tt.pattern, CaseSensitive: tt.cs})
			if err != nil {
				t.Fatalf("compileSearchRegex: %v", err)
			}
			if got := re.MatchString(tt.haystack); got != tt.wantMatch {
				t.Errorf("match(%q, %q) = %v, want %v", tt.pattern, tt.haystack, got, tt.wantMatch)
			}
		})
	}
}

func TestSmartCase_ExplicitFalseForcesInsensitive_ReadFile(t *testing.T) {
	re, err := compileReadFilePattern("FSYNC", false, boolPtr(false))
	if err != nil {
		t.Fatalf("compileReadFilePattern: %v", err)
	}
	if !re.MatchString("fsync enabled") {
		t.Error("case_sensitive:false must force an insensitive match on an uppercase pattern")
	}
	// Unset keeps smart-case: an uppercase pattern stays case-sensitive.
	re, err = compileReadFilePattern("FSYNC", false, nil)
	if err != nil {
		t.Fatalf("compileReadFilePattern: %v", err)
	}
	if re.MatchString("fsync enabled") {
		t.Error("unset case_sensitive must keep smart-case (uppercase pattern → sensitive)")
	}
}

// find_replace and search_memories were already correct — pinned here so the
// four tools cannot drift apart again.
func TestSmartCase_ExplicitFalseHonouredByFindReplaceAndMemories(t *testing.T) {
	a := findReplaceArgs{Pattern: "FSYNC", CaseSensitive: boolPtr(false)}
	applyFindReplaceDefaults(&a)
	if a.caseSensitive {
		t.Error("find_replace: case_sensitive:false must force insensitive")
	}

	re, err := buildMemoryRegex("FSYNC", false, boolPtr(false))
	if err != nil {
		t.Fatalf("buildMemoryRegex: %v", err)
	}
	if !re.MatchString("fsync") {
		t.Error("search_memories: case_sensitive:false must force insensitive")
	}
}
