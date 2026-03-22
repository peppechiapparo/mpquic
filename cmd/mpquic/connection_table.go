package main

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

// connectionTable maps TUN peer IPs to their QUIC connection for multi-conn server.
// When a packet is read from the shared TUN, the dst IP is looked up to find the
// right QUIC connection for the return path.
//
// In addition to the primary peerIP (the TUN address), the table also learns
// "routed" source IPs that clients forward through the tunnel (e.g. LAN hosts
// behind the client). This allows return traffic to be dispatched to the correct
// QUIC connection even when the dst IP in the reply packet is not the peer's
// TUN address but a LAN host behind it.
//
// Multi-path support: a single peerIP may have multiple QUIC connections
// (one per WAN path). The table aggregates them in a connGroup and the
// lookup method round-robins across alive paths for the return direction.
type connectionTable struct {
	mu      sync.RWMutex
	byIP    map[netip.Addr]*connGroup  // primary: peerIP → group of paths
	routed  map[netip.Addr]netip.Addr  // learned: srcIP → peerIP (reverse map)
	dedup   *packetDedup               // optional: de-duplicate packets from multi-path clients
}

// pathConn represents a single QUIC connection (path) within a connGroup.
// Each pathConn has a sendCh for non-blocking async sends from the TUN reader.
// A dedicated drain goroutine reads from sendCh and calls dc.SendDatagram().
// This prevents one path's congestion window from blocking the TUN reader
// and starving other paths (critical for multi-pipe where N pipes share a link).
type pathConn struct {
	quicConn     quic.Connection
	dc           datagramConn
	cancel       context.CancelFunc
	remoteAddr   string        // conn.RemoteAddr().String() — unique per path
	lastRecv     time.Time     // last time data was received from this path
	sendCh       chan []byte    // buffered async send queue (TUN→QUIC)
	sendDone     chan struct{}  // closed when the drain goroutine exits
	dispatchHit  uint64        // atomic: packets successfully queued via dispatch
	dispatchDrop uint64        // atomic: packets dropped (sendCh full)
	fecCapable   bool          // true for stripe paths (FEC handles reordering)
}

// connGroup holds all QUIC connections from the same peer (same TUN IP).
// For single-path clients this has exactly one entry; for multi-path clients
// it has one entry per WAN path.
type connGroup struct {
	peerIP     netip.Addr
	paths      []*pathConn
	rr         int  // round-robin index for send distribution
	allFEC     bool // cached: true when all paths are fecCapable
	// flowPaths assigns each TCP/UDP flow (by hash) to a specific active
	// path index via round-robin. All packets in the same flow go through
	// the same path (prevents TCP reordering) but different flows are spread
	// evenly across paths (prevents load imbalance from hash collisions).
	//
	// Generational GC: flowPaths is periodically swept by flowGCTick().
	// On each tick, flowPrev replaces flowPaths and a new empty map is
	// allocated. Active flows re-register on first packet (promoted from
	// flowPrev). After two ticks without traffic, a flow entry is evicted.
	// This prevents unbounded growth of stale flow entries (bug: flow_count
	// growing indefinitely even without traffic).
	flowPaths  map[uint32]int
	flowPrev   map[uint32]int // previous generation for promotion
	flowRR     int  // round-robin for new flow assignment
}

