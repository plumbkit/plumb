package mcp

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// TestAgentsToolCount pins the "## Available tools (N)" heading in AGENTS.md to
// the canonical self-test coverage list. The integration parity guard
// (cmd/smoke TestSmoke_ToolListParity) in turn pins that list to the live
// tools/list, so together the two checks close the drift loop: adding or
// removing a tool without updating the documented count fails plain `go test`,
// not just the integration lane.
func TestAgentsToolCount(t *testing.T) {
	data, err := os.ReadFile("../../AGENTS.md")
	if err != nil {
		t.Fatalf("reading AGENTS.md: %v", err)
	}

	re := regexp.MustCompile(`(?m)^## Available tools \((\d+)\)$`)
	matches := re.FindAllStringSubmatch(string(data), -1)
	if len(matches) != 1 {
		t.Fatalf(`expected exactly one "## Available tools (N)" heading in AGENTS.md, found %d`, len(matches))
	}

	documented, err := strconv.Atoi(matches[0][1])
	if err != nil {
		t.Fatalf("parsing documented tool count %q: %v", matches[0][1], err)
	}

	if canonical := len(selftestToolNames()); documented != canonical {
		t.Errorf(`AGENTS.md documents %d tools but the canonical coverage list has %d — update the "## Available tools (N)" heading (and its index bullets) in AGENTS.md alongside the selftest group in selftest_prompt.go`,
			documented, canonical)
	}
}
