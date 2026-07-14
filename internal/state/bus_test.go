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
