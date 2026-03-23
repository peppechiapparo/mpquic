package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
	"time"
)

// makeShardData builds a shard payload in the same format used by the real
// data path: [2-byte big-endian length][payload bytes].
func makeShardData(payload []byte) []byte {
	buf := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(payload)))
	copy(buf[2:], payload)
	return buf
}

// TestRSILTxRx_NoLoss verifies that when all data shards arrive, the RX side
// does NOT produce any recovered packets (no work needed).
func TestRSILTxRx_NoLoss(t *testing.T) {
	K, M, D := 4, 1, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILTx: %v", err)
	}
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	// Send K*D = 16 packets to fill all D groups once.
	var parities []rsilParity
	for seq := uint32(0); seq < uint32(K*D); seq++ {
		data := makeShardData([]byte(fmt.Sprintf("pkt-%d", seq)))
		rx.storeShard(seq, data) // all delivered
		ps := tx.addSource(seq, data)
		parities = append(parities, ps...)
	}

	// We should get D parity shards (one per group)
	if len(parities) != D {
		t.Fatalf("expected %d parities, got %d", D, len(parities))
	}

	// Feed parity to RX — should recover nothing (all data present)
	for _, p := range parities {
		recovered := rx.addParity(p.BaseSeq, int(p.ShardIdx), int(p.K), p.Data)
		if len(recovered) != 0 {
			t.Errorf("expected 0 recovered, got %d for baseSeq=%d", len(recovered), p.BaseSeq)
		}
	}

	recov, attempts, _, _ := rx.stats()
	if recov != 0 {
		t.Errorf("expected 0 recovered total, got %d", recov)
	}
	if attempts != uint64(D) {
		t.Errorf("expected %d attempts, got %d", D, attempts)
	}
}

// TestRSILTxRx_SingleLoss verifies recovery of 1 lost packet per group.
func TestRSILTxRx_SingleLoss(t *testing.T) {
	K, M, D := 4, 1, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILTx: %v", err)
	}
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	// Packets 0..15 fill all groups. We'll lose packet seq=0 (group 0, slot 0).
	lostSeq := uint32(0)
	var parities []rsilParity

	for seq := uint32(0); seq < uint32(K*D); seq++ {
		data := makeShardData([]byte(fmt.Sprintf("pkt-%d", seq)))
		ps := tx.addSource(seq, data)
		parities = append(parities, ps...)

		if seq != lostSeq {
			rx.storeShard(seq, data)
		}
		// seq=0 is NOT stored in RX → simulates loss
	}

	// Feed parity for group 0 (baseSeq=0)
	var totalRecovered int
	for _, p := range parities {
		recovered := rx.addParity(p.BaseSeq, int(p.ShardIdx), int(p.K), p.Data)
		for _, rp := range recovered {
			totalRecovered++
			if rp.Seq != lostSeq {
				t.Errorf("expected recovered seq=%d, got %d", lostSeq, rp.Seq)
			}
			expected := []byte(fmt.Sprintf("pkt-%d", lostSeq))
			if !bytes.Equal(rp.Pkt, expected) {
				t.Errorf("recovered payload mismatch: got %q, want %q", rp.Pkt, expected)
			}
		}
	}

	if totalRecovered != 1 {
		t.Fatalf("expected 1 recovered packet, got %d", totalRecovered)
	}

	recov, _, successes, _ := rx.stats()
	if recov != 1 {
		t.Errorf("stats: expected recovered=1, got %d", recov)
	}
	if successes != 1 {
		t.Errorf("stats: expected successes=1, got %d", successes)
	}
}

// TestRSILTxRx_BurstLoss verifies that a burst of D consecutive losses
// (one per interleave group) can all be recovered.
func TestRSILTxRx_BurstLoss(t *testing.T) {
	K, M, D := 4, 1, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILTx: %v", err)
	}
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	// Lose a burst of D consecutive packets: 4, 5, 6, 7
	// Seq 4 → group 0, seq 5 → group 1, seq 6 → group 2, seq 7 → group 3
	// Each group loses exactly 1 shard → RS(4,1) can recover each
	lostSeqs := map[uint32]bool{4: true, 5: true, 6: true, 7: true}
	var parities []rsilParity
	payloads := make(map[uint32][]byte)

	for seq := uint32(0); seq < uint32(K*D); seq++ {
		payload := []byte(fmt.Sprintf("burst-%d", seq))
		payloads[seq] = payload
		data := makeShardData(payload)
		ps := tx.addSource(seq, data)
		parities = append(parities, ps...)

		if !lostSeqs[seq] {
			rx.storeShard(seq, data)
		}
	}

	recoveredMap := make(map[uint32][]byte)
	for _, p := range parities {
		recovered := rx.addParity(p.BaseSeq, int(p.ShardIdx), int(p.K), p.Data)
		for _, rp := range recovered {
			recoveredMap[rp.Seq] = rp.Pkt
		}
	}

	if len(recoveredMap) != D {
		t.Fatalf("expected %d recovered packets, got %d", D, len(recoveredMap))
	}

	for seq := range lostSeqs {
		pkt, ok := recoveredMap[seq]
		if !ok {
			t.Errorf("seq %d not recovered", seq)
			continue
		}
		if !bytes.Equal(pkt, payloads[seq]) {
			t.Errorf("seq %d payload mismatch: got %q, want %q", seq, pkt, payloads[seq])
		}
	}
}

