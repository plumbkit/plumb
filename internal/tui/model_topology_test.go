package tui

import "testing"

func TestByteSizeLabel(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{3 * 1024 * 1024, "3.0 MiB"},
	}
	for _, c := range cases {
		if got := byteSizeLabel(c.in); got != c.want {
			t.Errorf("byteSizeLabel(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTopologyDetailRow_OmittedWhenNoIndex(t *testing.T) {
	var m Model // topoStatusOK defaults to false
	if _, ok := m.topologyDetailRow(); ok {
		t.Error("expected no topology row when topoStatusOK is false")
	}
}

func TestTopologyDetailRow_PresentWhenIndexed(t *testing.T) {
	RebuildStyles()
	m := Model{topoStatusOK: true}
	m.topoStatus.TotalNodes = 1234
	m.topoStatus.IndexedFiles = 56
	m.topoStatus.Languages = []string{"go"}
	row, ok := m.topologyDetailRow()
	if !ok {
		t.Fatal("expected a topology row when topoStatusOK is true")
	}
	if row == "" {
		t.Error("expected non-empty topology row")
	}
}
