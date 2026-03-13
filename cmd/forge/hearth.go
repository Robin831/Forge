package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/hearth"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

func init() {
	rootCmd.AddCommand(hearthCmd)
	hearthCmd.Flags().Bool("no-mouse", false, "disable mouse reporting (restores normal terminal text selection)")
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

		anvilNames := make([]string, 0, len(cfg.Anvils))
		for name := range cfg.Anvils {
			anvilNames = append(anvilNames, name)
		}
		sort.Strings(anvilNames)

		ds := &hearth.DataSource{
			DB:                       db,
			MaxCIFixAttempts:         cfg.Settings.MaxCIFixAttempts,
			MaxReviewFixAttempts:     cfg.Settings.MaxReviewFixAttempts,
			MaxRebaseAttempts:        cfg.Settings.MaxRebaseAttempts,
			AnvilNames:               anvilNames,
			DailyCostLimit:           cfg.Settings.DailyCostLimit,
			CopilotDailyRequestLimit: cfg.Settings.CopilotDailyRequestLimit,
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
		model.OnRetryBead = func(beadID, anvil string, prID int) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.RetryBeadPayload{
				BeadID: beadID,
				Anvil:  anvil,
				PRID:   prID,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "retry_bead",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return ipcError(resp)
			}
			return nil
		}
		model.OnDismissBead = func(beadID, anvil string, prID int) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.DismissBeadPayload{
				BeadID: beadID,
				Anvil:  anvil,
				PRID:   prID,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "dismiss_bead",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return ipcError(resp)
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
		model.OnMergePR = func(prID, prNumber int, anvil string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.MergePRPayload{
				PRID:     prID,
				PRNumber: prNumber,
				Anvil:    anvil,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "merge_pr",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return ipcError(resp)
			}
			return nil
		}
		model.OnPRAction = func(prID, prNumber int, anvil, beadID, branch, action string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()
			payload, _ := json.Marshal(ipc.PRActionPayload{
				PRID:     prID,
				PRNumber: prNumber,
				Anvil:    anvil,
				BeadID:   beadID,
				Branch:   branch,
				Action:   action,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "pr_action",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return ipcError(resp)
			}
			return nil
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
				return ipcError(resp)
			}
			return nil
		}

		model.OnCloseBead = func(beadID, anvil string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()

			payload, _ := json.Marshal(ipc.CloseBeadPayload{
				BeadID: beadID,
				Anvil:  anvil,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "close_bead",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return ipcError(resp)
			}
			return nil
		}

		model.OnForceRunBead = func(beadID, anvil string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()

			payload, _ := json.Marshal(ipc.RunBeadPayload{
				BeadID:   beadID,
				Anvil:    anvil,
				ForceRun: true,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "run_bead",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return ipcError(resp)
			}
			return nil
		}

		model.OnStopBead = func(beadID, anvil string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()

			payload, _ := json.Marshal(ipc.StopBeadPayload{
				BeadID: beadID,
				Anvil:  anvil,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "stop_bead",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return ipcError(resp)
			}
			return nil
		}

		model.OnResolveOrphan = func(beadID, anvil, action string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return err
			}
			defer client.Close()

			payload, _ := json.Marshal(ipc.ResolveOrphanPayload{
				BeadID: beadID,
				Anvil:  anvil,
				Action: action,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "resolve_orphan",
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return err
			}
			if resp.Type != "ok" {
				return ipcError(resp)
			}
			return nil
		}

		model.OnAppendNotes = func(beadID, anvil, notes string) error {
			client, err := ipc.NewClient()
			if err != nil {
				return fmt.Errorf("connecting to daemon: %w", err)
			}
			defer client.Close()

			payload, _ := json.Marshal(ipc.AppendNotesPayload{
				BeadID: beadID,
				Anvil:  anvil,
				Notes:  notes,
			})
			resp, err := client.Send(ipc.Command{
				Type:    "append_notes",
				Payload: payload,
			})
			if err != nil {
				return fmt.Errorf("sending append_notes command: %w", err)
			}
			if resp.Type == "error" {
				var msg map[string]string
				_ = json.Unmarshal(resp.Payload, &msg)
				return fmt.Errorf("daemon error: %s", msg["message"])
			}
			return nil
		}

		noMouse, _ := cmd.Flags().GetBool("no-mouse")
		mouseEnabled := !noMouse
		model.SetMouseEnabled(mouseEnabled)
		opts := []tea.ProgramOption{tea.WithAltScreen()}
		if mouseEnabled {
			// Mouse reporting enables click-to-focus and wheel scrolling.
			// Press 'm' inside Hearth to toggle mouse off and restore terminal text selection.
			opts = append(opts, tea.WithMouseCellMotion())
		}
		p := tea.NewProgram(&model, opts...)
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			return err
		}
		return nil
	},
}

// ipcError extracts a human-readable error message from a daemon error response.
func ipcError(resp *ipc.Response) error {
	var payload struct{ Message string }
	if json.Unmarshal(resp.Payload, &payload) == nil && payload.Message != "" {
		return fmt.Errorf("daemon: %s", payload.Message)
	}
	return fmt.Errorf("daemon returned: %s", resp.Type)
}
