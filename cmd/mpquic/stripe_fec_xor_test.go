package main

import (
	"encoding/binary"
	"math/rand"
	"sync/atomic"
	"testing"
)

// makeShard builds a shard in wire format: [2-byte length prefix][payload]
func makeShard(payload []byte) []byte {
	shard := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(shard[0:2], uint16(len(payload)))
	copy(shard[2:], payload)
	return shard
}

func TestXorFECSender_BasicWindow(t *testing.T) {
	tx := newXorFECSender(3) // window=3

	s0 := makeShard([]byte{0xAA, 0xBB, 0xCC})
	s1 := makeShard([]byte{0x11, 0x22, 0x33})
	s2 := makeShard([]byte{0x44, 0x55, 0x66})

	repair0, _, ok0 := tx.addSource(100, s0)
	if ok0 {
		t.Fatal("unexpected repair after 1 source")
	}
	_ = repair0

	repair1, _, ok1 := tx.addSource(101, s1)
	if ok1 {
		t.Fatal("unexpected repair after 2 sources")
	}
	_ = repair1

	repair2, firstSeq, ok2 := tx.addSource(102, s2)
	if !ok2 {
		t.Fatal("expected repair after 3 sources")
	}
	if firstSeq != 100 {
		t.Fatalf("firstSeq = %d, want 100", firstSeq)
	}
	if len(repair2) != len(s0) {
		t.Fatalf("repair len = %d, want %d", len(repair2), len(s0))
	}

	// Verify: repair = s0 XOR s1 XOR s2
	expected := make([]byte, len(s0))
	for i := range expected {
		expected[i] = s0[i] ^ s1[i] ^ s2[i]
	}
	for i, b := range repair2 {
		if b != expected[i] {
			t.Fatalf("repair[%d] = 0x%02x, want 0x%02x", i, b, expected[i])
		}
	}

	if atomic.LoadUint64(&tx.emitted) != 1 {
		t.Fatalf("emitted = %d, want 1", tx.emitted)
	}
}

func TestXorFECSender_VariableLengthShards(t *testing.T) {
	tx := newXorFECSender(3)

	s0 := makeShard([]byte{0xAA, 0xBB})        // len=4
	s1 := makeShard([]byte{0x11, 0x22, 0x33})   // len=5 (longer)
	s2 := makeShard([]byte{0x44})                // len=3

	tx.addSource(0, s0)
	tx.addSource(1, s1)
	repair, _, ok := tx.addSource(2, s2)
	if !ok {
		t.Fatal("expected repair")
	}

	// Repair length = max(4, 5, 3) = 5
	if len(repair) != 5 {
		t.Fatalf("repair len = %d, want 5", len(repair))
	}

	// Verify: pad all to 5, XOR
	p0 := make([]byte, 5); copy(p0, s0)
	p1 := make([]byte, 5); copy(p1, s1)
	p2 := make([]byte, 5); copy(p2, s2)
	expected := make([]byte, 5)
	for i := range expected {
		expected[i] = p0[i] ^ p1[i] ^ p2[i]
	}
	for i, b := range repair {
		if b != expected[i] {
			t.Fatalf("repair[%d] = 0x%02x, want 0x%02x", i, b, expected[i])
		}
	}
}

func TestXorFECSender_Flush(t *testing.T) {
	tx := newXorFECSender(5) // window=5

	s0 := makeShard([]byte{0x01, 0x02})
	s1 := makeShard([]byte{0x03, 0x04})

	tx.addSource(10, s0)
	tx.addSource(11, s1)

	// Flush partial window (2 of 5)
	repair, firstSeq, window, ok := tx.flush()
	if !ok {
		t.Fatal("expected flush to produce repair")
	}
	if firstSeq != 10 {
		t.Fatalf("firstSeq = %d, want 10", firstSeq)
	}
	if window != 2 {
		t.Fatalf("window = %d, want 2", window)
	}

	// Verify: repair = s0 XOR s1
	expected := make([]byte, len(s0))
	for i := range expected {
		expected[i] = s0[i] ^ s1[i]
	}
	for i, b := range repair {
		if b != expected[i] {
			t.Fatalf("repair[%d] = 0x%02x, want 0x%02x", i, b, expected[i])
		}
	}

	// After flush, next addSource starts a new window
	_, _, ok = tx.addSource(20, s0)
	if ok {
		t.Fatal("unexpected repair after 1 source in new window")
	}
}

