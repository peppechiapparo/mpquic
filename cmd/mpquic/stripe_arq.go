package main

// stripe_arq.go — Hybrid ARQ with NACK-based selective retransmission.
//
// Design:
//   TX side stores recently sent M=0 packets in a ring buffer.
//   RX side tracks received GroupSeq values via a circular bitmap.
//   When the RX detects gaps (missing seqs older than arqNackThresh),
//   it sends a NACK packet carrying a bitmap of up to 64 missing seqs.
//   TX receives the NACK, looks up the ring buffer, and retransmits.
//
// Only active in M=0 mode. When adaptive FEC enables M>0 (parity),
// FEC handles loss recovery and ARQ pauses.
//
// Wire format for NACK packets:
//   [stripeHdr 16B][base_seq 4B][bitmap 8B]
//   bit N set in bitmap → base_seq+N is missing.

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Constants ────────────────────────────────────────────────────────────

const (
	arqBufSize  = 1 << 12 // 4096 TX ring buffer entries (~200ms at 20K pps)
	arqBufMask  = arqBufSize - 1
	arqWinSize  = 1 << 13 // 8192 RX window bits (circular bitmap)
	arqWinMask  = arqWinSize - 1
	arqWinWords = arqWinSize / 64 // 128 uint64 words

	// arqNackThresh is now dynamically computed in arqRxTracker (defaulting to 96)
	arqNackInterval   time.Duration = 5 * time.Millisecond  // NACK check/send interval
	arqNackCooldown   time.Duration = 30 * time.Millisecond // min time between NACKs (~1 Starlink RTT)
	arqNackMaxBits                  = 64                   // max missing seqs per NACK packet
	arqNackPayloadLen               = 12                   // [base_seq 4B][bitmap 8B]
)

// ─── TX retransmit buffer ─────────────────────────────────────────────────

type arqTxEntry struct {
	seq       uint32
	shardData []byte // [2B length prefix][IP packet] — plaintext
	dataLen   uint16
	valid     bool
}

// arqTxBuf is a fixed-size ring buffer of recently sent M=0 packets.
// Keyed by GroupSeq. Thread-safe via its own RWMutex.
type arqTxBuf struct {
	mu   sync.RWMutex
	ring [arqBufSize]arqTxEntry
}

// store saves a sent packet's plaintext data for potential retransmission.
func (b *arqTxBuf) store(seq uint32, shardData []byte, dataLen uint16) {
	data := make([]byte, len(shardData))
	copy(data, shardData)
	b.mu.Lock()
	b.ring[seq&arqBufMask] = arqTxEntry{
		seq:       seq,
		shardData: data,
		dataLen:   dataLen,
		valid:     true,
	}
	b.mu.Unlock()
}

// lookup retrieves a previously stored packet by GroupSeq.
// Returns nil shardData if the entry was overwritten or never stored.
func (b *arqTxBuf) lookup(seq uint32) (shardData []byte, dataLen uint16, ok bool) {
	b.mu.RLock()
	e := &b.ring[seq&arqBufMask]
	if e.valid && e.seq == seq {
		shardData, dataLen, ok = e.shardData, e.dataLen, true
	}
	b.mu.RUnlock()
	return
}

// ─── RX gap tracker (circular bitmap) ─────────────────────────────────────

// arqRxTracker detects gaps in received M=0 sequences using a circular bitmap.
// The bitmap uses modular addressing: seq maps to position (seq % arqWinSize).
// Thread-safe via internal mutex.
type arqRxTracker struct {
	mu      sync.Mutex
	base    uint32                 // first potentially missing seq (window start)
	highest uint32                 // highest seq received so far
	bitmap  [arqWinWords]uint64    // circular bitmap: bit set = received
	started bool

	// Dynamic NACK threshold (Step 4.30)
	nackThresh uint32
	maxOOO     uint32

	// Stats (atomic, read outside lock)
	nacksSent    uint64
	retxReceived uint64
	dupFiltered  uint64 // packets dropped by dedup before TUN write

	// NACK rate limiting (atomic)
	lastNackSendTime int64 // UnixNano of last NACK sent
}

func newArqRxTracker() *arqRxTracker {
	return &arqRxTracker{
		nackThresh: 96,
		maxOOO:     64, // roughly nackThresh - 32
	}
}

func (t *arqRxTracker) bitWord(seq uint32) (wordIdx, bitPos uint32) {
	offset := seq & arqWinMask
	return offset / 64, offset % 64
}

func (t *arqRxTracker) testBit(seq uint32) bool {
	w, b := t.bitWord(seq)
	return t.bitmap[w]&(1<<b) != 0
}

func (t *arqRxTracker) setBit(seq uint32) {
	w, b := t.bitWord(seq)
	t.bitmap[w] |= 1 << b
}

func (t *arqRxTracker) clearBit(seq uint32) {
	w, b := t.bitWord(seq)
	t.bitmap[w] &^= 1 << b
}

