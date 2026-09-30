package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestTaskEnvConfigRows_ShowProvenance pins `plumb config show` for #537: each
// env entry is listed with the layer it came from, and a project entry says it
// is trust-gated.
func TestTaskEnvConfigRows_ShowProvenance(t *testing.T) {
	global := config.Defaults()
	global.Tasks["go"] = config.TasksConfig{Env: map[string]string{"SHARED": "g"}}
	project := config.Defaults()
	project.Tasks["go"] = config.TasksConfig{Env: map[string]string{"SHARED": "g", "GOTMPDIR": "{workspace}/.testcache"}}

	rows := taskEnvConfigRows("go", global, project)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, strings.Join(r, "|"))
	}
	want := "env.GOTMPDIR|{workspace}/.testcache|project config (trust-gated)\nenv.SHARED|g|global config"
	if strings.Join(got, "\n") != want {
		t.Errorf("rows =\n%s\nwant\n%s", strings.Join(got, "\n"), want)
	}
}
