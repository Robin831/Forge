package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/spf13/cobra"
)

func init() {
	bellowsStopCmd.Flags().StringP("anvil", "a", "", "Anvil the PR belongs to")
	_ = bellowsStopCmd.MarkFlagRequired("anvil")
	bellowsCmd.AddCommand(bellowsStopCmd)

	bellowsResumeCmd.Flags().StringP("anvil", "a", "", "Anvil the PR belongs to")
	_ = bellowsResumeCmd.MarkFlagRequired("anvil")
	bellowsCmd.AddCommand(bellowsResumeCmd)

	rootCmd.AddCommand(bellowsCmd)
}

var bellowsCmd = &cobra.Command{
	Use:     "bellows",
	Short:   "Control the Bellows PR monitor per pull request",
	GroupID: "work",
}

// bellowsLongCommon is the shared prologue of both mute verbs' help text. Each
// command appends the paragraph describing its own direction.
const bellowsLongCommon = `Mute or unmute Bellows for a single pull request.

A muted ("detached") PR is still watched — its mergeability and terminal state
keep being refreshed, so the PR panel goes on telling the truth — but Bellows
emits no events for it and dispatches no automatic CI fixes, review fixes,
rebases or Assay runs. Its worker row stays in place, marked detached.

The mute is reversible and never bricks the PR: manual verbs (` + "`forge assay run`" + `,
` + "`forge queue run`" + `, the dashboard's fix buttons) still run a single pass by hand,
and a detached PR that merges is still recorded as merged and its bead closed.`

var bellowsStopCmd = &cobra.Command{
	Use:   "stop <pr-number>",
	Short: "Stop Bellows working a PR automatically (detach)",
	Long: bellowsLongCommon + `

Stopping also kills the CI-fix, review-fix and rebase workers already running
for the PR, so nothing pushes one more commit to the branch after the mute.

'forge bellows resume <pr-number> --anvil <name>' is the inverse.`,
	Example: "  forge bellows stop 431 --anvil heimdall",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		anvil, _ := cmd.Flags().GetString("anvil")
		return runBellowsMute(ipc.PRActionDetachBellows, args, anvil, os.Stdout)
	},
}

var bellowsResumeCmd = &cobra.Command{
	Use:   "resume <pr-number>",
	Short: "Resume Bellows working a PR automatically (reattach)",
	Long: bellowsLongCommon + `

Resuming drops the snapshot Bellows cached while the PR was muted, so problems
that outlived the mute — failing CI, a conflict, unresolved threads — are
re-detected as fresh transitions instead of being swallowed as state Bellows has
already seen. It restarts nothing that was skipped: automatic work resumes from
the next cycle onwards.`,
	Example: "  forge bellows resume 431 --anvil heimdall",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		anvil, _ := cmd.Flags().GetString("anvil")
		return runBellowsMute(ipc.PRActionReattachBellows, args, anvil, os.Stdout)
	},
}

// errBellowsAnvilRequired is the client-side refusal for a missing --anvil.
// Cobra's required-flag check already catches it on a real invocation; this
// covers the paths that do not go through cobra's parser and states why the
// flag is not optional.
var errBellowsAnvilRequired = errors.New("--anvil is required: PR numbers are per-repository, so the anvil is the other half of the identifier")

// runBellowsMute is the whole body of both verbs — they differ only by the
// action constant, so the argument handling, the refusals and the confirmation
// line cannot drift between stop and resume.
//
// The PR is addressed the way an operator reads it off a PR page: its GitHub
// number, scoped by --anvil. Resolving that pair to a concrete `prs` row is the
// daemon's job (resolvePRTarget, via pr_action's shared
// resolvePRTargetPreferID) because the daemon owns state.db — and it is the
// same lookup for both kinds of PR, since an externally-opened `ext-*` PR has
// an ordinary row keyed by anvil and number just like a forge-authored one. A
// PR that will not resolve is refused there rather than reported as muted here.
func runBellowsMute(action string, args []string, anvil string, w io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("expected exactly one PR number, got %d", len(args))
	}
	prNumber, err := parsePRNumberArg(args[0])
	if err != nil {
		return err
	}
	anvil = strings.TrimSpace(anvil)
	if anvil == "" {
		return errBellowsAnvilRequired
	}

	if _, err := bellowsSend(ipc.PRActionPayload{
		Action:   action,
		PRNumber: prNumber,
		Anvil:    anvil,
	}); err != nil {
		return err
	}

	fmt.Fprintln(w, bellowsStateLine(action, prNumber, anvil))
	return nil
}

// bellowsStateLine renders the confirmation. The daemon's own reply for these
// verbs is the generic "PR #431: detach_bellows", which names the wire verb
// rather than what changed, so the state is spelled out here instead.
func bellowsStateLine(action string, prNumber int, anvil string) string {
	switch action {
	case ipc.PRActionDetachBellows:
		return fmt.Sprintf("PR #%d (%s): bellows stopped — no automatic CI fixes, review fixes, rebases or Assay runs until it is resumed.", prNumber, anvil)
	case ipc.PRActionReattachBellows:
		return fmt.Sprintf("PR #%d (%s): bellows resumed — automatic work returns from the next cycle.", prNumber, anvil)
	default:
		return fmt.Sprintf("PR #%d (%s): %s", prNumber, anvil, action)
	}
}

// bellowsSend is the seam the tests replace: everything above it is argument
// handling and rendering, everything below it is the socket.
var bellowsSend = sendPRAction

// sendPRAction sends one pr_action command and returns the daemon's decoded
// message map. A non-ok response is surfaced with the daemon's own text —
// "cannot detach PR #404: PR #404 not found on anvil "munin"" is the daemon's
// sentence, and rephrasing it here would lose which of the two halves of the
// identifier was wrong.
func sendPRAction(p ipc.PRActionPayload) (map[string]string, error) {
	client, err := ipc.NewClient()
	if err != nil {
		return nil, fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
	}
	defer client.Close()

	payload, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encoding payload: %w", err)
	}

	resp, err := client.Send(ipc.Command{Type: "pr_action", Payload: payload})
	if err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}
	if resp.Type != "ok" {
		return nil, ipcError(resp)
	}

	var result map[string]string
	if len(resp.Payload) > 0 {
		if err := json.Unmarshal(resp.Payload, &result); err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}
	}
	return result, nil
}