// markReceived records that a packet with the given GroupSeq was received.
// Returns false if the seq is a duplicate or too old (behind window).
func (t *arqRxTracker) markReceived(seq uint32) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.started {
		t.base = seq
		t.highest = seq
		t.started = true
		t.setBit(seq)
		return true
	}

	// Too old — behind the window
	if int32(seq-t.base) < 0 {
		return false
	}

	// Too far ahead — advance window to make room
	offset := seq - t.base
	if offset >= arqWinSize {
		t.advanceBaseLocked(seq - arqWinSize + 1)
	}

	// Check for duplicate
	if t.testBit(seq) {
		return false
	}

	// Step 4.30: Track Out-Of-Order distance to dynamically adjust NACK threshold
	if int32(t.highest-seq) > 0 {
		dist := t.highest - seq
		if dist > t.maxOOO {
			t.maxOOO = dist
			newThresh := dist + 16 // Safety margin
			if newThresh > 512 {   // Upper bound avoiding stalling forever
				newThresh = 512
			}
			t.nackThresh = newThresh
		}
	}

	t.setBit(seq)

	if int32(seq-t.highest) > 0 {
		t.highest = seq
	}

	// Advance base past contiguous received seqs
	t.advanceContiguousLocked()

	return true
}

// advanceBaseLocked clears old bitmap entries and moves the window forward.
func (t *arqRxTracker) advanceBaseLocked(newBase uint32) {
	if int32(newBase-t.base) <= 0 {
		return
	}
	delta := newBase - t.base
	if delta >= arqWinSize {
		for i := range t.bitmap {
			t.bitmap[i] = 0
		}
	} else {
		for seq := t.base; seq != newBase; seq++ {
			t.clearBit(seq)
		}
	}
	t.base = newBase
}

// advanceContiguousLocked moves base past all contiguous received seqs.
func (t *arqRxTracker) advanceContiguousLocked() {
	for int32(t.highest-t.base) >= 0 && t.testBit(t.base) {
		t.clearBit(t.base)
		t.base++
	}
}

// getMissing scans for missing sequences and returns a NACK bitmap.
// Only reports gaps that are at least t.nackThresh behind highest.
// Returns count=0 if no qualifying gaps are found.
func (t *arqRxTracker) getMissing() (baseSeq uint32, nackBitmap uint64, count int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.started {
		return 0, 0, 0
	}

	// Decay maxOOO slowly: every 5ms interval reduces it by 1 (200/sec decay)
	if t.maxOOO > 0 {
		t.maxOOO--
	}
	// Recompute and clamp threshold
	newThresh := t.maxOOO + 16
	if newThresh < 32 {
		newThresh = 32
	} else if newThresh > 512 {
		newThresh = 512
	}
	t.nackThresh = newThresh

	// Only NACK gaps that are sufficiently old (threshold behind highest)
	if int32(t.highest-t.base) < int32(t.nackThresh) {
		return 0, 0, 0
	}
	scanEnd := t.highest - t.nackThresh

	foundBase := false

	for seq := t.base; int32(seq-scanEnd) <= 0; seq++ {
		if t.testBit(seq) {
			continue // received, skip
		}
		// Missing sequence
		if !foundBase {
			baseSeq = seq
			foundBase = true
		}
		relBit := seq - baseSeq
		if relBit >= 64 {
			break // can't fit more in 64-bit bitmap
		}
		nackBitmap |= 1 << relBit
		count++
	}

	return baseSeq, nackBitmap, count
}

// reset clears the tracker state. Used when switching between M=0 and M>0.
func (t *arqRxTracker) reset() {
	t.mu.Lock()
	t.started = false
	t.base = 0
	t.highest = 0
	for i := range t.bitmap {
		t.bitmap[i] = 0
	}
	t.mu.Unlock()
}

// ─── NACK payload encode/decode ───────────────────────────────────────────

func encodeNackPayload(buf []byte, baseSeq uint32, bitmap uint64) {
	binary.BigEndian.PutUint32(buf[0:4], baseSeq)
	binary.BigEndian.PutUint64(buf[4:12], bitmap)
}

func decodeNackPayload(buf []byte) (baseSeq uint32, bitmap uint64, ok bool) {
	if len(buf) < arqNackPayloadLen {
		return 0, 0, false
	}
	return binary.BigEndian.Uint32(buf[0:4]), binary.BigEndian.Uint64(buf[4:12]), true
}

// ─── ARQ stats helpers ────────────────────────────────────────────────────

func (t *arqRxTracker) addNacksSent(n uint64) {
	atomic.AddUint64(&t.nacksSent, n)
}

func (t *arqRxTracker) addRetxReceived(n uint64) {
	atomic.AddUint64(&t.retxReceived, n)
}

func (t *arqRxTracker) addDupFiltered(n uint64) {
	atomic.AddUint64(&t.dupFiltered, n)
}

func (t *arqRxTracker) stats() (nacksSent, retxReceived, dupFiltered uint64) {
	return atomic.LoadUint64(&t.nacksSent), atomic.LoadUint64(&t.retxReceived), atomic.LoadUint64(&t.dupFiltered)
}

// canSendNack returns true if enough time has elapsed since the last NACK.
// This limits NACK rate to ~1 per RTT (arqNackCooldown) to avoid flooding.
func (t *arqRxTracker) canSendNack() bool {
	last := atomic.LoadInt64(&t.lastNackSendTime)
	return time.Since(time.Unix(0, last)) >= arqNackCooldown
}

// recordNackSent marks the current time as last NACK send time.
func (t *arqRxTracker) recordNackSent() {
	atomic.StoreInt64(&t.lastNackSendTime, time.Now().UnixNano())
}
