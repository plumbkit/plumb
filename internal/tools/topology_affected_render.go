package tools

import (
	"fmt"
	"strings"
)

// Rendering for topology_affected: turning the gathered result into the text a
// caller reads, and telling them when that text is short.

// maxNamedTests caps the individually-named tests in the changed package. Past
// a few dozen the list stops informing a decision and starts costing context.
const maxNamedTests = 40

func formatAffectedResult(result *affectedResult, a topologyAffectedArgs, scope TestScope) string {
	if result == nil {
		return "topology_affected: none of the given files or symbols are in the index"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology affected: %d files, %d symbols changed\n",
		len(a.Files), len(a.Symbols))
	sb.WriteString("source=topology — heuristic, biased toward recall: a missed test is worse " +
		"than an extra. A package is reached by containing the change, or by importing a " +
		"package that does; within a reached package every test is listed, because " +
		"co-location cannot say which ones exercise the change. Verify before relying.\n\n")

	if len(result.Tests) == 0 {
		sb.WriteString("likely affected tests: (none found)\n")
		if len(result.Dependents) > 0 {
			fmt.Fprintf(&sb, "\naffected files (%d):\n", len(result.Dependents))
			for _, n := range result.Dependents {
				fmt.Fprintf(&sb, "  %s %s — %s\n", string(n.Kind), n.Name, n.Path)
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	}

	// Aggregate by package. Enumerating every test is what made this response
	// 298 KB for a one-line change: 2,546 lines carrying the same two labels. The
	// unit a caller acts on is the package, because that is the granularity every
	// test runner scopes at, so lead with one row per package — carrying the
	// run_task target where one can be spelled for this workspace.
	pkgs := aggregateTestsByPackage(result.Tests)
	fmt.Fprintf(&sb, "run these packages (%d)%s\n", len(pkgs), runHeaderSuffix(scope))
	for _, p := range pkgs {
		fmt.Fprintf(&sb, "  %-42s %5d tests   %s\n", packageRunLabel(scope, p.Dir), p.Count, p.Reason)
	}

	// Name individual tests only where naming them helps: the packages the change
	// actually landed in. Elsewhere every test carries an identical label, so the
	// list is noise. EVERY changed package is named, not just the first — a caller
	// who changed three files in three packages has no reason to get test names for
	// one of them and a bare count for the others.
	for i := range pkgs {
		p := &pkgs[i]
		if p.Reason != reasonChanged {
			continue
		}
		fmt.Fprintf(&sb, "\ntests in %s (%d):\n", p.Dir, p.Count)
		shown := p.Tests
		if len(shown) > maxNamedTests {
			shown = shown[:maxNamedTests]
		}
		for _, ts := range shown {
			fmt.Fprintf(&sb, "  %s — %s L%d\n", ts.Node.Name, ts.Node.Path, ts.Node.StartLine)
		}
		if rest := p.Count - len(shown); rest > 0 {
			fmt.Fprintf(&sb, "  … (+%d more in this package)\n", rest)
		}
	}

	if result.GraphTruncated {
		sb.WriteString("\n[truncated: dependent discovery hit its traversal budget — " +
			"packages reached only through the dropped part of the graph are NOT listed, " +
			"and max_results cannot recover them]\n")
	}
	if result.Truncated {
		sb.WriteString("\n[truncated: max_results reached — raise max_results for the full package list]\n")
	}
	return withTruncationBanner(strings.TrimRight(sb.String(), "\n"), cutPackagesNotice(result, a))
}

// graphCutNotice is deliberately not the max_results sentence: naming
// max_results as the remedy for a traversal cut sends the caller to a knob that
// cannot widen the walk, and leaves them believing the answer is now complete.
const graphCutNotice = "dependent discovery was cut at its traversal budget. Packages " +
	"reachable only through the dropped part of the dependency graph are NOT listed " +
	"below, and raising max_results will not recover them — narrow the change set instead."

// cutPackagesNotice describes the cut for the leading banner, or "" when the
// answer is complete. Named for what it reports rather than for the word
// "truncate": it shortens no string, and the arch guard that watches for
// re-implemented string truncation is right to read that name as a claim. Both
// causes are reported when both fired: a package cut drops packages the caller
// can get back, a traversal cut drops packages they cannot see at all.
func cutPackagesNotice(result *affectedResult, a topologyAffectedArgs) string {
	var causes []string
	if result.GraphTruncated {
		causes = append(causes, graphCutNotice)
	}
	if result.Truncated {
		causes = append(causes, fmt.Sprintf("packages were cut at max_results=%d. Some packages "+
			"that should be tested are NOT listed below. Raise max_results for the full set.",
			a.MaxResults))
	}
	return strings.Join(causes, " ")
}
