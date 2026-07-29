package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// quietLogger returns a Logger that swallows everything (level=error and
// no Errorf log destination interception is needed for unit tests).
func quietLogger() *Logger {
	return newLogger("error")
}

// newPathState builds a multipathPathState that looks alive and is wired with
// the minimum fields the health-check / select logic needs.
func newPathState(name string, priority int) *multipathPathState {
	return &multipathPathState{
		cfg:   MultipathPathConfig{Name: name, Priority: priority, Weight: 1},
		alive: true,
		dc:    &mockDC{},
	}
}

// ─── healthCheckTickPath: hysteresis down ─────────────────────────────────

func TestHealthCheckLoopHysteresisDown(t *testing.T) {
	p := newPathState("mp1", 10)

	now := time.Now()
	nowNs := now.UnixNano()
	thresholdNs := (3 * time.Second).Nanoseconds()
	recoveryNs := (1 * time.Second).Nanoseconds()

	// Last RX was 3.1s ago — over threshold → must transition to degraded.
	atomic.StoreInt64(&p.lastRxNs, nowNs-int64(3100*time.Millisecond))

	healthCheckTickPath(p, nowNs, thresholdNs, recoveryNs, quietLogger())

	if got := atomic.LoadUint32(&p.degraded); got != 1 {
		t.Fatalf("degraded = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&p.degradedSinceNs); got != nowNs {
		t.Errorf("degradedSinceNs = %d, want %d", got, nowNs)
	}
	if got := atomic.LoadUint64(&p.degradedTotal); got != 1 {
		t.Errorf("degradedTotal = %d, want 1", got)
	}
	if got := atomic.LoadUint64(&p.failoverTotal); got != 1 {
		t.Errorf("failoverTotal = %d, want 1", got)
	}
	if got := atomic.LoadUint64(&p.failbackTotal); got != 0 {
		t.Errorf("failbackTotal = %d, want 0", got)
	}
}

// ─── healthCheckTickPath: hysteresis up ───────────────────────────────────

func TestHealthCheckLoopHysteresisUp(t *testing.T) {
	p := newPathState("mp1", 10)

	now := time.Now()
	nowNs := now.UnixNano()
	thresholdNs := (3 * time.Second).Nanoseconds()
	recoveryNs := (1 * time.Second).Nanoseconds()

	// Pre-set degraded state, started 2s ago.
	since := nowNs - int64(2*time.Second)
	atomic.StoreUint32(&p.degraded, 1)
	atomic.StoreInt64(&p.degradedSinceNs, since)
	// Fresh RX 0.5s ago → silent < recovery → must recover.
	atomic.StoreInt64(&p.lastRxNs, nowNs-int64(500*time.Millisecond))

	healthCheckTickPath(p, nowNs, thresholdNs, recoveryNs, quietLogger())

	if got := atomic.LoadUint32(&p.degraded); got != 0 {
		t.Fatalf("degraded = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&p.degradedSinceNs); got != 0 {
		t.Errorf("degradedSinceNs = %d, want 0", got)
	}
	if got := atomic.LoadUint64(&p.failbackTotal); got != 1 {
		t.Errorf("failbackTotal = %d, want 1", got)
	}
	bh := atomic.LoadInt64(&p.blackholeNs)
	if bh <= 0 {
		t.Errorf("blackholeNs = %d, want > 0", bh)
	}
	// Sanity: blackhole must equal nowNs - since (≈ 2s).
	wantBh := nowNs - since
	if bh != wantBh {
		t.Errorf("blackholeNs = %d, want %d", bh, wantBh)
	}
}

// ─── healthCheckTickPath: no transition under threshold ───────────────────

func TestHealthCheckLoopNoChangeWhenHealthy(t *testing.T) {
	p := newPathState("mp1", 10)
	nowNs := time.Now().UnixNano()
	atomic.StoreInt64(&p.lastRxNs, nowNs-int64(500*time.Millisecond))

	healthCheckTickPath(p, nowNs, (3 * time.Second).Nanoseconds(), (1 * time.Second).Nanoseconds(), quietLogger())

	if got := atomic.LoadUint32(&p.degraded); got != 0 {
		t.Errorf("degraded = %d, want 0 (silent < threshold)", got)
	}
	if got := atomic.LoadUint64(&p.failoverTotal); got != 0 {
		t.Errorf("failoverTotal = %d, want 0", got)
	}
}

// ─── healthCheckTickPath: silent path stays degraded if not recovered ─────

func TestHealthCheckLoopStaysDegradedWhenStillSilent(t *testing.T) {
	p := newPathState("mp1", 10)
	nowNs := time.Now().UnixNano()

	atomic.StoreUint32(&p.degraded, 1)
	atomic.StoreInt64(&p.degradedSinceNs, nowNs-int64(2*time.Second))
	// silent still > recovery → no flip back.
	atomic.StoreInt64(&p.lastRxNs, nowNs-int64(2*time.Second))

	healthCheckTickPath(p, nowNs, (3 * time.Second).Nanoseconds(), (1 * time.Second).Nanoseconds(), quietLogger())

	if got := atomic.LoadUint32(&p.degraded); got != 1 {
		t.Errorf("degraded = %d, want 1 (still silent)", got)
	}
	if got := atomic.LoadUint64(&p.failbackTotal); got != 0 {
		t.Errorf("failbackTotal = %d, want 0", got)
	}
}

// ─── selectBestPath: degraded path is excluded ────────────────────────────

func newTestMultipathConn(t *testing.T, paths []*multipathPathState) *multipathConn {
	t.Helper()
	cfg := &Config{MultipathPolicy: "priority"}
	return &multipathConn{
		paths:  paths,
		logger: quietLogger(),
		cfg:    cfg,
	}
}

func TestSelectBestPathExcludesDegraded(t *testing.T) {
	p0 := newPathState("mp0", 10)
	p1 := newPathState("mp1", 10)
	atomic.StoreUint32(&p0.degraded, 1)

	m := newTestMultipathConn(t, []*multipathPathState{p0, p1})

	idx, conn := m.selectBestPath(DataplaneClassPolicy{}, nil, 0, false)
	if idx != 1 {
		t.Fatalf("selectBestPath idx = %d, want 1 (p0 degraded)", idx)
	}
	if conn == nil {
		t.Fatal("selectBestPath conn = nil, want non-nil")
	}
}

// ─── selectBestPath: best-of-bad fallback ─────────────────────────────────

func TestSelectBestPathBestOfBad(t *testing.T) {
	p0 := newPathState("mp0", 10)
	p1 := newPathState("mp1", 10)
	atomic.StoreUint32(&p0.degraded, 1)
	atomic.StoreUint32(&p1.degraded, 1)

	now := time.Now()
	atomic.StoreInt64(&p0.lastRxNs, now.Add(-5*time.Second).UnixNano())
	atomic.StoreInt64(&p1.lastRxNs, now.Add(-3*time.Second).UnixNano()) // freshest

	m := newTestMultipathConn(t, []*multipathPathState{p0, p1})

	idx, _ := m.selectBestPath(DataplaneClassPolicy{}, nil, 0, false)
	if idx != 1 {
		t.Fatalf("selectBestPath idx = %d, want 1 (freshest lastRx)", idx)
	}
}

func TestSelectBestPathBestOfBad_TiebreakConsecutiveFails(t *testing.T) {
	p0 := newPathState("mp0", 10)
	p1 := newPathState("mp1", 10)
	atomic.StoreUint32(&p0.degraded, 1)
	atomic.StoreUint32(&p1.degraded, 1)

	rxNs := time.Now().Add(-3 * time.Second).UnixNano()
	atomic.StoreInt64(&p0.lastRxNs, rxNs)
	atomic.StoreInt64(&p1.lastRxNs, rxNs)

	p0.consecutiveFails = 5
	p1.consecutiveFails = 1 // should win on tie.

	m := newTestMultipathConn(t, []*multipathPathState{p0, p1})

	idx, _ := m.selectBestPath(DataplaneClassPolicy{}, nil, 0, false)
	if idx != 1 {
		t.Fatalf("selectBestPath idx = %d, want 1 (lower consecutiveFails)", idx)
	}
}

// ─── flushDegradedOnReset: reconnect path flushes blackhole accounting ────

func TestReconnectFlushesBlackhole(t *testing.T) {
	p := newPathState("mp1", 10)

	// Simulate a path that has been degraded for 2s.
	now := time.Now()
	since := now.Add(-2 * time.Second).UnixNano()
	atomic.StoreUint32(&p.degraded, 1)
	atomic.StoreInt64(&p.degradedSinceNs, since)

	resetNs := now.UnixNano()
	bh := flushDegradedOnReset(p, resetNs)

	if bh <= 0 {
		t.Fatalf("flushDegradedOnReset returned bh = %d, want > 0", bh)
	}
	if got := atomic.LoadInt64(&p.blackholeNs); got != bh {
		t.Errorf("blackholeNs = %d, want %d", got, bh)
	}
	if got := atomic.LoadUint64(&p.failbackTotal); got != 1 {
		t.Errorf("failbackTotal = %d, want 1", got)
	}
	if got := atomic.LoadUint32(&p.degraded); got != 0 {
		t.Errorf("degraded = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&p.degradedSinceNs); got != 0 {
		t.Errorf("degradedSinceNs = %d, want 0", got)
	}

	// Calling flush again on a healthy path is a no-op (no extra failback).
	bh2 := flushDegradedOnReset(p, time.Now().UnixNano())
	if bh2 != 0 {
		t.Errorf("flushDegradedOnReset on healthy path returned %d, want 0", bh2)
	}
	if got := atomic.LoadUint64(&p.failbackTotal); got != 1 {
		t.Errorf("failbackTotal = %d, want 1 (unchanged)", got)
	}
}

// ─── re-register cadence: cfg-driven ──────────────────────────────────────

func TestReregisterCadenceCfgDriven(t *testing.T) {
	tests := []struct {
		name string
		ka   time.Duration
		want int
	}{
		{"keepalive_1s", 1 * time.Second, 30},
		{"keepalive_200ms", 200 * time.Millisecond, 150},
		{"keepalive_60s_clamp", 60 * time.Second, 1}, // ratio 0.5 → clamped to 1
		{"keepalive_30s_exact", 30 * time.Second, 1},
		{"keepalive_5s_legacy", 5 * time.Second, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			every := int(stripeReregisterInterval / tt.ka)
			if every < 1 {
				every = 1
			}
			if every != tt.want {
				t.Errorf("cadence(%v) = %d, want %d", tt.ka, every, tt.want)
			}
		})
	}
}
