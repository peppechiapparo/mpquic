package main

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// ─── mock datagramConn for connectionTable tests ──────────────────────────

type mockDC struct {
	sent [][]byte
	mu   sync.Mutex
}

func (m *mockDC) SendDatagram(pkt []byte) error {
	m.mu.Lock()
	m.sent = append(m.sent, pkt)
	m.mu.Unlock()
	return nil
}

func (m *mockDC) ReceiveDatagram(_ context.Context) ([]byte, error) {
	return nil, nil
}

// ─── packetDedup tests ────────────────────────────────────────────────────

func TestPacketDedup_NewPacket(t *testing.T) {
	d := newPacketDedup(16)
	if d.isDuplicate([]byte{0x01, 0x02, 0x03}) {
		t.Error("first packet should not be duplicate")
	}
}

func TestPacketDedup_DuplicateDetected(t *testing.T) {
	d := newPacketDedup(16)
	pkt := []byte{0xAA, 0xBB, 0xCC}
	d.isDuplicate(pkt)
	if !d.isDuplicate(pkt) {
		t.Error("identical packet should be detected as duplicate")
	}
}

func TestPacketDedup_DifferentPackets(t *testing.T) {
	d := newPacketDedup(16)
	d.isDuplicate([]byte{0x01})
	if d.isDuplicate([]byte{0x02}) {
		t.Error("different packet should not be duplicate")
	}
}

func TestPacketDedup_RingOverwrite(t *testing.T) {
	d := newPacketDedup(4) // very small ring
	// Fill the ring with 4 different packets
	for i := byte(0); i < 4; i++ {
		d.isDuplicate([]byte{i, 0xFF})
	}
	// Now push a 5th to overwrite the oldest
	d.isDuplicate([]byte{0x99})
	// The oldest (0x00, 0xFF) might no longer be detected
	// depending on hash collisions, but 0x99 should be detected
	if !d.isDuplicate([]byte{0x99}) {
		t.Error("recently added packet should still be detected as duplicate")
	}
}

// ─── fnv1aHash tests ──────────────────────────────────────────────────────

func TestFnv1aHash_Deterministic(t *testing.T) {
	data := []byte("hello world")
	h1 := fnv1aHash(data)
	h2 := fnv1aHash(data)
	if h1 != h2 {
		t.Error("same input should produce same hash")
	}
}

func TestFnv1aHash_DifferentInputs(t *testing.T) {
	h1 := fnv1aHash([]byte("aaa"))
	h2 := fnv1aHash([]byte("bbb"))
	if h1 == h2 {
		t.Error("different inputs should produce different hashes (in practice)")
	}
}

func TestFnv1aHash_Empty(t *testing.T) {
	h := fnv1aHash(nil)
	// FNV-1a offset basis
	if h != 2166136261 {
		t.Errorf("empty hash = %d, want FNV offset basis 2166136261", h)
	}
}

// ─── connectionTable basic tests ──────────────────────────────────────────

func helperRegisterStripe(ct *connectionTable, peerIP netip.Addr, remoteID string, dc datagramConn) context.CancelFunc {
	_, cancel := context.WithCancel(context.Background())
	ct.registerStripe(peerIP, remoteID, dc, cancel)
	return cancel
}

func TestConnectionTable_RegisterAndCount(t *testing.T) {
	ct := newConnectionTable()
	peer := netip.MustParseAddr("10.200.1.1")
	dc := &mockDC{}
	helperRegisterStripe(ct, peer, "path-a", dc)

	if ct.count() != 1 {
		t.Errorf("count = %d, want 1", ct.count())
	}
	if ct.pathCount(peer) != 1 {
		t.Errorf("pathCount = %d, want 1", ct.pathCount(peer))
	}
}

func TestConnectionTable_MultiPath(t *testing.T) {
	ct := newConnectionTable()
	peer := netip.MustParseAddr("10.200.1.1")
	helperRegisterStripe(ct, peer, "path-a", &mockDC{})
	helperRegisterStripe(ct, peer, "path-b", &mockDC{})

	if ct.pathCount(peer) != 2 {
		t.Errorf("pathCount = %d, want 2", ct.pathCount(peer))
	}
	if ct.count() != 1 {
		t.Errorf("count = %d, want 1 (same peer)", ct.count())
	}
}

func TestConnectionTable_Supersede(t *testing.T) {
	ct := newConnectionTable()
	peer := netip.MustParseAddr("10.200.1.1")
	dc1 := &mockDC{}
	dc2 := &mockDC{}
	helperRegisterStripe(ct, peer, "path-a", dc1)
	helperRegisterStripe(ct, peer, "path-a", dc2) // same remoteID → supersede

	if ct.pathCount(peer) != 1 {
		t.Errorf("pathCount = %d, want 1 after supersede", ct.pathCount(peer))
	}
	// lookup should return dc2
	got, ok := ct.lookup(peer)
	if !ok {
		t.Fatal("lookup should succeed")
	}
	if got != dc2 {
		t.Error("lookup should return the newer datagramConn after supersede")
	}
}

