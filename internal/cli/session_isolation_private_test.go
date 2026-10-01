package cli

import (
	"errors"
	"strconv"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// TestPerTestDataDirGivesAPrivateRegistry pins what TestMain relies on: a test
// that sets its own XDG_DATA_HOME gets a session registry of its own, not one
// shared with every other test in the binary.
//
// It matters because a shared registry made CI flaky (#554): sessions that tests
// leave live accumulate there, the random name draw is from a pool of about six
// thousand, and a test that renames onto a fixed name such as "gentle-mink" was
// refused whenever an earlier test's session had drawn it.
//
// Both rounds take the same fixed name and leave their session live, as most
// tests' sessions are. They can only both succeed if each registry is private.
func TestPerTestDataDirGivesAPrivateRegistry(t *testing.T) {
	for round := range 2 {
		t.Run(strconv.Itoa(round), func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			_, err := session.Register(session.Info{Name: "private-registry-probe", Folder: t.TempDir()})
			if errors.Is(err, session.ErrNameTaken) {
				t.Fatal("the name was already held: this test's registry is shared with an earlier one")
			}
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
		})
	}
}
