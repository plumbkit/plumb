package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// gomodfile.go reads the two facts the automatic GOWORK decision needs (git_gowork.go)
// out of go.work and go.mod files: the `use` directories of a go.work and the
// `module` path of a go.mod.
//
// It follows the go command's own grammar (golang.org/x/mod/modfile) rather than
// approximating it, because a reader that disagrees with go in EITHER direction
// breaks the decision: one that misses a `use` go honours (the valid `use(` with no
// space, say) calls a listed module excluded and detaches it from its workspace; one
// that accepts an operand go rejects (a backquoted path) calls a module listed that
// go cannot load at all. Concretely:
//
//   - Tokens are the punctuation ( ) [ ] { } , and newline, "double-quoted" and
//     `backquoted` strings (single-line; backslash escapes only in the former), `//`
//     comments to end of line, and runs of any other printable non-space runes. A
//     `/*` comment, an unterminated string, or a non-printable rune is an error.
//   - A statement is the tokens up to the end of its line. `verb (` followed by the
//     end of the line opens a block whose every line is one more `verb` statement,
//     closed by a line that starts with `)`; a `(` anywhere else is an ordinary token.
//   - An operand is a quoted string (strconv.Unquote), or a bare token that contains
//     no quote character. Anything else is an error, as it is to go.
//
// Directives other than `use` and `module` are skipped without validation: this is
// not a linter, and a directive a newer go adds must not make an older plumb read a
// valid file as broken. Every error means "the file could not be read the way go
// reads it", and the caller treats that as a reason to leave the environment alone.

// goConfigReadLimit caps how much of a go.work, go.mod or go env file is read. Real
// ones are a few kilobytes; the cap is what keeps a repository-controlled path from
// making the daemon read without bound.
const goConfigReadLimit = 1 << 20

// errGoConfigNotRegular is returned for a path that is not a regular file.
var errGoConfigNotRegular = errors.New("not a regular file")

// readGoConfigFile reads a small regular file that a repository controls. The daemon
// reads it on the git tool's and run_task's spawn path, so a file that is really a
// device, a FIFO or something enormous — `go.work -> /dev/zero` committed to a
// clone — must fail fast rather than exhaust memory or block forever:
//
//   - the path must stat as a regular file, following symlinks, before it is opened;
//   - it is opened non-blocking and re-checked on the open descriptor, so a swap
//     between the two checks cannot turn the read into one on a FIFO;
//   - at most goConfigReadLimit bytes are read, and a longer file is an error.
func readGoConfigFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errGoConfigNotRegular
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the path was found by walking up from a directory plumb is about to run a child in; it is read, never executed
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info, err = f.Stat(); err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errGoConfigNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(f, goConfigReadLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > goConfigReadLimit {
		return nil, fmt.Errorf("larger than %d bytes", goConfigReadLimit)
	}
	return data, nil
}

// goWorkUses returns the directory of every `use` directive in the go.work at
// workFile, each made absolute against the go.work's directory and cleaned, as the
// go command resolves them. Paths are compared LEXICALLY by the caller, because
// that is what go does: a `use` spelled through a symlink does not cover the
// directory the link points at.
func goWorkUses(workFile string) ([]string, error) {
	src, err := readGoConfigFile(workFile)
	if err != nil {
		return nil, err
	}
	stmts, err := parseGoModStmts(string(src))
	if err != nil {
		return nil, err
	}
	workDir := filepath.Dir(workFile)
	var dirs []string
	for _, s := range stmts {
		if s.verb != "use" {
			continue
		}
		if len(s.args) != 1 {
			return nil, fmt.Errorf("line %d: usage: use local/dir", s.line)
		}
		dir, err := goModOperand(s.args[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", s.line, err)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workDir, dir)
		}
		dirs = append(dirs, filepath.Clean(dir))
	}
	return dirs, nil
}

// goModulePath returns the module path a go.mod declares, and false when the file
// cannot be read, does not parse, or does not declare exactly one module.
func goModulePath(modFile string) (string, bool) {
	src, err := readGoConfigFile(modFile)
	if err != nil {
		return "", false
	}
	stmts, err := parseGoModStmts(string(src))
	if err != nil {
		return "", false
	}
	path := ""
	for _, s := range stmts {
		if s.verb != "module" {
			continue
		}
		if path != "" || len(s.args) != 1 {
			return "", false
		}
		if path, err = goModOperand(s.args[0]); err != nil || path == "" {
			return "", false
		}
	}
	return path, path != ""
}

// goModOperand decodes one operand token: a double-quoted string is unquoted, and a
// bare token is taken as written unless it contains a quote character, which go
// reserves. A backquoted token is therefore an error, as it is to go.
func goModOperand(tok string) (string, error) {
	if strings.HasPrefix(tok, `"`) {
		s, err := strconv.Unquote(tok)
		if err != nil {
			return "", fmt.Errorf("invalid quoted string %s: %w", tok, err)
		}
		return s, nil
	}
	if strings.ContainsAny(tok, "\"'`") {
		return "", fmt.Errorf("unquoted string cannot contain quote: %s", tok)
	}
	return tok, nil
}

// goModStmt is one directive: its verb, its operand tokens (raw, quotes kept), and
// the line it is on. A block contributes one statement per line inside it.
type goModStmt struct {
	verb string
	args []string
	line int
}

// parseGoModStmts splits src into statements by the grammar in the file comment.
func parseGoModStmts(src string) ([]goModStmt, error) {
	lines, err := lexGoModLines(src)
	if err != nil {
		return nil, err
	}
	var stmts []goModStmt
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if len(l.toks) == 0 {
			continue
		}
		// `verb (` at the end of a line opens a block; `verb ( )` is an empty one.
		n := len(l.toks)
		switch {
		case n >= 2 && l.toks[n-1] == "(":
			end, block, err := goModBlock(lines, i+1)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", l.num, err)
			}
			if n == 2 {
				for _, b := range block {
					stmts = append(stmts, goModStmt{verb: l.toks[0], args: b.toks, line: b.num})
				}
			}
			// A block with more than one token before its `(` is an error to go
			// ("unknown block type"); it is skipped here like any other directive
			// this reader does not interpret, and cannot hold a `use`.
			i = end
		case n == 3 && l.toks[1] == "(" && l.toks[2] == ")":
			// Empty block: nothing to add.
		default:
			stmts = append(stmts, goModStmt{verb: l.toks[0], args: l.toks[1:], line: l.num})
		}
	}
	return stmts, nil
}

