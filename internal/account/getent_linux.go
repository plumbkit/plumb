//go:build linux

package account

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
)

// The getent(1) exec path lives here, tagged for linux ALONE rather than
// `linux && !cgo` like the platformHome that calls it in the release build.
//
// The looser tag is what makes the exec reachable at all. `!cgo` is the shape
// goreleaser builds (.goreleaser.yml sets CGO_ENABLED=0), and it is the shape no
// CI leg builds: a Linux toolchain with a C compiler defaults to CGO_ENABLED=1,
// so on ubuntu-latest and inside scripts/verify-full-container.sh a
// `linux && !cgo` file is dropped from the build entirely and runGetent went on
// never having run. Under `linux` it compiles wherever Linux does, which is what
// lets getent_linux_test.go execute the real binary on every Linux PR.
//
// Production behaviour is identical either way: with cgo, platformHome comes from
// home_linux_cgo.go (os/user, through getpwuid_r) and never calls in here, and
// without cgo the call is the one it always was.

// runGetent asks getent(1) for uid's passwd entry, and reports whether it
// answered. The paths are the absolute getentPaths — never a PATH lookup,
// because PATH is environment and this lookup is the daemon's account anchor —
// and the exit-status rule that turns "no such key" into a refusal lives in
// queryGetent, where every platform's tests can reach it.
func runGetent(ctx context.Context, uid int) (string, bool) {
	return queryGetent(ctx, uid, getentPaths[:], execGetentPath)
}

// execGetentPath runs one getent binary and reports what it did.
//
// A binary that could not be run at all — absent, or killed when the lookup's
// deadline expired — is an error rather than a status, which is what tells
// queryGetent to try the next path. A status means the command RAN: that
// includes the "no such key" status, which is the name service answering rather
// than a reason to ask again.
func execGetentPath(ctx context.Context, path string, uid int) (string, int, error) {
	// G204: path is one of the two absolute getentPaths, this package's own
	// closed list, and the rest of the argv is literal.
	out, err := exec.CommandContext(ctx, path, "passwd", strconv.Itoa(uid)).Output() //nolint:gosec // closed list, literal argv
	if err == nil {
		return string(out), 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode(), nil
	}
	return "", 0, err
}