// flowHash extracts a lightweight hash from an IP packet's 5-tuple
// (src IP, dst IP, protocol, src port, dst port) so that packets belonging
// to the same TCP/UDP flow consistently map to the same path index.
// Returns (hash, true) for parseable IPv4 TCP/UDP packets, (0, false) otherwise.
func flowHash(pkt []byte) (uint32, bool) {
	if len(pkt) < 20 {
		return 0, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	proto := pkt[9]
	if ihl < 20 || len(pkt) < ihl+4 {
		return 0, false
	}
	// Only hash TCP (6) and UDP (17) — other protocols fall back to round-robin
	if proto != 6 && proto != 17 {
		return 0, false
	}
	// FNV-1a-inspired hash of: srcIP(4) + dstIP(4) + proto(1) + srcPort(2) + dstPort(2)
	h := uint32(2166136261)
	for _, b := range pkt[12:20] { // src IP + dst IP
		h ^= uint32(b)
		h *= 16777619
	}
	h ^= uint32(proto)
	h *= 16777619
	for _, b := range pkt[ihl : ihl+4] { // src port + dst port
		h ^= uint32(b)
		h *= 16777619
	}
	return h, true
}

func newConnectionTable() *connectionTable {
	return &connectionTable{
		byIP:   make(map[netip.Addr]*connGroup),
		routed: make(map[netip.Addr]netip.Addr),
		dedup:  newPacketDedup(4096),
	}
}

// register adds (or replaces) a connection in the group for peerIP.
// If a connection with the same RemoteAddr already exists it is superseded.
// Otherwise the connection is appended to the group (multi-path).
// Each registered pathConn gets a sendCh and a drain goroutine for async sends.
func (ct *connectionTable) register(peerIP netip.Addr, quicConn quic.Connection, dc datagramConn, cancel context.CancelFunc) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	remote := quicConn.RemoteAddr().String()
	pc := &pathConn{
		quicConn:   quicConn,
		dc:         dc,
		cancel:     cancel,
		remoteAddr: remote,
		sendCh:     make(chan []byte, 256),
		sendDone:   make(chan struct{}),
	}
	go pc.drainSendCh()

	grp, exists := ct.byIP[peerIP]
	if !exists {
		ct.byIP[peerIP] = &connGroup{peerIP: peerIP, paths: []*pathConn{pc}, allFEC: false}
		return
	}

	// Replace existing path from same remote address (reconnect case)
	for i, old := range grp.paths {
		if old.remoteAddr == remote {
			old.stopSendCh()
			old.cancel()
			if old.quicConn != nil {
				_ = old.quicConn.CloseWithError(0, "superseded")
			}
			grp.paths[i] = pc
			ct.refreshAllFEC(grp)
			return
		}
	}

	// New path from a different remote address → append (multi-path)
	grp.paths = append(grp.paths, pc)
	ct.refreshAllFEC(grp)
}

// registerStripe adds a stripe transport connection to the group for peerIP.
// Unlike register(), it does not require a quic.Connection — only a datagramConn
// and a unique remote identifier string.
func (ct *connectionTable) registerStripe(peerIP netip.Addr, remoteID string, dc datagramConn, cancel context.CancelFunc) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	pc := &pathConn{
		dc:         dc,
		cancel:     cancel,
		remoteAddr: remoteID,
		sendCh:     make(chan []byte, 256),
		sendDone:   make(chan struct{}),
		lastRecv:   time.Now(), // mark as active on registration
		fecCapable: true,       // stripe paths use FEC — reorder-safe
	}
	go pc.drainSendCh()

	grp, exists := ct.byIP[peerIP]
	if !exists {
		ct.byIP[peerIP] = &connGroup{peerIP: peerIP, paths: []*pathConn{pc}, allFEC: true}
		return
	}

	for i, old := range grp.paths {
		if old.remoteAddr == remoteID {
			old.stopSendCh()
			old.cancel()
			if old.quicConn != nil {
				_ = old.quicConn.CloseWithError(0, "superseded")
			}
			grp.paths[i] = pc
			ct.refreshAllFEC(grp)
			return
		}
	}
	grp.paths = append(grp.paths, pc)
	ct.refreshAllFEC(grp)
}

// refreshAllFEC recomputes grp.allFEC. Must be called with ct.mu held.
func (ct *connectionTable) refreshAllFEC(grp *connGroup) {
	grp.allFEC = true
	for _, pc := range grp.paths {
		if !pc.fecCapable {
			grp.allFEC = false
			return
		}
	}
}

