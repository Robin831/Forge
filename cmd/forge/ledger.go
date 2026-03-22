package main

import (
	"fmt"
	"io"
	"log"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ledger"
	"github.com/Robin831/Forge/internal/state"
)

func init() {
	rootCmd.AddCommand(ledgerCmd)
	ledgerCmd.Flags().Bool("no-mouse", false, "disable mouse reporting (restores normal terminal text selection)")
}

var ledgerCmd = &cobra.Command{
	Use:     "ledger",
	Short:   "Open the interactive bead management TUI",
	Long:    "Opens the Ledger TUI for browsing and managing beads across all registered anvils.\n\nUnlike Hearth (which shows daemon state), Ledger queries beads directly from each anvil\nand does not require the daemon to be running.",
	GroupID: "work",
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state database: %w", err)
		}
		defer db.Close()

		cfg, err := config.Load("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to load config (using defaults): %v\n", err)
			cfg = &config.Config{}
			*cfg = config.Defaults()
		} else if cfg == nil {
			cfg = &config.Config{}
			*cfg = config.Defaults()
		}

		anvils := make(map[string]string, len(cfg.Anvils))
		for name, a := range cfg.Anvils {
			anvils[name] = a.Path
		}

		model := ledger.NewModel(anvils, cfg.Anvils, db)
		noMouse, _ := cmd.Flags().GetBool("no-mouse")
		// tea.WithMouseCellMotion enables mouse events (scroll wheel, clicks) throughout
		// the Ledger TUI, including list, kanban, hierarchy, and help overlay.
		// Pass --no-mouse to disable and restore normal terminal text selection.
		// Redirect Go's default logger away from stderr while the TUI is
		// running. Background goroutines use log.Printf which writes to
		// stderr and corrupts the alt-screen.
		prevLogOut := log.Writer()
		log.SetOutput(io.Discard)

		opts := []tea.ProgramOption{tea.WithAltScreen()}
		if !noMouse {
			opts = append(opts, tea.WithMouseCellMotion())
		}
		p := tea.NewProgram(model, opts...)
		_, err = p.Run()
		log.SetOutput(prevLogOut)
		if err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			return err
		}
		return nil
	},
}
