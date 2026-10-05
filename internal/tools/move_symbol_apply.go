package tools

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/history"
)

// movePlan is one file's share of a move: its pre- and post-move bytes, its
// mode, and whether it existed before (a created destination is removed on
// rollback rather than restored).
type movePlan struct {
	path          string
	before        []byte
	after         []byte
	mode          os.FileMode
	existedBefore bool
	written       fileSnapshot
}

func beforeSide(p movePlan) history.Side {
	if p.existedBefore {
		return history.SideFromBytes(p.before)
	}
	return history.Side{}
}

// applyMovePlans writes each plan in order and rolls every prior write back on a
// mid-sequence failure — a created file is removed, an existing file restored to
// its pre-move bytes — keeping a two-file move all-or-nothing at the filesystem
// level. onApplied runs after all writes succeed. The caller holds the per-path
// locks for every plan (see applyMove); this helper performs no locking so it is
// also directly unit-testable.
func applyMovePlans(plans []movePlan, onApplied func(), sink historySink) ([]string, error) {
	var written []movePlan
	for i, p := range plans {
		res, err := safeWrite(p.path, p.after, p.mode)
		if err != nil {
			if rbErr := rollbackMove(written, sink); rbErr != nil {
				return nil, fmt.Errorf("writing %s: %w; rollback failed: %w", p.path, err, rbErr)
			}
			return nil, withRevertNote(fmt.Errorf("writing %s: %w", p.path, err), moveReverted(written))
		}
		plans[i].written = res.written
		op := history.OpUpdate
		if !p.existedBefore {
			op = history.OpCreate
		}
		sink.recordHistory(history.Change{
			Op:     op,
			Tool:   "move_symbol",
			Path:   p.path,
			Before: beforeSide(p),
			After:  history.SideFromBytes(p.after),
		})
		written = append(written, p)
	}
	if onApplied != nil {
		onApplied()
	}
	out := make([]string, len(plans))
	for i, p := range plans {
		out[i] = p.path
	}
	return out, nil
}

// moveReverted lists the paths a move rollback put back, for the failed call's
// revert summary.
func moveReverted(written []movePlan) []revertedPath {
	out := make([]revertedPath, 0, len(written))
	for _, p := range written {
		out = append(out, revertedPath{path: p.path, before: string(p.before), after: string(p.after)})
	}
	return out
}

func rollbackMove(written []movePlan, sink historySink) error {
	var errs []string
	for i := len(written) - 1; i >= 0; i-- {
		p := written[i]
		if !p.existedBefore {
			if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Sprintf("%s: %v", p.path, err))
			} else {
				sink.recordHistory(history.Change{
					Op:             history.OpRevert,
					Tool:           "move_symbol",
					Path:           p.path,
					Before:         history.SideFromBytes(p.after),
					RevertsOwnCall: true,
					Reason:         "move_rollback",
				})
			}
			continue
		}
		if _, err := safeWrite(p.path, p.before, p.mode); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p.path, err))
		} else {
			sink.recordHistory(history.Change{
				Op:             history.OpRevert,
				Tool:           "move_symbol",
				Path:           p.path,
				Before:         history.SideFromBytes(p.after),
				After:          history.SideFromBytes(p.before),
				RevertsOwnCall: true,
				Reason:         "move_rollback",
			})
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
