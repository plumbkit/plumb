// smoke is a one-shot diagnostic tool: register a fake session, list it, unregister.
// Run: go run ./cmd/smoke
// Delete this package once IPC is verified working.
package main

import (
	"fmt"
	"os"

	"github.com/golimpio/plumb/internal/session"
)

func main() {
	dir, err := session.Dir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "session.Dir:", err)
		os.Exit(1)
	}
	fmt.Println("session dir:", dir)

	id, err := session.Register(session.Info{
		Language: "go",
		Folder:   "/tmp/test-project",
		Adapter:  "gopls",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}
	fmt.Println("registered:", id)

	sessions, err := session.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list:", err)
		os.Exit(1)
	}
	fmt.Printf("listed %d session(s)\n", len(sessions))
	for _, s := range sessions {
		fmt.Printf("  id=%-28s  language=%-6s  pid=%d\n", s.ID, s.Language, s.PID)
	}

	session.Unregister(id)
	after, _ := session.List()
	fmt.Printf("after unregister: %d session(s)\n", len(after))
}
