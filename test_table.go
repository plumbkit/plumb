package main

import (
	"fmt"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
)

func main() {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderRow(true).
		BorderColumn(true)

	t.Row("edits", "strict\nrate_limit_per_minute", "false\n120")
	t.Row("cache", "ttl\nmax_size\nlog_level", "5m0s\n1000\ninfo")

	fmt.Println(t.Render())
}