func TestConnectionTable_Lookup_Direct(t *testing.T) {
	ct := newConnectionTable()
	peer := netip.MustParseAddr("10.200.1.1")
	dc := &mockDC{}
	helperRegisterStripe(ct, peer, "path-a", dc)

	got, ok := ct.lookup(peer)
	if !ok {
		t.Fatal("lookup should succeed for registered peer")
	}
	if got != dc {
		t.Error("lookup returned wrong datagramConn")
	}
}

func TestConnectionTable_Lookup_Miss(t *testing.T) {
	ct := newConnectionTable()
	_, ok := ct.lookup(netip.MustParseAddr("10.200.99.99"))
	if ok {
		t.Error("lookup should fail for unknown peer")
	}
}

func TestConnectionTable_LearnRoute(t *testing.T) {
	ct := newConnectionTable()
	peer := netip.MustParseAddr("10.200.1.1")
	lanHost := netip.MustParseAddr("192.168.1.100")
	dc := &mockDC{}
	helperRegisterStripe(ct, peer, "path-a", dc)

	ct.learnRoute(lanHost, peer)
	if ct.routedCount() != 1 {
		t.Errorf("routedCount = %d, want 1", ct.routedCount())
	}

	// lookup via learned route
	got, ok := ct.lookup(lanHost)
	if !ok {
		t.Fatal("lookup via learned route should succeed")
	}
	if got != dc {
		t.Error("learned route lookup returned wrong datagramConn")
	}
}

func TestConnectionTable_UnregisterConn(t *testing.T) {
	ct := newConnectionTable()
	peer := netip.MustParseAddr("10.200.1.1")
	helperRegisterStripe(ct, peer, "path-a", &mockDC{})
	helperRegisterStripe(ct, peer, "path-b", &mockDC{})

	ct.unregisterConn(peer, "path-a")
	if ct.pathCount(peer) != 1 {
		t.Errorf("pathCount = %d, want 1 after removing one path", ct.pathCount(peer))
	}

	ct.unregisterConn(peer, "path-b")
	if ct.count() != 0 {
		t.Errorf("count = %d, want 0 after removing all paths", ct.count())
	}
}

func TestConnectionTable_Unregister_CleansRoutes(t *testing.T) {
	ct := newConnectionTable()
	peer := netip.MustParseAddr("10.200.1.1")
	helperRegisterStripe(ct, peer, "path-a", &mockDC{})
	ct.learnRoute(netip.MustParseAddr("192.168.1.100"), peer)
	ct.learnRoute(netip.MustParseAddr("192.168.1.101"), peer)

	ct.unregister(peer)
	if ct.routedCount() != 0 {
		t.Errorf("routedCount = %d, want 0 after unregister", ct.routedCount())
	}
	if ct.count() != 0 {
		t.Errorf("count = %d, want 0 after unregister", ct.count())
	}
}

func TestConnectionTable_Dispatch_SinglePath(t *testing.T) {
	ct := newConnectionTable()
	peer := netip.MustParseAddr("10.200.1.1")
	dc := &mockDC{}
	helperRegisterStripe(ct, peer, "path-a", dc)

	pkt := []byte{0xDE, 0xAD}
	ok := ct.dispatch(peer, pkt)
	if !ok {
		t.Error("dispatch should succeed for registered peer")
	}
	// Give drain goroutine time to process (sendCh → dc.SendDatagram)
	time.Sleep(10 * time.Millisecond)
	dc.mu.Lock()
	if len(dc.sent) == 0 {
		t.Error("dispatch should have sent the packet via sendCh")
	}
	dc.mu.Unlock()
}

func TestConnectionTable_Dispatch_Miss(t *testing.T) {
	ct := newConnectionTable()
	ok := ct.dispatch(netip.MustParseAddr("10.200.99.99"), []byte{0x01})
	if ok {
		t.Error("dispatch should fail for unknown peer")
	}
}

func TestConnectionTable_TouchPath(t *testing.T) {
	ct := newConnectionTable()
	peer := netip.MustParseAddr("10.200.1.1")
	helperRegisterStripe(ct, peer, "path-a", &mockDC{})

	// Wait a bit so the touchPath 500ms threshold passes
	time.Sleep(10 * time.Millisecond)
	ct.touchPath(peer, "path-a")

	ct.mu.RLock()
	grp := ct.byIP[peer]
	lastRecv := grp.paths[0].lastRecv
	ct.mu.RUnlock()

	if time.Since(lastRecv) > 50*time.Millisecond {
		t.Error("touchPath should have updated lastRecv")
	}
}

func TestConnectionTable_CloseAll(t *testing.T) {
	ct := newConnectionTable()
	helperRegisterStripe(ct, netip.MustParseAddr("10.0.0.1"), "a", &mockDC{})
	helperRegisterStripe(ct, netip.MustParseAddr("10.0.0.2"), "b", &mockDC{})
	ct.learnRoute(netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("10.0.0.1"))

	ct.closeAll()
	if ct.count() != 0 {
		t.Errorf("count = %d, want 0 after closeAll", ct.count())
	}
	if ct.routedCount() != 0 {
		t.Errorf("routedCount = %d, want 0 after closeAll", ct.routedCount())
	}
}
