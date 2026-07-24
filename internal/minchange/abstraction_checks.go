package minchange

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// forwardCallRe matches a single forwarding statement: an optional `return`,
// then a callee (possibly dotted/generic-free) and its argument list spanning
// the rest of the statement.
var forwardCallRe = regexp.MustCompile(`^(?:return\s+)?(?P<callee>[\w.]+)\((?P<args>.*)\)$`)

// thinWrapperFindings flags a newly-added function whose entire body is a single
// call that forwards the function's own parameters, unchanged and in order, to
// another function. Such a wrapper adds a name but no behaviour, so the call
// site could invoke the target directly. Proven from the diff text alone
// (High confidence).
func thinWrapperFindings(added []addedSymbol, opts Options) []Finding {
	var out []Finding
	for _, s := range added {
		if s.Kind == "type" || !s.fullyAdded || s.bodyStmt == "" {
			continue
		}
		callee, args, ok := parseForwardingCall(s.bodyStmt)
		if !ok {
			continue
		}
		if baseName(callee) == s.Name {
			continue // recursion, not a wrapper
		}
		if !argsForwardParams(args, s.params) {
			continue // the call transforms or reorders arguments — not a pure passthrough
		}
		sev := Warning
		if len(s.params) == 0 {
			sev = Info // a zero-argument passthrough is weaker evidence
		}
		f := Finding{
			Severity:   sev,
			Kind:       KindThinWrapper,
			Confidence: High,
			File:       s.File,
			Line:       s.Line,
			Rationale:  fmt.Sprintf("%s only forwards its parameters to %s, adding a name but no behaviour", s.Name, callee),
			Evidence:   fmt.Sprintf("%s { %s }", s.signature, s.bodyStmt),
		}
		if opts.IncludeSuggestions {
			f.Alternative = fmt.Sprintf("call %s directly at the call site, or keep the wrapper only if it exists to satisfy an interface or stabilise an API boundary", callee)
		}
		out = append(out, f)
	}
	return out
}

// parseForwardingCall extracts the callee and argument list from a single body
// statement when it is a bare or returned function call.
func parseForwardingCall(stmt string) (callee, args string, ok bool) {
	m := forwardCallRe.FindStringSubmatch(strings.TrimSpace(stmt))
	if m == nil {
		return "", "", false
	}
	return m[forwardCallRe.SubexpIndex("callee")], m[forwardCallRe.SubexpIndex("args")], true
}

// argsForwardParams reports whether the call's arguments are exactly the
// wrapper's parameter names, in order — the signature of a pure passthrough. An
// empty parameter list matches an empty argument list.
func argsForwardParams(args string, params []string) bool {
	args = strings.TrimSpace(args)
	if len(params) == 0 {
		return args == ""
	}
	segs := splitTopLevel(args)
	if len(segs) != len(params) {
		return false
	}
	for i, seg := range segs {
		arg := strings.TrimSpace(seg)
		// Allow a variadic spread of the final slice param ("xs...").
		arg = strings.TrimSuffix(arg, "...")
		if arg != params[i] {
			return false
		}
	}
	return true
}

// baseName returns the last dotted segment of a callee ("pkg.Do" → "Do").
func baseName(callee string) string {
	if i := strings.LastIndexByte(callee, '.'); i >= 0 {
		return callee[i+1:]
	}
	return callee
}

// singleUseFindings flags a newly-added function or method that the topology
// index shows with exactly one caller. Such a symbol may be an abstraction the
// change did not need — the logic could live at its single call site. The
// signal is Low confidence: the topology call graph is intra-file, so a symbol
// with cross-file callers can still appear single-use here.
func singleUseFindings(ctx context.Context, added []addedSymbol, deps Deps, opts Options) []Finding {
	if deps.CallerCount == nil {
		return nil
	}
	var out []Finding
	seen := map[string]bool{}
	for _, s := range added {
		if s.Kind == "type" || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		count, site, found := deps.CallerCount(ctx, s.Name)
		if !found || count != 1 {
			continue
		}
		f := Finding{
			Severity:   Info,
			Kind:       KindSingleUse,
			Confidence: Low,
			File:       s.File,
			Line:       s.Line,
			Rationale:  fmt.Sprintf("%s appears to have a single call site — it may not need to be a separate %s", s.Name, s.Kind),
			Evidence:   fmt.Sprintf("topology found one intra-file call site: %s:%d (cross-file callers are not counted — confirm with find_references before acting)", site.Path, site.Line),
		}
		if opts.IncludeSuggestions {
			f.Alternative = fmt.Sprintf("if %s truly has one caller, consider inlining its body there; keep it if you expect reuse or it aids readability", s.Name)
		}
		out = append(out, f)
	}
	return out
}

// duplicateHelperFindings flags a newly-added free function whose tokenised name
// closely matches an existing indexed function in another file — a possible
// re-implementation of something that already exists. Low confidence: a name
// match is a hint, not proof the two do the same thing. Restricted to free
// functions (methods share names far too often to flag usefully).
func duplicateHelperFindings(ctx context.Context, added []addedSymbol, deps Deps, opts Options) []Finding {
	if deps.SimilarSymbols == nil {
		return nil
	}
	var out []Finding
	seen := map[string]bool{}
	for _, s := range added {
		if s.Kind != "function" || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		matches := deps.SimilarSymbols(ctx, s.Name, s.File)
		if len(matches) == 0 {
			continue
		}
		m := matches[0]
		f := Finding{
			Severity:   Info,
			Kind:       KindDuplicateHelper,
			Confidence: Low,
			File:       s.File,
			Line:       s.Line,
			Rationale:  fmt.Sprintf("%s closely resembles an existing symbol — it may duplicate logic already in the codebase", s.Name),
			Evidence:   fmt.Sprintf("existing %s %s at %s:%d has a near-identical tokenised name (verify they are not intentionally distinct)", m.Kind, m.Name, m.Path, m.Line),
		}
		if opts.IncludeSuggestions {
			f.Alternative = fmt.Sprintf("check whether %s already does what you need before adding %s", m.Name, s.Name)
		}
		out = append(out, f)
	}
	return out
}
