//go:build !darwin && !linux

package tools

// sandbox_other.go is the fallback for platforms with no supported OS sandbox
// (currently anything other than macOS or Linux). The command runs unsandboxed;
// require_sandbox lets a cautious user refuse to run at all on such a platform.

func sandboxWrap(argv []string, opts SandboxOpts) ([]string, SandboxStatus) {
	return argv, SandboxStatus{Reason: "no OS sandbox on this platform"}
}
