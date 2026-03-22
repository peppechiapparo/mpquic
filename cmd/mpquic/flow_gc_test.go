package main

import (
	"context"
	"net/netip"
	"testing"
)

// TestFlowGCTick_EvictsStaleFlows verifies that flowGCTick() evicts flows
// that have been idle for 2 ticks while preserving active ones.
func TestFlowGCTick_EvictsStaleFlows(t *testing.T) {
	ct := newConnectionTable()

	// Create a fake connGroup with 2 paths
	peerIP := netip.MustParseAddr("10.200.17.1")
	_, cancel := context.WithCancel(context.Background())
	grp := &connGroup{
		peerIP: peerIP,
		paths: []*pathConn{
			{remoteAddr: "wan5", sendCh: make(chan []byte, 1), sendDone: make(chan struct{}), cancel: cancel},
			{remoteAddr: "wan6", sendCh: make(chan []byte, 1), sendDone: make(chan struct{}), cancel: cancel},
		},
		allFEC:    true,
		flowPaths: map[uint32]int{0xAAAA: 0, 0xBBBB: 1, 0xCCCC: 0},
		flowRR:    3,
	}
	ct.byIP[peerIP] = grp

	// Before GC: 3 flows
	if len(grp.flowPaths) != 3 {
		t.Fatalf("pre-GC: expected 3 flows, got %d", len(grp.flowPaths))
	}

	// Tick 1: current → prev, new empty map
	ct.flowGCTick()
	if len(grp.flowPaths) != 0 {
		t.Fatalf("tick 1: expected 0 current flows, got %d", len(grp.flowPaths))
	}
	if len(grp.flowPrev) != 3 {
		t.Fatalf("tick 1: expected 3 prev flows, got %d", len(grp.flowPrev))
	}

	// Simulate active flow 0xAAAA: promote from prev
	if prev, ok := grp.flowPrev[0xAAAA]; ok {
		grp.flowPaths[0xAAAA] = prev
	}

	// Tick 2: promoted flow survives, stale flows evicted
	ct.flowGCTick()
	if len(grp.flowPrev) != 1 {
		t.Fatalf("tick 2: expected 1 prev flow (promoted), got %d", len(grp.flowPrev))
	}
	if _, ok := grp.flowPrev[0xAAAA]; !ok {
		t.Fatal("tick 2: flow 0xAAAA should be in prev (was active)")
	}
	// 0xBBBB and 0xCCCC are now gone — evicted
	if len(grp.flowPaths) != 0 {
		t.Fatalf("tick 2: expected 0 current flows, got %d", len(grp.flowPaths))
	}

	// Tick 3: no activity → everything evicted
	ct.flowGCTick()
	if grp.flowPrev != nil {
		t.Fatalf("tick 3: expected nil prev, got %d entries", len(grp.flowPrev))
	}
}

// TestFlowGCTick_EmptyMapNoAlloc verifies that GC on empty groups
// doesn't allocate unnecessary maps.
func TestFlowGCTick_EmptyMapNoAlloc(t *testing.T) {
	ct := newConnectionTable()
	peerIP := netip.MustParseAddr("10.200.17.1")
	_, cancel := context.WithCancel(context.Background())
	grp := &connGroup{
		peerIP: peerIP,
		paths: []*pathConn{
			{remoteAddr: "wan5", sendCh: make(chan []byte, 1), sendDone: make(chan struct{}), cancel: cancel},
		},
		flowPaths: nil,
	}
	ct.byIP[peerIP] = grp

	ct.flowGCTick()
	if grp.flowPrev != nil {
		t.Fatal("GC on nil flowPaths should not allocate flowPrev")
	}
	if grp.flowPaths != nil {
		t.Fatal("GC on nil flowPaths should keep flowPaths nil")
	}
}

// TestFlowHashDeterministic verifies the same 5-tuple always produces
// the same hash (flow affinity correctness).
func TestFlowHashDeterministic(t *testing.T) {
	// Minimal IPv4 TCP packet: IHL=5 (20 bytes), proto=6
	pkt := make([]byte, 24)
	pkt[0] = 0x45               // version=4, IHL=5
	pkt[9] = 6                  // proto=TCP
	pkt[12], pkt[13] = 10, 0    // srcIP: 10.0.0.1
	pkt[14], pkt[15] = 0, 1
	pkt[16], pkt[17] = 10, 0    // dstIP: 10.0.0.2
	pkt[18], pkt[19] = 0, 2
	pkt[20], pkt[21] = 0x1F, 0x90 // srcPort: 8080
	pkt[22], pkt[23] = 0x00, 0x50 // dstPort: 80

	h1, ok1 := flowHash(pkt)
	h2, ok2 := flowHash(pkt)
	if !ok1 || !ok2 {
		t.Fatal("flowHash should succeed for valid TCP packet")
	}
	if h1 != h2 {
		t.Fatalf("flowHash not deterministic: %d != %d", h1, h2)
	}
	if h1 == 0 {
		t.Fatal("flowHash should not be zero for valid packet")
	}
}

// TestFlowHashRejectsShortPackets verifies edge cases.
func TestFlowHashRejectsShortPackets(t *testing.T) {
	tests := []struct {
		name string
		pkt  []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"too short", make([]byte, 10)},
		{"ICMP proto", func() []byte {
			p := make([]byte, 24)
			p[0] = 0x45
			p[9] = 1 // ICMP
			return p
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := flowHash(tt.pkt)
			if ok {
				t.Error("flowHash should return false for this packet")
			}
		})
	}
}
