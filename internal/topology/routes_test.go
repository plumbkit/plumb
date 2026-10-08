package topology

import (
	"strings"
	"testing"
)

// TestGoRouteSitesQueryCoversEveryVerb keeps the literal callee list in
// goRouteSitesQuery in step with the verb maps classifyHTTP reads: a verb the
// maps know but the query omits would never reach classification, silently.
func TestGoRouteSitesQueryCoversEveryVerb(t *testing.T) {
	for _, verbs := range []map[string]bool{upperVerbs, chiVerbs} {
		for v := range verbs {
			if !strings.Contains(goRouteSitesQuery, "'"+v+"'") {
				t.Errorf("goRouteSitesQuery omits verb %q", v)
			}
		}
	}
	for _, c := range []string{"Handle", "HandleFunc", "AddCommand", "Use", "Run", "RunE"} {
		if !strings.Contains(goRouteSitesQuery, "'"+c+"'") {
			t.Errorf("goRouteSitesQuery omits %q", c)
		}
	}
}
