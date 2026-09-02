package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/smelter"
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

	wardenRestoreCmd.Flags().StringP("anvil", "a", "", "Anvil name (required)")
	_ = wardenRestoreCmd.MarkFlagRequired("anvil")

	wardenCmd.AddCommand(wardenLearnCmd)
	wardenCmd.AddCommand(wardenForgetCmd)
	wardenCmd.AddCommand(wardenListCmd)
	wardenCmd.AddCommand(wardenConsolidateCmd)
	wardenCmd.AddCommand(wardenRestoreCmd)
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
	Long: `Fetch Copilot review comments from recent or specified PRs,
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

var wardenConsolidateCmd = &cobra.Command{
	Use:   "consolidate <anvil>",
	Short: "Run the three-pass smelter consolidation against an anvil",
	Long: `Off-cycle manual trigger for the same three-pass merge logic the
scheduled smelter runs:
  Pass 1 — cluster near-duplicate rules and merge each cluster.
  Pass 2 — archive rules whose Added date is older than archive_after_days,
           then evict the lowest-value rules over max_rules_in_file.
  Pass 3 — backfill the Paths field from each rule's source PR(s).

Always writes .forge/warden-rules.yaml when any pass produced changes.
.forge/warden-rules.archive.yaml is written only when Pass 1 or Pass 2
produced archive entries (duplicate or stale rules); a backfill-only run
leaves the archive file unchanged. The pending warden rules queue in
state.db is NOT consulted — this command only operates on what is already
in the active rules file.`,
	Args:    cobra.ExactArgs(1),
	Example: "  forge warden consolidate munin",
	RunE: func(cmd *cobra.Command, args []string) error {
		anvilName := args[0]

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

		opts := smelter.ConsolidateOptions{
			AnvilPath:        anvil.Path,
			AnvilName:        anvilName,
			Consolidator:     warden.DefaultConsolidationRunner(),
			DedupThreshold:   cfg.Settings.Warden.ResolvedDedupThreshold(),
			OverlapThreshold: cfg.Settings.Warden.ResolvedOverlapThreshold(),
			ArchiveAfterDays: cfg.Settings.Warden.ResolvedArchiveAfterDays(),
			MaxRulesInFile:   cfg.Settings.Warden.ResolvedMaxRulesInFile(),
		}

		fmt.Fprintf(os.Stderr, "Running three-pass consolidation against %s (dedup_threshold=%.2f, overlap_threshold=%.2f, archive_after_days=%d, max_rules_in_file=%d)...\n",
			anvilName, opts.DedupThreshold, opts.OverlapThreshold, opts.ArchiveAfterDays, opts.MaxRulesInFile)

		result, err := smelter.ConsolidateAnvil(rootCtx, opts)
		if err != nil {
			return fmt.Errorf("consolidate %s: %w", anvilName, err)
		}

		// Print a structured summary.
		fmt.Printf("Anvil:           %s\n", anvilName)
		fmt.Printf("Active before:   %d\n", result.InitialCount)
		fmt.Printf("Active after:    %d\n", result.FinalActive)
		fmt.Printf("Archive size:    %d\n", result.ArchiveCount)
		if n := len(result.Passes.Consolidated); n > 0 {
			fmt.Printf("Consolidated:    %d cluster(s)\n", n)
		}
		// The archive list carries two reasons and they are printed as two
		// lines: a rule evicted for losing a slot to the ceiling did not age
		// out, and reporting it as stale is the one claim this summary must
		// not make.
		stale, overCap := result.Passes.ArchivedByReason()
		if stale > 0 {
			fmt.Printf("Archived stale:  %d rule(s)\n", stale)
		}
		if overCap > 0 {
			fmt.Printf("Evicted:         %d rule(s) over the file ceiling\n", overCap)
		}
		if n := len(result.Passes.Backfilled); n > 0 {
			fmt.Printf("Backfilled:      %d rule(s)\n", n)
		}
		if len(result.Passes.Contradictions) > 0 {
			// Printed to stderr, and never folded into the change summary
			// above: nothing was written for these, and a human has to pick
			// which convention the codebase actually follows.
			fmt.Fprintf(os.Stderr, "\n%s\n", warden.FormatContradictions(result.Passes.Contradictions))
		}
		if !result.Passes.HasChanges() {
			fmt.Println("No changes — active rules file already at steady state.")
		} else {
			fmt.Printf("\nWrote %s\n", warden.RulesPath(anvil.Path))
			if len(result.Passes.Consolidated) > 0 || len(result.Passes.Archived) > 0 {
				fmt.Printf("Wrote %s\n", warden.ArchivePath(anvil.Path))
			}
			fmt.Println("Review and commit the changes when ready.")
		}
		if result.FirstError != nil {
			fmt.Fprintf(os.Stderr, "Warning: first consolidation cluster error: %v\n", result.FirstError)
		}
		return nil
	},
}

var wardenRestoreCmd = &cobra.Command{
	Use:   "restore <rule-id>",
	Short: "Move an archived warden rule back into the active rules file",
	Long: `Look up rule-id in .forge/warden-rules.archive.yaml for the given
anvil, remove it from the archive, re-insert it into .forge/warden-rules.yaml,
and write both files. The embedded Rule (id, category, pattern, check, source,
added, paths) is preserved verbatim so a subsequent consolidate pass would
archive the same content.

Archive bookkeeping fields (archived_at, last_seen, archive_reason,
superseded_by) are intentionally dropped on restore — the active file does
not carry them. Re-archiving the rule later generates fresh bookkeeping.`,
	Args:    cobra.ExactArgs(1),
	Example: "  forge warden restore stale-rule-id --anvil munin",
	RunE: func(cmd *cobra.Command, args []string) error {
		ruleID := args[0]
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

		archivePath := warden.ArchivePath(anvil.Path)
		archive, err := warden.LoadArchive(archivePath)
		if err != nil {
			return fmt.Errorf("loading archive: %w", err)
		}
		if len(archive.Rules) == 0 {
			return fmt.Errorf("archive is empty at %s", archivePath)
		}

		archived, found := archive.Remove(ruleID)
		if !found {
			return fmt.Errorf("rule %q not found in archive %s", ruleID, archivePath)
		}

		rf, err := warden.LoadRules(anvil.Path)
		if err != nil {
			return fmt.Errorf("loading active rules: %w", err)
		}

		// AddRule preserves the embedded Rule's fields verbatim (including
		// Added) and only fills Added when empty — restore keeps the original
		// timestamp so a subsequent consolidate sees the same Rule content.
		if !rf.AddRule(archived.Rule) {
			return fmt.Errorf("rule %q already exists in active rules file %s; archive entry was not removed",
				ruleID, warden.RulesPath(anvil.Path))
		}

		// Write active rules first: if archive save then fails, the rule is
		// duplicated rather than lost. The reverse ordering would risk losing
		// the rule entirely if the active-file write failed mid-flight.
		if err := warden.SaveRules(anvil.Path, rf); err != nil {
			return fmt.Errorf("saving active rules: %w", err)
		}
		if err := archive.Save(archivePath); err != nil {
			return fmt.Errorf("saving archive (active file already updated; rule duplicated): %w", err)
		}

		fmt.Printf("Restored rule %q to %s\n", ruleID, warden.RulesPath(anvil.Path))
		fmt.Printf("Removed from %s (was archived: reason=%q, superseded_by=%q)\n",
			archivePath, archived.ArchiveReason, archived.SupersededBy)
		fmt.Printf("Active rules: %d  Archive: %d\n", len(rf.Rules), len(archive.Rules))
		return nil
	},
}
