package main

// stripe_fec_xor.go — Sliding-Window XOR Forward Error Correction (RFC 8681)
//
// Unlike block-based Reed-Solomon (K data + M parity), XOR FEC uses a simpler
// approach: every W consecutive source packets are XORed together to produce
// exactly 1 repair packet.  The receiver can recover exactly 1 lost packet per
// window; multi-loss within the same window falls back to ARQ NACK retransmission.
//
// Advantages over RS:
//   - O(W) XOR operations vs O(W·M·log) GF(2^8) multiplications → ~10× cheaper
//   - No block-alignment delay (data packets sent immediately)
//   - Constant overhead: 1/W (W=10 → 10%, W=12 → 8.3%)
//
// Wire format: XOR repair packets reuse the existing stripeHdr (16 bytes):
//   Type      = stripeXOR_REPAIR (0x06)
//   GroupSeq  = sequence number of the FIRST source packet in the window
//   ShardIdx  = 0
//   GroupDataN = W (window size, so receiver knows the range)
//   DataLen   = 0 (no direct IP payload)
//   Payload   = XOR of all W source shards (each padded to maxLen = len(payload))

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
)

const (
	xorFECDefaultWindow = 10
	// Keep receiver buffer entries for this many windows beyond current highwater.
	// The previous value (4) retained only 40 packets with W=10, far below the
	// natural reorder span observed on Starlink multipath. A much larger history
	// materially improves repair usefulness when the repair itself arrives late.
	xorRxBufKeepWindows = 32
	// Minimum number of source shards to retain regardless of W.
	xorRxMinCapacity = 512
)

// ─── XOR FEC Sender ───────────────────────────────────────────────────────

// xorFECSender accumulates W source shards and produces 1 XOR repair per window.
// NOT thread-safe — caller must hold txMu.
type xorFECSender struct {
	window int // W: source packets per repair packet
	stride int // emit one repair every `stride` new sources once the window is full

	history         []xorSourceShard // last up to W source shards (sliding window)
	sinceLastRepair int              // number of new sources since the last emitted repair

	// Stats (atomic — safe to read from metrics goroutine)
	emitted uint64 // total XOR repair packets emitted
}

type xorSourceShard struct {
	seq  uint32
	data []byte
}

func newXorFECSender(window int) *xorFECSender {
	if window <= 0 {
		window = xorFECDefaultWindow
	}
	stride := (window + 1) / 2
	if stride <= 0 {
		stride = 1
	}
	return &xorFECSender{
		window:  window,
		stride:  stride,
		history: make([]xorSourceShard, 0, window),
	}
}

// addSource adds a source shard to the sliding protection window.
// Once the window is full, one repair is emitted every `stride` new shards,
// covering the last up-to-W packets. This overlapping protection materially
// improves recovery against short burst losses compared to disjoint windows.
// shardData is [2-byte length prefix][payload] (same format used by RS FEC).
// Caller must hold txMu.
func (x *xorFECSender) addSource(seq uint32, shardData []byte) (repair []byte, firstSeq uint32, ok bool) {
	dataCopy := make([]byte, len(shardData))
	copy(dataCopy, shardData)
	x.history = append(x.history, xorSourceShard{seq: seq, data: dataCopy})
	if len(x.history) > x.window {
		copy(x.history, x.history[1:])
		x.history = x.history[:x.window]
	}
	x.sinceLastRepair++

	if len(x.history) < x.window || x.sinceLastRepair < x.stride {
		return nil, 0, false
	}

	repair, firstSeq = x.buildRepairLocked(len(x.history))
	x.sinceLastRepair = 0
	atomic.AddUint64(&x.emitted, 1)
	return repair, firstSeq, true

}

func (x *xorFECSender) buildRepairLocked(count int) (repair []byte, firstSeq uint32) {
	if count <= 0 || count > len(x.history) {
		return nil, 0
	}

	start := len(x.history) - count
	firstSeq = x.history[start].seq
	maxLen := 0
	for i := start; i < len(x.history); i++ {
		if l := len(x.history[i].data); l > maxLen {
			maxLen = l
		}
	}
	repair = make([]byte, maxLen)
	for i := start; i < len(x.history); i++ {
		for j := 0; j < len(x.history[i].data); j++ {
			repair[j] ^= x.history[i].data[j]
		}
	}
	return repair, firstSeq
}

// flush emits a partial-window repair if count > 0.
// This ensures the last few packets before idle are still protected. When a
// full-window repair was emitted recently, flush only covers the new tail that
// has not yet triggered the stride threshold.
// Returns window=count (partial tail or full window), or ok=false if nothing to flush.
// Caller must hold txMu.
func (x *xorFECSender) flush() (repair []byte, firstSeq uint32, window int, ok bool) {
	if len(x.history) == 0 {
		return nil, 0, 0, false
	}
	count := len(x.history)
	if len(x.history) >= x.window && x.sinceLastRepair > 0 && x.sinceLastRepair < x.window {
		count = x.sinceLastRepair
	}

	repair, firstSeq = x.buildRepairLocked(count)
	window = count
	ok = true
	x.sinceLastRepair = 0
	atomic.AddUint64(&x.emitted, 1)

	return
}

// ─── XOR FEC Receiver ─────────────────────────────────────────────────────

// xorRxSlot is a single entry in the pre-allocated ring buffer.
type xorRxSlot struct {
	seq   uint32 // actual seq stored (distinguishes from stale wraparound)
	data  []byte // shard data — backing array reused across packets
	valid bool   // true if this slot holds a current entry
}