// goModBlock collects the lines of a block opened on the line before start and
// returns the index of its closing line. The closing line must be a lone `)`.
func goModBlock(lines []goModLine, start int) (int, []goModLine, error) {
	var block []goModLine
	for j := start; j < len(lines); j++ {
		l := lines[j]
		if len(l.toks) == 0 {
			continue
		}
		if l.toks[0] == ")" {
			if len(l.toks) != 1 {
				return 0, nil, fmt.Errorf("line %d: syntax error (expected newline after closing paren)", l.num)
			}
			return j, block, nil
		}
		block = append(block, l)
	}
	return 0, nil, errors.New("unexpected EOF in block")
}

// goModLine is the tokens of one source line, comments removed.
type goModLine struct {
	toks []string
	num  int
}

// lexGoModLines tokenises src into lines by the rules in the file comment.
func lexGoModLines(src string) ([]goModLine, error) {
	var lines []goModLine
	cur := goModLine{num: 1}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			lines = append(lines, cur)
			cur = goModLine{num: cur.num + 1}
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			return nil, fmt.Errorf("line %d: mod files must use // comments, not /* */ comments", cur.num)
		case strings.IndexByte("()[]{},", c) >= 0:
			cur.toks = append(cur.toks, string(c))
			i++
		case c == '"' || c == '`':
			end, err := goModStringEnd(src, i)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", cur.num, err)
			}
			cur.toks = append(cur.toks, src[i:end])
			i = end
		default:
			end, err := goModIdentEnd(src, i)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", cur.num, err)
			}
			cur.toks = append(cur.toks, src[i:end])
			i = end
		}
	}
	return append(lines, cur), nil
}

// goModStringEnd returns the index just past the quoted string starting at i.
func goModStringEnd(src string, i int) (int, error) {
	quote := src[i]
	for j := i + 1; j < len(src); j++ {
		switch c := src[j]; {
		case c == '\n':
			return 0, errors.New("unexpected newline in string")
		case c == quote:
			return j + 1, nil
		case c == '\\' && quote == '"':
			j++ // the escaped byte cannot close the string
		}
	}
	return 0, errors.New("unexpected EOF in string")
}

// goModIdentEnd returns the index just past the bare token starting at i, which
// must begin with a token rune.
func goModIdentEnd(src string, i int) (int, error) {
	j := i
	for j < len(src) {
		if strings.HasPrefix(src[j:], "//") {
			break
		}
		if strings.HasPrefix(src[j:], "/*") {
			return 0, errors.New("mod files must use // comments, not /* */ comments")
		}
		r, size := utf8.DecodeRuneInString(src[j:])
		if !goModIdentRune(r) {
			break
		}
		j += size
	}
	if j == i {
		r, _ := utf8.DecodeRuneInString(src[i:])
		return 0, fmt.Errorf("unexpected input character %#q", r)
	}
	return j, nil
}

// goModIdentRune reports whether r may appear in a bare token.
func goModIdentRune(r rune) bool {
	switch r {
	case ' ', '(', ')', '[', ']', '{', '}', ',':
		return false
	}
	return !unicode.IsSpace(r) && unicode.IsPrint(r)
}