// drainSendCh is the per-path goroutine that reads from sendCh and writes
// to the QUIC stream/datagram. Runs until sendCh is closed.
//
// Uses batch-drain: after receiving one packet (blocking), non-blocking drain
// any additional queued packets, then flush the TX batch via sendmmsg.
// This reduces per-packet syscall overhead by ~8× on the TX hot path.
func (pc *pathConn) drainSendCh() {
	defer close(pc.sendDone)
	batcher, hasBatch := pc.dc.(txBatcher)
	for pkt := range pc.sendCh {
		_ = pc.dc.SendDatagram(pkt)
		if !hasBatch {
			continue
		}
		// Non-blocking drain: process any additional queued packets
		drain := true
		for drain {
			select {
			case pkt2, ok := <-pc.sendCh:
				if !ok {
					batcher.FlushTxBatch()
					return
				}
				_ = pc.dc.SendDatagram(pkt2)
			default:
				drain = false
			}
		}
		batcher.FlushTxBatch()
	}
}

// stopSendCh closes the send channel and waits for the drain goroutine to exit.
func (pc *pathConn) stopSendCh() {
	if pc.sendCh != nil {
		close(pc.sendCh)
		<-pc.sendDone
	}
}

// unregisterConn removes a specific connection (by remoteAddr) from the
// group for peerIP. If the group becomes empty, the entire entry is removed.
func (ct *connectionTable) unregisterConn(peerIP netip.Addr, remoteAddr string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	grp, exists := ct.byIP[peerIP]
	if !exists {
		return
	}

	for i, pc := range grp.paths {
		if pc.remoteAddr == remoteAddr {
			pc.stopSendCh()
			grp.paths = append(grp.paths[:i], grp.paths[i+1:]...)
			ct.refreshAllFEC(grp)
			break
		}
	}

	if len(grp.paths) == 0 {
		// No more paths — remove peer and all learned routes
		for src, peer := range ct.routed {
			if peer == peerIP {
				delete(ct.routed, src)
			}
		}
		delete(ct.byIP, peerIP)
	}
}

// unregister removes ALL connections for a peerIP (full teardown).
func (ct *connectionTable) unregister(peerIP netip.Addr) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if grp, ok := ct.byIP[peerIP]; ok {
		for _, pc := range grp.paths {
			pc.stopSendCh()
			pc.cancel()
			if pc.quicConn != nil {
				_ = pc.quicConn.CloseWithError(0, "unregistered")
			}
		}
	}
	for src, peer := range ct.routed {
		if peer == peerIP {
			delete(ct.routed, src)
		}
	}
	delete(ct.byIP, peerIP)
}

// learnRoute records that srcIP was seen coming from the connection identified
// by peerIP. This is called for every packet received from a client so that
// return traffic (with dst=srcIP) can be dispatched to the right connection.
// Only records new mappings to avoid write-lock contention on every packet.
func (ct *connectionTable) learnRoute(srcIP netip.Addr, peerIP netip.Addr) {
	ct.mu.RLock()
	existing, ok := ct.routed[srcIP]
	ct.mu.RUnlock()
	if ok && existing == peerIP {
		return // already known
	}
	ct.mu.Lock()
	ct.routed[srcIP] = peerIP
	ct.mu.Unlock()
}

// touchPath updates the lastRecv timestamp for a specific path.
// Called by runServerMultiConnTunnel on every received packet.
// Uses RLock first for a cheap freshness check, upgrading to Lock only
// when the timestamp actually needs updating (reduces write contention).
func (ct *connectionTable) touchPath(peerIP netip.Addr, remoteAddr string) {
	ct.mu.RLock()
	grp, ok := ct.byIP[peerIP]
	if !ok {
		ct.mu.RUnlock()
		return
	}
	for _, pc := range grp.paths {
		if pc.remoteAddr == remoteAddr {
			// Skip if updated within last 500ms (reduces write lock contention)
			if time.Since(pc.lastRecv) < 500*time.Millisecond {
				ct.mu.RUnlock()
				return
			}
			ct.mu.RUnlock()
			ct.mu.Lock()
			pc.lastRecv = time.Now()
			ct.mu.Unlock()
			return
		}
	}
	ct.mu.RUnlock()
}

