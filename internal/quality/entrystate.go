package quality

// entrystate.go classifies one configured [quality] analysers entry, so every
// surface that shows the list — the TUI Settings pane, `plumb doctor`, and the
// startup log — says the same thing about it.
//
// The three of them used to say nothing at all: cli.buildAnalysers dropped an
// unrecognised name mid-loop and no caller ever learned an entry had been
// discarded. Sharing one classifier is what stops those surfaces drifting into
// three different half-answers.

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// EntryStatus is what plumb will do with a configured analyser entry.
type EntryStatus int

const (
	// EntryOK — a recognised name with an adapter and a resolved binary. It will
	// run.
	EntryOK EntryStatus = iota
	// EntryBinaryMissing — plumb supports this analyser but cannot find its
	// executable. Installing it, or pointing [quality.bin] at it, fixes this.
	EntryBinaryMissing
	// EntryUnsupported — the thing named exists, but plumb has no adapter for
	// it: a recognised tool with Implemented=false, or an unrecognised name that
	// nonetheless resolves to an executable. Installing something will not help;
	// only a new adapter will.
	EntryUnsupported
	// EntryUnknown — plumb does not recognise the name and cannot resolve it to
	// an executable either. Typically a typo.
	EntryUnknown
)

// Blocking reports whether the entry names something plumb could have run but
// cannot find — as opposed to something it can see and does not support. The
// distinction drives the display: a missing binary is the user's to fix, an
// unsupported tool is plumb's, and showing them the same way would send the user
// to install a tool that would still not run.
func (s EntryStatus) Blocking() bool {
	return s == EntryBinaryMissing || s == EntryUnknown
}

// EntryState is the classification of one entry.
type EntryState struct {
	// Entry is the string exactly as configured.
	Entry string
	// Status is what plumb will do with it.
	Status EntryStatus
	// Tool is the registry row, valid only when Known.
	Tool Tool
	// Known reports whether Entry matched a registry name.
	Known bool
	// Binary is the resolved executable, empty when none was found.
	Binary string
	// Reason is the full explanation of a non-OK status, for a doctor line or a
	// log field: it names every directory searched, and the working spelling when
	// there is one. It carries no glyphs, so each caller frames it its own way.
	Reason string
	// Short is the same answer in a few words, for a fixed-width column that has
	// no room for Reason. It exists because the TUI's value column is a handful of
	// cells wide and an ellipsis-truncated Reason would cut off exactly the part
	// that distinguishes the statuses — leaving a row that is coloured but says
	// nothing.
	Short string
}

// Language returns the language the entry analyses, or "" when unrecognised.
func (e EntryState) Language() string {
	if !e.Known {
		return ""
	}
	return e.Tool.Language
}

// ClassifyEntry resolves one [quality] analysers entry against the registry and
// the filesystem. bin is the [quality.bin] override map (may be nil).
//
// The order of questions is deliberate. A recognised name is answered from the
// registry first, because whether plumb has an adapter is a fact about plumb and
// does not depend on what the machine has installed: telling a user that eslint
// is "not found" would send them to install eslint, after which it would still
// not run. Only for an unrecognised string does the filesystem get a say, and
// there it answers exactly the question the user is asking — is this thing I
// typed a real executable that plumb simply does not know?
func ClassifyEntry(entry string, bin map[string]string) EntryState {
	st := EntryState{Entry: entry}

	if t, ok := ToolByName(entry); ok {
		st.Tool, st.Known = t, true
		if !t.Implemented {
			st.Status = EntryUnsupported
			st.Reason = "recognised, but plumb has no adapter for it yet"
			st.Short = "no plumb adapter"
			return st
		}
		path, found := LookBinary(t, bin[t.Name])
		if !found {
			st.Status = EntryBinaryMissing
			st.Reason = "executable not found on PATH" + searchedSuffix(t.Ecosystem)
			st.Short = "not found"
			return st
		}
		st.Status, st.Binary = EntryOK, path
		return st
	}

	// Unrecognised. Resolve it as an executable so the two failure modes the
	// user cares about stay distinguishable: a name plumb does not support, and
	// a name that does not exist at all.
	st.Binary, _ = resolveLiteral(entry)
	switch {
	case st.Binary != "":
		st.Status = EntryUnsupported
		st.Reason = "not a recognised analyser name"
		st.Short = "not a plumb analyser"
	default:
		st.Status = EntryUnknown
		st.Reason = "not a recognised analyser name, and no such executable"
		st.Short = "not found"
	}
	if hint, ok := nameHintFor(entry); ok {
		st.Reason += "; entries are names, not paths — use " + strconv.Quote(hint)
		// The hint REPLACES the short label rather than extending it: in a column
		// this narrow, "use \"ruff\"" is the whole answer, and "not a plumb
		// analyser" is just the observation that led to it.
		st.Short = "use the name " + strconv.Quote(hint)
	}
	return st
}

// resolveLiteral resolves an unrecognised entry as an executable: a path is
// stat'd, a bare word goes through PATH.
func resolveLiteral(entry string) (string, bool) {
	if strings.ContainsRune(entry, filepath.Separator) || strings.HasPrefix(entry, "~") {
		return executableAt(paths.ExpandHome(entry))
	}
	if p, err := lookPath(entry); err == nil {
		return p, true
	}
	return "", false
}

// nameHintFor spots the commonest way to get this wrong: pasting the absolute
// path of a tool plumb DOES support. The entry is rejected either way — a path
// must never become an argv plumb runs — but a rejection that names the working
// spelling is the difference between a fix and a guess.
func nameHintFor(entry string) (string, bool) {
	base := filepath.Base(paths.ExpandHome(entry))
	if base == entry {
		return "", false // not a path; nothing to suggest
	}
	if _, ok := ToolByName(base); ok {
		return base, true
	}
	return "", false
}

// ClassifyEntries classifies a whole configured list, preserving order.
func ClassifyEntries(entries []string, bin map[string]string) []EntryState {
	out := make([]EntryState, 0, len(entries))
	for _, e := range entries {
		out = append(out, ClassifyEntry(e, bin))
	}
	return out
}

// searchedSuffix names the ecosystem directories that were searched beyond PATH,
// because "not found on PATH" alone leaves a user who installed the tool with
// nowhere to look — that is precisely how the golangci-lint case went unnoticed.
func searchedSuffix(e Ecosystem) string {
	dirs := BinDirs(e)
	if len(dirs) == 0 {
		return ""
	}
	return " or " + strings.Join(dirs, ", ")
}
