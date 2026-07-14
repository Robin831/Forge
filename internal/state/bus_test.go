package state

import (
	"sync"
	"testing"
	"time"
)

// recv reads one BusEvent from ch, failing the test if nothing arrives within a
// short timeout.
func recv(t *testing.T, ch <-chan BusEvent) BusEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return BusEvent{}
	}
}

func TestBusSubscribePublishRoundTrip(t *testing.T) {
	b := NewBus(4)
	ch, unsub := b.Subscribe()
	defer unsub()

	want := BusEvent{Seq: 7, Event: Event{ID: 7, Message: "hello", BeadID: "Forge-1", Anvil: "a"}}
	b.Publish(want)

	got := recv(t, ch)
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got.GapMarker {
		t.Fatal("unexpected gap marker on normal delivery")
	}
}

func TestBusFanOut(t *testing.T) {
	b := NewBus(4)
	ch1, unsub1 := b.Subscribe()
	defer unsub1()
	ch2, unsub2 := b.Subscribe()
	defer unsub2()

	ev := BusEvent{Seq: 1, Event: Event{ID: 1, Message: "fan"}}
	b.Publish(ev)

	if got := recv(t, ch1); got != ev {
		t.Fatalf("sub1 got %+v, want %+v", got, ev)
	}
	if got := recv(t, ch2); got != ev {
		t.Fatalf("sub2 got %+v, want %+v", got, ev)
	}
}

func TestBusOverflowInsertsGapMarker(t *testing.T) {
	b := NewBus(2)
	ch, unsub := b.Subscribe()
	defer unsub()

	// Publish more than the buffer can hold without draining. The buffer is 2,
	// so publishing 5 events overflows and drop-oldest + gap-marker kicks in.
	for i := 1; i <= 5; i++ {
		b.Publish(BusEvent{Seq: int64(i), Event: Event{ID: i}})
	}

	// Drain everything currently buffered and assert we observe a gap marker
	// somewhere in the delivered sequence.
	sawGap := false
	for {
		select {
		case ev := <-ch:
			if ev.GapMarker {
				sawGap = true
			}
		default:
			if !sawGap {
				t.Fatal("expected a gap marker after buffer overflow")
			}
			return
		}
	}
}

// TestBusFanOutToNSubscribers exercises design point 5's fan-out requirement:
// every one of N independent subscribers receives the full ordered event stream
// with no duplicates and no loss. Buffers are sized to hold the whole batch so
// nobody overflows and the assertion is purely about fan-out correctness.
func TestBusFanOutToNSubscribers(t *testing.T) {
	const (
		nSubs   = 5
		nEvents = 20
	)
	b := NewBus(nEvents + 1)

	chans := make([]<-chan BusEvent, nSubs)
	for i := 0; i < nSubs; i++ {
		ch, unsub := b.Subscribe()
		defer unsub()
		chans[i] = ch
	}

	for i := 1; i <= nEvents; i++ {
		b.Publish(BusEvent{Seq: int64(i), Event: Event{ID: i, Message: "e"}})
	}

	for si, ch := range chans {
		for i := 1; i <= nEvents; i++ {
			ev := recv(t, ch)
			if ev.GapMarker {
				t.Fatalf("sub %d: unexpected gap marker at event %d", si, i)
			}
			if ev.Seq != int64(i) {
				t.Fatalf("sub %d: got Seq %d want %d (event lost or out of order)", si, ev.Seq, i)
			}
		}
		// After the exact batch there must be nothing left buffered — no dupes.
		select {
		case extra := <-ch:
			t.Fatalf("sub %d: unexpected extra event %+v (duplicate delivery)", si, extra)
		default:
		}
	}
}

// TestBusSlowSubscriberDoesNotStallFastDelivery covers the slow-subscriber half
// of design point 5 at the bus level: a subscriber that never drains overflows
// and receives a gap marker (drop-oldest), yet its full buffer neither blocks
// Publish nor perturbs a fast subscriber's ordered, gap-free stream. The DB
// re-sync that follows a gap marker is exercised end-to-end in the web package's
// SSE gap-resync test.
func TestBusSlowSubscriberDoesNotStallFastDelivery(t *testing.T) {
	const nEvents = 12
	b := NewBus(2) // tiny buffer so the undrained subscriber overflows quickly

	slow, unsubSlow := b.Subscribe() // deliberately never drained until the end
	defer unsubSlow()

	fast, unsubFast := b.Subscribe()
	defer unsubFast()

	// Publish then immediately drain the fast subscriber after each event so it
	// never overflows. If the slow subscriber's full buffer stalled Publish this
	// loop would block on recv and the test would time out.
	for i := 1; i <= nEvents; i++ {
		b.Publish(BusEvent{Seq: int64(i), Event: Event{ID: i}})
		ev := recv(t, fast)
		if ev.GapMarker {
			t.Fatalf("fast subscriber saw an unexpected gap marker at event %d", i)
		}
		if ev.Seq != int64(i) {
			t.Fatalf("fast subscriber got Seq %d want %d", ev.Seq, i)
		}
	}

	// The slow subscriber's bounded buffer must have overflowed into at least one
	// gap marker, proving drop-oldest + gap-marker fired without blocking anyone.
	sawGap := false
	for {
		select {
		case ev := <-slow:
			if ev.GapMarker {
				sawGap = true
			}
		default:
			if !sawGap {
				t.Fatal("expected the never-drained subscriber to receive a gap marker")
			}
			return
		}
	}
}

func TestBusUnsubscribeStopsDeliveryAndClosesChannel(t *testing.T) {
	b := NewBus(4)
	ch, unsub := b.Subscribe()

	unsub()

	// Channel must be closed after unsubscribe.
	if _, ok := <-ch; ok {
		t.Fatal("expected channel to be closed after unsubscribe")
	}

	// Publishing after unsubscribe must not panic (no send on closed channel).
	b.Publish(BusEvent{Seq: 1, Event: Event{ID: 1}})

	// Unsubscribe is idempotent.
	unsub()
}

func TestBusConcurrentPublishSubscribe(t *testing.T) {
	// Run with -race to catch data races on the subscriber set.
	b := NewBus(8)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Publishers.
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
					b.Publish(BusEvent{Seq: int64(i), Event: Event{ID: i}})
				}
			}
		}()
	}

	// Subscribers that churn: subscribe, drain a bit, unsubscribe.
	for s := 0; s < 4; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ch, unsub := b.Subscribe()
				for i := 0; i < 16; i++ {
					select {
					case <-ch:
					default:
					}
				}
				unsub()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
