package forgechat

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/provider"
)

// TestClaudeRunner_Turn_TimeoutReturnsSentinel exercises the timeout path of
// ClaudeRunner.Turn without spawning a real claude session. The parent context
// is created with a deadline in the past so turnCtx is already expired when
// Turn begins — the sentinel path fires deterministically without relying on
// any wall-clock race. The Provider is pointed at a guaranteed-missing path
// under t.TempDir() so smith.SpawnWithProvider fails at cmd.Start() and the
// DeadlineExceeded check is the first thing evaluated afterwards.
//
// The three assertions track the contract from Forge-rjad: (1) Messages is
// exactly the templated sentinel, (2) no truncated stream-json preamble
// leaks into the body, (3) the call returns nil error because the sentinel
// is a non-error completion — the chat UI renders it as a normal assistant
// message rather than surfacing a backend failure.
func TestClaudeRunner_Turn_TimeoutReturnsSentinel(t *testing.T) {
	// Pre-expire the context so the deadline is guaranteed to have already
	// fired before Turn even starts — no reliance on setup latency.
	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	runner := &ClaudeRunner{
		Provider: provider.Provider{
			Kind:    provider.Claude,
			Command: filepath.Join(t.TempDir(), "claude-fake"),
		},
		MaxTurns: 1,
		Timeout:  5 * time.Minute, // large; pre-expired parent ctx triggers sentinel
	}

	resp, err := runner.Turn(expiredCtx, TurnRequest{
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
