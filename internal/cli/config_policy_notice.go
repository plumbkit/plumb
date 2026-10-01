package cli

import (
	"fmt"

	"github.com/plumbkit/plumb/internal/config"
)

// printProjectPolicyNotice states, in one place and in full, what this project's
// config asked for in the capability-granting sections and whether it is in
// force. It prints the requested VALUES, not just the key names, because that is
// what a user needs in order to decide whether to trust them — and because there
// is otherwise nowhere at all to see them: the resolved table above shows the
// value in effect, which for an untrusted request is precisely not what the
// project wrote.
func printProjectPolicyNotice(ws string, st config.ProjectPolicyStatus) {
	if st.Spec.IsEmpty() {
		return
	}
	if st.Trusted {
		fmt.Println(configShowOkStyle().Render(
			fmt.Sprintf("✓ this project's capability-granting config is trusted — %d key(s) in effect:", len(st.Spec))))
		for _, line := range st.Spec.Describe() {
			fmt.Println(configShowMutedStyle().Render("    " + line))
		}
		if st.InheritedFrom != "" {
			fmt.Println(configShowMutedStyle().Render(
				"  the grant is shared from " + st.InheritedFrom + " — this is a linked git worktree of the same repository" +
					"\n  with identical config; revoke it there with `plumb trust --revoke " + st.InheritedFrom + "`"))
		}
		fmt.Println()
		return
	}
	fmt.Println(configShowWarnStyle().Render(
		fmt.Sprintf("! this project's .plumb/config.toml sets %d capability-granting key(s) that are NOT in effect —", len(st.Spec)) +
			"\n  plumb ignores [git] and the exec-deciding [lsp.<lang>] fields from an untrusted project config" +
			"\n  (a cloned repository ships one, and it would otherwise run its own argv on attach):"))
	for _, line := range st.Spec.Describe() {
		fmt.Println(configShowWarnStyle().Render("    " + line))
	}
	fmt.Println(configShowWarnStyle().Render(
		"  → run `plumb trust " + ws + "` to honour them; the values above are what you would be approving"))
	fmt.Println()
}
