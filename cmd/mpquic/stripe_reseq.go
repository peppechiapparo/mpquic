package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// reseqBuffer restores per-packet ordering at the STRIPES receiver.
//
// STRIPES stripes every packet round-robin across N pipes (for FEC resilience:
// a burst loss on one pipe only costs a few shards of a group). Without
// resequencing the receiver delivers packets in arrival order, and the skew
// between pipes (independent queues/ARQ/pacing, plus Starlink handover jitter)
// shows up to TCP as reordering — read as loss, collapsing cwnd on a single
// flow while a -P30 aggregate still fills the link. This buffer holds
// out-of-order packets keyed on the per-packet session sequence and releases
// them contiguously.
//
// Two release triggers bound head-of-line blocking:
//   - window: an adaptive reorder window in packets (seeded from the measured
//     reorder distance, arqRx.maxOOO). When the highest seen seq runs more than
//     `window` ahead of the gap, the gap is declared lost and skipped — same
//     idea as RACK's reorder window (RFC 8985).
//   - backstop: a time limit on how long the head gap may stall. A genuinely
//     lost packet (not recovered by FEC/ARQ) delays its neighbours by at most
//     this long.
//
// FEC-recovered packets are inserted here too: a recovery fills the gap before
// the backstop fires, so the contiguous run resumes and TCP sees no loss. That
// is what finally makes FEC useful to a TCP stream instead of late, reordered
// noise.
//
// Ordering key: the per-packet session sequence, reconstructed by the caller as
// GroupSeq+ShardIdx for data shards (in the M=0 fast path ShardIdx=0, so it is
// just GroupSeq; in the M>0 group path a group of K packets shares GroupSeq and
// shard i carries the i-th consecutive seq). Recovered packets carry their own
// tracked per-packet seq.
type reseqBuffer struct {
	mu      sync.Mutex
	deliver func([]byte) // in-order sink (push to rxCh); set at construction
	nowFn   func() time.Time

	started  bool
	expected uint32 // next seq to deliver in order
	highest  uint32 // highest seq inserted (wrap-aware)
	occ      int    // occupied slots

	ring []reseqSlot
	mask uint32

	window     uint32        // reorder window in packets (adaptive)
	autoWindow bool          // true = adapt window from measured reorder distance
	backstop   time.Duration // max head-gap stall
	gapSince   time.Time     // when the current head gap started (zero = no gap)

	// metrics (atomic)
	mInOrder    uint64 // delivered without waiting (arrived == expected)
	mReordered  uint64 // delivered out of arrival order (held then released)
	mFECFilled  uint64 // gaps filled by an insert whose seq was already skippable? see insertRecovered
	mGapSkipped uint64 // head gaps declared lost (window or backstop)
	mDupOld     uint64 // dropped: duplicate or older than expected
	mOverflow   uint64 // forced advance because seq ran past the ring
	mReset      uint64 // sender sequence resets detected (re-key/reconnect)
}

type reseqSlot struct {
	seq   uint32
	pkt   []byte
	valid bool
}

// newReseqBuffer builds a resequencing buffer. size is rounded up to a power of
// two (min 256). deliver is the in-order sink. window/backstop bound HoL.
func newReseqBuffer(size int, window uint32, backstop time.Duration, deliver func([]byte)) *reseqBuffer {
	n := 256
	for n < size {
		n <<= 1
	}
	auto := window == 0
	if window == 0 || window > uint32(n) {
		window = uint32(n) / 2
	}
	if backstop <= 0 {
		backstop = 40 * time.Millisecond
	}
	return &reseqBuffer{
		deliver:    deliver,
		nowFn:      time.Now,
		ring:       make([]reseqSlot, n),
		mask:       uint32(n - 1),
		window:     window,
		autoWindow: auto,
		backstop:   backstop,
	}
}

// setWindow updates the adaptive reorder window (clamped to the ring capacity).
func (r *reseqBuffer) setWindow(w uint32) {
	cap32 := uint32(len(r.ring))
	if w < 8 {
		w = 8
	}
	if w > cap32 {
		w = cap32
	}
	r.mu.Lock()
	r.window = w
	r.mu.Unlock()
}

