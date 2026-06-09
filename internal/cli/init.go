package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

var initFlagDiscover bool

var initCmd = &cobra.Command{
	Use:   "init [directory]",
	Short: "Initialise a .plumb workspace in the current (or given) directory",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runInit,
}

func init() {
	initCmd.Flags().BoolVar(&initFlagDiscover, "discover", false, "auto-detect project structure (build system, entry points, test layout) and seed context.md")
}

const contextTemplate = `# Project Context

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

func runInit(_ *cobra.Command, args []string) error {
	PrintLogo()

	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	if len(args) == 1 {
		dir = args[0]
	}

	plumbDir := filepath.Join(dir, ".plumb")
	if _, err := os.Stat(plumbDir); err == nil {
		tui.RebuildStyles()
		ctxStr := fmt.Sprintf(".plumb already exists at:\n↳ %s", plumbDir)
		fmt.Println(render.ContextBox(tui.MutedStyle.Render(ctxStr), tui.SepStyle))
		fmt.Println()
		return nil
	}

	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		return fmt.Errorf("creating .plumb directory: %w", err)
	}

	contextBody := contextTemplate
	var disc *Discovery
	if initFlagDiscover {
		cfg, cfgErr := config.Load()
		if cfgErr != nil {
			return fmt.Errorf("loading config for discovery: %w", cfgErr)
		}
		var err error
		disc, err = Discover(dir, cfg.Walk.RefuseHomeRoots)
		if err != nil {
			return fmt.Errorf("discovery: %w", err)
		}
		contextBody = renderDiscoveryContext(disc)
	}

	contextPath := filepath.Join(plumbDir, "context.md")
	if err := os.WriteFile(contextPath, []byte(contextBody), 0o644); err != nil { //nolint:gosec // G306: context.md is a user-edited project file; 0644 is intentional
		return fmt.Errorf("writing context.md: %w", err)
	}

	ctxStr := fmt.Sprintf("Initialised .plumb at %s", plumbDir)
	if disc != nil {
		ctxStr += "\n\nDiscovered:"
		if len(disc.Languages) > 0 {
			ctxStr += fmt.Sprintf("\n  Languages:    %s", strings.Join(disc.Languages, ", "))
		}
		if len(disc.BuildSystems) > 0 {
			ctxStr += fmt.Sprintf("\n  Build:        %s", strings.Join(disc.BuildSystems, ", "))
		}
		if len(disc.EntryPoints) > 0 {
			ctxStr += fmt.Sprintf("\n  Entry points: %d", len(disc.EntryPoints))
		}
		if len(disc.TestDirs) > 0 {
			ctxStr += fmt.Sprintf("\n  Test layout:  %s", strings.Join(disc.TestDirs, ", "))
		}
		ctxStr += "\n\nSeeded .plumb/context.md from discovery — review and edit it."
	} else {
		ctxStr += "\nEdit .plumb/context.md to describe your project — plumb loads it on every session.\nTip: run `plumb init --discover` to auto-fill context.md from your project structure."
	}

	tui.RebuildStyles()
	fmt.Println(render.ContextBox(tui.MutedStyle.Render(ctxStr), tui.SepStyle))
	fmt.Println("\nAdd .plumb to your .gitignore if you prefer to keep it local, or commit it to share with your team.")
	return nil
}
