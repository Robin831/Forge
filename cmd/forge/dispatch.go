package main

import (
	"encoding/json"
	"fmt"

	"github.com/Robin831/Forge/internal/daemon"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(pauseCmd)
	rootCmd.AddCommand(resumeCmd)
}

// sendDispatchToggle sends a pause_dispatch / resume_dispatch command to the
// daemon and prints the daemon's acknowledgement. It is shared by the pause
// and resume CLI commands.
func sendDispatchToggle(cmdType, action string) error {
	if _, running := daemon.IsRunning(); !running {
		return fmt.Errorf("daemon is not running")
	}
	client, err := ipc.NewClient()
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer client.Close()

	resp, err := client.Send(ipc.Command{Type: cmdType})
	if err != nil {
		return fmt.Errorf("sending %s: %w", cmdType, err)
	}
	if resp.Type == "error" {
		var e map[string]string
		if err := json.Unmarshal(resp.Payload, &e); err == nil && e["message"] != "" {
			return fmt.Errorf("daemon error: %s", e["message"])
		}
		return fmt.Errorf("daemon error: %s", string(resp.Payload))
	}

	if jsonOutput {
		fmt.Println(string(resp.Payload))
		return nil
	}
	var msg map[string]string
	_ = json.Unmarshal(resp.Payload, &msg)
	if m := msg["message"]; m != "" {
		fmt.Println(m)
	} else {
		fmt.Printf("dispatch %s\n", action)
	}
	return nil
}

var pauseCmd = &cobra.Command{
	Use:     "pause",
	Short:   "Pause auto-dispatch of new workers",
	GroupID: "daemon",
	Long: `Pause daemon-wide auto-dispatch. Forge stops claiming and dispatching
NEW beads, but all currently-running workers are left untouched and finish
normally. Manual 'forge queue run <id>' still works while paused.

Use this to drain the active worker set to zero before rebuilding or restarting
the daemon mid-day without trampling in-flight work: pause, wait for workers to
finish, then restart.

Note: pausing does not make a restart free — workers still running at restart
are killed and their beads reset to open (orphan recovery). The value of pausing
is letting the active set drain to empty first.

The pause is in-memory only and resets on daemon restart (a restart resumes
dispatch by default).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return sendDispatchToggle("pause_dispatch", "paused")
	},
}

var resumeCmd = &cobra.Command{
	Use:     "resume",
	Short:   "Resume auto-dispatch of new workers",
	GroupID: "daemon",
	Long: `Resume daemon-wide auto-dispatch after a 'forge pause'. New beads are
dispatched again on the next poll (a poll is triggered immediately).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return sendDispatchToggle("resume_dispatch", "resumed")
	},
}
