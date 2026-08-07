package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/spf13/cobra"
)

// previewStopWait bounds how long `forge preview stop` waits for the queued
// teardown to resolve. The daemon caps the teardown itself at 2 minutes
// (daemon.previewStopTimeout); the slack covers the round trips around it so
// the CLI reports the daemon's own outcome rather than its own impatience.
const previewStopWait = 150 * time.Second

// previewStartWait bounds how long `forge preview start` waits for the queued
// start to resolve. The daemon caps the start itself at 15 minutes
// (daemon.previewStartTimeout) — a manifest's setup command can be a full
// dependency install — and, as with the stop, the slack covers the round trips
// around it so the CLI reports the daemon's outcome and not its own impatience.
const previewStartWait = 16 * time.Minute

// previewPoll is how often a queued preview command's outcome is re-checked.
const previewPoll = 300 * time.Millisecond

func init() {
	previewStartCmd.Flags().StringP("anvil", "a", "", "Anvil the branch lives in (required)")
	_ = previewStartCmd.MarkFlagRequired("anvil")
	previewStartCmd.Flags().StringP("branch", "b", "", "Branch to preview (default: the bead's forge/<bead-id> branch)")

	previewCmd.AddCommand(previewListCmd)
	previewCmd.AddCommand(previewStartCmd)
	previewCmd.AddCommand(previewStopCmd)
	rootCmd.AddCommand(previewCmd)
}

var previewCmd = &cobra.Command{
	Use:     "preview",
	Short:   "Start, inspect and tear down Kiln preview environments",
	GroupID: "work",
}

var previewListCmd = &cobra.Command{
	Use:   "list",
	Short: "List running preview environments",
	Long: `Lists every live Kiln preview environment with its status, entry URL,
idle countdown and the resources (services and ports) it is holding.

Use --json for the raw daemon payload, which additionally carries per-service
health and the anvils a preview can be started for.`,
	Args:    cobra.NoArgs,
	Example: "  forge preview list\n  forge preview list --json",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		resp, err := client.Send(ipc.Command{Type: "preview_list"})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}
		if resp.Type != "ok" {
			return ipcError(resp)
		}

		if jsonOutput {
			// Echo the daemon's payload verbatim: it is already the documented
			// preview_list shape, and re-encoding a decoded struct would quietly
			// drop any field this binary predates.
			fmt.Println(string(resp.Payload))
			return nil
		}

		var list ipc.PreviewListResponse
		if err := json.Unmarshal(resp.Payload, &list); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}
		renderPreviewList(os.Stdout, list)
		return nil
	},
}

