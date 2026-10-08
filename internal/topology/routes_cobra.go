package topology

import (
	"path"
	"strings"
)

// Cobra command trees are recovered in two steps: the field sites of each
// `&cobra.Command{...}` literal are grouped into one Command, then AddCommand
// calls link commands by the variable each is declared under.

// cobraDeclKey names a command by the package-level variable it initialises.
type cobraDeclKey struct{ dir, name string }

// cobraBuilder accumulates one literal's fields while sites stream past.
type cobraBuilder struct {
	cmd     *Command
	key     [2]int64 // file, enclosing declaration
	site    goSite   // first field site, for handler resolution
	fields  map[string]bool
	runText string
	runE    string
}

func (r *goRouteResolver) cobraTree(fields, adds []goSite, rep *RouteReport) error {
	cmds, err := r.groupCobraCommands(fields)
	if err != nil {
		return err
	}
	byDecl := map[cobraDeclKey][]*Command{}
	for _, c := range cmds {
		// Only a variable names a command: a factory function's name is not
		// what AddCommand is passed.
		if c.DeclKind == KindVariable && c.Decl != "" {
			k := cobraDeclKey{path.Dir(c.Path), c.Decl}
			byDecl[k] = append(byDecl[k], c)
		}
	}
	for _, s := range adds {
		if r.importsAny(s.fileID, importCobra) {
			rep.UnlinkedChildArgs += s.argCount - r.linkAddCommand(s, byDecl)
		}
	}
	rep.CommandCount += len(cmds)
	for _, c := range cmds {
		if !c.parented {
			rep.Commands = append(rep.Commands, c)
		}
	}
	return nil
}

// groupCobraCommands turns field sites, in source order, into commands. A new
// command starts at a new enclosing declaration, or where a field repeats
// within one: two literals in one factory cannot both set Use on one command.
func (r *goRouteResolver) groupCobraCommands(fields []goSite) ([]*Command, error) {
	var cmds []*Command
	var b *cobraBuilder
	for _, s := range fields {
		if !r.isCobraCommandField(s) {
			continue
		}
		key := [2]int64{s.fileID, s.enclosingID}
		if b == nil || b.key != key || b.fields[s.callee] {
			if err := r.finishCobraCommand(b, &cmds); err != nil {
				return nil, err
			}
			b = &cobraBuilder{
				cmd:    &Command{Decl: s.enclosingName, DeclKind: s.enclosingKind, Path: s.path, Line: s.line, Dynamic: true},
				key:    key,
				site:   s,
				fields: map[string]bool{},
			}
		}
		b.add(s)
	}
	if err := r.finishCobraCommand(b, &cmds); err != nil {
		return nil, err
	}
	return cmds, nil
}

// isCobraCommandField accepts a field of a `<cobra>.Command` literal, where
// <cobra> is this file's own import name for spf13/cobra.
func (r *goRouteResolver) isCobraCommandField(s goSite) bool {
	i := strings.LastIndex(s.qualifier, ".")
	return i >= 0 && s.qualifier[i+1:] == "Command" && r.imports[s.fileID][s.qualifier[:i]] == importCobra
}

func (b *cobraBuilder) add(s goSite) {
	b.fields[s.callee] = true
	var value string
	if len(s.idents) == 1 {
		value = s.idents[0]
	}
	switch s.callee {
	case "Use":
		if s.hasString {
			b.cmd.Use, b.cmd.Dynamic = s.str, false
		}
		b.cmd.Line = s.line
	case "Run":
		b.runText = value
	case "RunE":
		b.runE = value
	}
}

// finishCobraCommand resolves the builder's handler and appends its command. A
// command with neither Run nor RunE is a group: it has no handler at all, which
// is a different fact from a handler that did not resolve.
func (r *goRouteResolver) finishCobraCommand(b *cobraBuilder, cmds *[]*Command) error {
	if b == nil {
		return nil
	}
	if b.fields["Run"] || b.fields["RunE"] {
		text := b.runE
		if text == "" {
			text = b.runText
		}
		h, err := r.handler(b.site.fileID, b.site.path, text)
		if err != nil {
			return err
		}
		b.cmd.Handler = h
	}
	*cmds = append(*cmds, b.cmd)
	return nil
}

// linkAddCommand links one `parent.AddCommand(children...)` call and returns
// how many of its arguments produced at least one edge. A spread
// (`AddCommand(cmds...)`) links every command declared under that variable.
func (r *goRouteResolver) linkAddCommand(s goSite, byDecl map[cobraDeclKey][]*Command) int {
	parents := r.lookupCommands(s, s.qualifier, byDecl)
	if len(parents) != 1 {
		return 0
	}
	linked := 0
	for _, arg := range s.idents {
		children := r.lookupCommands(s, arg, byDecl)
		for _, c := range children {
			parents[0].Children = append(parents[0].Children, c)
			c.parented = true
		}
		if len(children) > 0 {
			linked++
		}
	}
	return linked
}

// lookupCommands finds the commands declared under name as seen from site s's
// file: a bare name in the same package, or `pkg.Name` through s's own import.
func (r *goRouteResolver) lookupCommands(s goSite, name string, byDecl map[cobraDeclKey][]*Command) []*Command {
	dir := path.Dir(s.path)
	if i := strings.LastIndex(name, "."); i >= 0 {
		imp, ok := r.imports[s.fileID][name[:i]]
		if !ok {
			return nil
		}
		if dir, ok = matchImportDir(imp, r.tables.pkgDirs); !ok {
			return nil
		}
		name = name[i+1:]
	}
	return byDecl[cobraDeclKey{dir, name}]
}
