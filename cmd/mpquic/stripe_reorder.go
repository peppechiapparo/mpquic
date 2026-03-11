package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// ─── Stripe RX Reorder Buffer ─────────────────────────────────────────────
//
// Step 4.18: packets sent round-robin across multiple UDP pipes arrive
// out-of-order at the receiver.  TCP inside the tunnel interprets this
// as loss → DupACK → retransmit → congestion-control back-off, which
// limits throughput to ~350 Mbps instead of the theoretical ~500 Mbps.
//
// The reorder buffer holds arriving packets in a small window, indexed
// by their GroupSeq, and releases them in monotonically increasing order.
// If a gap persists longer than the configured timeout (default 3 ms)
// the buffer skips the missing sequence and delivers what it has.
//
// Thread-safe: Insert may be called concurrently from multiple
// recvPipeLoop goroutines.

const (
	stripeReorderDefaultWindow    = 128
	stripeReorderDefaultTimeoutMs = 3
)

type stripeReorderBuf struct {
	mu      sync.Mutex
	slots   map[uint32][]byte // buffered out-of-order packets
	nextSeq uint32            // next sequence we want to deliver
	started bool              // false until first packet initialises nextSeq

	window  int           // max slots to buffer before force-flushing
	timeout time.Duration // gap timeout

	rxCh    chan []byte   // destination channel (existing stripe rxCh)
	closeCh chan struct{} // shutdown signal

	timer *time.Timer // armed when there is a gap

	// Stats (atomic)
	delivered uint64 // packets delivered in-order (or after reorder)
	skipped   uint64 // gaps skipped by timeout
	buffered  uint64 // current buffered count (informational)
}

func newStripeReorderBuf(rxCh chan []byte, closeCh chan struct{}, window int, timeoutMs int) *stripeReorderBuf {
	if window <= 0 {
		window = stripeReorderDefaultWindow
	}
	if timeoutMs <= 0 {
		timeoutMs = stripeReorderDefaultTimeoutMs
	}
	rb := &stripeReorderBuf{
		slots:   make(map[uint32][]byte, window),
		window:  window,
		timeout: time.Duration(timeoutMs) * time.Millisecond,
		rxCh:    rxCh,
		closeCh: closeCh,
	}
	// Timer starts stopped; armed on first gap.
	rb.timer = time.AfterFunc(time.Hour, rb.onTimeout)
	rb.timer.Stop()
	return rb
}

// Insert adds a packet with the given sequence number into the reorder
// buffer.  If the packet is the one we are waiting for (nextSeq), it is
// delivered immediately together with any consecutive buffered packets.
// Otherwise it is held until its turn or the gap times out.
func (rb *stripeReorderBuf) Insert(seq uint32, pkt []byte) {
	rb.mu.Lock()

	// ── First packet ever: bootstrap nextSeq ──
	if !rb.started {
		rb.started = true
		rb.nextSeq = seq
	}

	// ── Detect sequence reset (e.g. client/server reconnect) ──
	// If the incoming seq is far behind nextSeq (more than half the
	// uint32 space), treat it as a forward wrap.  If it is simply
	// behind by more than the window, assume a reset and re-bootstrap.
	diff := seq - rb.nextSeq // unsigned subtraction wraps correctly
	if diff > uint32(rb.window) && diff < 0x80000000 {
		// seq is way behind — stale duplicate, deliver directly.
		rb.deliverOne(pkt)
		rb.mu.Unlock()
		return
	}
	if diff >= 0x80000000 {
		// seq wrapped backward past nextSeq by a huge amount — treat
		// as a sequence reset: flush everything and re-bootstrap.
		rb.flushAll()
		rb.nextSeq = seq
	}

	if seq == rb.nextSeq {
		// ── In order: deliver immediately and flush consecutive run ──
		rb.deliverOne(pkt)
		rb.nextSeq++
		rb.flushConsecutive()
		// If no more gaps, disarm the timer.
		if len(rb.slots) == 0 {
			rb.timer.Stop()
		}
		rb.mu.Unlock()
		return
	}

	// ── Out of order: buffer ──
	rb.slots[seq] = pkt
	atomic.AddUint64(&rb.buffered, 1)

	// Arm the timeout if not already running (one-shot per gap).
	// Reset to ensure we get at least `timeout` from now.
	rb.timer.Reset(rb.timeout)

	// Safety valve: if we buffered more than window, force-flush.
	if len(rb.slots) >= rb.window {
		rb.advanceToMinBuffered()
		rb.flushConsecutive()
	}
	rb.mu.Unlock()
}

// Reset clears all buffered state — called on session re-key.
func (rb *stripeReorderBuf) Reset() {
	rb.mu.Lock()
	rb.timer.Stop()
	rb.slots = make(map[uint32][]byte, rb.window)
	rb.started = false
	rb.nextSeq = 0
	atomic.StoreUint64(&rb.buffered, 0)
	rb.mu.Unlock()
}

// Stop disarms the timer (call on connection close).
func (rb *stripeReorderBuf) Stop() {
	rb.timer.Stop()
}

// ─── internal helpers (must be called with mu held) ───────────────────────

// deliverOne pushes a single packet to rxCh (non-blocking).
func (rb *stripeReorderBuf) deliverOne(pkt []byte) {
	atomic.AddUint64(&rb.delivered, 1)
	select {
	case rb.rxCh <- pkt:
	case <-rb.closeCh:
	default:
		// Drop if buffer full — same backpressure as before.
	}
}

// flushConsecutive delivers all packets starting from nextSeq that are
// present in slots, advancing nextSeq for each.
func (rb *stripeReorderBuf) flushConsecutive() {
	for {
		pkt, ok := rb.slots[rb.nextSeq]
		if !ok {
			return
		}
		delete(rb.slots, rb.nextSeq)
		atomic.AddUint64(&rb.buffered, ^uint64(0)) // decrement
		rb.deliverOne(pkt)
		rb.nextSeq++
	}
}

// flushAll delivers every buffered packet (in ascending order) and clears
// the map — used on sequence reset.
func (rb *stripeReorderBuf) flushAll() {
	if len(rb.slots) == 0 {
		return
	}
	// Deliver in order by finding min repeatedly (slots is small).
	for len(rb.slots) > 0 {
		var minSeq uint32
		first := true
		for s := range rb.slots {
			if first || s-rb.nextSeq < minSeq-rb.nextSeq {
				minSeq = s
				first = false
			}
		}
		rb.deliverOne(rb.slots[minSeq])
		delete(rb.slots, minSeq)
	}
	atomic.StoreUint64(&rb.buffered, 0)
	rb.timer.Stop()
}

// advanceToMinBuffered sets nextSeq to the smallest buffered sequence,
// counting skipped gaps.
func (rb *stripeReorderBuf) advanceToMinBuffered() {
	if len(rb.slots) == 0 {
		return
	}
	var minSeq uint32
	first := true
	for s := range rb.slots {
		if first || s-rb.nextSeq < minSeq-rb.nextSeq {
			minSeq = s
			first = false
		}
	}
	gap := minSeq - rb.nextSeq
	atomic.AddUint64(&rb.skipped, uint64(gap))
	rb.nextSeq = minSeq
}

// onTimeout fires when the gap timer expires — advance past the gap and
// deliver whatever is available.
func (rb *stripeReorderBuf) onTimeout() {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if len(rb.slots) == 0 {
		return
	}

	rb.advanceToMinBuffered()
	rb.flushConsecutive()

	// Re-arm if there are still gaps.
	if len(rb.slots) > 0 {
		rb.timer.Reset(rb.timeout)
	}
}
