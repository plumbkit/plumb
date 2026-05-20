package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var gitInitSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Absolute path of the directory to initialise as a git repository. Created if it does not exist."
    },
    "init_plumb": {
      "type": "boolean",
      "description": "Also create a .plumb/ workspace marker with a blank context.md so plumb attaches to the project on the next session. Default false."
    }
  },
  "required": ["path"]
}`)

const plumbContextTemplate = `# Project Context

This file is read by plumb to provide Claude with project-specific context.
Keep it concise — it is loaded automatically on every session.

## Overview

<!-- What does this project do? -->

## Architecture

<!-- Key design decisions, layer responsibilities, important invariants. -->

## Conventions

<!-- Naming, formatting, testing patterns, anything non-obvious. -->

## Known gotchas

<!-- Footguns, non-obvious behaviour, things that have burned you before. -->
`

// GitInit initialises a new git repository and optionally a .plumb/ workspace marker.
//
// Concurrency: Execute is safe for concurrent use (no shared mutable state).
type GitInit struct {
	deps WriteDeps
}

func NewGitInit(deps WriteDeps) *GitInit { return &GitInit{deps: deps} }

func (t *GitInit) Name() string                 { return "git_init" }
func (t *GitInit) InputSchema() json.RawMessage { return gitInitSchema }
func (t *GitInit) Description() string {
	return "Initialise a new git repository at the given path (git init). " +
		"The directory is created if it does not exist. " +
		"Set init_plumb: true to also create a .plumb/ workspace marker with a blank " +
		"context.md, so plumb attaches to the project automatically on the next session."
}

type gitInitArgs struct {
	Path      string `json:"path"`
	InitPlumb bool   `json:"init_plumb"`
}

func (a gitInitArgs) validate() error {
	if strings.TrimSpace(a.Path) == "" {
		return fmt.Errorf("git_init: path is required")
	}
	return nil
}

func (t *GitInit) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("git_init", t.deps.Limiter)
	}
	a, err := parseGitInitArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	return t.run(ctx, a)
}

func parseGitInitArgs(raw json.RawMessage) (gitInitArgs, error) {
	var a gitInitArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("git_init: invalid arguments: %w", err)
	}
	return a, nil
}

func (t *GitInit) run(ctx context.Context, a gitInitArgs) (string, error) {
	if err := os.MkdirAll(a.Path, 0o755); err != nil {
		return "", fmt.Errorf("git_init: creating directory: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = a.Path
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git_init: %s", strings.TrimSpace(string(out)))
	}
	if !a.InitPlumb {
		return fmt.Sprintf("initialised git repository at %s", a.Path), nil
	}
	if err := createPlumbMarker(a.Path); err != nil {
		return "", fmt.Errorf("git_init: %w", err)
	}
	return fmt.Sprintf("initialised git repository and .plumb/ workspace at %s", a.Path), nil
}

func createPlumbMarker(root string) error {
	plumbDir := filepath.Join(root, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		return fmt.Errorf("creating .plumb/: %w", err)
	}
	contextPath := filepath.Join(plumbDir, "context.md")
	if _, err := os.Stat(contextPath); err == nil {
		return nil // already exists — do not overwrite
	}
	return os.WriteFile(contextPath, []byte(plumbContextTemplate), 0o644) //nolint:gosec // G306: context.md is a user-edited project file; 0644 is intentional
}
