package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/config"
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
	PrintLogo("ɪ ɴ ɪ ᴛ")

	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	if len(args) == 1 {
		dir = args[0]
	}

	plumbDir := filepath.Join(dir, ".plumb")
	if _, err := os.Stat(plumbDir); err == nil {
		fmt.Printf(".plumb already exists at %s\n", plumbDir)
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
	if err := os.WriteFile(contextPath, []byte(contextBody), 0o644); err != nil {
		return fmt.Errorf("writing context.md: %w", err)
	}

	fmt.Printf("Initialised .plumb at %s\n", plumbDir)
	if disc != nil {
		fmt.Println()
		fmt.Println("Discovered:")
		if len(disc.Languages) > 0 {
			fmt.Printf("  Languages:    %s\n", strings.Join(disc.Languages, ", "))
		}
		if len(disc.BuildSystems) > 0 {
			fmt.Printf("  Build:        %s\n", strings.Join(disc.BuildSystems, ", "))
		}
		if len(disc.EntryPoints) > 0 {
			fmt.Printf("  Entry points: %d\n", len(disc.EntryPoints))
		}
		if len(disc.TestDirs) > 0 {
			fmt.Printf("  Test layout:  %s\n", strings.Join(disc.TestDirs, ", "))
		}
		fmt.Println()
		fmt.Println("Seeded .plumb/context.md from discovery — review and edit it.")
	} else {
		fmt.Println("Edit .plumb/context.md to describe your project — plumb loads it on every session.")
		fmt.Println("Tip: run `plumb init --discover` to auto-fill context.md from your project structure.")
	}
	fmt.Println("Add .plumb to your .gitignore if you prefer to keep it local, or commit it to share with your team.")
	return nil
}