// xorFECReceiver stores recent source shards in a pre-allocated ring buffer
// and attempts single-loss recovery when an XOR repair packet arrives.
//
// The ring buffer eliminates per-packet heap allocations: after the first
// pass through the ring, the backing []byte slices are reused in-place
// (zero allocs in steady state).  Slot validity is checked via seq match
// to avoid stale data from previous wrap-arounds.
type xorFECReceiver struct {
	window   int
	capacity int // ring buffer size: window * xorRxBufKeepWindows

	mu    sync.Mutex
	ring  []xorRxSlot
	hiSeq uint32 // highest source seq stored

	// Stats (atomic)
	recovered     uint64 // packets recovered via XOR
	unrecoverable uint64 // multi-loss windows (repair arrived but ≥2 missing)
}

func newXorFECReceiver(window int) *xorFECReceiver {
	if window <= 0 {
		window = xorFECDefaultWindow
	}
	cap := window * xorRxBufKeepWindows
	if cap < xorRxMinCapacity {
		cap = xorRxMinCapacity
	}
	ring := make([]xorRxSlot, cap)
	// Pre-allocate backing slices so the first pass also avoids allocs.
	for i := range ring {
		ring[i].data = make([]byte, 0, stripeMaxPayload+2)
	}
	return &xorFECReceiver{
		window:   window,
		capacity: cap,
		ring:     ring,
	}
}

// storeShard saves a source shard for potential XOR recovery.
// Called for every successfully received data packet.
// After the ring is warm, this is ZERO allocations (reuses slot backing array).
func (r *xorFECReceiver) storeShard(seq uint32, data []byte) {
	idx := int(seq % uint32(r.capacity))
	r.mu.Lock()
	slot := &r.ring[idx]
	// Reuse backing array if capacity is sufficient; otherwise grow (rare).
	if cap(slot.data) >= len(data) {
		slot.data = slot.data[:len(data)]
	} else {
		slot.data = make([]byte, len(data))
	}
	copy(slot.data, data)
	slot.seq = seq
	slot.valid = true
	if seq > r.hiSeq {
		r.hiSeq = seq
	}
	r.mu.Unlock()
}

// lookupShard returns the shard data at seq if the slot is valid and matches.
// Caller must hold r.mu.
func (r *xorFECReceiver) lookupShard(seq uint32) ([]byte, bool) {
	idx := int(seq % uint32(r.capacity))
	slot := &r.ring[idx]
	if slot.valid && slot.seq == seq {
		return slot.data, true
	}
	return nil, false
}

// tryRecover attempts to recover a single missing packet from an XOR repair.
// firstSeq is the first source seq in the window; W is the window size.
// repairData is the XOR of all W source shards (each padded to maxLen).
//
// Returns the recovered IP packet (no shard prefix) and its seq, or ok=false
// if 0 or ≥2 packets are missing (recovery not possible).
func (r *xorFECReceiver) tryRecover(firstSeq uint32, W int, repairData []byte) (pkt []byte, recoveredSeq uint32, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var missingSeq uint32
	missingCount := 0

	for i := 0; i < W; i++ {
		seq := firstSeq + uint32(i)
		if _, present := r.lookupShard(seq); !present {
			missingCount++
			missingSeq = seq
			if missingCount > 1 {
				// ≥2 missing: XOR cannot recover, fall back to ARQ.
				atomic.AddUint64(&r.unrecoverable, 1)
				return nil, 0, false
			}
		}
	}

	if missingCount == 0 {
		// All present — no recovery needed, discard repair.
		return nil, 0, false
	}

	// Exactly 1 missing — recover it via XOR of repair and all present shards.
	// recovered = repair ⊕ shard[0] ⊕ shard[1] ⊕ ... ⊕ shard[W-1] (skip missing)
	recovered := make([]byte, len(repairData))
	copy(recovered, repairData)

	for i := 0; i < W; i++ {
		seq := firstSeq + uint32(i)
		if seq == missingSeq {
			continue
		}
		shard, _ := r.lookupShard(seq)
		// XOR present shard into recovered.  shard may be shorter than repairData
		// (different payload sizes) — bytes beyond len(shard) are implicitly
		// zero-padded, so XOR identity applies (no-op for those bytes).
		for j := 0; j < len(shard) && j < len(recovered); j++ {
			recovered[j] ^= shard[j]
		}
	}

	// Extract IP packet from recovered shard: [2-byte len][payload]
	if len(recovered) < 2 {
		return nil, 0, false
	}
	dataLen := binary.BigEndian.Uint16(recovered[0:2])
	if dataLen == 0 || int(dataLen)+2 > len(recovered) {
		return nil, 0, false
	}

	pkt = make([]byte, dataLen)
	copy(pkt, recovered[2:2+dataLen])

	// Store the recovered shard in the ring too.
	idx := int(missingSeq % uint32(r.capacity))
	slot := &r.ring[idx]
	if cap(slot.data) >= len(recovered) {
		slot.data = slot.data[:len(recovered)]
	} else {
		slot.data = make([]byte, len(recovered))
	}
	copy(slot.data, recovered)
	slot.seq = missingSeq
	slot.valid = true

	atomic.AddUint64(&r.recovered, 1)
	return pkt, missingSeq, true
}

// gc is a no-op for the ring buffer implementation.
// Old entries are implicitly overwritten when the sequence wraps around
// the ring, and slot validity is verified by seq match in lookupShard.
func (r *xorFECReceiver) gc() {
	// Ring buffer: no explicit GC needed.
}
