//go:build integration

package smoke_test

import (
	"os"
	"testing"

	"github.com/plumbkit/plumb/internal/separateenv"
)

// TestMain refuses on a developer machine before any test builds plumb or
// starts a `plumb serve`, whose daemon would sit beside the developer's own:
// the suite runs in CI or `make verify-full-container` (internal/separateenv).
func TestMain(m *testing.M) {
	separateenv.RequireForTestMain("cmd/smoke")
	os.Exit(m.Run())
}
