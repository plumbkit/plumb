package tools

// sandbox.go is the OS-level execution sandbox shared by run_command and
// execute_shell_command. It is defence-in-depth layered UNDER the primary
// safety controls (the fixed-argv allow-list for run_command; the explicit
// opt-in + trust gate for execute_shell_command): even a command that should
// not have run is confined by the OS.
//
// The model is a write jail plus optional network denial: reads and process
// execution stay permissive (build tools need the toolchain, the module cache,
// and to fork compilers), writes are confined to a temp/cache set plus the
// workspace when the command opts into allow_writes, and the network is cut only
// when a command sets deny_network. This is deliberately permissive enough that
// `go build` / `go test` / linters run unbroken while a stray write to, say,
// ~/.ssh is refused by the kernel.
//
// Implementations are platform-specific (sandbox_darwin.go via sandbox-exec,
// sandbox_linux.go via bwrap, sandbox_other.go a no-op). When the platform has
// no sandbox, or the sandbox binary is absent, Sandbox returns argv unchanged
// and an inactive status; the caller decides whether require_sandbox forbids
// running unsandboxed.
//
// Concurrency: Sandbox is a pure function of its inputs — safe for concurrent use.

// SandboxOpts describes the confinement for one command run.
type SandboxOpts struct {
	// WorkspaceRoot is the absolute workspace root. Writes are allowed inside it
	// only when AllowWrites is true.
	WorkspaceRoot string
	// AllowWrites permits the command to write inside WorkspaceRoot.
	AllowWrites bool
	// DenyNetwork cuts the command's network access.
	DenyNetwork bool
}

// SandboxStatus reports whether a command was wrapped and by what mechanism.
type SandboxStatus struct {
	Active    bool   // true when argv was wrapped in an OS sandbox
	Mechanism string // "sandbox-exec", "bwrap", or "" when inactive
	Reason    string // when inactive: why (surfaced to the agent + logged)
}

// String renders the status for a tool response line.
func (s SandboxStatus) String() string {
	if s.Active {
		return "active (" + s.Mechanism + ")"
	}
	if s.Reason != "" {
		return "inactive — " + s.Reason
	}
	return "inactive"
}

// Sandbox wraps argv so it runs under the platform OS sandbox with the given
// confinement, returning the wrapped argv and the status. On an unsupported
// platform or when the sandbox binary is missing it returns argv unchanged with
// an inactive status.
func Sandbox(argv []string, opts SandboxOpts) ([]string, SandboxStatus) {
	if len(argv) == 0 {
		return argv, SandboxStatus{Reason: "empty command"}
	}
	return sandboxWrap(argv, opts)
}
