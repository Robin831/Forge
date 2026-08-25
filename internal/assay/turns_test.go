package assay

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// messageEvent builds an assistant stream event carrying a given message id,
// which is the only field turnCounter reads. (smithEvent is the fixed-id
// sibling used where the content blocks are what matters.)
func messageEvent(id, content string) smith.StreamEvent {
	return smith.StreamEvent{
		Type:    "assistant",
		Message: json.RawMessage(`{"id":"` + id + `","content":` + content + `}`),
	}
}

// TestTurnCounterCountsMessagesNotEvents is the whole point of the counter:
// Claude emits one assistant event per content block, so a thinking + text +
// tool_use turn arrives as three events sharing one message id and must count
// once. Counting events is what made the recorded figure incomparable to the
// --max-turns budget in the first place.
func TestTurnCounterCountsMessagesNotEvents(t *testing.T) {
	c := &turnCounter{}
	c.observe(messageEvent("msg_a", `[{"type":"thinking"}]`))
	c.observe(messageEvent("msg_a", `[{"type":"text","text":"looking"}]`))
	c.observe(messageEvent("msg_a", `[{"type":"tool_use","name":"Read"}]`))
	c.observe(messageEvent("msg_b", `[{"type":"tool_use","name":"Read"}]`))
	// Two parallel tool calls in one message are still one turn against the
	// budget, however many tool results come back.
	c.observe(messageEvent("msg_c", `[{"type":"tool_use","name":"Read"},{"type":"tool_use","name":"Grep"}]`))
	if got := c.count(); got != 3 {
		t.Errorf("count = %d; want 3 distinct messages", got)
	}
}

// TestTurnCounterIgnoresUncountableEvents pins the events that must not move
// the count: anything that is not an assistant message, and an assistant
// message with no id to deduplicate on.
func TestTurnCounterIgnoresUncountableEvents(t *testing.T) {
	c := &turnCounter{}
	c.observe(smith.StreamEvent{Type: "system", Subtype: "init"})
	c.observe(smith.StreamEvent{Type: "user", Message: json.RawMessage(`{"id":"msg_x"}`)})
	c.observe(smith.StreamEvent{Type: "result", Message: json.RawMessage(`{"id":"msg_y"}`)})
	c.observe(smith.StreamEvent{Type: "assistant"})
	c.observe(smith.StreamEvent{Type: "assistant", Message: json.RawMessage(`{"content":[]}`)})
	c.observe(smith.StreamEvent{Type: "assistant", Message: json.RawMessage(`not json`)})
	if got := c.count(); got != 0 {
		t.Errorf("count = %d; want 0 — none of these is a countable model message", got)
	}
}

// TestTurnCounterNilSafe checks the counter behaves on the paths that have no
// counter at all (an injected runner, a stub session).
func TestTurnCounterNilSafe(t *testing.T) {
	var c *turnCounter
	c.observe(messageEvent("msg_a", `[]`))
	if got := c.count(); got != 0 {
		t.Errorf("nil count = %d; want 0", got)
	}
}

// TestObservedTurnsFallsBackToProvider pins the degradation direction: a
// backend whose messages carry no ids (Gemini's deltas) reports exactly the
// number it always did rather than zero.
func TestObservedTurnsFallsBackToProvider(t *testing.T) {
	if got := observedTurns(nil, 9); got != 9 {
		t.Errorf("observedTurns(nil, 9) = %d; want the provider's 9", got)
	}
	empty := &turnCounter{}
	if got := observedTurns(empty, 9); got != 9 {
		t.Errorf("observedTurns(uncounted, 9) = %d; want the provider's 9", got)
	}
	counted := &turnCounter{}
	counted.observe(messageEvent("msg_a", `[]`))
	counted.observe(messageEvent("msg_b", `[]`))
	if got := observedTurns(counted, 9); got != 2 {
		t.Errorf("observedTurns(counted, 9) = %d; want the counted 2", got)
	}
}

// TestSessionOutcomeReportsCountedTurns checks both outcome paths report the
// counted figure rather than the provider's num_turns.
//
// The two fixtures are the shapes real sessions produced under a 12-turn cap:
// a session that answered reports num_turns as tool-result rounds (well above
// the messages it actually took), and one the budget killed reports the
// constant cap+1 whatever it did.
func TestSessionOutcomeReportsCountedTurns(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude}

	counted := func(n int) *turnCounter {
		c := &turnCounter{}
		for i := range n {
			c.observe(messageEvent(string(rune('a'+i)), `[]`))
		}
		return c
	}

	out, err := sessionOutcome("logic", nil, counted(10), &smith.Result{
		ResultSubtype: "success", FullOutput: "{}", NumTurns: 17,
	}, pv)
	if err != nil {
		t.Fatalf("sessionOutcome: %v", err)
	}
	if out.Turns != 10 {
		t.Errorf("Turns = %d; want the 10 messages counted, not the provider's 17", out.Turns)
	}

	_, err = sessionOutcome("logic", nil, counted(12), &smith.Result{
		ExitCode: 1, IsError: true, ResultSubtype: ReasonMaxTurns, NumTurns: 13,
	}, pv)
	var perr *PassError
	if !errors.As(err, &perr) {
		t.Fatalf("want *PassError, got %v", err)
	}
	if perr.Turns != 12 {
		t.Errorf("PassError.Turns = %d; want the 12 messages counted, not the provider's 13", perr.Turns)
	}
}
