package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/spf13/cobra"
)

func TestQuestCommandRegistered(t *testing.T) {
	// Verify the quest command is registered on rootCmd
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "quest" {
			found = true

			// Verify subcommands
			subNames := make(map[string]bool)
			for _, sub := range cmd.Commands() {
				subNames[sub.Name()] = true
			}
			if !subNames["list"] {
				t.Error("quest command missing 'list' subcommand")
			}
			if !subNames["run"] {
				t.Error("quest command missing 'run' subcommand")
			}
			break
		}
	}
	if !found {
		t.Fatal("quest command not registered on rootCmd")
	}
}

func TestQuestRunRequiresAnvilFlag(t *testing.T) {
	var runCmd *cobra.Command
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "quest" {
			for _, sub := range cmd.Commands() {
				if sub.Name() == "run" {
					runCmd = sub
					break
				}
			}
			break
		}
	}
	if runCmd == nil {
		t.Fatal("quest run command not found")
	}

	anvilFlag := runCmd.Flag("anvil")
	if anvilFlag == nil {
		t.Fatal("quest run missing --anvil flag")
	}
}

func TestQuestListHasAnvilFlag(t *testing.T) {
	var listCmd *cobra.Command
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "quest" {
			for _, sub := range cmd.Commands() {
				if sub.Name() == "list" {
					listCmd = sub
					break
				}
			}
			break
		}
	}
	if listCmd == nil {
		t.Fatal("quest list command not found")
	}

	anvilFlag := listCmd.Flag("anvil")
	if anvilFlag == nil {
		t.Fatal("quest list missing --anvil flag")
	}
}

func TestQuestListNoAnvils(t *testing.T) {
	// Save and restore global cfg.
	origCfg := cfg
	defer func() { cfg = origCfg }()

	cfg = &config.Config{
		Anvils: map[string]config.AnvilConfig{},
	}

	var listCmd *cobra.Command
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "quest" {
			for _, sub := range cmd.Commands() {
				if sub.Name() == "list" {
					listCmd = sub
					break
				}
			}
			break
		}
	}
	if listCmd == nil {
		t.Fatal("quest list command not found")
	}

	// Should not error with empty anvils — just prints a message.
	err := listCmd.RunE(listCmd, nil)
	if err != nil {
		t.Fatalf("expected no error for empty anvils, got: %v", err)
	}
}

func TestQuestListFilterUnknownAnvil(t *testing.T) {
	origCfg := cfg
	defer func() { cfg = origCfg }()

	cfg = &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"real": {Path: t.TempDir()},
		},
	}

	var listCmd *cobra.Command
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "quest" {
			for _, sub := range cmd.Commands() {
				if sub.Name() == "list" {
					listCmd = sub
					break
				}
			}
			break
		}
	}
	if listCmd == nil {
		t.Fatal("quest list command not found")
	}

	// Reset flags so --anvil can be set fresh.
	if err := listCmd.Flags().Set("anvil", "nonexistent"); err != nil {
		t.Fatalf("failed to set --anvil flag: %v", err)
	}
	t.Cleanup(func() { listCmd.Flags().Set("anvil", "") })
	err := listCmd.RunE(listCmd, nil)
	if err == nil {
		t.Fatal("expected error for unknown anvil filter")
	}
	if got := err.Error(); !strings.Contains(got, "not found") {
		t.Errorf("expected 'not found' in error, got: %s", got)
	}
}

func TestQuestListWithQuests(t *testing.T) {
	origCfg := cfg
	defer func() { cfg = origCfg }()

	// Create a temp anvil with a quest file.
	anvilDir := t.TempDir()
	questDir := filepath.Join(anvilDir, ".forge", "quests")
	if err := os.MkdirAll(questDir, 0o755); err != nil {
		t.Fatal(err)
	}
	questYAML := `name: smoke-test
url: http://localhost:8080
tags: [smoke]
steps:
  - action: navigate
    url: /health
`
	if err := os.WriteFile(filepath.Join(questDir, "smoke.yaml"), []byte(questYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg = &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"testanvil": {Path: anvilDir},
		},
	}

	var listCmd *cobra.Command
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "quest" {
			for _, sub := range cmd.Commands() {
				if sub.Name() == "list" {
					listCmd = sub
					break
				}
			}
			break
		}
	}
	if listCmd == nil {
		t.Fatal("quest list command not found")
	}

	// Should succeed and not error.
	err := listCmd.RunE(listCmd, nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestQuestRunUnknownAnvil(t *testing.T) {
	origCfg := cfg
	defer func() { cfg = origCfg }()

	cfg = &config.Config{
		Anvils: map[string]config.AnvilConfig{},
	}

	var runCmd *cobra.Command
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "quest" {
			for _, sub := range cmd.Commands() {
				if sub.Name() == "run" {
					runCmd = sub
					break
				}
			}
			break
		}
	}
	if runCmd == nil {
		t.Fatal("quest run command not found")
	}

	if err := runCmd.Flags().Set("anvil", "missing"); err != nil {
		t.Fatalf("failed to set --anvil flag: %v", err)
	}
	t.Cleanup(func() { runCmd.Flags().Set("anvil", "") })
	err := runCmd.RunE(runCmd, []string{"some-quest"})
	if err == nil {
		t.Fatal("expected error for unknown anvil")
	}
	if got := err.Error(); !strings.Contains(got, "not found") {
		t.Errorf("expected 'not found' in error, got: %s", got)
	}
}

func TestQuestRunQuestNotFound(t *testing.T) {
	origCfg := cfg
	defer func() { cfg = origCfg }()

	anvilDir := t.TempDir()
	// Create empty quests directory.
	questDir := filepath.Join(anvilDir, ".forge", "quests")
	if err := os.MkdirAll(questDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg = &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"testanvil": {Path: anvilDir},
		},
	}

	var runCmd *cobra.Command
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "quest" {
			for _, sub := range cmd.Commands() {
				if sub.Name() == "run" {
					runCmd = sub
					break
				}
			}
			break
		}
	}
	if runCmd == nil {
		t.Fatal("quest run command not found")
	}

	if err := runCmd.Flags().Set("anvil", "testanvil"); err != nil {
		t.Fatalf("failed to set --anvil flag: %v", err)
	}
	t.Cleanup(func() { runCmd.Flags().Set("anvil", "") })
	err := runCmd.RunE(runCmd, []string{"nonexistent-quest"})
	if err == nil {
		t.Fatal("expected error for quest not found")
	}
	if got := err.Error(); !strings.Contains(got, "no quests found") {
		t.Errorf("expected 'no quests found' in error, got: %s", got)
	}
}

