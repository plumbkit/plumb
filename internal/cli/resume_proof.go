package cli

// resume_proof.go — the proof that makes the identity hook's binding verifiable
// (docs/identity-resume-credential-design.md section 3, "Presentation").
//
// `plumb serve` presents a stored resume credential for the conversation a session_start
// names. A conversation id is a claim: Claude Code's identity hook overwrites it with the
// real conversation, but a client with no hook lets the model type it, and a serve that
// presented on a typed claim would hand conversation A's credential to conversation B. So
// the proxy presents only when it can tell the hook did the naming.
//
// The hook and the serve are both the user's own processes, and share one secret the model
// never sees: a per-user key in a 0600 file under plumb's state directory. The hook adds
//
//	plumb_hook_proof = base64url(HMAC-SHA256(key, "resume-v1\x00" + stamp))
//
// to a session_start's arguments beside the stamp it writes, and the serve recomputes it
// from the stamp it reads. A proof that verifies means whoever wrote that stamp could read
// the key. The proof is bound to the stamp, not to the call: it does not stop a process
// that can read the user's key, or one that lifts a (stamp, proof) pair out of another
// conversation's transcript. Both are the same-user boundary the threat model already
// states (A6); what it does stop is a model that types a conversation id.
//
// Every failure here is quiet and safe in one direction: the hook fails open (no proof,
// the call is stamped as before) and the serve fails closed (no verified proof, no
// presentation, which is the name-only resume that shipped before the credential).

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/plumbkit/plumb/internal/paths"
)

const (
	resumeProofKeyLen  = 32
	resumeProofContext = "resume-v1\x00"
	resumeProofKeyFile = "hook-proof.key"
)

// resumeProofKeyPath is where the key lives: beside the proxy's credential store, under
// the user's state directory and never in a workspace.
func resumeProofKeyPath() string {
	return filepath.Join(paths.StateDir(), "serve", resumeProofKeyFile)
}

// resumeProofFor is the proof for a stamp: base64url, unpadded.
func resumeProofFor(key []byte, stamp string) string {
	return base64.RawURLEncoding.EncodeToString(resumeProofMAC(key, stamp))
}

func resumeProofMAC(key []byte, stamp string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(resumeProofContext + stamp))
	return mac.Sum(nil)
}

// verifyResumeProof reports whether proof is the proof for stamp under key. The compare
// is constant-time. An empty stamp or proof, a key of the wrong length, and a proof that
// is not base64url never verify.
func verifyResumeProof(key []byte, stamp, proof string) bool {
	if len(key) != resumeProofKeyLen || stamp == "" || proof == "" {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(proof)
	if err != nil {
		return false
	}
	return hmac.Equal(got, resumeProofMAC(key, stamp))
}

// readResumeProofKey reads the key file. It refuses anything but a regular file of exactly
// the key's length that no other user can read: a key that was ever group- or
// world-readable is not a secret, and a wrong-sized file is not ours.
func readResumeProofKey(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("resume proof key %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("resume proof key %s is readable by other users (mode %o)", path, fi.Mode().Perm())
	}
	key, err := os.ReadFile(path) //nolint:gosec // G304: plumb's own key file under its state directory
	if err != nil {
		return nil, err
	}
	if len(key) != resumeProofKeyLen {
		return nil, fmt.Errorf("resume proof key %s holds %d bytes, want %d", path, len(key), resumeProofKeyLen)
	}
	return key, nil
}

// loadResumeProofKey reads the key without making one: the serve's side, where a missing
// key just means no hook has ever proved anything.
func loadResumeProofKey() ([]byte, error) { return readResumeProofKey(resumeProofKeyPath()) }

// ensureResumeProofKey reads the key, making it first when it does not exist: the side
// that installs the hook, and the hook itself on its first run. The key is written to a
// temporary file and linked into place, so a hook racing another creates no partial file
// and no key is overwritten; both read back whichever one landed.
func ensureResumeProofKey() ([]byte, error) {
	path := resumeProofKeyPath()
	key, err := readResumeProofKey(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	fresh := make([]byte, resumeProofKeyLen)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, "hook-proof-*.tmp") // 0600
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(fresh); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return readResumeProofKey(path)
}
