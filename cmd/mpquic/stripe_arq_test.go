package main

import (
	"testing"
	"time"
)

// ─── arqTxBuf tests ───────────────────────────────────────────────────────

func TestArqTxBuf_StoreAndLookup(t *testing.T) {
	buf := &arqTxBuf{}
	data := []byte{0x01, 0x02, 0x03}
	buf.store(42, data, 3)

	got, dl, ok := buf.lookup(42)
	if !ok {
		t.Fatal("lookup should succeed for stored seq")
	}
	if dl != 3 {
		t.Errorf("dataLen = %d, want 3", dl)
	}
	if len(got) != 3 || got[0] != 0x01 {
		t.Errorf("shardData mismatch: got %v", got)
	}
}

func TestArqTxBuf_LookupMiss(t *testing.T) {
	buf := &arqTxBuf{}
	_, _, ok := buf.lookup(99)
	if ok {
		t.Error("lookup should fail for unstored seq")
	}
}

func TestArqTxBuf_Overwrite(t *testing.T) {
	buf := &arqTxBuf{}
	buf.store(42, []byte{0xAA}, 1)
	// Overwrite with a seq that maps to the same slot
	overwriteSeq := uint32(42 + arqBufSize)
	buf.store(overwriteSeq, []byte{0xBB}, 1)

	// Original should no longer be found
	_, _, ok := buf.lookup(42)
	if ok {
		t.Error("lookup should fail after slot overwritten by newer seq")
	}

	got, _, ok := buf.lookup(overwriteSeq)
	if !ok {
		t.Fatal("lookup should succeed for newer seq")
	}
	if got[0] != 0xBB {
		t.Errorf("expected 0xBB, got 0x%X", got[0])
	}
}

func TestArqTxBuf_StoreIsolation(t *testing.T) {
	// Ensure store() copies the data (caller can mutate original)
	buf := &arqTxBuf{}
	data := []byte{0x01, 0x02}
	buf.store(1, data, 2)
	data[0] = 0xFF // mutate original

	got, _, ok := buf.lookup(1)
	if !ok {
		t.Fatal("lookup should succeed")
	}
	if got[0] != 0x01 {
		t.Error("store should copy data, not reference original slice")
	}
}

// ─── arqRxTracker tests ──────────────────────────────────────────────────

func TestArqRxTracker_FirstPacket(t *testing.T) {
	tr := newArqRxTracker()
	ok := tr.markReceived(100)
	if !ok {
		t.Error("first packet should not be duplicate")
	}
	if !tr.started {
		t.Error("tracker should be started after first packet")
	}
}

func TestArqRxTracker_Duplicate(t *testing.T) {
	tr := newArqRxTracker()
	tr.markReceived(100)
	ok := tr.markReceived(100)
	if ok {
		t.Error("second markReceived for same seq should return false (duplicate)")
	}
}

func TestArqRxTracker_Sequential(t *testing.T) {
	tr := newArqRxTracker()
	for seq := uint32(0); seq < 200; seq++ {
		ok := tr.markReceived(seq)
		if !ok {
			t.Errorf("seq %d should not be duplicate", seq)
		}
	}
	// Base should have advanced past all received
	if tr.base < 200 {
		t.Errorf("base should be >= 200, got %d", tr.base)
	}
}

func TestArqRxTracker_TooOld(t *testing.T) {
	tr := newArqRxTracker()
	tr.markReceived(1000)
	// push window forward
	for seq := uint32(1001); seq < 1000+arqWinSize; seq++ {
		tr.markReceived(seq)
	}
	// seq 1000 is now behind the window
	ok := tr.markReceived(999)
	if ok {
		t.Error("too-old seq should be rejected (return false)")
	}
}

func TestArqRxTracker_GetMissing_NoGaps(t *testing.T) {
	tr := newArqRxTracker()
	// Send 200 sequential packets — no gaps
	for seq := uint32(0); seq < 200; seq++ {
		tr.markReceived(seq)
	}
	_, _, count := tr.getMissing()
	if count != 0 {
		t.Errorf("no gaps expected, got count=%d", count)
	}
}

