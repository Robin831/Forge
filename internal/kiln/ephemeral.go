package kiln

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// ipLocalPortRangePath is where Linux publishes its ephemeral port range. It is
// a variable so the parser can be exercised against a fixture.
var ipLocalPortRangePath = "/proc/sys/net/ipv4/ip_local_port_range"

// windowsEphemeralLo/Hi is the dynamic port range Windows has used since Vista
// (`netsh int ipv4 show dynamicport tcp`). It is assumed rather than read: the
// range is only ever narrowed from that floor in practice, so assuming it can
// warn about a range that no longer overlaps but never miss one that does.
const (
	windowsEphemeralLo = 49152
	windowsEphemeralHi = 65535
)

// EphemeralPortRange returns the inclusive range the kernel assigns ephemeral
// (source) ports from, and whether it could be determined at all.
//
// This matters because Kiln allocates a port minutes before the service it was
// allocated for binds it (a cold restore+build sits in between). Anything in
// the same network namespace that opens an outbound connection in that window
// can be handed the allocated port as its local port, and the preview service
// then dies at bind with "address already in use" — a failure that leaves no
// trace, since the connection that took the port is long gone by the time
// anyone looks. A preview range that sits below the ephemeral floor cannot lose
// that race.
//
// An unknown range (a read or parse failure, or a platform whose range Forge
// does not know how to ask for) is reported as ok=false rather than guessed at:
// a warning naming a made-up range is worse than no warning.
func EphemeralPortRange() (lo, hi int, ok bool) {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile(ipLocalPortRangePath)
		if err != nil {
			return 0, 0, false
		}
		return parseIPLocalPortRange(string(data))
	case "windows":
		return windowsEphemeralLo, windowsEphemeralHi, true
	default:
		return 0, 0, false
	}
}

// parseIPLocalPortRange reads the two whitespace-separated bounds Linux writes
// to ip_local_port_range (e.g. "32768\t60999\n"). Anything else is unknown.
func parseIPLocalPortRange(raw string) (lo, hi int, ok bool) {
	fields := strings.Fields(raw)
	if len(fields) != 2 {
		return 0, 0, false
	}
	lo, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, false
	}
	hi, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	if lo <= 0 || hi <= 0 || lo > hi {
		return 0, 0, false
	}
	return lo, hi, true
}

// EphemeralOverlap reports whether the inclusive preview port range [lo, hi]
// intersects the host's ephemeral range, returning that range so the caller can
// name both in its warning. An ephemeral range that could not be determined is
// reported as no overlap — Forge warns about what it knows, and never guesses.
func EphemeralOverlap(lo, hi int) (elo, ehi int, overlap bool) {
	elo, ehi, ok := EphemeralPortRange()
	if !ok {
		return 0, 0, false
	}
	return elo, ehi, lo <= ehi && elo <= hi
}

// EphemeralOverlapWarning returns the operator-facing explanation of an overlap
// between the configured preview port range and the host ephemeral range, or ""
// when the two do not overlap (or the ephemeral range is unknown).
//
// It is a warning and never an error: an operator may have narrowed the kernel
// range, moved it, or simply decided to live with the race, and rejecting their
// configured range would take a working Forge down over a probabilistic risk.
func EphemeralOverlapWarning(lo, hi int, suggested string) string {
	elo, ehi, overlap := EphemeralOverlap(lo, hi)
	if !overlap {
		return ""
	}
	msg := fmt.Sprintf("preview_port_range %d-%d overlaps the host ephemeral port range %d-%d; "+
		"the kernel can hand an allocated port to an outbound connection before the preview service binds it, "+
		"making the service fail with \"address already in use\"", lo, hi, elo, ehi)
	if suggested != "" {
		msg += fmt.Sprintf(". Choose a range below the ephemeral floor (e.g. %s)", suggested)
	}
	return msg
}
