package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// WorkingDirError reports that a command's working directory cannot be entered:
// it does not exist, or is not a directory.
//
// It exists because os/exec reports neither against the directory. The child's
// chdir fails with ENOENT (or ENOTDIR), and the error reads
// `fork/exec /opt/homebrew/bin/go: no such file or directory` — naming a binary
// that exists and sending the caller hunting for a PATH problem (#522).
type WorkingDirError struct {
	Dir    string
	NotDir bool // the path exists but is not a directory
}

func (e *WorkingDirError) Error() string {
	if e.NotDir {
		return fmt.Sprintf("working directory %s is not a directory", e.Dir)
	}
	return fmt.Sprintf("working directory %s does not exist", e.Dir)
}

// checkWorkingDir refuses a working directory the child could not chdir into.
// "" means "inherit the daemon's", which always exists. Any other stat failure
// (a permissions blip) is left to exec, whose error then describes it.
func checkWorkingDir(dir string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &WorkingDirError{Dir: dir}
	case err == nil && !info.IsDir():
		return &WorkingDirError{Dir: dir, NotDir: true}
	}
	return nil
}

// workingDirOrigin is the " (from <setting>)" suffix a run_task or mutation_test
// refusal adds when the command could not start because its working directory
// is missing and the resolver named the setting that produced it — so the
// remedy points at the config line, not only at the path it expanded to.
func workingDirOrigin(err error, cmd TaskCommand) string {
	var wd *WorkingDirError
	if cmd.WorkingDirSource == "" || !errors.As(err, &wd) {
		return ""
	}
	return " (from " + cmd.WorkingDirSource + ")"
}