// lookup finds the best datagramConn for a destination IP.
// For multi-path peers it prefers recently-active paths (those that
// received data within 3 seconds of the most recent), avoiding stale
// paths whose client connection may have silently died.
// Among active paths it round-robins.
func (ct *connectionTable) lookup(dstIP netip.Addr) (datagramConn, bool) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	grp := ct.resolveGroup(dstIP)
	if grp == nil || len(grp.paths) == 0 {
		return nil, false
	}

	// Single path — fast path (most common)
	if len(grp.paths) == 1 {
		return grp.paths[0].dc, true
	}

	// Find the most recent lastRecv across all paths
	var newest time.Time
	for _, pc := range grp.paths {
		if pc.lastRecv.After(newest) {
			newest = pc.lastRecv
		}
	}

	// Paths with lastRecv within 3s of the newest are considered "active".
	// Stale paths (client disconnected but server hasn't timed out yet)
	// are excluded from selection.
	staleThreshold := newest.Add(-3 * time.Second)
	active := make([]int, 0, len(grp.paths))
	for i, pc := range grp.paths {
		if pc.lastRecv.After(staleThreshold) {
			active = append(active, i)
		}
	}

	// Fallback: if nothing looks active use all paths
	if len(active) == 0 {
		for i := range grp.paths {
			active = append(active, i)
		}
	}

	start := grp.rr % len(active)
	idx := active[start]
	grp.rr = (start + 1) % len(active)
	return grp.paths[idx].dc, true
}

// dispatch sends a packet to the best path for dstIP asynchronously.
// Unlike lookup+SendDatagram, this is non-blocking: the packet is pushed
// to the path's sendCh and the drain goroutine handles the actual write.
// If the send buffer is full, the packet is dropped (backpressure).
// Returns true if the packet was queued, false if no path or buffer full.
//
// Path selection strategy:
//   - FEC-capable groups (stripe): per-flow round-robin assignment.
//     Each new TCP/UDP flow is assigned to the next active path via
//     round-robin; subsequent packets in the same flow reuse that path.
//     This prevents TCP reordering (same-flow affinity) while ensuring
//     even distribution across paths (no hash collisions).
//   - QUIC groups: flow-hash on the IP 5-tuple so that packets from the
//     same TCP/UDP connection always traverse the same path, preventing
//     reordering that would cripple TCP throughput.
func (ct *connectionTable) dispatch(dstIP netip.Addr, pkt []byte) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	grp := ct.resolveGroup(dstIP)
	if grp == nil || len(grp.paths) == 0 {
		return false
	}

	// Single path — fast path
	if len(grp.paths) == 1 {
		select {
		case grp.paths[0].sendCh <- pkt:
			return true
		default:
			return false // buffer full, drop
		}
	}

	// Multi-path: find active paths
	var newest time.Time
	for _, pc := range grp.paths {
		if pc.lastRecv.After(newest) {
			newest = pc.lastRecv
		}
	}
	staleThreshold := newest.Add(-3 * time.Second)
	var activeArr [8]int // stack-allocated — avoids heap escape on hot path
	active := activeArr[:0]
	for i, pc := range grp.paths {
		if pc.lastRecv.After(staleThreshold) {
			active = append(active, i)
		}
	}
	if len(active) == 0 {
		for i := range grp.paths {
			active = append(active, i)
		}
	}

	var idx int
	if grp.allFEC {
		// FEC-capable paths: per-flow affinity with round-robin assignment.
		// Each unique flow is assigned to the next active path in order;
		// all packets in the same flow reuse that assignment.
		// This prevents TCP reordering while distributing flows evenly.
		if h, ok := flowHash(pkt); ok {
			if grp.flowPaths == nil {
				grp.flowPaths = make(map[uint32]int, 8)
			}
			assignedIdx, exists := grp.flowPaths[h]
			// Promote from previous generation if not in current
			if !exists && grp.flowPrev != nil {
				if prev, ok2 := grp.flowPrev[h]; ok2 {
					assignedIdx = prev
					exists = true
					grp.flowPaths[h] = prev // promote to current gen
				}
			}
			// Check if assigned path is still in the active set
			isActive := false
			if exists {
				for _, ai := range active {
					if ai == assignedIdx {
						isActive = true
						break
					}
				}
			}
			if !exists || !isActive {
				// New flow or stale assignment → round-robin assign
				pos := grp.flowRR % len(active)
				assignedIdx = active[pos]
				grp.flowRR = pos + 1
				grp.flowPaths[h] = assignedIdx
			}
			idx = assignedIdx
		} else {
			// Non-TCP/UDP: round-robin
			start := grp.rr % len(active)
			idx = active[start]
			grp.rr = (start + 1) % len(active)
		}
	} else {
		// QUIC paths: flow-hash for TCP reordering avoidance.
		// Falls back to round-robin for non-TCP/UDP or unparseable packets.
		if h, ok := flowHash(pkt); ok {
			idx = active[int(h)%len(active)]
		} else {
			start := grp.rr % len(active)
			idx = active[start]
			grp.rr = (start + 1) % len(active)
		}
	}

	var dispatched bool
	select {
	case grp.paths[idx].sendCh <- pkt:
		atomic.AddUint64(&grp.paths[idx].dispatchHit, 1)
		dispatched = true
	default:
		atomic.AddUint64(&grp.paths[idx].dispatchDrop, 1)
	}

	return dispatched
}

