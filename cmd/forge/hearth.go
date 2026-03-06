package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

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
	Long:    "Opens the Hearth TUI dashboard showing the bead queue, active workers, and event log.",
	GroupID: "daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state database: %w", err)
		}
		defer db.Close()

		ctx, cancel := context.WithCancel(rootCtx)
		defer cancel()

		ds := &hearth.DataSource{
			DB:     db,
			Anvils: cfg.Anvils,
			Ctx:    ctx,
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
		model.OnResetBead = func(beadID, anvil string) {
			client, err := ipc.NewClient()
			if err != nil {
				return
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.ResetBeadPayload{
				BeadID: beadID,
				Anvil:  anvil,
			})
			_, _ = client.Send(ipc.Command{
				Type:    "reset_bead",
				Payload: json.RawMessage(payload),
			})
		}
		model.OnDismissBead = func(beadID, anvil string) {
			client, err := ipc.NewClient()
			if err != nil {
				return
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.DismissBeadPayload{
				BeadID: beadID,
				Anvil:  anvil,
			})
			_, _ = client.Send(ipc.Command{
				Type:    "dismiss_bead",
				Payload: json.RawMessage(payload),
			})
		}

		p := tea.NewProgram(&model, tea.WithAltScreen())
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			return err
		}
		return nil
	},
}