// adaptWindow tracks the measured reorder distance (e.g. arqRx.maxOOO) when the
// window is in auto mode, sizing the reorder window to the link's real behaviour.
func (r *reseqBuffer) adaptWindow(maxOOO uint32) {
	if !r.autoWindow {
		return
	}
	r.setWindow(maxOOO + reseqWindowMargin)
}

// insert adds a data packet at its per-packet sequence and releases any packets
// that are now contiguous. pkt ownership transfers to the buffer.
func (r *reseqBuffer) insert(seq uint32, pkt []byte) {
	r.mu.Lock()
	r.insertLocked(seq, pkt, false)
	r.mu.Unlock()
}

// insertRecovered is like insert but tags the packet as FEC/ARQ recovered, for
// the fec_filled KPI (a recovery that lands on the current head gap).
func (r *reseqBuffer) insertRecovered(seq uint32, pkt []byte) {
	r.mu.Lock()
	r.insertLocked(seq, pkt, true)
	r.mu.Unlock()
}

func (r *reseqBuffer) insertLocked(seq uint32, pkt []byte, recovered bool) {
	if !r.started {
		r.started = true
		r.expected = seq
		r.highest = seq
	}

	// Session reset: the sender restarted its sequence (re-key / reconnect /
	// NAT rebind resets txSeq to 0). A seq a full ring behind is not a stale
	// duplicate — those sit within a reorder window — but a fresh stream. Drop
	// the stale buffer and re-anchor here instead of discarding everything as
	// "old" (which would blackhole the direction until restart).
	if int32(seq-r.expected) < -int32(len(r.ring)) {
		atomic.AddUint64(&r.mReset, 1)
		r.clearRingLocked()
		r.expected = seq
		r.highest = seq
		r.gapSince = time.Time{}
	}

	// Older than what we've already delivered/skipped → duplicate or too late.
	if int32(seq-r.expected) < 0 {
		atomic.AddUint64(&r.mDupOld, 1)
		return
	}

	// Fast path: exactly the packet we're waiting for and nothing buffered ahead
	// contiguously — deliver straight through without touching the ring.
	if seq == r.expected {
		if recovered && r.gapSince != (time.Time{}) {
			atomic.AddUint64(&r.mFECFilled, 1)
		}
		r.deliverLocked(pkt)
		r.expected++
		atomic.AddUint64(&r.mInOrder, 1)
		if int32(r.highest-r.expected) < 0 {
			r.highest = r.expected
		}
		r.drainContiguousLocked()
		r.updateGapLocked()
		return
	}

	// Beyond the ring's reach → force-advance (declare the span up to seq-cap+1
	// lost) so we never overwrite a slot still awaiting delivery.
	capacity := uint32(len(r.ring))
	if uint32(seq-r.expected) >= capacity {
		atomic.AddUint64(&r.mOverflow, 1)
		r.forceAdvanceLocked(seq - capacity + 1)
	}

	slot := &r.ring[seq&r.mask]
	if slot.valid && slot.seq == seq {
		atomic.AddUint64(&r.mDupOld, 1) // duplicate already buffered
		return
	}
	slot.seq = seq
	slot.pkt = pkt
	slot.valid = true
	r.occ++
	if recovered && r.gapSince != (time.Time{}) {
		// A recovery landed while a head gap is open; if it plugs the head it
		// will be counted when drainContiguous reaches it.
	}
	if int32(seq-r.highest) > 0 {
		r.highest = seq
	}

	r.drainContiguousLocked()
	r.enforceWindowLocked()
	r.updateGapLocked()
}

// enforceWindowLocked applies the reorder-window trigger: when the highest seq
// has run a full window ahead of a persistent head gap, the missing head
// packet(s) are declared lost and we resync to the next buffered packet
// (RFC 8985-style reorder window). All leading empty slots are skipped, not
// just one, so a far-ahead packet is never stranded.
func (r *reseqBuffer) enforceWindowLocked() {
	if int32(r.highest-r.expected) < int32(r.window) {
		return
	}
	for r.occ > 0 && !r.ring[r.expected&r.mask].valid {
		atomic.AddUint64(&r.mGapSkipped, 1)
		r.expected++
	}
	r.drainContiguousLocked()
}