func TestXorFECReceiver_RecoverOneLoss(t *testing.T) {
	// Sender: 3 sources, 1 repair
	tx := newXorFECSender(3)
	rx := newXorFECReceiver(3)

	pkt0 := []byte{10, 20, 30, 40}
	pkt1 := []byte{50, 60, 70, 80}
	pkt2 := []byte{90, 100}

	s0 := makeShard(pkt0)
	s1 := makeShard(pkt1)
	s2 := makeShard(pkt2)

	tx.addSource(0, s0)
	tx.addSource(1, s1)
	repair, firstSeq, ok := tx.addSource(2, s2)
	if !ok {
		t.Fatal("expected repair")
	}

	// Receiver gets s0 and s2, but not s1 (lost)
	rx.storeShard(0, s0)
	rx.storeShard(2, s2)

	recovered, recSeq, recOK := rx.tryRecover(firstSeq, 3, repair)
	if !recOK {
		t.Fatal("expected recovery")
	}
	if recSeq != 1 {
		t.Fatalf("recovered seq = %d, want 1", recSeq)
	}

	// Verify recovered packet matches pkt1
	if len(recovered) != len(pkt1) {
		t.Fatalf("recovered len = %d, want %d", len(recovered), len(pkt1))
	}
	for i, b := range recovered {
		if b != pkt1[i] {
			t.Fatalf("recovered[%d] = %d, want %d", i, b, pkt1[i])
		}
	}
}

func TestXorFECReceiver_NoLoss(t *testing.T) {
	tx := newXorFECSender(3)
	rx := newXorFECReceiver(3)

	s0 := makeShard([]byte{1, 2, 3})
	s1 := makeShard([]byte{4, 5, 6})
	s2 := makeShard([]byte{7, 8, 9})

	tx.addSource(0, s0)
	tx.addSource(1, s1)
	repair, firstSeq, _ := tx.addSource(2, s2)

	// All present
	rx.storeShard(0, s0)
	rx.storeShard(1, s1)
	rx.storeShard(2, s2)

	_, _, ok := rx.tryRecover(firstSeq, 3, repair)
	if ok {
		t.Fatal("expected no recovery when all present")
	}
}

func TestXorFECReceiver_TwoLosses(t *testing.T) {
	tx := newXorFECSender(3)
	rx := newXorFECReceiver(3)

	s0 := makeShard([]byte{1})
	s1 := makeShard([]byte{2})
	s2 := makeShard([]byte{3})

	tx.addSource(0, s0)
	tx.addSource(1, s1)
	repair, firstSeq, _ := tx.addSource(2, s2)

	// Only s0 present — 2 missing
	rx.storeShard(0, s0)

	_, _, ok := rx.tryRecover(firstSeq, 3, repair)
	if ok {
		t.Fatal("expected no recovery with 2 losses")
	}
	if atomic.LoadUint64(&rx.unrecoverable) != 1 {
		t.Fatalf("unrecoverable = %d, want 1", rx.unrecoverable)
	}
}

func TestXorFECReceiver_VariableLength(t *testing.T) {
	tx := newXorFECSender(3)
	rx := newXorFECReceiver(3)

	// Variable-length packets
	pkt0 := []byte{0xDE, 0xAD}
	pkt1 := []byte{0xBE, 0xEF, 0xCA, 0xFE}
	pkt2 := []byte{0x42}

	s0 := makeShard(pkt0)
	s1 := makeShard(pkt1)
	s2 := makeShard(pkt2)

	tx.addSource(0, s0)
	tx.addSource(1, s1)
	repair, firstSeq, _ := tx.addSource(2, s2)

	// Lose pkt1 (the longest one)
	rx.storeShard(0, s0)
	rx.storeShard(2, s2)

	recovered, _, ok := rx.tryRecover(firstSeq, 3, repair)
	if !ok {
		t.Fatal("expected recovery")
	}
	if len(recovered) != len(pkt1) {
		t.Fatalf("recovered len = %d, want %d", len(recovered), len(pkt1))
	}
	for i, b := range recovered {
		if b != pkt1[i] {
			t.Fatalf("recovered[%d] = 0x%02x, want 0x%02x", i, b, pkt1[i])
		}
	}
}