// TestRSILTxRx_MultiLossSameGroup verifies that losing 2+ shards in the same
// group with M=1 cannot be recovered (insufficient).
func TestRSILTxRx_MultiLossSameGroup(t *testing.T) {
	K, M, D := 4, 1, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILTx: %v", err)
	}
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	// Lose seq=0 and seq=4 → both in group 0 → 2 losses, only M=1 parity
	lostSeqs := map[uint32]bool{0: true, 4: true}
	var parities []rsilParity

	for seq := uint32(0); seq < uint32(K*D); seq++ {
		data := makeShardData([]byte(fmt.Sprintf("ml-%d", seq)))
		ps := tx.addSource(seq, data)
		parities = append(parities, ps...)

		if !lostSeqs[seq] {
			rx.storeShard(seq, data)
		}
	}

	var totalRecovered int
	for _, p := range parities {
		recovered := rx.addParity(p.BaseSeq, int(p.ShardIdx), int(p.K), p.Data)
		totalRecovered += len(recovered)
	}

	if totalRecovered != 0 {
		t.Errorf("expected 0 recovered (insufficient shards), got %d", totalRecovered)
	}

	_, _, _, insufficient := rx.stats()
	if insufficient == 0 {
		t.Error("expected insufficient > 0")
	}
}

// TestRSILTxRx_M2_TwoLosses verifies that M=2 can recover 2 losses per group.
func TestRSILTxRx_M2_TwoLosses(t *testing.T) {
	K, M, D := 4, 2, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILTx: %v", err)
	}
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	// Total packets: K*D = 16. Lose seq 0 and seq 4 (both group 0).
	// With M=2, group 0 has 2 parity shards → can recover 2 missing data shards.
	lostSeqs := map[uint32]bool{0: true, 4: true}
	payloads := make(map[uint32][]byte)
	var parities []rsilParity

	for seq := uint32(0); seq < uint32(K*D); seq++ {
		payload := []byte(fmt.Sprintf("m2-%d", seq))
		payloads[seq] = payload
		data := makeShardData(payload)
		ps := tx.addSource(seq, data)
		parities = append(parities, ps...)

		if !lostSeqs[seq] {
			rx.storeShard(seq, data)
		}
	}

	recoveredMap := make(map[uint32][]byte)
	for _, p := range parities {
		recovered := rx.addParity(p.BaseSeq, int(p.ShardIdx), int(p.K), p.Data)
		for _, rp := range recovered {
			recoveredMap[rp.Seq] = rp.Pkt
		}
	}

	if len(recoveredMap) != 2 {
		t.Fatalf("expected 2 recovered packets with M=2, got %d", len(recoveredMap))
	}

	for seq := range lostSeqs {
		pkt, ok := recoveredMap[seq]
		if !ok {
			t.Errorf("seq %d not recovered", seq)
			continue
		}
		if !bytes.Equal(pkt, payloads[seq]) {
			t.Errorf("seq %d payload mismatch: got %q, want %q", seq, pkt, payloads[seq])
		}
	}
}

// TestRSILTx_Flush verifies that flush() emits parity for partially filled groups.
func TestRSILTx_Flush(t *testing.T) {
	K, M, D := 4, 1, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILTx: %v", err)
	}

	// Add 2 packets: seq 0 → group 0, seq 1 → group 1
	tx.addSource(0, makeShardData([]byte("a")))
	tx.addSource(1, makeShardData([]byte("b")))

	// Neither group is full, so no parities yet
	if tx.stats() != 0 {
		t.Fatalf("expected 0 emitted before flush")
	}

	// Flush should emit parity for both partial groups
	parities := tx.flush()
	if len(parities) != 2 {
		t.Fatalf("expected 2 parities from flush, got %d", len(parities))
	}
	if tx.stats() != 2 {
		t.Errorf("expected 2 emitted after flush, got %d", tx.stats())
	}
}