// clearRingLocked drops all buffered packets without touching started/expected.
func (r *reseqBuffer) clearRingLocked() {
	for i := range r.ring {
		r.ring[i].valid = false
		r.ring[i].pkt = nil
	}
	r.occ = 0
}

// drainContiguousLocked delivers the contiguous run starting at expected.
func (r *reseqBuffer) drainContiguousLocked() {
	for {
		slot := &r.ring[r.expected&r.mask]
		if !slot.valid || slot.seq != r.expected {
			return
		}
		r.deliverLocked(slot.pkt)
		atomic.AddUint64(&r.mReordered, 1)
		slot.valid = false
		slot.pkt = nil
		r.occ--
		r.expected++
	}
}

// forceAdvanceLocked moves expected up to newExpected, delivering any buffered
// packets it passes over and dropping the rest (declared lost).
func (r *reseqBuffer) forceAdvanceLocked(newExpected uint32) {
	for int32(newExpected-r.expected) > 0 {
		slot := &r.ring[r.expected&r.mask]
		if slot.valid && slot.seq == r.expected {
			r.deliverLocked(slot.pkt)
			slot.valid = false
			slot.pkt = nil
			r.occ--
		} else if slot.valid {
			// Stale occupant from far behind (shouldn't happen with wrap math);
			// leave it — it will be overwritten or aged out.
		}
		r.expected++
	}
	r.drainContiguousLocked()
}

// updateGapLocked maintains gapSince: set when a head gap opens, cleared when
// the head is contiguous.
func (r *reseqBuffer) updateGapLocked() {
	headMissing := r.occ > 0 && !r.ring[r.expected&r.mask].valid
	if headMissing {
		if r.gapSince == (time.Time{}) {
			r.gapSince = r.nowFn()
		}
	} else {
		r.gapSince = time.Time{}
	}
}

// tick is called periodically. If the head gap has stalled past the backstop,
// the missing packet is declared lost and the buffer drains.
func (r *reseqBuffer) tick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gapSince == (time.Time{}) || r.occ == 0 {
		return
	}
	if r.nowFn().Sub(r.gapSince) < r.backstop {
		return
	}
	// Skip missing head seqs until we hit a buffered packet, then drain.
	for r.occ > 0 && !r.ring[r.expected&r.mask].valid {
		atomic.AddUint64(&r.mGapSkipped, 1)
		r.expected++
	}
	r.drainContiguousLocked()
	r.updateGapLocked()
}

func (r *reseqBuffer) deliverLocked(pkt []byte) {
	// deliver may block on rxCh; that is the intended backpressure and callers
	// run it off the hot UDP read path via rxCh's own buffering.
	r.deliver(pkt)
}

// reset clears all state (session reset / re-key). Buffered packets are dropped.
func (r *reseqBuffer) reset() {
	r.mu.Lock()
	r.clearRingLocked()
	r.started = false
	r.gapSince = time.Time{}
	r.mu.Unlock()
}

// reseqStats is a snapshot of the resequencing counters.
type reseqStats struct {
	inOrder, reordered, fecFilled, gapSkipped, dupOld, overflow, reset uint64
	window, occ                                                        uint32
}

func (r *reseqBuffer) stats() reseqStats {
	r.mu.Lock()
	w, o := r.window, uint32(r.occ)
	r.mu.Unlock()
	return reseqStats{
		inOrder:    atomic.LoadUint64(&r.mInOrder),
		reordered:  atomic.LoadUint64(&r.mReordered),
		fecFilled:  atomic.LoadUint64(&r.mFECFilled),
		gapSkipped: atomic.LoadUint64(&r.mGapSkipped),
		dupOld:     atomic.LoadUint64(&r.mDupOld),
		overflow:   atomic.LoadUint64(&r.mOverflow),
		reset:      atomic.LoadUint64(&r.mReset),
		window:     w,
		occ:        o,
	}
}
