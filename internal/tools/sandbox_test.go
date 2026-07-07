package tools

import "testing"

func TestSandboxStatus_String(t *testing.T) {
	cases := []struct {
		s    SandboxStatus
		want string
	}{
		{SandboxStatus{Active: true, Mechanism: "sandbox-exec"}, "active (sandbox-exec)"},
		{SandboxStatus{Reason: "bwrap not found on PATH"}, "inactive — bwrap not found on PATH"},
		{SandboxStatus{}, "inactive"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestSandbox_EmptyArgv(t *testing.T) {
	got, st := Sandbox(nil, SandboxOpts{})
	if got != nil || st.Active {
		t.Fatalf("Sandbox(nil) = %v, %+v; want nil, inactive", got, st)
	}
}
