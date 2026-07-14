package state

import "sync"

// BusEvent is the payload carried on the in-process event Bus. It reuses the
// EventsSince row shape (via the embedded Event) and adds a monotonic sequence
// number plus a gap-marker sentinel.
//
// Seq mirrors the Last-Event-ID / SSE sequence: subscribers track the highest
// Seq they have observed so they can re-sync via DB.EventsSince(seq) after a
// reconnect or an overflow.
//
// GapMarker distinguishes an overflow-signalling event from a real row. When a
// subscriber's bounded buffer overflows, the Bus drops the oldest buffered
// event and delivers a BusEvent with GapMarker set to true, telling the client
// it has missed events and must re-sync from its last known Seq. A gap marker
// carries no meaningful Event payload.
type BusEvent struct {
	Seq       int64
	GapMarker bool
	Event
}

// subscriber wraps a per-subscriber bounded channel. The pointer identity is
// used as the map key so each Subscribe call is independent even if two
// subscribers share the same buffer size.
type subscriber struct {
	ch     chan BusEvent
	closed bool
}

// Bus is a concurrency-safe in-process publish/subscribe fan-out for BusEvents.
//
// Each subscriber gets its own bounded buffered channel. Publish never blocks:
// if a subscriber's buffer is full, the Bus drops the oldest buffered event and
// enqueues a gap marker so the subscriber knows to re-sync. This keeps a slow
// consumer from stalling the publisher or other subscribers.
//
// The zero value is not usable; construct a Bus with NewBus.
type Bus struct {
	mu      sync.Mutex
	bufSize int
	subs    map[*subscriber]struct{}
}

// NewBus returns a Bus whose per-subscriber channels are buffered to bufSize.
// A bufSize <= 0 is treated as an unbuffered-but-safe minimum of 1 so that
// Publish's non-blocking send/drop semantics still behave sensibly.
func NewBus(bufSize int) *Bus {
	if bufSize <= 0 {
		bufSize = 1
	}
	return &Bus{
		bufSize: bufSize,
		subs:    make(map[*subscriber]struct{}),
	}
}

// Subscribe registers a new subscriber and returns a receive-only channel of
// BusEvents plus an unsubscribe function. The channel is bounded to the Bus's
// configured buffer size. The returned unsubscribe function is idempotent: it
// removes the subscriber from the fan-out set and closes the channel; calling
// it more than once is safe. After unsubscribing, no further events are
// delivered and the channel is closed.
func (b *Bus) Subscribe() (<-chan BusEvent, func()) {
	sub := &subscriber{ch: make(chan BusEvent, b.bufSize)}

	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if sub.closed {
			return
		}
		sub.closed = true
		delete(b.subs, sub)
		close(sub.ch)
	}

	return sub.ch, unsubscribe
}

// Publish fans ev out to every current subscriber. It never blocks: for each
// subscriber it attempts a non-blocking send, and on a full buffer it drops the
// oldest buffered event and enqueues a gap marker so the subscriber learns it
// must re-sync.
//
// The entire fan-out runs under the Bus mutex, which also guards Subscribe and
// unsubscribe, so a concurrent unsubscribe can never close a channel mid-send.
func (b *Bus) Publish(ev BusEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for sub := range b.subs {
		select {
		case sub.ch <- ev:
			// Delivered.
		default:
			// Buffer full: drop the oldest buffered event to make room, then
			// deliver a gap marker so the subscriber re-syncs from its last Seq.
			// The extra drain guards against the (already-full) channel still
			// being full if the receiver hasn't drained since the first send.
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- BusEvent{GapMarker: true}:
			default:
				// Still no room (a concurrent receiver could refill between the
				// drain and this send). Drop once more and retry so the marker
				// is not silently lost.
				select {
				case <-sub.ch:
				default:
				}
				select {
				case sub.ch <- BusEvent{GapMarker: true}:
				default:
				}
			}
		}
	}
}
