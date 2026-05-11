package mcp

import (
	"context"
	"fmt"
	"strings"
)

// workspacePrompt is the shared base for workspace-aware prompts. The
// workspaceFn is the same lazy resolver used by tools: it returns "" until
// the first file URI is seen, at which point the daemon resolves the project
// root via gopls.
type workspacePrompt struct {
	name        string
	description string
	workspaceFn func() string
}

func (p *workspacePrompt) Name() string        { return p.name }
func (p *workspacePrompt) Description() string { return p.description }

func wsArg() []PromptArgument {
	return []PromptArgument{
		{
			Name:        "workspace",
			Description: "Absolute path to the project root. Leave blank to use the daemon's resolved workspace.",
			Required:    false,
		},
	}
}

func resolveWS(args map[string]string, fn func() string) string {
	if args != nil {
		if w := args["workspace"]; w != "" {
			return w
		}
	}
	if fn != nil {
		return fn()
	}
	return ""
}

// ── orient ───────────────────────────────────────────────────────────────────

// OrientPrompt tells Claude to call session_start and summarise the project.
type OrientPrompt struct{ workspacePrompt }

func NewOrientPrompt(workspaceFn func() string) *OrientPrompt {
	return &OrientPrompt{workspacePrompt{
		name:        "orient",
		description: "Get oriented — load workspace context, memories, and diagnostics in one shot.",
		workspaceFn: workspaceFn,
	}}
}

func (p *OrientPrompt) Arguments() []PromptArgument { return wsArg() }

func (p *OrientPrompt) Expand(_ context.Context, args map[string]string) ([]PromptMessage, error) {
	ws := resolveWS(args, p.workspaceFn)

	var call string
	if ws != "" {
		call = fmt.Sprintf(`session_start({"workspace": %q})`, ws)
	} else {
		call = `session_start({})`
	}

	text := strings.Join([]string{
		"Please orient yourself to this project by calling the `session_start` tool:",
		"",
		"```",
		call,
		"```",
		"",
		"Once you have the output, give me a 3–5 sentence summary covering:",
		"1. What the project does and its primary language.",
		"2. The most important architectural decisions or conventions from context.md.",
		"3. Any active errors or warnings I should know about.",
		"4. Which tools I've been using most (from the stats), and anything that suggests",
		"   where I was last working.",
		"",
		"Keep it concise — one short paragraph per point.",
	}, "\n")

	return []PromptMessage{
		{Role: "user", Content: PromptContent{Type: "text", Text: text}},
	}, nil
}

// ── whats-broken ─────────────────────────────────────────────────────────────

// WhatsBrokenPrompt tells Claude to surface all current LSP errors and triage them.
type WhatsBrokenPrompt struct{ workspacePrompt }

func NewWhatsBrokenPrompt(workspaceFn func() string) *WhatsBrokenPrompt {
	return &WhatsBrokenPrompt{workspacePrompt{
		name:        "whats-broken",
		description: "Show all current LSP errors and warnings, then triage and suggest fixes.",
		workspaceFn: workspaceFn,
	}}
}

func (p *WhatsBrokenPrompt) Arguments() []PromptArgument { return wsArg() }

func (p *WhatsBrokenPrompt) Expand(_ context.Context, args map[string]string) ([]PromptMessage, error) {
	ws := resolveWS(args, p.workspaceFn)

	var diagCall string
	if ws != "" {
		diagCall = fmt.Sprintf(
			"First call `session_start({\"workspace\": %q})` to orient yourself, "+
				"then call `diagnostics({})` to get the full picture.", ws)
	} else {
		diagCall = "First call `session_start({})` to orient yourself, " +
			"then call `diagnostics({})` to get the full picture."
	}

	text := strings.Join([]string{
		diagCall,
		"",
		"Then, for each file that has errors or warnings:",
		"1. Read the relevant section of the file with `read_file` (use start_line/end_line to",
		"   focus on the problem area).",
		"2. Identify the root cause — is it a type error, a missing import, a changed API, etc?",
		"3. Suggest a concrete fix.",
		"",
		"Group your response by file. Lead with errors before warnings. If there are no",
		"diagnostics yet, say so and remind me the language server may still be indexing.",
	}, "\n")

	return []PromptMessage{
		{Role: "user", Content: PromptContent{Type: "text", Text: text}},
	}, nil
}

// ── recent-changes ────────────────────────────────────────────────────────────

// RecentChangesPrompt tells Claude to summarise recent git activity and surface any issues.
type RecentChangesPrompt struct{ workspacePrompt }

func NewRecentChangesPrompt(workspaceFn func() string) *RecentChangesPrompt {
	return &RecentChangesPrompt{workspacePrompt{
		name:        "recent-changes",
		description: "Summarise recent git commits, show what changed, and flag any new diagnostics.",
		workspaceFn: workspaceFn,
	}}
}

func (p *RecentChangesPrompt) Arguments() []PromptArgument {
	return append(wsArg(), PromptArgument{
		Name:        "since",
		Description: "How far back to look, e.g. '1 week ago', 'yesterday', or a commit SHA. Defaults to the last 10 commits.",
		Required:    false,
	})
}

func (p *RecentChangesPrompt) Expand(_ context.Context, args map[string]string) ([]PromptMessage, error) {
	ws := resolveWS(args, p.workspaceFn)
	since := ""
	if args != nil {
		since = args["since"]
	}

	var logArgs string
	if since != "" {
		logArgs = fmt.Sprintf(`"--since=%s", "--oneline"`, since)
	} else {
		logArgs = `"-10", "--oneline"`
	}

	var wsClause string
	if ws != "" {
		wsClause = fmt.Sprintf(`, "workspace": %q`, ws)
	}

	gitLogCall := fmt.Sprintf(`git({"subcommand": "log", "args": [%s]%s})`, logArgs, wsClause)
	gitDiffCall := fmt.Sprintf(`git({"subcommand": "diff", "args": ["HEAD~1", "--stat"]%s})`, wsClause)

	text := strings.Join([]string{
		"Please summarise what's changed recently in this project.",
		"",
		"Step 1 — get oriented:",
		fmt.Sprintf("  `session_start({%s})`", strings.TrimPrefix(wsClause, ", ")),
		"",
		"Step 2 — fetch recent git history:",
		fmt.Sprintf("  `%s`", gitLogCall),
		"",
		"Step 3 — get a change summary:",
		fmt.Sprintf("  `%s`", gitDiffCall),
		"",
		"Step 4 — check for diagnostics:",
		"  `diagnostics({})`",
		"",
		"Then give me:",
		"- A 2–3 sentence summary of what changed and why (inferred from commit messages).",
		"- The files most affected, and what kind of changes they received.",
		"- Any new errors or warnings introduced by the recent changes.",
		"- Your read on whether the changes look complete or if something seems unfinished.",
	}, "\n")

	return []PromptMessage{
		{Role: "user", Content: PromptContent{Type: "text", Text: text}},
	}, nil
}
