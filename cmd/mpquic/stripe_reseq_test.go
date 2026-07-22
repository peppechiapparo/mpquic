package main

import (
	"testing"
	"time"
)

// collector records delivered packets in order (first byte = tag).
type collector struct{ got []byte }

func (c *collector) deliver(p []byte) { c.got = append(c.got, p[0]) }

func newTestReseq(window uint32, backstop time.Duration) (*reseqBuffer, *collector, *time.Time) {
	c := &collector{}
	r := newReseqBuffer(1024, window, backstop, c.deliver)
	now := time.Unix(0, 0)
	clk := &now
	r.nowFn = func() time.Time { return *clk }
	return r, c, clk
}

func p(tag byte) []byte { return []byte{tag} }

func TestReseqInOrder(t *testing.T) {
	r, c, _ := newTestReseq(64, 40*time.Millisecond)
	for i := 0; i < 5; i++ {
		r.insert(uint32(i), p(byte(i)))
	}
	if string(c.got) != string([]byte{0, 1, 2, 3, 4}) {
		t.Fatalf("in-order delivery wrong: %v", c.got)
	}
}

func TestReseqReorderWithinWindow(t *testing.T) {
	r, c, _ := newTestReseq(64, 40*time.Millisecond)
	// Arrive 0,2,1,4,3 — should be released 0,1,2,3,4.
	order := []byte{0, 2, 1, 4, 3}
	for _, s := range order {
		r.insert(uint32(s), p(s))
	}
	if string(c.got) != string([]byte{0, 1, 2, 3, 4}) {
		t.Fatalf("reordered delivery wrong: %v", c.got)
	}
}

func TestReseqWindowSkip(t *testing.T) {
	r, c, _ := newTestReseq(4, time.Hour) // small window, backstop irrelevant here
	// 0 delivered; 1 missing; then 2,3,4,5 — once highest(5)-expected(1) >= window(4), skip 1.
	r.insert(0, p(0))
	for _, s := range []byte{2, 3, 4, 5} {
		r.insert(uint32(s), p(s))
	}
	// Expected: 0 delivered immediately; 1 skipped when window exceeded; then 2,3,4,5.
	if string(c.got) != string([]byte{0, 2, 3, 4, 5}) {
		t.Fatalf("window-skip delivery wrong: %v", c.got)
	}
	if r.stats().gapSkipped == 0 {
		t.Fatalf("expected a gap skip")
	}
}

func TestReseqBackstop(t *testing.T) {
	r, c, clk := newTestReseq(1024, 40*time.Millisecond)
	r.insert(0, p(0))
	r.insert(2, p(2)) // 1 missing, buffered 2
	if len(c.got) != 1 {
		t.Fatalf("should have delivered only 0, got %v", c.got)
	}
	// Advance clock past backstop and tick → 1 declared lost, 2 released.
	*clk = clk.Add(50 * time.Millisecond)
	r.tick()
	if string(c.got) != string([]byte{0, 2}) {
		t.Fatalf("backstop delivery wrong: %v", c.got)
	}
}

func TestReseqBackstopNotYet(t *testing.T) {
	r, c, clk := newTestReseq(1024, 40*time.Millisecond)
	r.insert(0, p(0))
	r.insert(2, p(2))
	*clk = clk.Add(10 * time.Millisecond) // under backstop
	r.tick()
	if len(c.got) != 1 {
		t.Fatalf("should still hold 2, got %v", c.got)
	}
}

func TestReseqFECFillsGap(t *testing.T) {
	r, c, clk := newTestReseq(1024, 40*time.Millisecond)
	r.insert(0, p(0))
	r.insert(2, p(2)) // gap at 1
	// FEC recovers 1 before backstop.
	*clk = clk.Add(10 * time.Millisecond)
	r.insertRecovered(1, p(1))
	if string(c.got) != string([]byte{0, 1, 2}) {
		t.Fatalf("fec-fill delivery wrong: %v", c.got)
	}
	s := r.stats()
	if s.fecFilled == 0 {
		t.Fatalf("expected fec_filled counted")
	}
	if s.gapSkipped != 0 {
		t.Fatalf("no gap should have been skipped, got %d", s.gapSkipped)
	}
}

func TestReseqDuplicateAndOld(t *testing.T) {
	r, c, _ := newTestReseq(64, 40*time.Millisecond)
	r.insert(0, p(0))
	r.insert(1, p(1))
	r.insert(0, p(0)) // old (already delivered)
	r.insert(1, p(1)) // old
	r.insert(3, p(3)) // buffered
	r.insert(3, p(9)) // duplicate of buffered 3 — ignored
	r.insert(2, p(2)) // fills gap → releases 2,3
	if string(c.got) != string([]byte{0, 1, 2, 3}) {
		t.Fatalf("dup/old handling wrong: %v", c.got)
	}
	if s := r.stats(); s.dupOld < 3 {
		t.Fatalf("expected >=3 dup/old drops, got %d", s.dupOld)
	}
}

func TestReseqOverflowForceAdvance(t *testing.T) {
	r, c, _ := newTestReseq(64, time.Hour)
	// ring cap 1024. Deliver 0, then jump far beyond cap → force advance.
	r.insert(0, p(0))
	r.insert(2000, p(1)) // 2000 - 1 >= 1024 → overflow, expected forced to 2000-1024+1=977
	// 2000 should be delivered once it becomes head (it is, after force-advance).
	if len(c.got) < 2 || c.got[0] != 0 || c.got[len(c.got)-1] != 1 {
		t.Fatalf("overflow delivery wrong: %v", c.got)
	}
	if s := r.stats(); s.overflow == 0 {
		t.Fatalf("expected an overflow event")
	}
}

func TestReseqSessionReset(t *testing.T) {
	r, c, _ := newTestReseq(64, 40*time.Millisecond)
	// Advance the stream well forward.
	for i := uint32(10000); i < 10005; i++ {
		r.insert(i, p(byte(i)))
	}
	// Sender restarts sequence at 0 (re-key). Must re-anchor, not drop as "old".
	for i := uint32(0); i < 4; i++ {
		r.insert(i, p(byte(100+i)))
	}
	tail := c.got[len(c.got)-4:]
	if string(tail) != string([]byte{100, 101, 102, 103}) {
		t.Fatalf("session-reset re-anchor wrong: %v", c.got)
	}
	if r.stats().reset == 0 {
		t.Fatalf("expected a session reset to be detected")
	}
}

func TestReseqWrapAround(t *testing.T) {
	r, c, _ := newTestReseq(64, 40*time.Millisecond)
	base := uint32(0xFFFFFFFE)
	// Deliver base, base+1 (=0xFFFFFFFF), base+2 (=0, wrapped), base+3 (=1).
	r.insert(base, p(10))
	r.insert(base+3, p(13)) // buffered across the wrap
	r.insert(base+1, p(11))
	r.insert(base+2, p(12)) // fills → releases 12,13
	if string(c.got) != string([]byte{10, 11, 12, 13}) {
		t.Fatalf("wrap-around delivery wrong: %v", c.got)
	}
}
