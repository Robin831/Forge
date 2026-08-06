package kiln

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
)

// freePort returns a port that is free right now, used to build ranges that do
// not collide with whatever else the test machine is running.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestNewPortAllocatorRejectsInvalidRange(t *testing.T) {
	for _, tc := range []struct{ lo, hi int }{{0, 100}, {100, 99}, {-1, 5}} {
		if _, err := NewPortAllocator("", tc.lo, tc.hi); err == nil {
			t.Errorf("NewPortAllocator(%d, %d) accepted an invalid range", tc.lo, tc.hi)
		}
	}
}

func TestAllocateReturnsDistinctPortsInRange(t *testing.T) {
	lo := freePort(t)
	a, err := NewPortAllocator("127.0.0.1", lo, lo+20)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	seen := map[int]bool{}
	for i := 0; i < 5; i++ {
		port, err := a.Allocate()
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if port < lo || port > lo+20 {
			t.Fatalf("port %d outside range %d-%d", port, lo, lo+20)
		}
		if seen[port] {
			t.Fatalf("port %d handed out twice", port)
		}
		seen[port] = true
	}
	if a.InUse() != 5 {
		t.Errorf("InUse = %d, want 5", a.InUse())
	}
}

func TestAllocateSkipsPortHeldByAnotherProcess(t *testing.T) {
	// A one-port range whose only port is occupied has nothing to hand out.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer held.Close()
	busy := held.Addr().(*net.TCPAddr).Port

	a, err := NewPortAllocator("127.0.0.1", busy, busy)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	if port, err := a.Allocate(); !errors.Is(err, ErrPortRangeExhausted) {
		t.Fatalf("Allocate over a busy port = %d, %v; want ErrPortRangeExhausted", port, err)
	}

	// Once the port is released, the same allocator can use it.
	held.Close()
	port, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate after release: %v", err)
	}
	if port != busy {
		t.Errorf("Allocate = %d, want %d", port, busy)
	}
}

func TestAllocateExhaustsRange(t *testing.T) {
	lo := freePort(t)
	a, err := NewPortAllocator("127.0.0.1", lo, lo+1)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	first, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if _, err := a.Allocate(); err != nil {
		t.Fatalf("second Allocate: %v", err)
	}
	if _, err := a.Allocate(); !errors.Is(err, ErrPortRangeExhausted) {
		t.Fatalf("third Allocate error = %v, want ErrPortRangeExhausted", err)
	}

	// Releasing makes room again.
	a.Release(first)
	if _, err := a.Allocate(); err != nil {
		t.Fatalf("Allocate after Release: %v", err)
	}
}

func TestAllocateNIsAllOrNothing(t *testing.T) {
	lo := freePort(t)
	a, err := NewPortAllocator("127.0.0.1", lo, lo+1)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	if _, err := a.AllocateN(3); !errors.Is(err, ErrPortRangeExhausted) {
		t.Fatalf("AllocateN(3) error = %v, want ErrPortRangeExhausted", err)
	}
	if a.InUse() != 0 {
		t.Errorf("a failed AllocateN leaked %d reservations", a.InUse())
	}

	ports, err := a.AllocateN(2)
	if err != nil {
		t.Fatalf("AllocateN(2): %v", err)
	}
	if len(ports) != 2 || ports[0] == ports[1] {
		t.Errorf("AllocateN(2) = %v, want two distinct ports", ports)
	}
}

func TestAllocateIsConcurrencySafe(t *testing.T) {
	lo := freePort(t)
	const n = 20
	a, err := NewPortAllocator("127.0.0.1", lo, lo+200)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	var (
		mu    sync.Mutex
		seen  = map[int]bool{}
		wg    sync.WaitGroup
		fails []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port, err := a.Allocate()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fails = append(fails, err)
				return
			}
			if seen[port] {
				fails = append(fails, errors.New("duplicate port "+strconv.Itoa(port)))
			}
			seen[port] = true
		}()
	}
	wg.Wait()
	for _, err := range fails {
		t.Errorf("concurrent Allocate: %v", err)
	}
	if len(seen) != n {
		t.Errorf("got %d distinct ports, want %d", len(seen), n)
	}
}
