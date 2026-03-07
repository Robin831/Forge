package main

import (
	"encoding/json"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/hearth"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

func init() {
	rootCmd.AddCommand(hearthCmd)
}

var hearthCmd = &cobra.Command{
	Use:     "hearth",
	Short:   "Open the TUI dashboard",
	Long:    "Opens the Hearth TUI dashboard showing the bead queue, active workers, and event log.\n\nThe daemon must be running (forge up) for the queue to be populated. Hearth reads\nthe daemon's cached poll data from the state database rather than polling anvils directly.",
	GroupID: "daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state database: %w", err)
		}
		defer db.Close()

		cfg, err := config.Load("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to load config for Hearth (using defaults): %v\n", err)
			cfg = &config.Config{}
			*cfg = config.Defaults()
		} else if cfg == nil {
			cfg = &config.Config{}
			*cfg = config.Defaults()
		}

		ds := &hearth.DataSource{
			DB:                   db,
			MaxCIFixAttempts:     cfg.Settings.MaxCIFixAttempts,
			MaxReviewFixAttempts: cfg.Settings.MaxReviewFixAttempts,
			MaxRebaseAttempts:    cfg.Settings.MaxRebaseAttempts,
		}

		model := hearth.NewModel(ds)
		model.OnKill = func(workerID string, pid int) {
			client, err := ipc.NewClient()
			if err != nil {
				return
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.KillWorkerPayload{
				WorkerID: workerID,
				PID:      pid,
			})
			_, _ = client.Send(ipc.Command{
				Type:    "kill_worker",
				Payload: json.RawMessage(payload),
			})
		}
		model.OnRetryBead = func(beadID, anvil string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.RetryBeadPayload{
				BeadID: beadID,
				Anvil:  anvil,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "retry_bead",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return fmt.Errorf("daemon returned: %s", resp.Type)
			}
			return nil
		}
		model.OnDismissBead = func(beadID, anvil string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.DismissBeadPayload{
				BeadID: beadID,
				Anvil:  anvil,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "dismiss_bead",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return fmt.Errorf("daemon returned: %s", resp.Type)
			}
			return nil
		}
		model.OnViewLogs = func(beadID string) (string, []string) {
			client, err := ipc.NewClient()
			if err != nil {
				return "", nil
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.ViewLogsPayload{
				BeadID: beadID,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "view_logs",
				Payload: json.RawMessage(payload),
			})
			if err != nil || resp.Type != "ok" {
				return "", nil
			}
			var result ipc.ViewLogsResponse
			if err := json.Unmarshal(resp.Payload, &result); err != nil {
				return "", nil
			}
			return result.LogPath, result.LastLines
		}
		model.OnTagBead = func(beadID, anvil string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()

			// The daemon derives the tag from its own (hot-reloaded) config, so
			// the client only needs to send the bead identity.
			payload, _ := json.Marshal(ipc.TagBeadPayload{
				BeadID: beadID,
				Anvil:  anvil,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "tag_bead",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return fmt.Errorf("daemon returned: %s", resp.Type)
			}
			return nil
		}

		p := tea.NewProgram(&model, tea.WithAltScreen())
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			return err
		}
		return nil
	},
}