// TestRSILTx_Stats verifies atomic emitted counter.
func TestRSILTx_Stats(t *testing.T) {
	K, M, D := 4, 1, 2
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILTx: %v", err)
	}

	if tx.stats() != 0 {
		t.Fatalf("initial emitted should be 0")
	}

	// Fill group 0: seqs 0, 2, 4, 6 (D=2, so seq%2==0 → group 0)
	for i := 0; i < K; i++ {
		tx.addSource(uint32(i*D), makeShardData([]byte{byte(i)}))
	}
	// Should have emitted M=1 parity
	if tx.stats() != 1 {
		t.Errorf("expected 1 emitted, got %d", tx.stats())
	}
}

// TestRSILRx_StoreShard verifies ring buffer storage and lookup.
func TestRSILRx_StoreShard(t *testing.T) {
	K, M, D := 4, 1, 4
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	data := makeShardData([]byte("testdata"))
	rx.storeShard(42, data)

	// Verify via ring buffer direct access
	rx.mu.Lock()
	idx := uint32(42) & rsilRxRingMask
	slot := &rx.ring[idx]
	if !slot.valid || slot.seq != 42 {
		t.Errorf("slot not stored correctly: valid=%v seq=%d", slot.valid, slot.seq)
	}
	if !bytes.Equal(slot.data, data) {
		t.Errorf("slot data mismatch")
	}
	rx.mu.Unlock()
}

// TestRSILRx_GC verifies garbage collection of stale pending groups.
func TestRSILRx_GC(t *testing.T) {
	K, M, D := 4, 1, 4
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	// Manually insert a stale group
	rx.mu.Lock()
	rx.pendingGroups[100] = &rsilRxGroup{
		baseSeq: 100,
		K:       K,
		shards:  make([][]byte, K+M),
		present: make([]bool, K+M),
		created: fixedPastTime(),
	}
	rx.mu.Unlock()

	rx.gc()

	rx.mu.Lock()
	remaining := len(rx.pendingGroups)
	rx.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected 0 pending groups after GC, got %d", remaining)
	}
}

// TestRSILRx_VaryingPayloadSizes verifies recovery with different-length payloads.
func TestRSILRx_VaryingPayloadSizes(t *testing.T) {
	K, M, D := 4, 1, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILTx: %v", err)
	}
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	// Create packets with varying sizes for group 0 (seqs 0, 4, 8, 12)
	payloads := map[uint32][]byte{
		0:  bytes.Repeat([]byte("A"), 10),
		4:  bytes.Repeat([]byte("B"), 200),
		8:  bytes.Repeat([]byte("C"), 50),
		12: bytes.Repeat([]byte("D"), 1400),
	}

	lostSeq := uint32(8) // lose the 50-byte one
	var parities []rsilParity

	for seq := uint32(0); seq < uint32(K*D); seq++ {
		var payload []byte
		if p, ok := payloads[seq]; ok {
			payload = p
		} else {
			payload = []byte(fmt.Sprintf("filler-%d", seq))
		}
		data := makeShardData(payload)
		ps := tx.addSource(seq, data)
		parities = append(parities, ps...)

		if seq != lostSeq {
			rx.storeShard(seq, data)
		}
	}

	var totalRecovered int
	for _, p := range parities {
		recovered := rx.addParity(p.BaseSeq, int(p.ShardIdx), int(p.K), p.Data)
		for _, rp := range recovered {
			totalRecovered++
			if rp.Seq != lostSeq {
				t.Errorf("recovered wrong seq: got %d, want %d", rp.Seq, lostSeq)
			}
			if !bytes.Equal(rp.Pkt, payloads[lostSeq]) {
				t.Errorf("recovered payload mismatch for varying sizes")
			}
		}
	}

	if totalRecovered != 1 {
		t.Fatalf("expected 1 recovered, got %d", totalRecovered)
	}
}

