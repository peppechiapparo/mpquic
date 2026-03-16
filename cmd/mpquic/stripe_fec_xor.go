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
	xorRxBufKeepWindows = 4
)

// ─── XOR FEC Sender ───────────────────────────────────────────────────────

// xorFECSender accumulates W source shards and produces 1 XOR repair per window.
// NOT thread-safe — caller must hold txMu.
type xorFECSender struct {
	window   int    // W: source packets per repair packet
	count    int    // sources accumulated in current window (0..W-1)
	xorBuf   []byte // running XOR (grows to maxLen in current window)
	firstSeq uint32 // seq of first source in current window

	// Stats (atomic — safe to read from metrics goroutine)
	emitted uint64 // total XOR repair packets emitted
}

func newXorFECSender(window int) *xorFECSender {
	if window <= 0 {
		window = xorFECDefaultWindow
	}
	return &xorFECSender{
		window: window,
		xorBuf: make([]byte, 0, stripeMaxPayload+2),
	}
}

// addSource XORs a source shard into the current window accumulator.
// shardData is [2-byte length prefix][payload] (same format used by RS FEC).
// Returns the repair payload and firstSeq when a complete window is ready.
// Caller must hold txMu.
func (x *xorFECSender) addSource(seq uint32, shardData []byte) (repair []byte, firstSeq uint32, ok bool) {
	if x.count == 0 {
		x.firstSeq = seq
		// Reset accumulator — clear and shrink to 0
		x.xorBuf = x.xorBuf[:0]
	}

	// Grow xorBuf if this shard is longer than the current accumulator.
	if len(shardData) > len(x.xorBuf) {
		newBuf := make([]byte, len(shardData))
		copy(newBuf, x.xorBuf) // preserve running XOR of shorter shards
		x.xorBuf = newBuf
	}

	// XOR shard into accumulator (byte-by-byte).
	// Bytes beyond len(shardData) are implicitly zero-padded (XOR identity).
	for i := 0; i < len(shardData); i++ {
		x.xorBuf[i] ^= shardData[i]
	}
	x.count++

	if x.count >= x.window {
		// Window complete — emit repair.
		repair = make([]byte, len(x.xorBuf))
		copy(repair, x.xorBuf)
		firstSeq = x.firstSeq
		ok = true

		// Reset for next window.
		x.count = 0
		x.xorBuf = x.xorBuf[:0]
		atomic.AddUint64(&x.emitted, 1)
	}

	return
}

// flush emits a partial-window repair if count > 0.
// This ensures the last few packets before idle are still protected.
// Returns window=count (partial), or ok=false if nothing to flush.
// Caller must hold txMu.
func (x *xorFECSender) flush() (repair []byte, firstSeq uint32, window int, ok bool) {
	if x.count == 0 {
		return nil, 0, 0, false
	}

	repair = make([]byte, len(x.xorBuf))
	copy(repair, x.xorBuf)
	firstSeq = x.firstSeq
	window = x.count
	ok = true

	// Reset.
	x.count = 0
	x.xorBuf = x.xorBuf[:0]
	atomic.AddUint64(&x.emitted, 1)

	return
}

// ─── XOR FEC Receiver ─────────────────────────────────────────────────────

// xorFECReceiver stores recent source shards and attempts single-loss recovery
// when an XOR repair packet arrives.
type xorFECReceiver struct {
	window int

	mu     sync.Mutex
	shards map[uint32][]byte // seq → raw shard data ([2B len][payload], NOT padded)
	hiSeq  uint32            // highest source seq stored (for GC)

	// Stats (atomic)
	recovered     uint64 // packets recovered via XOR
	unrecoverable uint64 // multi-loss windows (repair arrived but ≥2 missing)
}

func newXorFECReceiver(window int) *xorFECReceiver {
	if window <= 0 {
		window = xorFECDefaultWindow
	}
	return &xorFECReceiver{
		window: window,
		shards: make(map[uint32][]byte, window*4),
	}
}

// storeShard saves a source shard for potential XOR recovery.
// Called for every successfully received data packet.
func (r *xorFECReceiver) storeShard(seq uint32, data []byte) {
	r.mu.Lock()
	if _, exists := r.shards[seq]; !exists {
		stored := make([]byte, len(data))
		copy(stored, data)
		r.shards[seq] = stored
	}
	if seq > r.hiSeq {
		r.hiSeq = seq
	}
	r.mu.Unlock()
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
		if _, present := r.shards[seq]; !present {
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
		shard := r.shards[seq]
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

	// Store the recovered shard in the buffer too (in case overlapping windows
	// are used in a future sliding mode).
	r.shards[missingSeq] = recovered

	atomic.AddUint64(&r.recovered, 1)
	return pkt, missingSeq, true
}

// gc removes entries older than the retention threshold.
// Called periodically from gcRxGroups / gcLoop.
func (r *xorFECReceiver) gc() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.shards) == 0 {
		return
	}

	// Keep shards within xorRxBufKeepWindows × window of the highwater mark.
	// E.g., hiSeq=99, window=10, keep=4 → cutoff=60, delete seq < 60.
	keepCount := uint32(r.window * xorRxBufKeepWindows)
	if r.hiSeq < keepCount {
		return // not enough history to GC
	}
	cutoff := r.hiSeq - keepCount + 1

	for seq := range r.shards {
		if seq < cutoff {
			delete(r.shards, seq)
		}
	}
}
