package cli

import (
	"fmt"
	"strings"
	"testing"
)

func TestHandleCtrlConn_EnableLSP_Success(t *testing.T) {
	ln := testCtrlListener(t)
	var gotLang string
	fn := func(lang string) (string, error) {
		gotLang = lang
		return "enabled " + lang + " — its server attaches on the next matching file", nil
	}
	go serveControlSocket(ln, "info", "text", ctrlHandlers{enableLSP: fn})

	resp := sendCtrl(t, ln, "enable-lsp rust")
	if gotLang != "rust" {
		t.Errorf("enableLSP called with %q, want rust", gotLang)
	}
	if !strings.HasPrefix(resp, "enabled rust") {
		t.Fatalf("unexpected response %q", resp)
	}
}

func TestHandleCtrlConn_EnableLSP_Error(t *testing.T) {
	ln := testCtrlListener(t)
	fn := func(string) (string, error) { return "", fmt.Errorf("unknown language \"cobol\"") }
	go serveControlSocket(ln, "info", "text", ctrlHandlers{enableLSP: fn})

	resp := sendCtrl(t, ln, "enable-lsp cobol")
	if !strings.HasPrefix(resp, "error:") {
		t.Fatalf("expected error response, got %q", resp)
	}
	if !strings.Contains(resp, "cobol") {
		t.Errorf("error response %q should surface the daemon's message", resp)
	}
}

func TestHandleCtrlConn_EnableLSP_NilHandler(t *testing.T) {
	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", ctrlHandlers{}) // nil enableLSP
	resp := sendCtrl(t, ln, "enable-lsp rust")
	if !strings.HasPrefix(resp, "error:") {
		t.Fatalf("expected error response with nil handler, got %q", resp)
	}
}

// TestRunEnableLSP_EmptyArgRejected confirms the CLI rejects a blank language
// before dialing the daemon (so it never touches a live daemon in a unit test).
func TestRunEnableLSP_EmptyArgRejected(t *testing.T) {
	if err := runEnableLSP(nil, []string{"   "}); err == nil {
		t.Fatal("runEnableLSP should reject a blank language argument before dialing")
	}
}
