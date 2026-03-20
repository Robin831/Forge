package main

import (
	"testing"

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
	// Find the run subcommand
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

	// Verify the anvil flag is required
	anvilFlag := runCmd.Flag("anvil")
	if anvilFlag == nil {
		t.Fatal("quest run missing --anvil flag")
	}
}

func TestQuestListHasAnvilFlag(t *testing.T) {
	// Find the list subcommand
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

	// Verify the anvil flag exists (optional)
	anvilFlag := listCmd.Flag("anvil")
	if anvilFlag == nil {
		t.Fatal("quest list missing --anvil flag")
	}
}
