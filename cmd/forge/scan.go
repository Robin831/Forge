package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vulncheck"
	"github.com/spf13/cobra"
)

func init() {
	scanCmd.Flags().Bool("create-beads", true, "Auto-create beads for discovered vulnerabilities")
	scanCmd.Flags().StringP("anvil", "a", "", "Scan only the named anvil (default: all)")
	rootCmd.AddCommand(scanCmd)
}

var scanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Run govulncheck on registered anvils",
	Long: `Runs govulncheck on all Go-based registered anvils (or a specific one).
Vulnerabilities found are reported and optionally turned into beads with
appropriate priority based on severity.`,
	GroupID: "work",
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

		// Filter to a specific anvil if requested
		anvils := cfg.Anvils
		if anvilName, _ := cmd.Flags().GetString("anvil"); anvilName != "" {
			a, ok := cfg.Anvils[anvilName]
			if !ok {
				return fmt.Errorf("unknown anvil %q", anvilName)
			}
			anvils = map[string]config.AnvilConfig{anvilName: a}
		}

		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state db: %w", err)
		}
		defer db.Close()

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))

		scanner := vulncheck.New(db, logger, anvils, cfg.Settings.VulncheckTimeout)
		results := scanner.ScanAll(rootCtx)

		// Create beads regardless of output format
		createBeads, _ := cmd.Flags().GetBool("create-beads")
		if createBeads {
			created, err := scanner.CreateBeads(rootCtx, results)
			if err != nil {
				return fmt.Errorf("creating beads: %w", err)
			}
			if !jsonOutput && created > 0 {
				fmt.Printf("\nCreated %d new beads for vulnerabilities\n", created)
			}
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(results)
		}

		// Display results
		totalVulns := 0
		anyErr := false
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "Error scanning %s: %v\n", r.Anvil, r.Err)
				anyErr = true
				continue
			}

			if len(r.Vulns) == 0 {
				fmt.Printf("%s: no vulnerabilities found\n", r.Anvil)
				continue
			}

			fmt.Printf("\n%s: %d vulnerabilities\n", r.Anvil, len(r.Vulns))
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "  ID\tSEVERITY\tPACKAGE\tFIX\tSUMMARY\n")
			for _, v := range r.Vulns {
				fix := v.FixedIn
				if fix == "" {
					fix = "none"
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
					v.ID, v.Severity, v.AffectedPkg, fix, truncate(v.Summary, 50))
			}
			tw.Flush()
			totalVulns += len(r.Vulns)
		}

		if totalVulns == 0 && !anyErr {
			fmt.Println("\nAll anvils clean — no vulnerabilities found. Looks like your code is tougher than a blacksmith's anvil!")
		}

		return nil
	},
}
