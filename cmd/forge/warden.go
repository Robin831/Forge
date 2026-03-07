package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/spf13/cobra"
)

func init() {
	wardenLearnCmd.Flags().StringP("anvil", "a", "", "Anvil name (required)")
	wardenLearnCmd.Flags().IntSliceP("pr", "p", nil, "PR number(s) to learn from (default: 10 most recent merged PRs)")
	wardenLearnCmd.Flags().BoolP("dry-run", "n", false, "Preview rules without saving")
	_ = wardenLearnCmd.MarkFlagRequired("anvil")

	wardenForgetCmd.Flags().StringP("anvil", "a", "", "Anvil name (required)")
	_ = wardenForgetCmd.MarkFlagRequired("anvil")

	wardenListCmd.Flags().StringP("anvil", "a", "", "Anvil name (required)")
	_ = wardenListCmd.MarkFlagRequired("anvil")

	wardenCmd.AddCommand(wardenLearnCmd)
	wardenCmd.AddCommand(wardenForgetCmd)
	wardenCmd.AddCommand(wardenListCmd)
	rootCmd.AddCommand(wardenCmd)
}

var wardenCmd = &cobra.Command{
	Use:     "warden",
	Short:   "Manage Warden review rules",
	GroupID: "work",
}

var wardenLearnCmd = &cobra.Command{
	Use:   "learn",
	Short: "Learn review rules from Copilot comments on recent PRs",
	Long: `Fetch unresolved Copilot review comments from recent or specified PRs,
group and deduplicate them, use Claude to distill each unique pattern into
a reusable review rule, and append to .forge/warden-rules.yaml.`,
	Example: `  forge warden learn --anvil heimdall
  forge warden learn --anvil heimdall --pr 65,72
  forge warden learn --anvil heimdall --dry-run`,
	RunE: func(cmd *cobra.Command, args []string) error {
		anvilName, _ := cmd.Flags().GetString("anvil")
		prNumbers, _ := cmd.Flags().GetIntSlice("pr")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}

		anvil, ok := cfg.Anvils[anvilName]
		if !ok {
			return fmt.Errorf("anvil %q not found in config", anvilName)
		}

		// Determine which PRs to scan
		if len(prNumbers) == 0 {
			fmt.Fprintf(os.Stderr, "Fetching recent merged PRs for %s...\n", anvilName)
			nums, err := warden.FetchRecentPRNumbers(rootCtx, anvil.Path, 10)
			if err != nil {
				return fmt.Errorf("fetching recent PRs: %w", err)
			}
			if len(nums) == 0 {
				fmt.Println("No recent merged PRs found.")
				return nil
			}
			prNumbers = nums
		}

		// Fetch Copilot comments from all PRs
		var allComments []warden.PRComment
		for _, pr := range prNumbers {
			if verbose {
				fmt.Fprintf(os.Stderr, "Scanning PR #%d...\n", pr)
			}
			comments, err := warden.FetchCopilotComments(rootCtx, anvil.Path, pr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: PR #%d: %v\n", pr, err)
				continue
			}
			allComments = append(allComments, comments...)
		}

		if len(allComments) == 0 {
			fmt.Println("No Copilot review comments found.")
			return nil
		}

		fmt.Fprintf(os.Stderr, "Found %d Copilot comment(s) across %d PR(s)\n", len(allComments), len(prNumbers))

		// Group similar comments
		groups := warden.GroupComments(allComments)

		// Load existing rules
		rf, err := warden.LoadRules(anvil.Path)
		if err != nil {
			return fmt.Errorf("loading existing rules: %w", err)
		}

		// Distill each group into a rule
		var newRules []warden.Rule
		for i, group := range groups {
			fmt.Fprintf(os.Stderr, "Distilling rule %d/%d...\n", i+1, len(groups))
			rule, err := warden.DistillRule(rootCtx, group, anvil.Path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to distill group %d: %v\n", i+1, err)
				continue
			}
			if rf.AddRule(*rule) {
				newRules = append(newRules, *rule)
			} else {
				fmt.Fprintf(os.Stderr, "  Skipped duplicate rule: %s\n", rule.ID)
			}
		}

		if len(newRules) == 0 {
			fmt.Println("No new rules to add (all duplicates or distillation failed).")
			return nil
		}

		// Display rules
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "ID\tCATEGORY\tCHECK\tSOURCE\n")
		for _, r := range newRules {
			check := r.Check
			if len(check) > 60 {
				check = check[:57] + "..."
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.Category, check, r.Source)
		}
		tw.Flush()

		if dryRun {
			fmt.Printf("\n[dry-run] Would add %d rule(s) to %s\n", len(newRules), warden.RulesPath(anvil.Path))
			return nil
		}

		// Save
		if err := warden.SaveRules(anvil.Path, rf); err != nil {
			return fmt.Errorf("saving rules: %w", err)
		}

		fmt.Printf("\nAdded %d rule(s) to %s\n", len(newRules), warden.RulesPath(anvil.Path))
		return nil
	},
}