// TestRSILTxRx_LargeSequenceNumbers verifies behavior with large seq values and wrapping.
func TestRSILTxRx_LargeSequenceNumbers(t *testing.T) {
	K, M, D := 4, 1, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILTx: %v", err)
	}
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	baseSeq := uint32(1000000)
	lostSeq := baseSeq + 4 // group 0, slot 1

	payloads := make(map[uint32][]byte)
	var parities []rsilParity

	for i := uint32(0); i < uint32(K*D); i++ {
		seq := baseSeq + i
		payload := []byte(fmt.Sprintf("large-%d", seq))
		payloads[seq] = payload
		data := makeShardData(payload)
		ps := tx.addSource(seq, data)
		parities = append(parities, ps...)

		if seq != lostSeq {
			rx.storeShard(seq, data)
		}
	}

	var totalRecovered int
	for _, p := range parities {
		recovered := rx.addParity(p.BaseSeq, int(p.ShardIdx), int(p.K), p.Data)
		for _, rp := range recovered {
			totalRecovered++
			if rp.Seq != lostSeq {
				t.Errorf("wrong seq recovered: got %d, want %d", rp.Seq, lostSeq)
			}
			if !bytes.Equal(rp.Pkt, payloads[lostSeq]) {
				t.Errorf("payload mismatch")
			}
		}
	}

	if totalRecovered != 1 {
		t.Fatalf("expected 1 recovered, got %d", totalRecovered)
	}
}

// TestRSILRx_InvalidParity verifies that invalid parity parameters are handled safely.
func TestRSILRx_InvalidParity(t *testing.T) {
	K, M, D := 4, 1, 4
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		t.Fatalf("newRSILRx: %v", err)
	}

	// Empty data
	if recovered := rx.addParity(0, 0, K, nil); len(recovered) != 0 {
		t.Error("expected nil for empty parity data")
	}

	// Invalid K
	if recovered := rx.addParity(0, 0, 0, []byte{1}); len(recovered) != 0 {
		t.Error("expected nil for K=0")
	}

	// Negative parityIdx (already handled by uint conversion, but test boundary)
	if recovered := rx.addParity(0, -1, K, []byte{1}); len(recovered) != 0 {
		t.Error("expected nil for negative parityIdx")
	}

	// parityIdx >= M
	if recovered := rx.addParity(0, M, K, []byte{1}); len(recovered) != 0 {
		t.Error("expected nil for parityIdx >= M")
	}
}

// TestRSILTx_NewErrors verifies constructor error handling.
func TestRSILTx_NewErrors(t *testing.T) {
	// reedsolomon.New requires K >= 1
	_, err := newRSILTx(0, 1, 4)
	if err == nil {
		t.Error("expected error for K=0")
	}
}

// fixedPastTime returns a time well beyond rsilGroupTTL.
func fixedPastTime() time.Time {
	return time.Now().Add(-2 * rsilGroupTTL)
}

// BenchmarkRSILTxAddSource measures TX encoding throughput.
func BenchmarkRSILTxAddSource(b *testing.B) {
	K, M, D := 4, 1, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		b.Fatalf("newRSILTx: %v", err)
	}
	data := makeShardData(bytes.Repeat([]byte("X"), 1400))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx.addSource(uint32(i), data)
	}
}

// BenchmarkRSILRxRecovery measures RX decode throughput with 1 loss per group.
func BenchmarkRSILRxRecovery(b *testing.B) {
	K, M, D := 4, 1, 4
	tx, err := newRSILTx(K, M, D)
	if err != nil {
		b.Fatalf("newRSILTx: %v", err)
	}
	rx, err := newRSILRx(K, M, D)
	if err != nil {
		b.Fatalf("newRSILRx: %v", err)
	}

	// Pre-fill data and parities
	type roundData struct {
		parities []rsilParity
		shards   []struct {
			seq  uint32
			data []byte
		}
		lostSeq uint32
	}

	rounds := make([]roundData, b.N)
	for r := 0; r < b.N; r++ {
		base := uint32(r * K * D)
		rd := roundData{lostSeq: base} // lose first of each round
		for i := 0; i < K*D; i++ {
			seq := base + uint32(i)
			data := makeShardData(bytes.Repeat([]byte("Y"), 1400))
			ps := tx.addSource(seq, data)
			rd.parities = append(rd.parities, ps...)
			rd.shards = append(rd.shards, struct {
				seq  uint32
				data []byte
			}{seq, data})
		}
		rounds[r] = rd
	}

	b.ResetTimer()
	for r := 0; r < b.N; r++ {
		rd := rounds[r]
		for _, s := range rd.shards {
			if s.seq != rd.lostSeq {
				rx.storeShard(s.seq, s.data)
			}
		}
		for _, p := range rd.parities {
			rx.addParity(p.BaseSeq, int(p.ShardIdx), int(p.K), p.Data)
		}
	}
}