func TestArqRxTracker_GetMissing_WithGaps(t *testing.T) {
	tr := newArqRxTracker()
	// Receive 0..99 except skip 10, 20, 30
	for seq := uint32(0); seq < 200; seq++ {
		if seq == 10 || seq == 20 || seq == 30 {
			continue
		}
		tr.markReceived(seq)
	}

	baseSeq, bitmap, count := tr.getMissing()
	if count == 0 {
		t.Fatal("expected missing seqs, got 0")
	}
	// baseSeq should be 10 (first missing)
	if baseSeq != 10 {
		t.Errorf("baseSeq = %d, want 10", baseSeq)
	}
	// bit 0 (seq 10) should be set
	if bitmap&1 == 0 {
		t.Error("bit 0 (seq 10) should be set in NACK bitmap")
	}
	// bit 10 (seq 20) should be set
	if bitmap&(1<<10) == 0 {
		t.Error("bit 10 (seq 20) should be set in NACK bitmap")
	}
	// bit 20 (seq 30) should be set
	if bitmap&(1<<20) == 0 {
		t.Error("bit 20 (seq 30) should be set in NACK bitmap")
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestArqRxTracker_GetMissing_BelowThreshold(t *testing.T) {
	tr := newArqRxTracker()
	// Only a few packets — gap not old enough to report
	tr.markReceived(0)
	// skip 1
	tr.markReceived(2)
	tr.markReceived(3)
	// highest=3, gap=1 packet from highest, which is < arqNackThresh (96)
	_, _, count := tr.getMissing()
	if count != 0 {
		t.Errorf("gap too recent to NACK, expected count=0, got %d", count)
	}
}

func TestArqRxTracker_Reset(t *testing.T) {
	tr := newArqRxTracker()
	tr.markReceived(100)
	tr.markReceived(200)
	tr.reset()

	if tr.started {
		t.Error("reset should clear started flag")
	}
	if tr.base != 0 || tr.highest != 0 {
		t.Error("reset should zero base and highest")
	}
}

func TestArqRxTracker_WindowAdvance(t *testing.T) {
	tr := newArqRxTracker()
	tr.markReceived(0)
	// Jump far ahead — should force window advance
	farSeq := uint32(arqWinSize + 100)
	ok := tr.markReceived(farSeq)
	if !ok {
		t.Error("far-ahead seq should be accepted")
	}
	if tr.highest != farSeq {
		t.Errorf("highest = %d, want %d", tr.highest, farSeq)
	}
}

func TestArqRxTracker_NackCooldown(t *testing.T) {
	tr := newArqRxTracker()
	// Initially should be OK to send
	if !tr.canSendNack() {
		t.Error("should be able to send NACK initially")
	}
	tr.recordNackSent()
	// Immediately after should be blocked
	if tr.canSendNack() {
		t.Error("should not be able to send NACK immediately after one was sent")
	}
	// After cooldown should be OK again
	time.Sleep(arqNackCooldown + 5*time.Millisecond)
	if !tr.canSendNack() {
		t.Error("should be able to send NACK after cooldown")
	}
}

func TestArqRxTracker_Stats(t *testing.T) {
	tr := newArqRxTracker()
	tr.addNacksSent(5)
	tr.addRetxReceived(3)
	tr.addDupFiltered(1)

	ns, rr, df := tr.stats()
	if ns != 5 || rr != 3 || df != 1 {
		t.Errorf("stats = (%d,%d,%d), want (5,3,1)", ns, rr, df)
	}
}

// ─── NACK payload encode/decode tests ──────────────────────────────────────

func TestNackPayload_EncodeDecode(t *testing.T) {
	var buf [arqNackPayloadLen]byte
	encodeNackPayload(buf[:], 12345, 0xDEADBEEFCAFEBABE)

	baseSeq, bitmap, ok := decodeNackPayload(buf[:])
	if !ok {
		t.Fatal("decode should succeed")
	}
	if baseSeq != 12345 {
		t.Errorf("baseSeq = %d, want 12345", baseSeq)
	}
	if bitmap != 0xDEADBEEFCAFEBABE {
		t.Errorf("bitmap = 0x%X, want 0xDEADBEEFCAFEBABE", bitmap)
	}
}

func TestNackPayload_TooShort(t *testing.T) {
	buf := make([]byte, 5)
	_, _, ok := decodeNackPayload(buf)
	if ok {
		t.Error("decode should fail on too-short buffer")
	}
}
