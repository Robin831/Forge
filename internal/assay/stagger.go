package assay

import (
	"context"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/smith"
)

// primerPass is the index into deepPasses of the pass that runs alone first and
// writes the shared prompt prefix into the provider's cache. It is a fixed
// index rather than a choice made per run — a replay of the same head must
// stagger the same way, or the cache accounting of two runs is not comparable
// and a regression in it is invisible.
const primerPass = 0

// primerWaitDefault bounds how long the remaining deep passes wait for the
// primer to start answering before they are released regardless.
//
// It is a backstop, not the mechanism: in the normal case the primer signals
// its first answered token in a second or two and the wait ends there. The
// cases this bounds are the ones with no signal to give — a provider that does
// not stream structured events, a primer wedged before its first token — and
// for those the right answer is to lose the cache saving rather than the run.
// 60s is comfortably past a normal time-to-first-token and comfortably short of
// a pass's own budget.
const primerWaitDefault = 60 * time.Second

// firstOutputKey carries the barrier release into the pass runner.
type firstOutputKey struct{}

// withFirstOutput returns a context carrying fn, which a pass runner invokes
// the first time the provider produces model output for that session.
//
// The signal rides on the context rather than on PassRunner's signature because
// PassRunner is the engine's one seam to the backend and every caller and stub
// implements it: an optional out-of-band notification is not worth widening a
// contract that six call sites and every test satisfy. A runner that ignores it
// — an injected stub, a future backend with no streaming — is not broken, it
// just falls back to primerWaitDefault.
//
// fn must be safe for concurrent use and must not block: it is called from the
// provider's stdout reader goroutine, which is also draining the session.
func withFirstOutput(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, firstOutputKey{}, fn)
}

// firstOutputFn returns the first-output callback carried by ctx, or nil.
func firstOutputFn(ctx context.Context) func() {
	fn, _ := ctx.Value(firstOutputKey{}).(func())
	return fn
}

// isModelOutput reports whether ev is the provider answering rather than
// session bookkeeping.
//
// The event the barrier wants is the earliest in-band proof that the request's
// input has been read and cached, which is the first token the model emits.
// Claude opens a stream-json session with a `system`/init event emitted before
// the model request is made at all, so releasing on "any event" would put the
// other four passes straight back into the simultaneous-miss race the stagger
// exists to break. A `result` event counts too: a session short enough to
// answer in one shot is finished, and there is nothing left to wait for.
//
// `message` is the Gemini-style delta event readStreamJSONEvents accumulates
// into FullOutput, and it is model output only for role "assistant" — the same
// filter that reader applies. Gemini also emits a role "user" message echoing
// the prompt back, which is emitted before the model request is made and is
// therefore exactly the bookkeeping-not-an-answer case above. A message with no
// role at all is taken as output: an unknown backend that streams deltas
// without labelling them should lose the wait, not sit through it, and the
// primer's own return opens the gate regardless.
func isModelOutput(ev smith.StreamEvent) bool {
	switch ev.Type {
	case "assistant", "result":
		return true
	case "message":
		return ev.Role != "user"
	default:
		return false
	}
}

// gate is a one-shot barrier: open() is idempotent and safe from any
// goroutine, wait() returns as soon as the gate opens, the deadline passes, or
// ctx is cancelled.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

// open releases every current and future waiter. Safe to call repeatedly and
// from multiple goroutines — the primer signals it from the provider's stream
// reader and again, unconditionally, when its pass returns.
func (g *gate) open() {
	g.once.Do(func() { close(g.ch) })
}

// wait blocks until the gate opens, d elapses, or ctx is cancelled. A
// non-positive d does not wait at all, which is what turns the stagger off.
func (g *gate) wait(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-g.ch:
	case <-t.C:
	case <-ctx.Done():
	}
}
