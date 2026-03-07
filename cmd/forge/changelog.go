package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Robin831/Forge/internal/changelog"
	"github.com/spf13/cobra"
)

func init() {
	changelogAssembleCmd.Flags().String("dir", ".", "Root directory containing changelog.d/")
	changelogAssembleCmd.Flags().String("output", "CHANGELOG.md", "Output file name")
	changelogAssembleCmd.Flags().Bool("dry-run", false, "Print assembled changelog without writing")

	changelogValidateCmd.Flags().String("dir", ".", "Root directory containing changelog.d/")

	changelogCmd.AddCommand(changelogAssembleCmd)
	changelogCmd.AddCommand(changelogValidateCmd)
	rootCmd.AddCommand(changelogCmd)
}

var changelogCmd = &cobra.Command{
	Use:     "changelog",
	Short:   "Manage changelog fragments",
	GroupID: "config",
}

var changelogAssembleCmd = &cobra.Command{
	Use:   "assemble",
	Short: "Assemble changelog.d fragments into CHANGELOG.md",
	Long: `Reads all changelog fragments from changelog.d/ and assembles them into
the [Unreleased] section of CHANGELOG.md. Fragments are grouped by category
(Added, Changed, Deprecated, Removed, Fixed, Security).

Use --dry-run to preview without writing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, _ := cmd.Flags().GetString("dir")
		output, _ := cmd.Flags().GetString("output")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		fragDir := filepath.Join(dir, "changelog.d")
		fragments, err := changelog.CollectFragments(fragDir)
		if err != nil {
			return fmt.Errorf("collecting fragments: %w", err)
		}

		if len(fragments) == 0 {
			fmt.Fprintln(os.Stderr, "No changelog fragments found in changelog.d/")
			return nil
		}

		clPath := filepath.Join(dir, output)
		content, err := changelog.UpdateChangelog(clPath, fragments)
		if err != nil {
			return fmt.Errorf("assembling changelog: %w", err)
		}

		if dryRun {
			fmt.Print(content)
			return nil
		}

		if err := os.WriteFile(clPath, []byte(content), 0644); err != nil {
			return fmt.Errorf("writing %s: %w", output, err)
		}

		fmt.Fprintf(os.Stderr, "Assembled %d fragments into %s\n", len(fragments), output)
		return nil
	},
}

var changelogValidateCmd = &cobra.Command{
	Use:   "validate [bead-id...]",
	Short: "Check that changelog fragments exist for the given bead IDs",
	Long: `Validates that each specified bead ID has a corresponding changelog fragment
in changelog.d/. Exits with code 1 if any are missing. Used by CI to enforce
fragment presence on PRs.

If no bead IDs are given, validates all existing fragments for correct format.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, _ := cmd.Flags().GetString("dir")
		fragDir := filepath.Join(dir, "changelog.d")

		if len(args) == 0 {
			// Validate all existing fragments
			fragments, err := changelog.CollectFragments(fragDir)
			if err != nil {
				return err
			}

			if jsonOutput {
				return json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
					"valid": true,
					"count": len(fragments),
				})
			}

			fmt.Fprintf(os.Stderr, "All %d fragments are valid\n", len(fragments))
			return nil
		}

		// Validate specific bead IDs have fragments
		missing := []string{}
		for _, id := range args {
			if !changelog.ValidateFragmentExists(fragDir, id) {
				missing = append(missing, id)
			}
		}

		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"valid":   len(missing) == 0,
				"missing": missing,
			})
		}

		if len(missing) > 0 {
			for _, id := range missing {
				fmt.Fprintf(os.Stderr, "Missing changelog fragment for %s\n", id)
			}
			return fmt.Errorf("%d changelog fragments missing", len(missing))
		}

		fmt.Fprintln(os.Stderr, "All changelog fragments present")
		return nil
	},
}