var previewStartCmd = &cobra.Command{
	Use:   "start <bead-id>",
	Short: "Start a preview environment for a branch",
	Long: `Starts a Kiln preview environment: checks the branch out into its own
detached preview checkout, runs the manifest's setup command and supervises the
services declared in <anvil>/.forge/preview.yaml.

The bead id is a registry key, not a lookup. It names the preview, keys its
logs and derives its hostname label, but it does not have to exist as a bd
issue — which is what makes this usable for ad-hoc work: smoke-testing a new
manifest, or previewing a branch that has no bead yet. Such previews
conventionally use ids like kiln-smoke-1.

Without --branch the bead's canonical forge/<bead-id> branch is previewed.

Starting runs asynchronously in the daemon (checkout, setup, health checks), so
this waits for the outcome and exits non-zero if it fails, reporting the
daemon's refusal — previews disabled, no manifest, the concurrency cap already
full — as the daemon phrased it. On success the entry URL is printed.

'forge preview stop <bead-id>' is the inverse; 'forge preview list' shows every
running preview.`,
	Args: cobra.ExactArgs(1),
	Example: "  forge preview start Forge-abc1 --anvil forge\n" +
		"  forge preview start kiln-smoke-1 --anvil heimdall --branch main",
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := strings.TrimSpace(args[0])
		if beadID == "" {
			return fmt.Errorf("bead id must not be empty")
		}
		anvil, _ := cmd.Flags().GetString("anvil")
		branch, _ := cmd.Flags().GetString("branch")

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.PreviewActionPayload{
			BeadID: beadID,
			Anvil:  strings.TrimSpace(anvil),
			// An unset --branch is sent as an empty field rather than a branch
			// name assembled here: the default belongs to the daemon, which
			// owns the forge/<bead-id> naming scheme.
			Branch: strings.TrimSpace(branch),
		})
		resp, err := client.Send(ipc.Command{
			Type:    "preview_start",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}
		// Previews disabled globally or for this anvil, an unknown anvil and a
		// missing bead id are all rejected synchronously.
		if resp.Type == "error" {
			return ipcError(resp)
		}

		message := fmt.Sprintf("preview for %s started", beadID)
		if resp.IsQueued() {
			outcome, err := awaitRequestOutcome(client, resp.RequestID, previewStartWait)
			if err != nil {
				return err
			}
			if outcome.Message != "" {
				message = outcome.Message
			}
		}

		// The resolved outcome carries only its message, so the entry URL comes
		// from a follow-up read. A preview that is already gone by then (a
		// concurrent stop, the idle reaper on a very short timeout) does not
		// make the start a failure — it just leaves nothing to link to.
		info, _ := lookupPreview(client, beadID)

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(ipc.PreviewStartResponse{
				BeadID:   beadID,
				Status:   info.Status,
				Message:  message,
				EntryURL: info.EntryURL,
			})
		}
		fmt.Println(message)
		if info.EntryURL != "" {
			fmt.Printf("URL: %s\n", info.EntryURL)
		}
		return nil
	},
}

var previewStopCmd = &cobra.Command{
	Use:   "stop <bead-id>",
	Short: "Tear down a bead's preview environment",
	Long: `Stops a bead's preview environment: kills its supervised services,
runs the manifest's teardown command and removes the preview checkout.

Teardown runs asynchronously in the daemon, so this waits for the outcome and
exits non-zero if it fails. A bead with no running preview is an error, not a
silent success.

'forge preview start <bead-id> --anvil <name>' is the inverse.`,
	Args:    cobra.ExactArgs(1),
	Example: "  forge preview stop Forge-abc1",
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.PreviewActionPayload{BeadID: beadID})
		resp, err := client.Send(ipc.Command{
			Type:    "preview_stop",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}
		// Previews disabled, an unknown bead id or a bead with no live preview
		// are all rejected synchronously.
		if resp.Type == "error" {
			return ipcError(resp)
		}

		message := fmt.Sprintf("preview for %s stopped", beadID)
		if resp.IsQueued() {
			outcome, err := awaitRequestOutcome(client, resp.RequestID, previewStopWait)
			if err != nil {
				return err
			}
			if outcome.Message != "" {
				message = outcome.Message
			}
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(ipc.PreviewStopResponse{
				Stopped: true,
				BeadID:  beadID,
				Message: message,
			})
		}
		fmt.Println(message)
		return nil
	},
}

// lookupPreview reads the daemon's preview list and returns the entry for one
// bead, if it has a live preview.
//
// It is how `preview start` gets its entry URL: the queued outcome a start
// resolves to carries only a message, and the URL is assembled by the daemon
// from settings (preview_proxy_base, preview_public_host) the CLI does not
// read. A failed read is reported as "no entry", never as an error — the start
// itself has already succeeded by the time this runs.
func lookupPreview(client *ipc.Client, beadID string) (ipc.PreviewInfo, bool) {
	resp, err := client.Send(ipc.Command{Type: "preview_list"})
	if err != nil || resp.Type != "ok" {
		return ipc.PreviewInfo{}, false
	}
	var list ipc.PreviewListResponse
	if err := json.Unmarshal(resp.Payload, &list); err != nil {
		return ipc.PreviewInfo{}, false
	}
	for _, p := range list.Previews {
		if p.BeadID == beadID {
			return p, true
		}
	}
	return ipc.PreviewInfo{}, false
}

