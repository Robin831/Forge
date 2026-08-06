package kiln

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
)

// ErrPortRangeExhausted is returned when every port in the configured range is
// either already handed out by this allocator or in use by another process.
var ErrPortRangeExhausted = errors.New("kiln: no free port left in the preview port range")

// PortAllocator hands out collision-free TCP ports from the configured preview
// port range (settings.preview_port_range).
//
// Two things can take a port: another preview started by this daemon, and any
// other process on the box. The first is tracked in `taken`, the second is
// detected by actually binding a candidate before returning it. The bind test
// leaves a race window — the port is free when tested, and a stranger could
// grab it before the service binds — but that window is unavoidable without
// passing a listening socket to the child, which the manifest's "run this
// command line" contract rules out. A service that loses that race fails its
// health check like any other broken service.
type PortAllocator struct {
	host   string
	lo, hi int

	mu    sync.Mutex
	taken map[int]bool
	// next is where the next scan starts, so consecutive allocations do not
	// re-probe the same low ports (and a just-released port is not immediately
	// reused, which some servers dislike while their old socket lingers).
	next int
}

// NewPortAllocator returns an allocator over the inclusive range [lo, hi],
// bind-testing on host. An empty host means loopback.
func NewPortAllocator(host string, lo, hi int) (*PortAllocator, error) {
	if lo <= 0 || hi <= 0 || lo > hi {
		return nil, fmt.Errorf("kiln: invalid preview port range %d-%d", lo, hi)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return &PortAllocator{host: host, lo: lo, hi: hi, taken: make(map[int]bool), next: lo}, nil
}

// Allocate reserves and returns one free port. The port stays reserved until
// Release is called, even though nothing is listening on it yet.
func (a *PortAllocator) Allocate() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.allocateLocked()
}

// AllocateN reserves n free ports at once. It is all-or-nothing: if fewer than
// n ports are available, everything reserved so far is released and the error
// is returned, so a preview never starts with a half-allocated service set.
func (a *PortAllocator) AllocateN(n int) ([]int, error) {
	if n <= 0 {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		port, err := a.allocateLocked()
		if err != nil {
			a.releaseLocked(ports)
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, nil
}

// Release returns ports to the pool. Releasing a port that was never allocated
// (or was already released) is a no-op, so teardown paths can call it blindly.
func (a *PortAllocator) Release(ports ...int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseLocked(ports)
}

// InUse reports how many ports this allocator currently has reserved.
func (a *PortAllocator) InUse() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.taken)
}

// allocateLocked walks the range once from a.next, wrapping around, and returns
// the first candidate that is neither reserved nor bindable by anyone else.
func (a *PortAllocator) allocateLocked() (int, error) {
	span := a.hi - a.lo + 1
	for i := 0; i < span; i++ {
		port := a.lo + ((a.next - a.lo + i) % span)
		if a.taken[port] {
			continue
		}
		if !a.free(port) {
			continue
		}
		a.taken[port] = true
		a.next = a.lo + ((port-a.lo)+1)%span
		return port, nil
	}
	return 0, fmt.Errorf("%w (%d-%d, %d reserved by running previews)",
		ErrPortRangeExhausted, a.lo, a.hi, len(a.taken))
}

func (a *PortAllocator) releaseLocked(ports []int) {
	for _, port := range ports {
		delete(a.taken, port)
	}
}

// free reports whether port can be bound on the allocator's host right now.
func (a *PortAllocator) free(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(a.host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	// Closing immediately is what makes this a probe rather than a reservation;
	// a close error would mean the port is still held, so treat it as taken.
	return ln.Close() == nil
}