var wardenForgetCmd = &cobra.Command{
	Use:     "forget <rule-id> [rule-id...]",
	Short:   "Remove learned review rules by ID",
	Args:    cobra.MinimumNArgs(1),
	Example: "  forge warden forget race-ctx-field --anvil heimdall",
	RunE: func(cmd *cobra.Command, args []string) error {
		anvilName, _ := cmd.Flags().GetString("anvil")

		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}

		anvil, ok := cfg.Anvils[anvilName]
		if !ok {
			return fmt.Errorf("anvil %q not found in config", anvilName)
		}

		rf, err := warden.LoadRules(anvil.Path)
		if err != nil {
			return fmt.Errorf("loading rules: %w", err)
		}

		var removed []string
		var notFound []string
		for _, id := range args {
			if rf.RemoveRule(id) {
				removed = append(removed, id)
			} else {
				notFound = append(notFound, id)
			}
		}

		if len(removed) > 0 {
			if err := warden.SaveRules(anvil.Path, rf); err != nil {
				return fmt.Errorf("saving rules: %w", err)
			}
			fmt.Printf("Removed rule(s): %s\n", strings.Join(removed, ", "))
		}
		if len(notFound) > 0 {
			fmt.Fprintf(os.Stderr, "Not found: %s\n", strings.Join(notFound, ", "))
		}

		fmt.Printf("%d rule(s) remaining in %s\n", len(rf.Rules), warden.RulesPath(anvil.Path))
		return nil
	},
}

var wardenListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List learned review rules for an anvil",
	Example: "  forge warden list --anvil heimdall",
	RunE: func(cmd *cobra.Command, args []string) error {
		anvilName, _ := cmd.Flags().GetString("anvil")

		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}

		anvil, ok := cfg.Anvils[anvilName]
		if !ok {
			return fmt.Errorf("anvil %q not found in config", anvilName)
		}

		rf, err := warden.LoadRules(anvil.Path)
		if err != nil {
			return fmt.Errorf("loading rules: %w", err)
		}

		if len(rf.Rules) == 0 {
			fmt.Printf("No warden rules for %s.\n", anvilName)
			fmt.Println("Run 'forge warden learn --anvil " + anvilName + "' to learn from Copilot comments.")
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "ID\tCATEGORY\tPATTERN\tCHECK\tSOURCE\tADDED\n")
		for _, r := range rf.Rules {
			pattern := truncateStr(r.Pattern, 40)
			check := truncateStr(r.Check, 50)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Category, pattern, check, r.Source, r.Added)
		}
		tw.Flush()

		fmt.Printf("\n%s rule(s) in %s\n", strconv.Itoa(len(rf.Rules)), warden.RulesPath(anvil.Path))
		return nil
	},
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
