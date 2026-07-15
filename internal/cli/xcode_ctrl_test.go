package cli

import (
	"strings"
	"testing"
)

func TestHandleCtrlConnXcodeStatus(t *testing.T) {
	ln := testCtrlListener(t)
	var got string
	go serveControlSocket(ln, "info", "text", ctrlHandlers{
		xcodeStatus: func(workspace string) string {
			got = workspace
			return `{"State":"warming","Detail":"restarting"}` + "\n"
		},
	})

	const root = "/Users/example/My Xcode App"
	resp := sendCtrlFull(t, ln, "xcode-status "+root)
	if got != root {
		t.Fatalf("workspace = %q, want %q", got, root)
	}
	if !strings.Contains(resp, `"State":"warming"`) {
		t.Fatalf("response = %q", resp)
	}
}
