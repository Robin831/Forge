package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/spf13/cobra"
)

func init() {
	assayRunCmd.Flags().Int("pr", 0, "PR id (state.db primary key) to re-review")
	assayRunCmd.Flags().StringP("anvil", "a", "", "Anvil the PR belongs to")
	_ = assayRunCmd.MarkFlagRequired("pr")
	_ = assayRunCmd.MarkFlagRequired("anvil")
	assayCmd.AddCommand(assayRunCmd)

	assayRerunCmd.Flags().StringP("anvil", "a", "", "Anvil the PR belongs to")
	_ = assayRerunCmd.MarkFlagRequired("anvil")
	assayCmd.AddCommand(assayRerunCmd)

	rootCmd.AddCommand(assayCmd)
}

var assayCmd = &cobra.Command{
	Use:     "assay",
	Short:   "Inspect and trigger Assay AI PR reviews",
	GroupID: "work",
}

const assayRerunLong = `Trigger a fresh Assay AI review pass over a PR's current head.

The rerun bypasses the Bellows trigger gate's head-SHA debounce, so it forces a
review even when the current head has already been reviewed. This is the same
action the web UI's "Re-run" button invokes via the daemon.`

var assayRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Re-run Assay review over a PR's current head (by state.db PR id)",
	Long: assayRerunLong + `

This verb addresses the PR by its state.db row id — the id the dashboard holds.
To use the GitHub PR number instead, run 'forge assay rerun <pr> --anvil <a>'.`,
	Example: "  forge assay run --pr 12 --anvil heimdall",
	RunE: func(cmd *cobra.Command, args []string) error {
		prID, _ := cmd.Flags().GetInt("pr")
		anvil, _ := cmd.Flags().GetString("anvil")
		return sendAssayRerun(ipc.AssayRerunPayload{Anvil: anvil, PR: prID})
	},
}

var assayRerunCmd = &cobra.Command{
	Use:   "rerun <pr>",
	Short: "Re-run Assay review over a PR's current head (by GitHub PR number)",
	Long: assayRerunLong + `

<pr> is the GitHub pull request number — what the PR page shows — scoped by
--anvil, since PR numbers are per-repository. To address the PR by its state.db
row id instead, run 'forge assay run --pr <id> --anvil <a>'.`,
	Example: "  forge assay rerun 431 --anvil heimdall",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prNumber, err := parsePRNumberArg(args[0])
		if err != nil {
			return err
		}
		anvil, _ := cmd.Flags().GetString("anvil")
		return sendAssayRerun(ipc.AssayRerunPayload{Anvil: anvil, PRNumber: prNumber})
	},
}

// parsePRNumberArg reads a positional GitHub PR number, accepting the leading
// "#" an operator copies off a PR page. Zero and negatives are rejected here
// rather than sent, since the daemon reads a zero as "no target supplied".
func parsePRNumberArg(arg string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(arg), "#"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid PR number %q: expected a positive integer", arg)
	}
	return n, nil
}

// sendAssayRerun sends one assay_rerun command and prints the daemon's message.
// Both assay verbs go through it so they differ only in how they address the
// PR — the payload the daemon receives, and the output an operator sees, are
// otherwise identical.
func sendAssayRerun(p ipc.AssayRerunPayload) error {
	client, err := ipc.NewClient()
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
	}
	defer client.Close()

	payload, _ := json.Marshal(p)

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
		return nil
	}
	if p.PRNumber > 0 {
		fmt.Printf("Assay re-review started for PR #%d\n", p.PRNumber)
	} else {
		fmt.Printf("Assay re-review started for PR id %d\n", p.PR)
	}
	return nil
}