// renderPreviewList writes the human-readable preview table. It takes an
// io.Writer so the rendering is testable without a daemon behind it.
func renderPreviewList(w io.Writer, list ipc.PreviewListResponse) {
	if !list.Enabled {
		fmt.Fprintln(w, "Preview environments are disabled (settings.preview_enabled).")
		return
	}
	if len(list.Previews) == 0 {
		fmt.Fprintln(w, "No previews are running.")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "BEAD\tSTATUS\tURL\tIDLE\tRESOURCES\n")
	for _, p := range list.Previews {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.BeadID,
			p.Status,
			orDash(p.EntryURL),
			formatPreviewIdle(p.IdleRemainingSeconds),
			orDash(p.ResourceNote),
		)
	}
	tw.Flush()

	// A failed service is why a preview reads as degraded or failed, and the
	// error is the only part of it the operator can act on — so it gets its own
	// lines rather than being truncated into the table.
	for _, p := range list.Previews {
		for _, svc := range p.Services {
			if svc.Error != "" {
				fmt.Fprintf(w, "  ! %s/%s: %s\n", p.BeadID, svc.Name, svc.Error)
			}
		}
	}

	fmt.Fprintf(w, "\n%d preview(s)\n", len(list.Previews))
}

// formatPreviewIdle renders the countdown to the idle reaper. A nil countdown
// means the reaper is disabled, which is not the same as an expired one: the
// former has no deadline at all, the latter is waiting for the next tick.
func formatPreviewIdle(secs *int64) string {
	switch {
	case secs == nil:
		return "-"
	case *secs <= 0:
		return "due"
	default:
		return (time.Duration(*secs) * time.Second).String()
	}
}

// orDash renders an empty optional column as a dash so columns stay aligned.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// awaitRequestOutcome resolves the request_id handed back with a "queued"
// response, polling request_status until the daemon reports a terminal state.
// An "error" outcome is returned as an error so the caller exits non-zero
// instead of printing a phantom success.
func awaitRequestOutcome(client *ipc.Client, requestID string, timeout time.Duration) (ipc.RequestStatusResponse, error) {
	payload, _ := json.Marshal(ipc.RequestStatusPayload{RequestID: requestID})
	// rootCtx is installed by the root command's PersistentPreRun; fall back so
	// a direct call (a test, a future non-cobra caller) cannot nil-deref.
	ctx := rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(timeout)
	for {
		resp, err := client.Send(ipc.Command{
			Type:    "request_status",
			Payload: payload,
		})
		if err != nil {
			return ipc.RequestStatusResponse{}, fmt.Errorf("polling request status: %w", err)
		}
		if resp.Type != "ok" {
			return ipc.RequestStatusResponse{}, ipcError(resp)
		}
		var out ipc.RequestStatusResponse
		if err := json.Unmarshal(resp.Payload, &out); err != nil {
			return ipc.RequestStatusResponse{}, fmt.Errorf("parsing request status: %w", err)
		}

		switch out.State {
		case ipc.RequestStateOK:
			return out, nil
		case ipc.RequestStateError:
			if out.Message == "" {
				return out, fmt.Errorf("daemon: request %s failed", requestID)
			}
			return out, fmt.Errorf("daemon: %s", out.Message)
		case ipc.RequestStateUnknown:
			// The daemon records a pending outcome the moment it queues the
			// work, so "unknown" here means the record was evicted — the work
			// may well have succeeded, but we cannot claim it did.
			return out, fmt.Errorf("daemon lost track of request %s; check 'forge preview list'", requestID)
		}

		if time.Now().After(deadline) {
			return out, fmt.Errorf("timed out after %s waiting for request %s; check 'forge preview list'", timeout, requestID)
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(previewPoll):
		}
	}
}
