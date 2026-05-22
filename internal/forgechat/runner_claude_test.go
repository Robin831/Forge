package forgechat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/provider"
)

// TestClaudeRunner_Turn_TimeoutReturnsSentinel exercises the timeout path of
// ClaudeRunner.Turn without spawning a real claude session. The Provider is
// pointed at a binary that does not exist so smith.SpawnWithProvider fails
// fast at cmd.Start(); by the time Turn checks turnCtx.Err(), the 1ms
// deadline has long since fired (mkdtemp + git rev-parse inside
// ValidateWorktreeDir + exec setup each take more than a millisecond), so we
// reliably hit the sentinel path and never need to wait on real I/O.
//
// The three assertions track the contract from Forge-rjad: (1) Messages is
// exactly the templated sentinel, (2) no truncated stream-json preamble
// leaks into the body, (3) the call returns nil error because the sentinel
// is a non-error completion — the chat UI renders it as a normal assistant
// message rather than surfacing a backend failure.
func TestClaudeRunner_Turn_TimeoutReturnsSentinel(t *testing.T) {
	runner := &ClaudeRunner{
		Provider: provider.Provider{
			Kind:    provider.Claude,
			Command: "/this/binary/intentionally/does/not/exist/claude-fake",
		},
		MaxTurns: 1,
		Timeout:  1 * time.Millisecond,
	}

	resp, err := runner.Turn(context.Background(), TurnRequest{
		Stage:    StageDrafting,
		Mode:     ModeChat,
		UserText: "hello",
	})

	if err != nil {
		t.Fatalf("expected nil error on timeout (sentinel is a non-error completion path), got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response on timeout, got nil")
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("expected exactly one sentinel message, got %d: %+v", len(resp.Messages), resp.Messages)
	}

	msg := resp.Messages[0]
	if msg.Kind != "text" {
		t.Errorf("sentinel message kind: want %q, got %q", "text", msg.Kind)
	}

	// The sentinel template is:
	//   "_(Drafter timed out after <elapsed>. Try a more focused follow-up
	//    question, or bump settings.forgechat.turn_timeout.)_"
	// The elapsed segment is rounded to seconds, so pin only the
	// stable prefix and suffix — a future tweak to the rounding granularity
	// must not break this test.
	const wantPrefix = "_(Drafter timed out after"
	const wantSuffix = "settings.forgechat.turn_timeout.)_"
	if !strings.HasPrefix(msg.Content, wantPrefix) {
		t.Errorf("sentinel prefix mismatch:\n  want prefix: %q\n  got: %q", wantPrefix, msg.Content)
	}
	if !strings.HasSuffix(msg.Content, wantSuffix) {
		t.Errorf("sentinel suffix mismatch:\n  want suffix: %q\n  got: %q", wantSuffix, msg.Content)
	}

	// Assertion (2): no truncated stream-json preamble bleeds through into
	// the sentinel body. Any of these substrings would indicate that the
	// runner parsed a partial claude response instead of returning the
	// sentinel template verbatim.
	for _, leak := range []string{
		"{",            // any JSON envelope
		`"type":`,      // stream-json event field
		"assistant",    // claude role marker
		"stream-json",  // CLI flag echo
		"tool_use",     // tool invocation event
	} {
		if strings.Contains(msg.Content, leak) {
			t.Errorf("sentinel body leaked claude output (found %q):\n  body: %q", leak, msg.Content)
		}
	}

	// A timeout must not advance the session stage or produce a plan — the
	// caller should be free to retry the same turn without any state having
	// shifted underneath it.
	if resp.NewStage != "" {
		t.Errorf("timeout must not advance stage, got NewStage=%q", resp.NewStage)
	}
	if resp.NewPlan != "" {
		t.Errorf("timeout must not produce a plan, got NewPlan=%q", resp.NewPlan)
	}
	if resp.Emission != nil {
		t.Errorf("timeout must not produce an emission envelope, got %+v", resp.Emission)
	}
}
