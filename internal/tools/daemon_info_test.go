package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDaemonInfo_OmitsConfigStatusWhenUnset(t *testing.T) {
	d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now())
	out, err := d.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "config generation") {
		t.Errorf("config status should be omitted when no provider is wired:\n%s", out)
	}
}

func TestDaemonInfo_IncludesConfigStatus(t *testing.T) {
	reloaded := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
		WithConfigStatus(func() ConfigStatus {
			return ConfigStatus{Generation: 5, LastReloaded: reloaded, RestartNeeded: true}
		})
	out, err := d.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "config generation: 5") {
		t.Errorf("missing generation line:\n%s", out)
	}
	if !strings.Contains(out, "restart needed:    yes") {
		t.Errorf("expected restart-needed yes:\n%s", out)
	}
}

func TestDaemonInfo_RestartNeededNo(t *testing.T) {
	d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
		WithConfigStatus(func() ConfigStatus {
			return ConfigStatus{Generation: 1, LastReloaded: time.Now(), RestartNeeded: false}
		})
	out, err := d.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "restart needed:    no") {
		t.Errorf("expected restart-needed no:\n%s", out)
	}
}