// pathCount returns the number of active path connections for a peer.
func (ct *connectionTable) pathCount(peerIP netip.Addr) int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	grp, ok := ct.byIP[peerIP]
	if !ok {
		return 0
	}
	return len(grp.paths)
}

// resolveGroup looks up a connGroup by direct IP or learned route.
// Caller must hold ct.mu (read or write).
func (ct *connectionTable) resolveGroup(dstIP netip.Addr) *connGroup {
	if grp, ok := ct.byIP[dstIP]; ok {
		return grp
	}
	if peerIP, ok := ct.routed[dstIP]; ok {
		if grp, ok := ct.byIP[peerIP]; ok {
			return grp
		}
	}
	return nil
}

func (ct *connectionTable) count() int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return len(ct.byIP)
}

func (ct *connectionTable) routedCount() int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return len(ct.routed)
}

// flowGCTick performs generational garbage collection on flowPaths for all
// connGroups. Should be called periodically (e.g. every 30s) from a timer.
// Each tick: current flowPaths → flowPrev, allocate fresh flowPaths.
// Active flows get promoted from flowPrev on their next packet (zero-cost
// in the hot path — just one extra map lookup). Flows idle for >2 ticks
// are evicted when flowPrev is overwritten.
//
// This fixes the unbounded growth of flowPaths (flow_count metric growing
// indefinitely even without traffic) that eventually caused memory pressure
// and session instability on long-running deployments.
func (ct *connectionTable) flowGCTick() {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	for _, grp := range ct.byIP {
		if len(grp.flowPaths) > 0 {
			grp.flowPrev = grp.flowPaths
			grp.flowPaths = make(map[uint32]int, min(len(grp.flowPrev), 64))
		} else {
			grp.flowPrev = nil
		}
	}
}

func (ct *connectionTable) closeAll() {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	for ip, grp := range ct.byIP {
		for _, pc := range grp.paths {
			pc.stopSendCh()
			pc.cancel()
			if pc.quicConn != nil {
				_ = pc.quicConn.CloseWithError(0, "shutdown")
			}
		}
		delete(ct.byIP, ip)
	}
	for src := range ct.routed {
		delete(ct.routed, src)
	}
}

// packetDedup tracks recently-seen packet hashes to de-duplicate packets
// received from multi-path clients using duplication mode.
// Uses a simple ring buffer of FNV-1a hashes.
type packetDedup struct {
	mu   sync.Mutex
	ring []uint32
	pos  int
	size int
}

func newPacketDedup(size int) *packetDedup {
	return &packetDedup{ring: make([]uint32, size), size: size}
}

// isDuplicate returns true if this packet was recently seen.
// It hashes the packet and checks against the ring buffer.
func (d *packetDedup) isDuplicate(pkt []byte) bool {
	h := fnv1aHash(pkt)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, existing := range d.ring {
		if existing == h {
			return true
		}
	}
	d.ring[d.pos] = h
	d.pos = (d.pos + 1) % d.size
	return false
}

// fnv1aHash computes a fast 32-bit FNV-1a hash of the packet.
func fnv1aHash(data []byte) uint32 {
	h := uint32(2166136261)
	for _, b := range data {
		h ^= uint32(b)
		h *= 16777619
	}
	return h
}
