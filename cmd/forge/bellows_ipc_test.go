//go:build !windows

package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
)

// bellowsFakeDaemon stands in for the daemon's pr_action handler: it records
// the payload and answers the way the real handler does for these two verbs —
// the generic okResponse whose message names the wire verb, which is exactly
// why the CLI renders its own confirmation instead of echoing it.
func bellowsFakeDaemon(t *testing.T, sent *ipc.PRActionPayload, refusal string) {
	t.Helper()
	startFakeDaemon(t, func(cmd ipc.Command) ipc.Response {
		if cmd.Type != "pr_action" {
			return errResp(t, "unexpected command "+cmd.Type)
		}
		if err := json.Unmarshal(cmd.Payload, sent); err != nil {
			return errResp(t, "bad payload")
		}
		if refusal != "" {
			return errResp(t, refusal)
		}
		return okResp(t, map[string]string{
			"message": "PR #" + strconv.Itoa(sent.PRNumber) + ": " + sent.Action,
		})
	})
}

func TestBellowsStop_RoundTrip(t *testing.T) {
	var sent ipc.PRActionPayload
	bellowsFakeDaemon(t, &sent, "")
	setBellowsAnvil(t, bellowsStopCmd, "heimdall")

	var err error
	out := captureStdout(t, func() {
		err = bellowsStopCmd.RunE(bellowsStopCmd, []string{"431"})
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ipc.PRActionPayload{Action: ipc.PRActionDetachBellows, PRNumber: 431, Anvil: "heimdall"}
	if sent != want {
		t.Errorf("daemon received %+v, want %+v", sent, want)
	}
	if !strings.Contains(out, "PR #431") || !strings.Contains(out, "bellows stopped") {
		t.Errorf("expected a confirmation naming the PR and the new state, got %q", out)
	}
}

func TestBellowsResume_RoundTrip(t *testing.T) {
	var sent ipc.PRActionPayload
	bellowsFakeDaemon(t, &sent, "")
	setBellowsAnvil(t, bellowsResumeCmd, "heimdall")

	var err error
	out := captureStdout(t, func() {
		err = bellowsResumeCmd.RunE(bellowsResumeCmd, []string{"431"})
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ipc.PRActionPayload{Action: ipc.PRActionReattachBellows, PRNumber: 431, Anvil: "heimdall"}
	if sent != want {
		t.Errorf("daemon received %+v, want %+v", sent, want)
	}
	if !strings.Contains(out, "PR #431") || !strings.Contains(out, "bellows resumed") {
		t.Errorf("expected a confirmation naming the PR and the new state, got %q", out)
	}
}

// TestBellowsStop_ExternalPR — an externally-opened PR is muted by the same
// number+anvil pair as a forge-authored one, and the CLI sends no bead id: the
// daemon resolves the row and reads the bead from it, so nothing here has to
// know an ext-* PR when it sees one.
func TestBellowsStop_ExternalPR(t *testing.T) {
	var sent ipc.PRActionPayload
	bellowsFakeDaemon(t, &sent, "")
	setBellowsAnvil(t, bellowsStopCmd, "munin")

	var err error
	out := captureStdout(t, func() {
		err = bellowsStopCmd.RunE(bellowsStopCmd, []string{"#7071"})
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sent.PRNumber != 7071 || sent.Anvil != "munin" {
		t.Errorf("daemon received %+v, want PR 7071 on munin", sent)
	}
	if sent.BeadID != "" || sent.PRID != 0 || sent.Branch != "" {
		t.Errorf("the CLI addresses a PR by number and anvil only, got %+v", sent)
	}
	if !strings.Contains(out, "PR #7071") {
		t.Errorf("expected the PR in the confirmation, got %q", out)
	}
}

// TestBellowsStop_UnknownPR — the daemon refuses a PR it cannot resolve, and
// that refusal reaches the operator intact rather than as a mute that never
// happened.
func TestBellowsStop_UnknownPR(t *testing.T) {
	var sent ipc.PRActionPayload
	bellowsFakeDaemon(t, &sent, `cannot detach PR #404: PR #404 not found on anvil "heimdall"`)
	setBellowsAnvil(t, bellowsStopCmd, "heimdall")

	var err error
	out := captureStdout(t, func() {
		err = bellowsStopCmd.RunE(bellowsStopCmd, []string{"404"})
	})
	if err == nil {
		t.Fatal("expected an error for an unresolvable PR")
	}
	if !strings.Contains(err.Error(), "not found on anvil") {
		t.Errorf("the daemon's own refusal should survive, got %q", err)
	}
	if out != "" {
		t.Errorf("a refused mute must print no confirmation, got %q", out)
	}
}

func TestBellows_DaemonDown(t *testing.T) {
	// An empty HOME means no socket, which is what "the daemon is not running"
	// looks like from the CLI side.
	t.Setenv("HOME", t.TempDir())
	setBellowsAnvil(t, bellowsStopCmd, "heimdall")

	err := bellowsStopCmd.RunE(bellowsStopCmd, []string{"431"})
	if err == nil {
		t.Fatal("expected an error when the daemon is not running")
	}
	if !strings.Contains(err.Error(), "forge up") {
		t.Errorf("error should tell the operator to start the daemon, got %q", err)
	}
}
