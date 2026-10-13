//go:build unix

package cli

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// The check that makes dialling a socket recorded in the authority metadata safe
// away from the runtime tree: the socket must belong to THIS user. A socket that
// another account bound is refused however live it looks — metadata is a record
// of where a daemon claimed to be, and it is worth acting on only because the
// authority lock it lives in is this user's own.
func TestOwnedByCurrentUser_RefusesASocketOfAnotherAccount(t *testing.T) {
	if !ownedByCurrentUser(stubFileInfo{uid: os.Getuid()}) {
		t.Error("a socket this user owns must be trusted")
	}
	if ownedByCurrentUser(stubFileInfo{uid: os.Getuid() + 1}) {
		t.Error("a socket bound by another account must be refused")
	}
}

// stubFileInfo is the minimum os.FileInfo ownedByCurrentUser reads: the unix stat
// behind Sys(). A real socket owned by another account cannot be created from a
// test without root, which is why the ownership check is pinned here.
type stubFileInfo struct{ uid int }

func (f stubFileInfo) Name() string       { return "plumb.sock" }
func (f stubFileInfo) Size() int64        { return 0 }
func (f stubFileInfo) Mode() os.FileMode  { return os.ModeSocket | 0o600 }
func (f stubFileInfo) ModTime() time.Time { return time.Time{} }
func (f stubFileInfo) IsDir() bool        { return false }
func (f stubFileInfo) Sys() any           { return &syscall.Stat_t{Uid: uint32(f.uid)} }
