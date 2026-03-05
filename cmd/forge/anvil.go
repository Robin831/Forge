package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	anvilCmd.AddCommand(anvilAddCmd)
	anvilCmd.AddCommand(anvilRemoveCmd)
	anvilCmd.AddCommand(anvilListCmd)

	rootCmd.AddCommand(anvilCmd)
}

var anvilCmd = &cobra.Command{
	Use:     "anvil",
	Short:   "Manage registered repositories (anvils)",
	GroupID: "anvil",
}

var anvilAddCmd = &cobra.Command{
	Use:   "add <name> <path>",
	Short: "Register a repository as an anvil",
	Long: `Register a repository for Forge to manage. The path must point to a
directory containing a .beads/ directory (indicating it uses beads for issue tracking).`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		repoPath := args[1]

		// Resolve to absolute path
		absPath, err := filepath.Abs(repoPath)
		if err != nil {
			return fmt.Errorf("resolving path: %w", err)
		}

		// Validate directory exists
		info, err := os.Stat(absPath)
		if err != nil {
			return fmt.Errorf("path %q: %w", absPath, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("path %q is not a directory", absPath)
		}

		// Validate .beads/ directory exists
		beadsDir := filepath.Join(absPath, ".beads")
		if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
			return fmt.Errorf("path %q does not contain a .beads/ directory — is this a beads-enabled repo?", absPath)
		}

		// Load existing config (or create new)
		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}

		// Check for duplicate name
		if _, exists := cfg.Anvils[name]; exists {
			return fmt.Errorf("anvil %q already exists (use 'forge anvil remove %s' first)", name, name)
		}

		// Add the anvil
		if cfg.Anvils == nil {
			cfg.Anvils = make(map[string]config.AnvilConfig)
		}
		cfg.Anvils[name] = config.AnvilConfig{
			Path:      absPath,
			MaxSmiths: 1, // Default: 1 concurrent Smith per anvil
		}

		// Save config
		savePath := configSavePath()
		if err := config.Save(cfg, savePath); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}

		fmt.Printf("Added anvil %q → %s\n", name, absPath)
		fmt.Printf("Config saved to %s\n", savePath)
		return nil
	},
}

var anvilRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Deregister an anvil",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}

		if _, exists := cfg.Anvils[name]; !exists {
			return fmt.Errorf("anvil %q not found", name)
		}

		delete(cfg.Anvils, name)

		savePath := configSavePath()
		if err := config.Save(cfg, savePath); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}

		fmt.Printf("Removed anvil %q\n", name)
		return nil
	},
}

var anvilListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered anvils",
	RunE: func(cmd *cobra.Command, args []string) error {
		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}

		if len(cfg.Anvils) == 0 {
			fmt.Println("No anvils registered. Use 'forge anvil add <name> <path>' to register one.")
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "NAME\tPATH\tMAX SMITHS\tSTATUS\n")

		for name, anvil := range cfg.Anvils {
			status := "ok"
			if _, err := os.Stat(anvil.Path); os.IsNotExist(err) {
				status = "missing"
			} else if _, err := os.Stat(filepath.Join(anvil.Path, ".beads")); os.IsNotExist(err) {
				status = "no .beads/"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", name, anvil.Path, anvil.MaxSmiths, status)
		}
		tw.Flush()

		return nil
	},
}

// configSavePath returns where to write the config file.
// Uses the --config flag if set, otherwise defaults to ./forge.yaml.
func configSavePath() string {
	if configFile != "" {
		return configFile
	}
	// If a config was already loaded from disk, write back there
	if path := config.ConfigFilePath(configFile); path != "" {
		return path
	}
	// Default to working directory
	return "forge.yaml"
}
