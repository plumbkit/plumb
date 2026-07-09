package tools

import (
	"reflect"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func namedSym(name string) protocol.DocumentSymbol {
	return protocol.DocumentSymbol{Name: name, Kind: protocol.SKFunction}
}

func TestSuggestSymbols(t *testing.T) {
	base := []protocol.DocumentSymbol{
		namedSym("fsWatcher"),
		namedSym("WatcherConfig"),
		namedSym("startIndexer"),
	}
	cases := []struct {
		name  string
		syms  []protocol.DocumentSymbol
		query string
		want  []string
	}{
		{
			name:  "substring hit",
			syms:  base,
			query: "Watcher",
			want:  []string{"fsWatcher", "WatcherConfig"},
		},
		{
			name:  "typo within distance 2",
			syms:  base,
			query: "fsWatchr",
			want:  []string{"fsWatcher"},
		},
		{
			name:  "case-insensitive exact-ish",
			syms:  base,
			query: "fswatcher",
			want:  []string{"fsWatcher"},
		},
		{
			name:  "no match",
			syms:  base,
			query: "Zebra",
			want:  nil,
		},
		{
			// Substring hits lead, ordered by ascending edit distance (doc
			// order breaking ties); the distance-only hit ranks last and the
			// cap drops it.
			name: "cap at 3 with ranking order",
			syms: []protocol.DocumentSymbol{
				namedSym("watcheX"),   // distance 1, not a substring — capped out
				namedSym("Watcher1"),  // substring, distance 1
				namedSym("fsWatcher"), // substring, distance 2
				namedSym("Watcher2"),  // substring, distance 1
			},
			query: "watcher",
			want:  []string{"Watcher1", "Watcher2", "fsWatcher"},
		},
		{
			// Non-substring hits order by ascending edit distance, not
			// document order.
			name: "distance hits rank ascending",
			syms: []protocol.DocumentSymbol{
				namedSym("watchers"), // distance 2
				namedSym("watcher"),  // distance 1
			},
			query: "watchr",
			want:  []string{"watcher", "watchers"},
		},
		{
			name: "nested method found via flatten",
			syms: []protocol.DocumentSymbol{
				{
					Name: "Server",
					Kind: protocol.SKStruct,
					Children: []protocol.DocumentSymbol{
						namedSym("handleConn"),
					},
				},
			},
			query: "handleconn",
			want:  []string{"handleConn"},
		},
		{
			// gopls flat Go methods are suggested in the dotted form the
			// resolver accepts, and a dotted typo query still lands.
			name: "dotted query against gopls flat method",
			syms: []protocol.DocumentSymbol{
				namedSym("(*Server).Start"),
			},
			query: "Server.Strt",
			want:  []string{"Server.Start"},
		},
		{
			name:  "never suggests the query itself",
			syms:  []protocol.DocumentSymbol{namedSym("Watcher")},
			query: "Watcher",
			want:  nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := suggestSymbols(c.syms, c.query)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("suggestSymbols(%q) = %v, want %v", c.query, got, c.want)
			}
		})
	}
}

func TestDidYouMean(t *testing.T) {
	if got := didYouMean(nil); got != "" {
		t.Errorf("didYouMean(nil) = %q, want empty", got)
	}
	got := didYouMean([]string{"fsWatcher", "WatcherConfig"})
	want := " Did you mean: `fsWatcher`, `WatcherConfig`? Use find_symbol for a fuzzy search."
	if got != want {
		t.Errorf("didYouMean = %q, want %q", got, want)
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"kitten", "sitting", 3},
		{"fswatchr", "fswatcher", 1},
		{"same", "same", 0},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
