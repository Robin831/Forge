package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Robin831/Forge/internal/adventurer"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/questgiver"
	"github.com/spf13/cobra"
)

func init() {
	questListCmd.Flags().StringP("anvil", "a", "", "Only list quests for a specific anvil")
	questRunCmd.Flags().StringP("anvil", "a", "", "Anvil to run the quest in (required)")
	_ = questRunCmd.MarkFlagRequired("anvil")

	questCmd.AddCommand(questListCmd)
	questCmd.AddCommand(questRunCmd)
	rootCmd.AddCommand(questCmd)
}

var questCmd = &cobra.Command{
	Use:     "quest",
	Short:   "Discover and run E2E quests",
	GroupID: "work",
}

var questListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List discovered quests across anvils",
	Example: "  forge quest list\n  forge quest list --anvil heimdall",
	RunE: func(cmd *cobra.Command, args []string) error {
		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}

		if len(cfg.Anvils) == 0 {
			fmt.Println("No anvils registered. Use 'forge anvil add <name> <path>' first.")
			return nil
		}

		anvilFilter, _ := cmd.Flags().GetString("anvil")

		anvils := cfg.Anvils
		if anvilFilter != "" {
			ac, ok := cfg.Anvils[anvilFilter]
			if !ok {
				return fmt.Errorf("anvil %q not found in config", anvilFilter)
			}
			anvils = map[string]config.AnvilConfig{anvilFilter: ac}
		}

		type questRow struct {
			Anvil    string
			Name     string
			Steps    int
			Tags     string
			FilePath string
		}
		var rows []questRow

		for name, ac := range anvils {
			quests, err := questgiver.DiscoverQuests(ac.Path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", name, err)
				continue
			}
			for _, q := range quests {
				rows = append(rows, questRow{
					Anvil:    name,
					Name:     q.Name,
					Steps:    len(q.Steps),
					Tags:     strings.Join(q.Tags, ", "),
					FilePath: q.FilePath,
				})
			}
		}

		if len(rows) == 0 {
			fmt.Println("No quests found. Place quest YAML files in <anvil>/.forge/quests/*.yaml")
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "ANVIL\tNAME\tSTEPS\tTAGS\tFILE\n")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", r.Anvil, r.Name, r.Steps, r.Tags, r.FilePath)
		}
		tw.Flush()

		fmt.Printf("\n%d quest(s) found\n", len(rows))
		return nil
	},
}

var questRunCmd = &cobra.Command{
	Use:     "run <quest-name>",
	Short:   "Execute a quest and report results",
	Args:    cobra.ExactArgs(1),
	Example: "  forge quest run login-flow --anvil heimdall",
	RunE: func(cmd *cobra.Command, args []string) error {
		questName := args[0]
		anvilName, _ := cmd.Flags().GetString("anvil")

		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}

		ac, ok := cfg.Anvils[anvilName]
		if !ok {
			return fmt.Errorf("anvil %q not found in config", anvilName)
		}

		quests, err := questgiver.DiscoverQuests(ac.Path)
		if err != nil {
			return fmt.Errorf("discovering quests: %w", err)
		}

		var target *questgiver.Quest
		var available []string
		for i := range quests {
			available = append(available, quests[i].Name)
			if quests[i].Name == questName {
				target = &quests[i]
			}
		}

		if target == nil {
			if len(available) == 0 {
				return fmt.Errorf("no quests found in anvil %q (expected in %s/.forge/quests/*.yaml)", anvilName, ac.Path)
			}
			return fmt.Errorf("quest %q not found in anvil %q; available quests: %s", questName, anvilName, strings.Join(available, ", "))
		}

		timeout := cfg.Settings.AdventurerTimeout
		if timeout == 0 {
			timeout = 5 * time.Minute
		}

		logger := slog.Default()
		if !verbose {
			logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		}

		executor := adventurer.New(timeout, logger)

		ctx := rootCtx
		if ctx == nil {
			ctx = context.Background()
		}

		fmt.Printf("Running quest %q from anvil %q...\n\n", target.Name, anvilName)
		result := executor.Execute(ctx, target)

		// Print per-step results
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "STEP\tACTION\tRESULT\tDURATION\tERROR\n")
		for _, sr := range result.StepResults {
			status := "PASS"
			if !sr.Passed {
				status = "FAIL"
			}
			errMsg := ""
			if sr.Error != "" {
				errMsg = sr.Error
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", sr.Index, sr.Action, status, sr.Duration.Round(time.Millisecond), errMsg)
		}
		tw.Flush()

		// Print summary
		fmt.Println()
		if result.Passed {
			fmt.Printf("PASSED — %s (%s)\n", result.QuestName, result.Duration.Round(time.Millisecond))
		} else {
			fmt.Printf("FAILED — %s (%s)\n", result.QuestName, result.Duration.Round(time.Millisecond))
			if result.ErrorMessage != "" {
				fmt.Printf("Error: %s\n", result.ErrorMessage)
			}
		}

		if !result.Passed {
			os.Exit(1)
		}
		return nil
	},
}
