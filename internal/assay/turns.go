package assay

import (
	"encoding/json"
	"sync"

	"github.com/Robin831/Forge/internal/smith"
)

// turnCounter counts the distinct API messages a pass session's model produced.
//
// That count is the unit `--max-turns` is denominated in, and therefore the
// only unit in which recorded turn telemetry can be compared against the budget
// the session was given. The provider's own `num_turns` is NOT that unit, which
// is what made the first attempt to re-tune the budget from logged data
// impossible (see docs/assay-turn-budget.md):
//
//   - On a session that answered, Claude reports num_turns as the number of
//     tool-result rounds plus one. A message carrying three parallel tool_use
//     blocks is one turn against the budget and three rounds in num_turns, so
//     the reported figure runs ahead of the spend: over the 451 sessions
//     analysed it overstated 38% of the successful ones, by a median of 2 and
//     up to 7 — enough to read as "this session used 17 of its 12 turns".
//   - On a session the budget killed, Claude reports the constant cap+1 (13 for
//     every one of the 32 capped sessions in that sample), which says nothing
//     about how much more the session wanted.
//
// Counting the messages Forge watches stream past sidesteps both. The count is
// deduplicated by message id for the same reason costTracker.AddTurnCost is:
// Claude emits one assistant event per content block, so a thinking + text +
// tool_use turn arrives as three events repeating one id.
//
// A message with no id is not counted — a backend that stamps none (Gemini's
// deltas) yields zero here and the caller falls back to the provider's own
// figure, which is the safe direction: a telemetry field that cannot be derived
// must degrade to the old number, never to zero.
//
// It is written from the provider's stdout reader goroutine and read from the
// pass goroutine, so its state is guarded.
type turnCounter struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// messageIDEnvelope is the one field this counter reads off an assistant
// message: the id that makes several content-block events one turn.
type messageIDEnvelope struct {
	ID string `json:"id"`
}

// observe records the message an assistant stream event belongs to. Safe to
// call from the stream reader, and on a nil counter.
func (c *turnCounter) observe(ev smith.StreamEvent) {
	if c == nil || ev.Type != "assistant" || len(ev.Message) == 0 {
		return
	}
	var env messageIDEnvelope
	if err := json.Unmarshal(ev.Message, &env); err != nil || env.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.seen[env.ID]; dup {
		return
	}
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	c.seen[env.ID] = struct{}{}
}

// count returns the number of distinct model messages observed. A nil counter,
// and a backend whose events carry no message ids, report 0.
func (c *turnCounter) count() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

// observedTurns resolves the turn figure a pass reports: the counted model
// messages when there are any, else whatever the provider reported.
//
// The fallback is what keeps this additive. A backend Forge cannot count turns
// for reports exactly the number it always did; only a session whose messages
// were counted reports the figure the budget is actually written in.
func observedTurns(c *turnCounter, reported int) int {
	if n := c.count(); n > 0 {
		return n
	}
	return reported
}
