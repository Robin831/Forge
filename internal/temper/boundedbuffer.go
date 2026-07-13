package temper

import "fmt"

// headTailBuffer is an io.Writer that retains at most a fixed number of bytes
// while preserving both the beginning and the end of the stream. The first
// headCap bytes are kept verbatim; the last tailCap bytes are kept in a ring
// buffer. When more than headCap+tailCap bytes are written, the middle is
// dropped and String() joins the head and tail with an elision marker naming
// the number of bytes removed.
//
// This bounds memory for verbose steps (a runaway test suite cannot balloon
// RSS) while keeping the actionable head (first error, setup) and tail (final
// summary, panic) that a warden/fix prompt needs.
type headTailBuffer struct {
	headCap int
	tailCap int
	head    []byte
	tail    []byte // ring buffer of length tailCap
	tpos    int    // next write index into tail
	tfull   bool   // whether the tail ring has wrapped (is full)
	total   int64  // total bytes ever written
}

// newHeadTailBuffer returns a buffer capped at capBytes total (split roughly
// evenly between head and tail). A non-positive capBytes falls back to
// DefaultOutputCap.
func newHeadTailBuffer(capBytes int) *headTailBuffer {
	if capBytes <= 0 {
		capBytes = DefaultOutputCap
	}
	headCap := capBytes / 2
	tailCap := capBytes - headCap
	return &headTailBuffer{
		headCap: headCap,
		tailCap: tailCap,
		head:    make([]byte, 0, headCap),
		tail:    make([]byte, tailCap),
	}
}

// Write implements io.Writer. It never returns an error and always reports the
// full input length as written so callers (exec.Cmd's stdout/stderr copy) never
// see a short write.
func (b *headTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)

	// Fill the head region first.
	if len(b.head) < b.headCap {
		room := b.headCap - len(b.head)
		take := room
		if take > len(p) {
			take = len(p)
		}
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}

	b.writeTail(p)
	return n, nil
}

// writeTail appends p to the tail ring buffer, retaining only the last tailCap
// bytes of the combined tail stream.
func (b *headTailBuffer) writeTail(p []byte) {
	if b.tailCap == 0 || len(p) == 0 {
		return
	}
	// If this write alone exceeds the ring, only its final tailCap bytes survive.
	if len(p) >= b.tailCap {
		copy(b.tail, p[len(p)-b.tailCap:])
		b.tpos = 0
		b.tfull = true
		return
	}
	end := b.tpos + len(p)
	if end <= b.tailCap {
		copy(b.tail[b.tpos:], p)
	} else {
		first := b.tailCap - b.tpos
		copy(b.tail[b.tpos:], p[:first])
		copy(b.tail, p[first:])
		b.tfull = true
	}
	if end >= b.tailCap {
		b.tfull = true
	}
	b.tpos = end % b.tailCap
}

// tailBytes returns the retained tail bytes in stream order.
func (b *headTailBuffer) tailBytes() []byte {
	if !b.tfull {
		return b.tail[:b.tpos]
	}
	out := make([]byte, b.tailCap)
	n := copy(out, b.tail[b.tpos:])
	copy(out[n:], b.tail[:b.tpos])
	return out
}

// String returns the retained output. When the full stream fit within the cap
// the output is returned verbatim; otherwise the dropped middle is replaced by
// an elision marker naming the number of bytes removed.
func (b *headTailBuffer) String() string {
	tail := b.tailBytes()
	if b.total <= int64(b.headCap+b.tailCap) {
		// Everything fit — head + tail is the exact, complete output.
		return string(b.head) + string(tail)
	}
	elided := b.total - int64(len(b.head)) - int64(len(tail))
	marker := fmt.Sprintf("\n... [%d bytes elided] ...\n", elided)
	return string(b.head) + marker + string(tail)
}
