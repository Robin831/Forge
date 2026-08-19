package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/spf13/cobra"
)

// stubBellowsSend replaces the IPC seam for one test and returns the slice the
// payloads land in, so a case can assert both what was sent and that nothing
// was sent at all.
func stubBellowsSend(t *testing.T, err error) *[]ipc.PRActionPayload {
	t.Helper()
	var sent []ipc.PRActionPayload
	orig := bellowsSend
	bellowsSend = func(p ipc.PRActionPayload) (map[string]string, error) {
		sent = append(sent, p)
		if err != nil {
			return nil, err
		}
		return map[string]string{"message": "PR #" + strings.TrimSpace(p.Action)}, nil
	}
	t.Cleanup(func() { bellowsSend = orig })
	return &sent
}

// setBellowsAnvil sets --anvil on a package-level cobra singleton and clears it
// again afterwards, so a value left behind cannot leak into the next test.
func setBellowsAnvil(t *testing.T, cmd *cobra.Command, anvil string) {
	t.Helper()
	if err := cmd.Flags().Set("anvil", anvil); err != nil {
		t.Fatalf("set --anvil: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set("anvil", "") })
}

func TestRunBellowsMute(t *testing.T) {
	tests := []struct {
		name         string
		action       string
		args         []string
		anvil        string
		sendErr      error
		wantErr      string
		wantDispatch bool
		wantPayload  ipc.PRActionPayload
		wantOut      []string
	}{
		{
			name:    "no PR number is a usage error and dispatches nothing",
			action:  ipc.PRActionDetachBellows,
			args:    nil,
			anvil:   "heimdall",
			wantErr: "exactly one PR number",
		},
		{
			name:    "two PR numbers are a usage error",
			action:  ipc.PRActionDetachBellows,
			args:    []string{"431", "432"},
			anvil:   "heimdall",
			wantErr: "exactly one PR number",
		},
		{
			name:    "a non-numeric PR is rejected before the socket",
			action:  ipc.PRActionDetachBellows,
			args:    []string{"whoops"},
			anvil:   "heimdall",
			wantErr: "invalid PR number",
		},
		{
			// Zero is the daemon's "no target supplied", so it must never be sent.
			name:    "PR zero is rejected before the socket",
			action:  ipc.PRActionDetachBellows,
			args:    []string{"0"},
			anvil:   "heimdall",
			wantErr: "invalid PR number",
		},
		{
			name:    "missing --anvil is refused, since a PR number alone names nothing",
			action:  ipc.PRActionDetachBellows,
			args:    []string{"431"},
			anvil:   "",
			wantErr: "--anvil is required",
		},
		{
			name:    "whitespace --anvil counts as missing",
			action:  ipc.PRActionDetachBellows,
			args:    []string{"431"},
			anvil:   "   ",
			wantErr: "--anvil is required",
		},
		{
			name:         "forge-authored PR detaches",
			action:       ipc.PRActionDetachBellows,
			args:         []string{"431"},
			anvil:        "heimdall",
			wantDispatch: true,
			wantPayload:  ipc.PRActionPayload{Action: "detach_bellows", PRNumber: 431, Anvil: "heimdall"},
			wantOut:      []string{"PR #431", "heimdall", "bellows stopped"},
		},
		{
			// An ext-* PR is addressed identically: the CLI knows only the
			// number and the anvil, and the daemon's resolvePRTarget finds the
			// same kind of `prs` row for both. A separate client-side path here
			// would be a second way to get the target wrong.
			name:         "external PR detaches through the same payload",
			action:       ipc.PRActionDetachBellows,
			args:         []string{"#7071"},
			anvil:        "munin",
			wantDispatch: true,
			wantPayload:  ipc.PRActionPayload{Action: "detach_bellows", PRNumber: 7071, Anvil: "munin"},
			wantOut:      []string{"PR #7071", "munin", "bellows stopped"},
		},
		{
			name:         "resume sends the reattach verb",
			action:       ipc.PRActionReattachBellows,
			args:         []string{"431"},
			anvil:        "heimdall",
			wantDispatch: true,
			wantPayload:  ipc.PRActionPayload{Action: "reattach_bellows", PRNumber: 431, Anvil: "heimdall"},
			wantOut:      []string{"PR #431", "heimdall", "bellows resumed"},
		},
		{
			// The daemon refuses a PR it cannot resolve; its sentence names
			// which half of the identifier was wrong, so it is passed through.
			name:         "unknown PR propagates the daemon's refusal",
			action:       ipc.PRActionDetachBellows,
			args:         []string{"404"},
			anvil:        "heimdall",
			sendErr:      errors.New(`daemon: cannot detach PR #404: PR #404 not found on anvil "heimdall"`),
			wantDispatch: true,
			wantPayload:  ipc.PRActionPayload{Action: "detach_bellows", PRNumber: 404, Anvil: "heimdall"},
			wantErr:      `PR #404 not found on anvil "heimdall"`,
		},
		{
			name:         "a dispatch failure is not reported as a mute",
			action:       ipc.PRActionReattachBellows,
			args:         []string{"431"},
			anvil:        "heimdall",
			sendErr:      errors.New("connecting to daemon: no socket (is 'forge up' running?)"),
			wantDispatch: true,
			wantPayload:  ipc.PRActionPayload{Action: "reattach_bellows", PRNumber: 431, Anvil: "heimdall"},
			wantErr:      "forge up",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sent := stubBellowsSend(t, tt.sendErr)
			var out bytes.Buffer

			err := runBellowsMute(tt.action, tt.args, tt.anvil, &out)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil (output %q)", tt.wantErr, out.String())
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				if out.Len() != 0 {
					t.Errorf("a failed verb must print no confirmation, got %q", out.String())
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantDispatch {
				if len(*sent) != 1 {
					t.Fatalf("expected exactly one dispatch, got %d", len(*sent))
				}
				if got := (*sent)[0]; got != tt.wantPayload {
					t.Errorf("payload = %+v, want %+v", got, tt.wantPayload)
				}
			} else if len(*sent) != 0 {
				t.Errorf("expected no dispatch, got %+v", *sent)
			}

			for _, want := range tt.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("confirmation %q should contain %q", out.String(), want)
				}
			}
		})
	}
}

