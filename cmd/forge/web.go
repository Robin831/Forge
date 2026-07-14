package main

import (
	"encoding/json"
	"fmt"

	"github.com/Robin831/Forge/internal/daemon"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/spf13/cobra"
)

func init() {
	webCmd.AddCommand(webRevokeSessionsCmd)
	rootCmd.AddCommand(webCmd)
}

var webCmd = &cobra.Command{
	Use:     "web",
	Short:   "Manage the Hearth web UI",
	GroupID: "daemon",
	Long:    `Commands for operating the Hearth 2.0 web UI served by the daemon.`,
}

var webRevokeSessionsCmd = &cobra.Command{
	Use:   "revoke-sessions",
	Short: "Revoke all web sessions, forcing every user to re-authenticate",
	Long: `Delete every active Hearth web session. All signed-in browsers are logged
out and must sign in again. Use this as an incident-response escape hatch when
a session cookie may have been compromised.

The command talks to the running daemon over IPC; the web server does not need
to be enabled for the revocation to succeed (the session table is simply
emptied).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, running := daemon.IsRunning(); !running {
			return fmt.Errorf("daemon is not running")
		}
		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w", err)
		}
		defer client.Close()

		resp, err := client.Send(ipc.Command{Type: "revoke_web_sessions"})
		if err != nil {
			return fmt.Errorf("sending revoke_web_sessions: %w", err)
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
		var body struct {
			Revoked int64  `json:"revoked"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(resp.Payload, &body)
		if body.Message != "" {
			fmt.Println(body.Message)
		} else {
			fmt.Printf("revoked %d web session(s)\n", body.Revoked)
		}
		return nil
	},
}
