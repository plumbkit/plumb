package cli

import (
	"errors"
	"testing"
)

func TestHandleCtrlConn_WebStart(t *testing.T) {
	ln := testCtrlListener(t)
	start := func(port int) (string, error) {
		if port != 9999 {
			return "", errors.New("unexpected port")
		}
		return "http://127.0.0.1:9999/?t=abc", nil
	}
	go serveControlSocket(ln, "info", "text", ctrlHandlers{webStart: start})

	resp := sendCtrl(t, ln, "web-start 9999")
	if resp != "http://127.0.0.1:9999/?t=abc" {
		t.Fatalf("web-start: got %q", resp)
	}
}

func TestHandleCtrlConn_WebStartNoPort(t *testing.T) {
	ln := testCtrlListener(t)
	start := func(port int) (string, error) {
		if port != 0 {
			return "", errors.New("expected default port")
		}
		return "http://127.0.0.1:8870/?t=z", nil
	}
	go serveControlSocket(ln, "info", "text", ctrlHandlers{webStart: start})

	if resp := sendCtrl(t, ln, "web-start"); resp != "http://127.0.0.1:8870/?t=z" {
		t.Fatalf("web-start (no port): got %q", resp)
	}
}

func TestHandleCtrlConn_WebStatusAndStop(t *testing.T) {
	ln := testCtrlListener(t)
	h := ctrlHandlers{
		webStatus: func() string { return "running http://127.0.0.1:8870/?t=q" },
		webStop:   func() error { return nil },
	}
	go serveControlSocket(ln, "info", "text", h)

	if resp := sendCtrl(t, ln, "web-status"); resp != "running http://127.0.0.1:8870/?t=q" {
		t.Fatalf("web-status: got %q", resp)
	}
	if resp := sendCtrl(t, ln, "web-stop"); resp != "ok" {
		t.Fatalf("web-stop: got %q", resp)
	}
}

func TestHandleCtrlConn_WebUnavailable(t *testing.T) {
	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", ctrlHandlers{}) // nil web handlers

	if resp := sendCtrl(t, ln, "web-status"); resp != "error: web server unavailable" {
		t.Fatalf("web-status nil: got %q", resp)
	}
	if resp := sendCtrl(t, ln, "web-start"); resp != "error: web server unavailable" {
		t.Fatalf("web-start nil: got %q", resp)
	}
}

func TestHandleCtrlConn_WebStartInvalidPort(t *testing.T) {
	ln := testCtrlListener(t)
	start := func(int) (string, error) { return "url", nil }
	go serveControlSocket(ln, "info", "text", ctrlHandlers{webStart: start})

	if resp := sendCtrl(t, ln, "web-start notaport"); resp != `error: invalid port "notaport"` {
		t.Fatalf("web-start bad port: got %q", resp)
	}
}