// TestBellowsActionsAreTheIPCConstants pins the wire verbs to the ones the IPC
// package defines. This sub-task fixes the canonical names the daemon switch
// and the web control both answer to, so a literal re-spelled here would be a
// verb the daemon rejects as unknown.
func TestBellowsActionsAreTheIPCConstants(t *testing.T) {
	if ipc.PRActionDetachBellows != "detach_bellows" {
		t.Errorf("detach verb = %q, want detach_bellows", ipc.PRActionDetachBellows)
	}
	if ipc.PRActionReattachBellows != "reattach_bellows" {
		t.Errorf("reattach verb = %q, want reattach_bellows", ipc.PRActionReattachBellows)
	}
}

// TestBellowsCmdWiring pins both verbs' shape: one positional PR number and a
// required --anvil, hanging off the bellows command.
func TestBellowsCmdWiring(t *testing.T) {
	for _, cmd := range []*cobra.Command{bellowsStopCmd, bellowsResumeCmd} {
		t.Run(cmd.Name(), func(t *testing.T) {
			if cmd.Args == nil {
				t.Fatal("must constrain its positional args")
			}
			if err := cmd.Args(cmd, nil); err == nil {
				t.Error("no PR number should be rejected")
			}
			if err := cmd.Args(cmd, []string{"1", "2"}); err == nil {
				t.Error("two positionals should be rejected")
			}
			if err := cmd.Args(cmd, []string{"431"}); err != nil {
				t.Errorf("one PR number should be accepted: %v", err)
			}

			flag := cmd.Flags().Lookup("anvil")
			if flag == nil {
				t.Fatal("must expose --anvil")
			}
			if flag.Annotations[cobra.BashCompOneRequiredFlag] == nil {
				t.Error("--anvil must be required")
			}

			if parent := cmd.Parent(); parent == nil || parent.Name() != "bellows" {
				t.Errorf("must hang off the bellows command, got %v", parent)
			}
		})
	}
}

// TestBellowsRunEReadsAnvilFlag covers the plumbing the table test bypasses:
// each verb reads --anvil off its own flag set, and refuses before dialling
// when it is empty.
func TestBellowsRunEReadsAnvilFlag(t *testing.T) {
	cases := []struct {
		cmd        *cobra.Command
		wantAction string
	}{
		{bellowsStopCmd, ipc.PRActionDetachBellows},
		{bellowsResumeCmd, ipc.PRActionReattachBellows},
	}
	for _, tc := range cases {
		t.Run(tc.cmd.Name()+"/empty anvil", func(t *testing.T) {
			sent := stubBellowsSend(t, nil)
			err := tc.cmd.RunE(tc.cmd, []string{"431"})
			if err == nil || !strings.Contains(err.Error(), "--anvil is required") {
				t.Fatalf("expected the missing-anvil refusal, got %v", err)
			}
			if len(*sent) != 0 {
				t.Errorf("nothing should be dispatched without an anvil, got %+v", *sent)
			}
		})
		t.Run(tc.cmd.Name()+"/anvil set", func(t *testing.T) {
			sent := stubBellowsSend(t, nil)
			setBellowsAnvil(t, tc.cmd, "heimdall")
			if err := tc.cmd.RunE(tc.cmd, []string{"431"}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(*sent) != 1 {
				t.Fatalf("expected one dispatch, got %d", len(*sent))
			}
			want := ipc.PRActionPayload{Action: tc.wantAction, PRNumber: 431, Anvil: "heimdall"}
			if (*sent)[0] != want {
				t.Errorf("payload = %+v, want %+v", (*sent)[0], want)
			}
		})
	}
}

func TestBellowsStateLine(t *testing.T) {
	stop := bellowsStateLine(ipc.PRActionDetachBellows, 431, "heimdall")
	if !strings.Contains(stop, "PR #431") || !strings.Contains(stop, "heimdall") || !strings.Contains(stop, "stopped") {
		t.Errorf("stop confirmation must name the PR, the anvil and the new state, got %q", stop)
	}
	resume := bellowsStateLine(ipc.PRActionReattachBellows, 431, "heimdall")
	if !strings.Contains(resume, "PR #431") || !strings.Contains(resume, "heimdall") || !strings.Contains(resume, "resumed") {
		t.Errorf("resume confirmation must name the PR, the anvil and the new state, got %q", resume)
	}
	if stop == resume {
		t.Error("stop and resume must not read the same")
	}
}
