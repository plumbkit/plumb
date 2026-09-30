package cli

import (
	"os"
	"testing"
)

// TestStreamArgv_AppliesTaskEnv pins the `plumb build|test|…` half of #537: the
// CLI runs the same stored commands, so it must run them with the same env.
func TestStreamArgv_AppliesTaskEnv(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	stdin := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = stdin }()

	probe := []string{"sh", "-c", `test "${PLUMB_CLI_PROBE-}" = set`}
	if err := streamArgv(t.TempDir(), probe, []string{"PLUMB_CLI_PROBE=set"}); err != nil {
		t.Errorf("the task env did not reach the command: %v", err)
	}
	// Positive control: without the env the same probe fails, so the pass above
	// is the env's doing.
	if err := streamArgv(t.TempDir(), probe, nil); err == nil {
		t.Error("the probe passed with no env — it proves nothing")
	}
}
