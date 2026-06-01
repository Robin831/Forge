package main

import (
	"encoding/json"
	"fmt"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/spf13/cobra"
)

func init() {
	assayRunCmd.Flags().Int("pr", 0, "PR id (state.db primary key) to re-review")
	assayRunCmd.Flags().StringP("anvil", "a", "", "Anvil the PR belongs to")
	_ = assayRunCmd.MarkFlagRequired("pr")
	_ = assayRunCmd.MarkFlagRequired("anvil")
	assayCmd.AddCommand(assayRunCmd)
	rootCmd.AddCommand(assayCmd)
}

var assayCmd = &cobra.Command{
	Use:     "assay",
	Short:   "Inspect and trigger Assay AI PR reviews",
	GroupID: "work",
}

var assayRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Re-run Assay review over a PR's current head",
	Long: `Trigger a fresh Assay AI review pass over a PR's current head.

The rerun bypasses the Bellows trigger gate's head-SHA debounce, so it forces a
review even when the current head has already been reviewed. This is the same
action the web UI's "Re-run" button invokes via the daemon.`,
	Example: "  forge assay run --pr 12 --anvil heimdall",
	RunE: func(cmd *cobra.Command, args []string) error {
		prID, _ := cmd.Flags().GetInt("pr")
		anvil, _ := cmd.Flags().GetString("anvil")

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.AssayRerunPayload{
			Anvil: anvil,
			PR:    prID,
		})

		resp, err := client.Send(ipc.Command{
			Type:    "assay_rerun",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}

		if resp.Type == "error" {
			var msg map[string]string
			var errMsg string
			if err := json.Unmarshal(resp.Payload, &msg); err == nil && msg["message"] != "" {
				errMsg = msg["message"]
			} else if len(resp.Payload) > 0 {
				errMsg = string(resp.Payload)
			} else {
				errMsg = "unknown error from daemon"
			}
			return fmt.Errorf("daemon error: %s", errMsg)
		}

		var result map[string]string
		if err := json.Unmarshal(resp.Payload, &result); err != nil {
			return fmt.Errorf("failed to unmarshal daemon response: %w", err)
		}
		if result["message"] != "" {
			fmt.Println(result["message"])
		} else {
			fmt.Printf("Assay re-review started for PR id %d\n", prID)
		}
		return nil
	},
}
