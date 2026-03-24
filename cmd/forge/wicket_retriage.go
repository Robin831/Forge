package main

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/spf13/cobra"
)

func init() {
	wicketCmd.AddCommand(wicketRetrageCmd)
}

var wicketRetrageCmd = &cobra.Command{
	Use:   "retriage <repo> <issue-number>",
	Short: "Reset a Wicket issue to pending for re-triage",
	Long: `Resets the triage decision for a tracked GitHub issue, clearing its action
and reason and setting it back to "pending" so the next Wicket scan will
re-evaluate it.

Useful when a triage decision was incorrect (e.g. auto-rejected but should
create a bead) or when you want to force a fresh look at an issue.`,
	Args:    cobra.ExactArgs(2),
	Example: `  forge wicket retriage owner/repo 42`,
	RunE: func(cmd *cobra.Command, args []string) error {
		repo := args[0]
		issueNumber, err := strconv.Atoi(args[1])
		if err != nil || issueNumber <= 0 {
			return fmt.Errorf("issue-number must be a positive integer, got %q", args[1])
		}

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.WicketRetragePayload{
			Repo:        repo,
			IssueNumber: issueNumber,
		})

		resp, err := client.Send(ipc.Command{
			Type:    "wicket_retriage",
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
		if err := json.Unmarshal(resp.Payload, &result); err == nil && result["message"] != "" {
			fmt.Println(result["message"])
		} else {
			fmt.Printf("%s#%d reset to pending for re-triage\n", repo, issueNumber)
		}
		return nil
	},
}