func TestXorFECReceiver_GC(t *testing.T) {
	rx := newXorFECReceiver(10)

	// Store 100 shards (ring capacity = 10*4 = 40 slots)
	for i := uint32(0); i < 100; i++ {
		rx.storeShard(i, makeShard([]byte{byte(i)}))
	}

	rx.gc() // no-op for ring buffer

	// Ring buffer: old entries are implicitly overwritten.
	// Only the last `capacity` unique seq values survive in the ring.
	// Verify that recent shards (seq >= 60) are still accessible.
	rx.mu.Lock()
	for seq := uint32(60); seq < 100; seq++ {
		if _, ok := rx.lookupShard(seq); !ok {
			t.Errorf("shard seq=%d should still be in ring", seq)
		}
	}
	// Verify that very old shards (seq < 60) are stale (overwritten).
	// With capacity=40, seq=0 maps to slot 0, which now holds seq=80.
	staleLookups := 0
	for seq := uint32(0); seq < 60; seq++ {
		if _, ok := rx.lookupShard(seq); !ok {
			staleLookups++
		}
	}
	if staleLookups == 0 {
		t.Error("expected some old shards to be overwritten in ring buffer")
	}
	rx.mu.Unlock()
}

func TestXorFEC_EndToEnd_RandomLoss(t *testing.T) {
	W := 10
	tx := newXorFECSender(W)
	rx := newXorFECReceiver(W)

	rng := rand.New(rand.NewSource(42))
	totalPkts := 1000
	lostPkts := 0
	recoveredPkts := 0

	// Generate packets in windows of W
	for windowStart := 0; windowStart < totalPkts; windowStart += W {
		// Generate W source shards
		shards := make([][]byte, W)
		seqs := make([]uint32, W)
		var repair []byte
		var firstSeq uint32
		for i := 0; i < W; i++ {
			seq := uint32(windowStart + i)
			pktLen := 100 + rng.Intn(1300) // random payload size
			payload := make([]byte, pktLen)
			rng.Read(payload)
			shard := makeShard(payload)
			shards[i] = shard
			seqs[i] = seq

			var ok bool
			repair, firstSeq, ok = tx.addSource(seq, shard)
			if i == W-1 && !ok {
				t.Fatal("expected repair at end of window")
			}
		}

		// Simulate: randomly lose up to 1 packet per window (5% chance)
		lostIdx := -1
		if rng.Float64() < 0.05 {
			lostIdx = rng.Intn(W)
			lostPkts++
		}

		// Store received shards
		for i := 0; i < W; i++ {
			if i == lostIdx {
				continue
			}
			rx.storeShard(seqs[i], shards[i])
		}

		// Try recovery
		if lostIdx >= 0 && repair != nil {
			pkt, recSeq, ok := rx.tryRecover(firstSeq, W, repair)
			if ok {
				recoveredPkts++
				if recSeq != seqs[lostIdx] {
					t.Fatalf("recovered seq %d != expected %d", recSeq, seqs[lostIdx])
				}
				// Verify recovered packet matches original
				originalPayload := shards[lostIdx][2:]
				originalLen := binary.BigEndian.Uint16(shards[lostIdx][:2])
				if len(pkt) != int(originalLen) {
					t.Fatalf("recovered len %d != original %d", len(pkt), originalLen)
				}
				for j := range pkt {
					if pkt[j] != originalPayload[j] {
						t.Fatalf("byte %d mismatch", j)
					}
				}
			}
		}
	}

	t.Logf("total=%d lost=%d recovered=%d xor_emitted=%d",
		totalPkts, lostPkts, recoveredPkts, atomic.LoadUint64(&tx.emitted))

	if lostPkts > 0 && recoveredPkts != lostPkts {
		t.Errorf("expected %d recoveries, got %d", lostPkts, recoveredPkts)
	}
}

func BenchmarkXorFECSender_AddSource(b *testing.B) {
	tx := newXorFECSender(10)
	shard := makeShard(make([]byte, 1400)) // typical MTU

	b.SetBytes(1400)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx.addSource(uint32(i), shard)
	}
}

func BenchmarkXorFECReceiver_StoreShard(b *testing.B) {
	rx := newXorFECReceiver(10)
	shard := makeShard(make([]byte, 1400))

	// Warm up the ring: fill all slots once so backing arrays are allocated.
	for i := 0; i < rx.capacity; i++ {
		rx.storeShard(uint32(i), shard)
	}

	b.SetBytes(1400)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rx.storeShard(uint32(rx.capacity+i), shard)
	}
}

func BenchmarkXorFECReceiver_RecoverOneLoss(b *testing.B) {
	W := 10
	shard := makeShard(make([]byte, 1400))

	// Pre-compute one window's repair
	tx := newXorFECSender(W)
	for i := 0; i < W-1; i++ {
		tx.addSource(uint32(i), shard)
	}
	repair, firstSeq, _ := tx.addSource(uint32(W-1), shard)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rx := newXorFECReceiver(W)
		// Store all but first
		for j := 1; j < W; j++ {
			rx.storeShard(uint32(j), shard)
		}
		rx.tryRecover(firstSeq, W, repair)
	}
}
