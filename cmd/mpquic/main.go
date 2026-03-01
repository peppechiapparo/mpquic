package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/songgao/water"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Role                  string                `yaml:"role"`
	BindIP                string                `yaml:"bind_ip"`
	RemoteAddr            string                `yaml:"remote_addr"`
	RemotePort            int                   `yaml:"remote_port"`
	MultiConnEnabled      bool                  `yaml:"multi_conn_enabled"`
	MultipathEnabled      bool                  `yaml:"multipath_enabled"`
	MultipathPolicy       string                `yaml:"multipath_policy"`
	DataplaneConfigFile   string                `yaml:"dataplane_config_file"`
	Dataplane             DataplaneConfig       `yaml:"dataplane"`
	MultipathPaths        []MultipathPathConfig `yaml:"multipath_paths"`
	TunName               string                `yaml:"tun_name"`
	TunCIDR               string                `yaml:"tun_cidr"`
	LogLevel              string                `yaml:"log_level"`
	TLSCertFile           string                `yaml:"tls_cert_file"`
	TLSKeyFile            string                `yaml:"tls_key_file"`
	TLSCAFile             string                `yaml:"tls_ca_file"`
	TLSServerName         string                `yaml:"tls_server_name"`
	TLSInsecureSkipVerify bool                  `yaml:"tls_insecure_skip_verify"`
	ControlAPIListen      string                `yaml:"control_api_listen"`
	ControlAPIAuthToken   string                `yaml:"control_api_auth_token"`
	CongestionAlgorithm   string                `yaml:"congestion_algorithm"`
	TransportMode         string                `yaml:"transport_mode"`
	DetectStarlink        bool                  `yaml:"detect_starlink"`
	StarlinkDefaultPipes  int                   `yaml:"starlink_default_pipes"`
	StarlinkTransport     string                `yaml:"starlink_transport"`
	StripePort            int                   `yaml:"stripe_port"`
	StripeDataShards      int                   `yaml:"stripe_data_shards"`
	StripeParityShards    int                   `yaml:"stripe_parity_shards"`
	StripeEnabled         bool                  `yaml:"stripe_enabled"`
	StripeAuthKey         string                `yaml:"stripe_auth_key"`
}

type MultipathPathConfig struct {
	Name       string `yaml:"name"`
	BindIP     string `yaml:"bind_ip"`
	RemoteAddr string `yaml:"remote_addr"`
	RemotePort int    `yaml:"remote_port"`
	Priority   int    `yaml:"priority"`
	Weight     int    `yaml:"weight"`
	Pipes      int    `yaml:"pipes"`
	BasePath   string `yaml:"-"`        // original path name before pipe expansion
	Transport  string `yaml:"transport"` // "quic" (default), "stripe", or "auto"
}

type DataplaneConfig struct {
	DefaultClass string                          `yaml:"default_class"`
	Classes      map[string]DataplaneClassPolicy `yaml:"classes"`
	Classifiers  []DataplaneClassifierRule       `yaml:"classifiers"`
}

type DataplaneClassPolicy struct {
	SchedulerPolicy string   `yaml:"scheduler_policy"`
	PreferredPaths  []string `yaml:"preferred_paths"`
	ExcludedPaths   []string `yaml:"excluded_paths"`
	Duplicate       bool     `yaml:"duplicate"`
	DuplicateCopies int      `yaml:"duplicate_copies"`
}

type DataplaneClassifierRule struct {
	Name      string   `yaml:"name"`
	ClassName string   `yaml:"class"`
	Protocol  string   `yaml:"protocol"`
	SrcCIDRs  []string `yaml:"src_cidrs"`
	DstCIDRs  []string `yaml:"dst_cidrs"`
	SrcPorts  []string `yaml:"src_ports"`
	DstPorts  []string `yaml:"dst_ports"`
	DSCP      []int    `yaml:"dscp"`
}

type compiledDataplane struct {
	defaultClass string
	classes      map[string]DataplaneClassPolicy
	classifiers  []compiledClassifierRule
}

type compiledClassifierRule struct {
	name      string
	className string
	protocol  string
	srcCIDRs  []netip.Prefix
	dstCIDRs  []netip.Prefix
	srcPorts  []portRange
	dstPorts  []portRange
	dscp      map[uint8]struct{}
}

type portRange struct {
	from uint16
	to   uint16
}

type packetMeta struct {
	protocol string
	srcAddr  netip.Addr
	dstAddr  netip.Addr
	srcPort  uint16
	dstPort  uint16
	hasPorts bool
	dscp     uint8
}

type trafficClassCounters struct {
	txPackets    uint64
	txErrors     uint64
	txDuplicates uint64
}

type multipathPathState struct {
	cfg              MultipathPathConfig
	udpConn          *net.UDPConn
	transport        *quic.Transport
	conn             quic.Connection
	dc               datagramConn
	stripeConn       *stripeClientConn   // non-nil for stripe transport paths
	alive            bool
	reconnecting     bool
	consecutiveFails int
	cooldownUntil    time.Time
	txPackets        uint64
	rxPackets        uint64
	txErrors         uint64
	rxErrors         uint64
	lastUp           time.Time
	lastDown         time.Time
}

type multipathConn struct {
	mu      sync.RWMutex
	paths   []*multipathPathState
	recvCh  chan []byte
	errCh   chan error
	errOnce sync.Once
	rr      int
	logger  *Logger
	cfg     *Config
	dataplane compiledDataplane
	classTx   map[string]*trafficClassCounters
	baseCtx context.Context
}

type datagramConn interface {
	SendDatagram([]byte) error
	ReceiveDatagram(context.Context) ([]byte, error)
}

// streamConn wraps a single bidirectional QUIC stream to provide reliable,
// ordered delivery with 2-byte length-prefixed framing.
// This allows the congestion control algorithm (BBR vs Cubic) to drive
// retransmissions, unlike QUIC DATAGRAM frames which are unreliable.
type streamConn struct {
	stream  quic.Stream
	writeMu sync.Mutex
}

func (s *streamConn) SendDatagram(pkt []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(pkt)))
	if _, err := s.stream.Write(hdr[:]); err != nil {
		return err
	}
	_, err := s.stream.Write(pkt)
	return err
}

func (s *streamConn) ReceiveDatagram(_ context.Context) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(s.stream, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint16(hdr[:])
	pkt := make([]byte, length)
	if _, err := io.ReadFull(s.stream, pkt); err != nil {
		return nil, err
	}
	return pkt, nil
}

func openStreamConn(ctx context.Context, conn quic.Connection) (*streamConn, error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}
	return &streamConn{stream: stream}, nil
}

func acceptStreamConn(ctx context.Context, conn quic.Connection) (*streamConn, error) {
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept stream: %w", err)
	}
	return &streamConn{stream: stream}, nil
}

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
}

// connGroup holds all QUIC connections from the same peer (same TUN IP).
// For single-path clients this has exactly one entry; for multi-path clients
// it has one entry per WAN path.
type connGroup struct {
	peerIP netip.Addr
	paths  []*pathConn
	rr     int // round-robin index for send distribution
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
		ct.byIP[peerIP] = &connGroup{peerIP: peerIP, paths: []*pathConn{pc}}
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
			return
		}
	}

	// New path from a different remote address → append (multi-path)
	grp.paths = append(grp.paths, pc)
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
	}
	go pc.drainSendCh()

	grp, exists := ct.byIP[peerIP]
	if !exists {
		ct.byIP[peerIP] = &connGroup{peerIP: peerIP, paths: []*pathConn{pc}}
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
			return
		}
	}
	grp.paths = append(grp.paths, pc)
}

// drainSendCh is the per-path goroutine that reads from sendCh and writes
// to the QUIC stream/datagram. Runs until sendCh is closed.
func (pc *pathConn) drainSendCh() {
	defer close(pc.sendDone)
	for pkt := range pc.sendCh {
		_ = pc.dc.SendDatagram(pkt)
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
// For multi-path groups, uses flow-based hashing on the IP 5-tuple so that
// packets from the same TCP/UDP connection always traverse the same path,
// preventing reordering that would cripple TCP throughput.
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
	active := make([]int, 0, len(grp.paths))
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

	// Flow-based hash: same 5-tuple → same path (prevents TCP reordering).
	// Falls back to round-robin for non-TCP/UDP or unparseable packets.
	var idx int
	if h, ok := flowHash(pkt); ok {
		idx = active[int(h)%len(active)]
	} else {
		start := grp.rr % len(active)
		idx = active[start]
		grp.rr = (start + 1) % len(active)
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

type Logger struct {
	level int
}

const (
	levelDebug = 10
	levelInfo  = 20
	levelError = 30
)

func newLogger(level string) *Logger {
	switch strings.ToLower(level) {
	case "debug":
		return &Logger{level: levelDebug}
	case "error":
		return &Logger{level: levelError}
	default:
		return &Logger{level: levelInfo}
	}
}

func (l *Logger) Debugf(format string, args ...any) {
	if l.level <= levelDebug {
		log.Printf("DEBUG "+format, args...)
	}
}

func (l *Logger) Infof(format string, args ...any) {
	if l.level <= levelInfo {
		log.Printf("INFO "+format, args...)
	}
}

func (l *Logger) Errorf(format string, args ...any) {
	if l.level <= levelError {
		log.Printf("ERROR "+format, args...)
	}
}

func main() {
	cfgPath := flag.String("config", "", "path to YAML config")
	flag.Parse()
	if *cfgPath == "" {
		log.Fatal("--config is required")
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	logger := newLogger(cfg.LogLevel)

	// Default congestion algorithm to "cubic" and normalize
	cfg.CongestionAlgorithm = strings.ToLower(strings.TrimSpace(cfg.CongestionAlgorithm))
	if cfg.CongestionAlgorithm == "" {
		cfg.CongestionAlgorithm = "cubic"
	}
	cfg.TransportMode = strings.ToLower(strings.TrimSpace(cfg.TransportMode))
	if cfg.TransportMode == "" {
		cfg.TransportMode = "datagram"
	}
	logger.Infof("congestion_algorithm=%s transport_mode=%s", cfg.CongestionAlgorithm, cfg.TransportMode)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Log shutdown initiation and enforce hard deadline
	go func() {
		<-ctx.Done()
		logger.Infof("shutdown signal received, stopping...")
		time.AfterFunc(10*time.Second, func() {
			logger.Errorf("shutdown deadline exceeded, forcing exit")
			os.Exit(1)
		})
	}()

	if cfg.Role == "server" {
		err = runServer(ctx, cfg, logger)
	} else {
		err = runClientLoop(ctx, cfg, logger)
	}
	logger.Infof("clean exit")
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("fatal: %v", err)
	}
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	cfg.Role = strings.ToLower(strings.TrimSpace(cfg.Role))
	cfg.LogLevel = strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	cfg.ControlAPIListen = strings.TrimSpace(cfg.ControlAPIListen)
	cfg.ControlAPIAuthToken = strings.TrimSpace(cfg.ControlAPIAuthToken)
	cfg.StripeAuthKey = strings.TrimSpace(cfg.StripeAuthKey)
	if cfg.Role != "client" && cfg.Role != "server" {
		return nil, fmt.Errorf("role must be client or server")
	}
	if cfg.BindIP == "" && !(cfg.Role == "client" && cfg.MultipathEnabled) {
		return nil, fmt.Errorf("bind_ip required")
	}
	if !(cfg.Role == "client" && cfg.MultipathEnabled) {
		if cfg.RemotePort <= 0 || cfg.RemotePort > 65535 {
			return nil, fmt.Errorf("remote_port invalid")
		}
	}
	if cfg.Role == "client" && cfg.MultipathEnabled {
		cfg.MultipathPolicy = strings.ToLower(strings.TrimSpace(cfg.MultipathPolicy))
		if cfg.MultipathPolicy == "" {
			cfg.MultipathPolicy = "priority"
		}
		if cfg.MultipathPolicy != "priority" && cfg.MultipathPolicy != "failover" && cfg.MultipathPolicy != "balanced" {
			return nil, fmt.Errorf("multipath_policy must be one of: priority, failover, balanced")
		}
		if len(cfg.MultipathPaths) == 0 {
			return nil, fmt.Errorf("multipath_paths required when multipath_enabled=true")
		}
		for i := range cfg.MultipathPaths {
			p := &cfg.MultipathPaths[i]
			if p.Name == "" {
				p.Name = fmt.Sprintf("path%d", i+1)
			}
			if p.BindIP == "" {
				return nil, fmt.Errorf("multipath_paths[%d].bind_ip required", i)
			}
			if p.RemoteAddr == "" {
				return nil, fmt.Errorf("multipath_paths[%d].remote_addr required", i)
			}
			if p.RemotePort <= 0 || p.RemotePort > 65535 {
				return nil, fmt.Errorf("multipath_paths[%d].remote_port invalid", i)
			}
			if p.Weight <= 0 {
				p.Weight = 1
			}
		}
		if err := loadAndValidateDataplaneConfig(path, cfg); err != nil {
			return nil, err
		}
	}
	if cfg.TunName == "" {
		return nil, fmt.Errorf("tun_name required")
	}
	if cfg.TunCIDR == "" {
		return nil, fmt.Errorf("tun_cidr required")
	}
	if cfg.Role == "client" && !cfg.MultipathEnabled && cfg.RemoteAddr == "" {
		return nil, fmt.Errorf("remote_addr required for client")
	}
	if cfg.Role == "server" {
		if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			return nil, fmt.Errorf("tls_cert_file and tls_key_file required for server")
		}
	}
	if cfg.Role == "client" {
		if !cfg.TLSInsecureSkipVerify && cfg.TLSCAFile == "" {
			return nil, fmt.Errorf("tls_ca_file required for client when tls_insecure_skip_verify=false")
		}
		if cfg.TLSServerName == "" {
			cfg.TLSServerName = "mpquic-server"
		}
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return cfg, nil
}

func loadAndValidateDataplaneConfig(configPath string, cfg *Config) error {
	dp := cfg.Dataplane

	if cfg.DataplaneConfigFile != "" {
		dpPath := cfg.DataplaneConfigFile
		if !filepath.IsAbs(dpPath) {
			dpPath = filepath.Join(filepath.Dir(configPath), dpPath)
		}
		cfg.DataplaneConfigFile = dpPath
		b, err := os.ReadFile(dpPath)
		if err != nil {
			return fmt.Errorf("dataplane_config_file read failed: %w", err)
		}
		fileDP := DataplaneConfig{}
		if err := yaml.Unmarshal(b, &fileDP); err != nil {
			return fmt.Errorf("dataplane_config_file parse failed: %w", err)
		}
		dp = mergeDataplaneConfig(dp, fileDP)
	}

	normalizeDataplaneConfig(&dp, cfg.MultipathPolicy)
	if err := validateDataplaneConfig(dp, cfg.MultipathPaths); err != nil {
		return err
	}
	cfg.Dataplane = dp
	return nil
}

func mergeDataplaneConfig(base DataplaneConfig, override DataplaneConfig) DataplaneConfig {
	out := base
	if strings.TrimSpace(override.DefaultClass) != "" {
		out.DefaultClass = override.DefaultClass
	}
	if len(override.Classes) > 0 {
		if out.Classes == nil {
			out.Classes = make(map[string]DataplaneClassPolicy, len(override.Classes))
		}
		for className, policy := range override.Classes {
			out.Classes[className] = policy
		}
	}
	if len(override.Classifiers) > 0 {
		out.Classifiers = override.Classifiers
	}
	return out
}

func normalizeDataplaneConfig(dp *DataplaneConfig, fallbackPolicy string) {
	if fallbackPolicy == "" {
		fallbackPolicy = "priority"
	}
	dp.DefaultClass = strings.ToLower(strings.TrimSpace(dp.DefaultClass))
	if dp.DefaultClass == "" {
		dp.DefaultClass = "default"
	}

	if dp.Classes == nil {
		dp.Classes = map[string]DataplaneClassPolicy{}
	}
	if len(dp.Classes) == 0 {
		dp.Classes[dp.DefaultClass] = DataplaneClassPolicy{SchedulerPolicy: fallbackPolicy}
	}

	normalizedClasses := make(map[string]DataplaneClassPolicy, len(dp.Classes))
	for className, policy := range dp.Classes {
		n := strings.ToLower(strings.TrimSpace(className))
		if n == "" {
			continue
		}
		policy.SchedulerPolicy = strings.ToLower(strings.TrimSpace(policy.SchedulerPolicy))
		if policy.SchedulerPolicy == "" {
			policy.SchedulerPolicy = fallbackPolicy
		}
		if policy.Duplicate && policy.DuplicateCopies < 2 {
			policy.DuplicateCopies = 2
		}
		if policy.DuplicateCopies > 3 {
			policy.DuplicateCopies = 3
		}
		normalizedClasses[n] = policy
	}
	dp.Classes = normalizedClasses

	for i := range dp.Classifiers {
		r := &dp.Classifiers[i]
		r.Name = strings.TrimSpace(r.Name)
		r.ClassName = strings.ToLower(strings.TrimSpace(r.ClassName))
		r.Protocol = strings.ToLower(strings.TrimSpace(r.Protocol))
	}
}

func validateDataplaneConfig(dp DataplaneConfig, paths []MultipathPathConfig) error {
	if len(dp.Classes) == 0 {
		return fmt.Errorf("dataplane.classes must not be empty")
	}
	if _, ok := dp.Classes[dp.DefaultClass]; !ok {
		return fmt.Errorf("dataplane.default_class=%q not found in dataplane.classes", dp.DefaultClass)
	}

	pathSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		pathSet[p.Name] = struct{}{}
		// Also accept base path names (before pipe expansion)
		if p.BasePath != "" {
			pathSet[p.BasePath] = struct{}{}
		}
	}

	for className, policy := range dp.Classes {
		if policy.SchedulerPolicy != "priority" && policy.SchedulerPolicy != "failover" && policy.SchedulerPolicy != "balanced" {
			return fmt.Errorf("dataplane.classes[%s].scheduler_policy must be one of: priority, failover, balanced", className)
		}
		for _, name := range policy.PreferredPaths {
			if _, ok := pathSet[name]; !ok {
				return fmt.Errorf("dataplane.classes[%s].preferred_paths references unknown path: %s", className, name)
			}
		}
		for _, name := range policy.ExcludedPaths {
			if _, ok := pathSet[name]; !ok {
				return fmt.Errorf("dataplane.classes[%s].excluded_paths references unknown path: %s", className, name)
			}
		}
	}

	for i, rule := range dp.Classifiers {
		if rule.ClassName == "" {
			return fmt.Errorf("dataplane.classifiers[%d].class required", i)
		}
		if _, ok := dp.Classes[rule.ClassName]; !ok {
			return fmt.Errorf("dataplane.classifiers[%d].class references unknown class: %s", i, rule.ClassName)
		}
		if rule.Protocol != "" && rule.Protocol != "udp" && rule.Protocol != "tcp" && rule.Protocol != "icmp" && rule.Protocol != "icmpv6" {
			return fmt.Errorf("dataplane.classifiers[%d].protocol invalid: %s", i, rule.Protocol)
		}
		if _, err := parseCIDRs(rule.SrcCIDRs); err != nil {
			return fmt.Errorf("dataplane.classifiers[%d].src_cidrs invalid: %w", i, err)
		}
		if _, err := parseCIDRs(rule.DstCIDRs); err != nil {
			return fmt.Errorf("dataplane.classifiers[%d].dst_cidrs invalid: %w", i, err)
		}
		if _, err := parsePortRanges(rule.SrcPorts); err != nil {
			return fmt.Errorf("dataplane.classifiers[%d].src_ports invalid: %w", i, err)
		}
		if _, err := parsePortRanges(rule.DstPorts); err != nil {
			return fmt.Errorf("dataplane.classifiers[%d].dst_ports invalid: %w", i, err)
		}
		for _, dscp := range rule.DSCP {
			if dscp < 0 || dscp > 63 {
				return fmt.Errorf("dataplane.classifiers[%d].dscp value out of range: %d", i, dscp)
			}
		}
	}

	return nil
}

func runServer(ctx context.Context, cfg *Config, logger *Logger) error {
	if cfg.MultiConnEnabled {
		return runServerMultiConn(ctx, cfg, logger)
	}
	return runServerSingleConn(ctx, cfg, logger)
}

// runServerMultiConn accepts N concurrent QUIC connections on the same port,
// all sharing a single TUN device. Each client registers its TUN peer IP by
// sending it as the first datagram. The TUN reader dispatches return packets
// to the correct connection by inspecting the destination IP.
func runServerMultiConn(ctx context.Context, cfg *Config, logger *Logger) error {
	bindIP, err := resolveBindIP(cfg.BindIP)
	if err != nil {
		return err
	}

	tun, err := water.New(water.Config{DeviceType: water.TUN, PlatformSpecificParams: water.PlatformSpecificParams{Name: cfg.TunName}})
	if err != nil {
		return err
	}
	defer tun.Close()

	ct := newConnectionTable()
	defer ct.closeAll()

	// Single TUN reader dispatches packets to the right connection via dst IP.
	go func() {
		buf := make([]byte, 65535)
		for {
			n, readErr := tun.Read(buf)
			if readErr != nil {
				logger.Errorf("tun read error: %v", readErr)
				return
			}
			pkt := buf[:n]
			if len(pkt) < 20 {
				continue
			}

			// Extract dst IP from IPv4 header (bytes 16-19)
			version := pkt[0] >> 4
			var dstIP netip.Addr
			if version == 4 && len(pkt) >= 20 {
				dstIP = netip.AddrFrom4([4]byte{pkt[16], pkt[17], pkt[18], pkt[19]})
			} else if version == 6 && len(pkt) >= 40 {
				var b [16]byte
				copy(b[:], pkt[24:40])
				dstIP = netip.AddrFrom16(b)
			} else {
				continue
			}

			pktCopy := append([]byte(nil), pkt...)
			if !ct.dispatch(dstIP, pktCopy) {
				logger.Debugf("dispatch failed for dst=%s (no path or buffer full)", dstIP)
			}
		}
	}()

	tlsConf, err := loadServerTLSConfig(cfg)
	if err != nil {
		return err
	}

	// Start stripe listener if enabled (for Starlink session bypass clients)
	if cfg.StripeEnabled {
		ss, err := newStripeServer(cfg, tun, ct, logger)
		if err != nil {
			logger.Errorf("stripe server init failed: %v (continuing with QUIC only)", err)
		} else {
			defer ss.Close()
			go ss.Run(ctx)
		}
	}

	listenAddr := net.JoinHostPort(bindIP, fmt.Sprintf("%d", cfg.RemotePort))
	logger.Infof("server multi-conn listen=%s tun=%s", listenAddr, cfg.TunName)
	listener, err := quic.ListenAddr(listenAddr, tlsConf, &quic.Config{
		EnableDatagrams:     true,
		KeepAlivePeriod:     15 * time.Second,
		MaxIdleTimeout:      60 * time.Second,
		CongestionAlgorithm: cfg.CongestionAlgorithm,
	})
	if err != nil {
		return err
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		logger.Infof("multi-conn accepted remote=%s", conn.RemoteAddr())

		go func(c quic.Connection) {
			if err := runServerMultiConnTunnel(ctx, c, tun, ct, cfg, logger); err != nil && !errors.Is(err, context.Canceled) {
				logger.Errorf("multi-conn tunnel closed: %v", err)
			}
		}(conn)
	}
}

// runServerMultiConnTunnel handles a single QUIC connection in multi-conn mode.
// First datagram received is expected to be a 4-byte registration containing the
// client's TUN IP. After registration, all received datagrams are written to TUN.
// Return path is handled by the shared TUN reader via connectionTable.
//
// Multi-path aware: multiple connections from the same peer IP are grouped in
// the connectionTable. When this goroutine exits, only this specific connection
// is removed from the group (not the entire peer entry).
func runServerMultiConnTunnel(parentCtx context.Context, conn quic.Connection, tun *water.Interface, ct *connectionTable, cfg *Config, logger *Logger) error {
	connCtx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	remoteAddr := conn.RemoteAddr().String()

	// Wrap connection based on transport mode
	var dc datagramConn
	if cfg.TransportMode == "reliable" {
		sc, err := acceptStreamConn(connCtx, conn)
		if err != nil {
			return fmt.Errorf("accept stream: %w", err)
		}
		dc = sc
	} else {
		dc = conn
	}

	// Wait for registration: first message = 4-byte IPv4 peer IP
	var peerIP netip.Addr
	registered := false

	// On exit, remove this specific connection from the group
	defer func() {
		if registered {
			ct.unregisterConn(peerIP, remoteAddr)
			logger.Infof("multi-conn unregistered peer=%s remote=%s remaining_paths=%d",
				peerIP, remoteAddr, ct.pathCount(peerIP))
		}
	}()

	for {
		pkt, err := dc.ReceiveDatagram(connCtx)
		if err != nil {
			return err
		}

		if !registered {
			// Registration: first datagram is a 4-byte IPv4 address
			if len(pkt) == 4 {
				peerIP = netip.AddrFrom4([4]byte{pkt[0], pkt[1], pkt[2], pkt[3]})
				ct.register(peerIP, conn, dc, cancel)
				registered = true
				logger.Infof("multi-conn registered peer=%s remote=%s paths=%d",
					peerIP, remoteAddr, ct.pathCount(peerIP))
				continue
			}
			// Not a registration packet, try to auto-detect from IP header
			if len(pkt) >= 20 {
				version := pkt[0] >> 4
				if version == 4 {
					peerIP = netip.AddrFrom4([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]})
					ct.register(peerIP, conn, dc, cancel)
					registered = true
					logger.Infof("multi-conn auto-registered peer=%s remote=%s paths=%d (from packet src)",
						peerIP, remoteAddr, ct.pathCount(peerIP))
					// Fall through to write this packet to TUN
				}
			}
		}

		if !registered {
			logger.Debugf("dropping pre-registration packet len=%d", len(pkt))
			continue
		}

		// Update last-received timestamp so lookup() prefers active paths.
		ct.touchPath(peerIP, remoteAddr)

		// De-duplicate: if a multi-path client sends the same packet via
		// multiple paths (duplication mode), skip writing it to TUN twice.
		if ct.pathCount(peerIP) > 1 && ct.dedup.isDuplicate(pkt) {
			logger.Debugf("dedup: skipping duplicate packet from peer=%s remote=%s len=%d", peerIP, remoteAddr, len(pkt))
			continue
		}

		// Learn source IP for return-path routing: if the client
		// forwards traffic from LAN hosts (src != peerIP), we record
		// src→peerIP so the TUN reader can dispatch replies.
		if len(pkt) >= 20 {
			version := pkt[0] >> 4
			if version == 4 {
				srcIP := netip.AddrFrom4([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]})
				if srcIP != peerIP {
					ct.learnRoute(srcIP, peerIP)
				}
			}
		}

		if _, err := tun.Write(pkt); err != nil {
			return err
		}
		logger.Debugf("RX %d bytes from peer=%s via %s", len(pkt), peerIP, remoteAddr)
	}
}

// runServerSingleConn is the original single-connection server mode.
// Only one active client at a time; new connections supersede old ones.
func runServerSingleConn(ctx context.Context, cfg *Config, logger *Logger) error {
	bindIP, err := resolveBindIP(cfg.BindIP)
	if err != nil {
		return err
	}
	tun, err := water.New(water.Config{DeviceType: water.TUN, PlatformSpecificParams: water.PlatformSpecificParams{Name: cfg.TunName}})
	if err != nil {
		return err
	}
	defer tun.Close()

	// Single TUN reader goroutine prevents goroutine leak on client reconnect.
	// Previously each runTunnelWithTUN call spawned its own tun.Read goroutine
	// that persisted after context cancellation (tun.Read is not ctx-aware),
	// causing stale goroutines to steal return-path packets from the active
	// connection. With N stale goroutines the active reader has 1/(N+1)
	// probability per packet, effectively killing VPS→client dataplane.
	tunReadCh := make(chan []byte, 64)
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := tun.Read(buf)
			if err != nil {
				logger.Errorf("tun read error: %v", err)
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			select {
			case tunReadCh <- pkt:
			default:
				// drop if channel full (no active conn or back-pressure)
			}
		}
	}()

	tlsConf, err := loadServerTLSConfig(cfg)
	if err != nil {
		return err
	}
	listenAddr := net.JoinHostPort(bindIP, fmt.Sprintf("%d", cfg.RemotePort))
	logger.Infof("server listen=%s tun=%s", listenAddr, cfg.TunName)
	listener, err := quic.ListenAddr(listenAddr, tlsConf, &quic.Config{
		EnableDatagrams:     true,
		KeepAlivePeriod:     15 * time.Second,
		MaxIdleTimeout:      60 * time.Second,
		CongestionAlgorithm: cfg.CongestionAlgorithm,
	})
	if err != nil {
		return err
	}
	defer listener.Close()

	var activeMu sync.Mutex
	var activeConn quic.Connection
	var activeCancel context.CancelFunc

	defer func() {
		activeMu.Lock()
		defer activeMu.Unlock()
		if activeCancel != nil {
			activeCancel()
		}
		if activeConn != nil {
			_ = activeConn.CloseWithError(0, "shutdown")
		}
	}()

	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		logger.Infof("accepted remote=%s", conn.RemoteAddr())

		activeMu.Lock()
		if activeCancel != nil {
			activeCancel()
		}
		if activeConn != nil {
			_ = activeConn.CloseWithError(0, "superseded")
		}
		connCtx, cancel := context.WithCancel(ctx)
		activeConn = conn
		activeCancel = cancel
		activeMu.Unlock()

		go func(c quic.Connection, cctx context.Context) {
			var dc datagramConn
			if cfg.TransportMode == "reliable" {
				sc, err := acceptStreamConn(cctx, c)
				if err != nil {
					logger.Errorf("accept stream: %v", err)
					return
				}
				dc = sc
			} else {
				dc = c
			}
			if err := runServerTunnel(cctx, dc, tun, tunReadCh, logger); err != nil && !errors.Is(err, context.Canceled) {
				logger.Errorf("tunnel closed: %v", err)
			}
		}(conn, connCtx)
	}
}

// runServerTunnel bridges a single QUIC connection to the shared TUN device.
// TX direction reads from the tunReadCh channel (fed by the single TUN reader)
// instead of calling tun.Read directly, so the goroutine exits cleanly when
// the context is cancelled — no leak, no packet theft.
func runServerTunnel(ctx context.Context, conn datagramConn, tun *water.Interface, tunReadCh <-chan []byte, logger *Logger) error {
	errCh := make(chan error, 1)

	// TX: tunReadCh → QUIC (context-aware, exits on cancel)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-tunReadCh:
				if !ok {
					return
				}
				if err := conn.SendDatagram(pkt); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				logger.Debugf("TX %d bytes", len(pkt))
			}
		}
	}()

	// RX: QUIC → TUN
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		default:
		}
		pkt, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			return err
		}
		if _, err := tun.Write(pkt); err != nil {
			return err
		}
		logger.Debugf("RX %d bytes", len(pkt))
	}
}

func runClientLoop(ctx context.Context, cfg *Config, logger *Logger) error {
	for {
		err := runClientOnce(ctx, cfg, logger)
		if err == nil || errors.Is(err, context.Canceled) {
			return err
		}
		logger.Errorf("reconnect in 3s: %v", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func runClientOnce(ctx context.Context, cfg *Config, logger *Logger) error {
	if cfg.MultipathEnabled {
		return runClientOnceMultipath(ctx, cfg, logger)
	}

	bindIP, err := resolveBindIP(cfg.BindIP)
	if err != nil {
		return err
	}
	localUDP := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
	udpConn, err := net.ListenUDP("udp", localUDP)
	if err != nil {
		return err
	}
	defer udpConn.Close()

	remoteUDP, err := net.ResolveUDPAddr("udp", net.JoinHostPort(cfg.RemoteAddr, fmt.Sprintf("%d", cfg.RemotePort)))
	if err != nil {
		return err
	}
	tlsConf, err := loadClientTLSConfig(cfg)
	if err != nil {
		return err
	}

	transport := quic.Transport{Conn: udpConn}
	conn, err := transport.Dial(ctx, remoteUDP, tlsConf, &quic.Config{
		EnableDatagrams:     true,
		KeepAlivePeriod:     15 * time.Second,
		MaxIdleTimeout:      60 * time.Second,
		CongestionAlgorithm: cfg.CongestionAlgorithm,
	})
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "shutdown")

	logger.Infof("connected local=%s remote=%s tun=%s", udpConn.LocalAddr(), remoteUDP.String(), cfg.TunName)

	var dc datagramConn
	if cfg.TransportMode == "reliable" {
		sc, err := openStreamConn(ctx, conn)
		if err != nil {
			return fmt.Errorf("open stream: %w", err)
		}
		dc = sc
	} else {
		dc = conn
	}
	return runTunnel(ctx, cfg, dc, logger)
}

func runClientOnceMultipath(ctx context.Context, cfg *Config, logger *Logger) error {
	mpConn, err := newMultipathConn(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer mpConn.closeAll(0, "shutdown")

	if cfg.ControlAPIListen != "" {
		stopAPI, err := startControlAPI(ctx, cfg, mpConn, logger)
		if err != nil {
			return err
		}
		defer stopAPI()
	}

	logger.Infof("connected multipath paths=%d policy=%s tun=%s", len(cfg.MultipathPaths), cfg.MultipathPolicy, cfg.TunName)
	return runTunnel(ctx, cfg, mpConn, logger)
}

func newMultipathConn(ctx context.Context, cfg *Config, logger *Logger) (*multipathConn, error) {
	dpRuntime, err := compileDataplaneConfig(cfg.Dataplane)
	if err != nil {
		return nil, err
	}

	// Expand pipes: paths with pipes > 1 become N internal path entries
	expandedPaths := expandMultipathPipes(cfg.MultipathPaths, cfg, logger)

	mp := &multipathConn{
		recvCh:  make(chan []byte, 512),
		errCh:   make(chan error, 1),
		logger:  logger,
		cfg:     cfg,
		dataplane: dpRuntime,
		classTx: make(map[string]*trafficClassCounters),
		baseCtx: ctx,
	}
	for className := range dpRuntime.classes {
		mp.classTx[className] = &trafficClassCounters{}
	}

	aliveCount := 0

	for _, p := range expandedPaths {
		state := &multipathPathState{cfg: p}
		mp.paths = append(mp.paths, state)

		effectiveTransport := resolvePathTransport(p, cfg, logger)

		// ── Stripe transport (Starlink-optimized) ─────────────────
		if effectiveTransport == "stripe" {
			sc, err := newStripeClientConn(ctx, cfg, p, logger)
			if err != nil {
				logger.Errorf("stripe init failed name=%s err=%v", p.Name, err)
				state.reconnecting = true
				continue
			}
			state.dc = sc
			state.stripeConn = sc
			state.alive = true
			state.reconnecting = false
			state.lastUp = time.Now()
			aliveCount++
			logger.Infof("stripe path up name=%s pipes=%d", p.Name, p.Pipes)
			continue
		}

		// ── QUIC transport (default) ──────────────────────────────
		bindIP, err := resolveBindIP(p.BindIP)
		if err != nil {
			logger.Errorf("path init failed name=%s step=bind-resolve err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		localUDP := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
		udpConn, err := net.ListenUDP("udp", localUDP)
		if err != nil {
			logger.Errorf("path init failed name=%s step=listen err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		remoteUDP, err := net.ResolveUDPAddr("udp", net.JoinHostPort(p.RemoteAddr, fmt.Sprintf("%d", p.RemotePort)))
		if err != nil {
			_ = udpConn.Close()
			logger.Errorf("path init failed name=%s step=remote-resolve err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		tlsConf, err := loadClientTLSConfig(cfg)
		if err != nil {
			_ = udpConn.Close()
			logger.Errorf("path init failed name=%s step=tls err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		transport := quic.Transport{Conn: udpConn}
		conn, err := transport.Dial(ctx, remoteUDP, tlsConf, &quic.Config{
			EnableDatagrams:     true,
			KeepAlivePeriod:     15 * time.Second,
			MaxIdleTimeout:      60 * time.Second,
			CongestionAlgorithm: cfg.CongestionAlgorithm,
		})
		if err != nil {
			_ = udpConn.Close()
			logger.Errorf("path init failed name=%s step=dial err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		state.udpConn = udpConn
		state.transport = &transport
		state.conn = conn
		if cfg.TransportMode == "reliable" {
			sc, err := openStreamConn(ctx, conn)
			if err != nil {
				_ = conn.CloseWithError(0, "stream-open-failed")
				_ = udpConn.Close()
				logger.Errorf("path init failed name=%s step=stream err=%v", p.Name, err)
				state.reconnecting = true
				continue
			}
			state.dc = sc
		} else {
			state.dc = conn
		}
		state.alive = true
		state.reconnecting = false
		state.lastUp = time.Now()
		aliveCount++
		logger.Infof("path up name=%s local=%s remote=%s", p.Name, udpConn.LocalAddr(), remoteUDP.String())
	}

	if aliveCount == 0 {
		mp.closeAll(0, "dial-error")
		return nil, fmt.Errorf("multipath: no initial path available")
	}

	for idx := range mp.paths {
		go mp.recvLoop(ctx, idx)
		if !mp.paths[idx].alive && mp.paths[idx].reconnecting {
			go mp.reconnectLoop(ctx, idx)
		}
	}

	go mp.telemetryLoop(ctx)

	return mp, nil
}

func (m *multipathConn) SendDatagram(pkt []byte) error {
	className, classPolicy := m.resolvePacketClass(pkt)

	if classPolicy.Duplicate {
		return m.sendDuplicate(pkt, className, classPolicy)
	}

	deadline := time.Now().Add(1200 * time.Millisecond)
	for {
		idx, conn := m.selectBestPath(classPolicy, nil)
		if idx < 0 || conn == nil {
			if time.Now().After(deadline) {
				m.markClassError(className)
				return fmt.Errorf("multipath: no active path available")
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if err := conn.SendDatagram(pkt); err != nil {
			m.markTxError(idx, err)
			continue
		}

		m.markTxSuccess(idx)
		m.markClassTx(className)
		return nil
	}
}

func (m *multipathConn) sendDuplicate(pkt []byte, className string, classPolicy DataplaneClassPolicy) error {
	copies := classPolicy.DuplicateCopies
	if copies < 2 {
		copies = 2
	}

	skip := make(map[int]struct{}, copies)
	sent := 0
	deadline := time.Now().Add(1200 * time.Millisecond)

	for sent < copies {
		idx, conn := m.selectBestPath(classPolicy, skip)
		if idx < 0 || conn == nil {
			if sent > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(80 * time.Millisecond)
			continue
		}

		if err := conn.SendDatagram(pkt); err != nil {
			m.markTxError(idx, err)
			skip[idx] = struct{}{}
			continue
		}

		m.markTxSuccess(idx)
		skip[idx] = struct{}{}
		sent++
	}

	if sent == 0 {
		m.markClassError(className)
		return fmt.Errorf("multipath: no active path available for duplicated send")
	}

	m.markClassTx(className)
	if sent > 1 {
		m.markClassDuplicate(className, uint64(sent-1))
	}
	return nil
}

func (m *multipathConn) selectBestPath(classPolicy DataplaneClassPolicy, skip map[int]struct{}) (int, datagramConn) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if len(m.paths) == 0 {
		return -1, nil
	}

	bestIdx := -1
	bestScore := int(^uint(0) >> 1)
	start := m.rr % len(m.paths)
	policy := classPolicy.SchedulerPolicy
	if policy == "" {
		policy = m.cfg.MultipathPolicy
	}
	if policy == "" {
		policy = "priority"
	}

	excluded := make(map[string]struct{}, len(classPolicy.ExcludedPaths))
	for _, name := range classPolicy.ExcludedPaths {
		excluded[name] = struct{}{}
	}

	preferred := make(map[string]struct{}, len(classPolicy.PreferredPaths))
	for _, name := range classPolicy.PreferredPaths {
		preferred[name] = struct{}{}
	}
	preferredOnly := len(preferred) > 0

	for pass := 0; pass < 2; pass++ {
		bestIdx = -1
		bestScore = int(^uint(0) >> 1)

		for i := 0; i < len(m.paths); i++ {
			idx := (start + i) % len(m.paths)
			if skip != nil {
				if _, blocked := skip[idx]; blocked {
					continue
				}
			}

			p := m.paths[idx]
			// Check excluded: match on both pipe name and base path name
			if _, blocked := excluded[p.cfg.Name]; blocked {
				continue
			}
			if p.cfg.BasePath != "" {
				if _, blocked := excluded[p.cfg.BasePath]; blocked {
					continue
				}
			}
			if preferredOnly && pass == 0 {
				_, nameOk := preferred[p.cfg.Name]
				_, baseOk := preferred[p.cfg.BasePath]
				if !nameOk && !baseOk {
					continue
				}
			}
			if !p.alive || p.dc == nil {
				continue
			}
			if now.Before(p.cooldownUntil) {
				continue
			}
			score := pathPolicyScore(policy, p)
			if score < bestScore {
				bestScore = score
				bestIdx = idx
			}
		}

		if bestIdx >= 0 {
			break
		}
		if !preferredOnly {
			break
		}
	}

	if bestIdx < 0 {
		return -1, nil
	}

	m.rr = (bestIdx + 1) % len(m.paths)
	return bestIdx, m.paths[bestIdx].dc
}

func pathPolicyScore(policy string, p *multipathPathState) int {
	base := p.cfg.Priority * 1000
	failPenalty := p.consecutiveFails * 100

	switch policy {
	case "failover":
		return base + failPenalty
	case "balanced":
		weightBonus := 0
		if p.cfg.Weight > 1 {
			weightBonus = (p.cfg.Weight - 1) * 120
		}
		return base + failPenalty - weightBonus
	default:
		weightBonus := 0
		if p.cfg.Weight > 1 {
			weightBonus = (p.cfg.Weight - 1) * 10
		}
		return base + failPenalty - weightBonus
	}
}

func (m *multipathConn) resolvePacketClass(pkt []byte) (string, DataplaneClassPolicy) {
	meta, ok := parsePacketMeta(pkt)
	if ok {
		for _, rule := range m.dataplane.classifiers {
			if rule.matches(meta) {
				if classPolicy, found := m.dataplane.classes[rule.className]; found {
					return rule.className, classPolicy
				}
			}
		}
	}

	className := m.dataplane.defaultClass
	classPolicy, found := m.dataplane.classes[className]
	if !found {
		className = "default"
		classPolicy = DataplaneClassPolicy{SchedulerPolicy: "priority"}
	}
	return className, classPolicy
}

func (m *multipathConn) markClassTx(className string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.classTx[className]
	if c == nil {
		c = &trafficClassCounters{}
		m.classTx[className] = c
	}
	c.txPackets++
}

func (m *multipathConn) markClassError(className string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.classTx[className]
	if c == nil {
		c = &trafficClassCounters{}
		m.classTx[className] = c
	}
	c.txErrors++
}

func (m *multipathConn) markClassDuplicate(className string, duplicates uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.classTx[className]
	if c == nil {
		c = &trafficClassCounters{}
		m.classTx[className] = c
	}
	c.txDuplicates += duplicates
}

func (m *multipathConn) markTxSuccess(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.paths[idx]
	p.txPackets++
	if p.consecutiveFails > 0 {
		p.consecutiveFails--
	}
}

func (m *multipathConn) markTxError(idx int, err error) {
	m.mu.Lock()
	p := m.paths[idx]
	p.txErrors++
	p.consecutiveFails++
	if p.consecutiveFails > 6 {
		p.consecutiveFails = 6
	}
	p.cooldownUntil = time.Now().Add(time.Duration(p.consecutiveFails) * time.Second)
	p.alive = false
	p.dc = nil
	if p.conn != nil {
		_ = p.conn.CloseWithError(0, "tx-error")
		p.conn = nil
	}
	if p.udpConn != nil {
		_ = p.udpConn.Close()
		p.udpConn = nil
	}
	name := p.cfg.Name
	needReconnect := !p.reconnecting
	if needReconnect {
		p.reconnecting = true
	}
	m.mu.Unlock()

	m.logger.Errorf("path tx failed name=%s err=%v", name, err)
	if needReconnect {
		go m.reconnectLoop(m.baseCtx, idx)
	}
}

func (m *multipathConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-m.errCh:
		return nil, err
	case pkt := <-m.recvCh:
		return pkt, nil
	}
}

func (m *multipathConn) recvLoop(ctx context.Context, idx int) {
	for {
		// Fast exit check — essential for clean shutdown
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn := m.currentPathConn(idx)
		if conn == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
			continue
		}

		pkt, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			// During shutdown, don't trigger reconnect — just exit
			if ctx.Err() != nil {
				return
			}
			m.onPathError(ctx, idx, err)
			continue
		}
		m.onPathSuccess(idx)

		copyPkt := append([]byte(nil), pkt...)
		select {
		case <-ctx.Done():
			return
		case m.recvCh <- copyPkt:
		}
	}
}

func (m *multipathConn) currentPathConn(idx int) datagramConn {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if idx < 0 || idx >= len(m.paths) {
		return nil
	}
	return m.paths[idx].dc
}

func (m *multipathConn) onPathSuccess(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.paths[idx]
	p.rxPackets++
	if p.consecutiveFails > 0 {
		p.consecutiveFails--
	}
}

func (m *multipathConn) onPathError(ctx context.Context, idx int, err error) {
	// Skip reconnect during shutdown
	if ctx.Err() != nil {
		return
	}

	m.mu.Lock()
	p := m.paths[idx]
	p.alive = false
	p.rxErrors++
	p.lastDown = time.Now()
	p.consecutiveFails++
	if p.consecutiveFails > 6 {
		p.consecutiveFails = 6
	}
	p.cooldownUntil = time.Now().Add(time.Duration(p.consecutiveFails) * time.Second)
	oldStripe := p.stripeConn
	oldConn := p.conn
	oldUDP := p.udpConn
	p.dc = nil
	p.stripeConn = nil
	p.conn = nil
	p.udpConn = nil
	name := p.cfg.Name
	needReconnect := !p.reconnecting
	if needReconnect {
		p.reconnecting = true
	}
	m.mu.Unlock()

	// Close outside lock to prevent deadlock with closeAll
	if oldStripe != nil {
		_ = oldStripe.Close()
	}
	if oldConn != nil {
		_ = oldConn.CloseWithError(0, "rx-error")
	}
	if oldUDP != nil {
		_ = oldUDP.Close()
	}

	m.logger.Errorf("path down name=%s err=%v", name, err)
	if needReconnect {
		go m.reconnectLoop(ctx, idx)
	}
}

func (m *multipathConn) reconnectLoop(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			if idx >= 0 && idx < len(m.paths) {
				m.paths[idx].reconnecting = false
			}
			m.mu.Unlock()
			return
		default:
		}

		m.mu.RLock()
		if idx < 0 || idx >= len(m.paths) {
			m.mu.RUnlock()
			return
		}
		pcfg := m.paths[idx].cfg
		m.mu.RUnlock()

		effectiveTransport := resolvePathTransport(pcfg, m.cfg, m.logger)

		// ── Stripe reconnect ──────────────────────────────────────
		if effectiveTransport == "stripe" {
			sc, err := newStripeClientConn(ctx, m.cfg, pcfg, m.logger)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				m.logger.Errorf("stripe redial failed name=%s err=%v", pcfg.Name, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
			m.mu.Lock()
			if idx >= 0 && idx < len(m.paths) {
				p := m.paths[idx]
				p.dc = sc
				p.stripeConn = sc
				p.alive = true
				p.reconnecting = false
				p.lastUp = time.Now()
				if p.consecutiveFails > 0 {
					p.consecutiveFails--
				}
			}
			m.mu.Unlock()
			m.logger.Infof("stripe path recovered name=%s pipes=%d", pcfg.Name, pcfg.Pipes)
			return
		}

		// ── QUIC reconnect (existing logic) ───────────────────────
		bindIP, err := resolveBindIP(pcfg.BindIP)
		if err != nil {
			m.logger.Errorf("path redial resolve failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		localUDP := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
		udpConn, err := net.ListenUDP("udp", localUDP)
		if err != nil {
			m.logger.Errorf("path redial listen failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		remoteUDP, err := net.ResolveUDPAddr("udp", net.JoinHostPort(pcfg.RemoteAddr, fmt.Sprintf("%d", pcfg.RemotePort)))
		if err != nil {
			_ = udpConn.Close()
			m.logger.Errorf("path redial remote resolve failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		tlsConf, err := loadClientTLSConfig(m.cfg)
		if err != nil {
			_ = udpConn.Close()
			m.logger.Errorf("path redial tls failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		transport := quic.Transport{Conn: udpConn}
		dialCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		conn, err := transport.Dial(dialCtx, remoteUDP, tlsConf, &quic.Config{
			EnableDatagrams:     true,
			KeepAlivePeriod:     15 * time.Second,
			MaxIdleTimeout:      60 * time.Second,
			CongestionAlgorithm: m.cfg.CongestionAlgorithm,
		})
		cancel()
		if err != nil {
			_ = udpConn.Close()
			m.logger.Errorf("path redial failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		var dc datagramConn
		if m.cfg.TransportMode == "reliable" {
			sc, err := openStreamConn(ctx, conn)
			if err != nil {
				_ = conn.CloseWithError(0, "stream-open-failed")
				_ = udpConn.Close()
				m.logger.Errorf("path redial stream failed name=%s err=%v", pcfg.Name, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
			dc = sc
		} else {
			dc = conn
		}

		m.mu.Lock()
		if idx >= 0 && idx < len(m.paths) {
			p := m.paths[idx]
			p.conn = conn
			p.dc = dc
			p.udpConn = udpConn
			p.transport = &transport
			p.alive = true
			p.reconnecting = false
			p.lastUp = time.Now()
			if p.consecutiveFails > 0 {
				p.consecutiveFails--
			}
		}
		m.mu.Unlock()

		m.logger.Infof("path recovered name=%s local=%s remote=%s", pcfg.Name, udpConn.LocalAddr(), remoteUDP.String())
		return
	}
}

func (m *multipathConn) telemetryLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.logTelemetrySnapshot()
		}
	}
}

func (m *multipathConn) logTelemetrySnapshot() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Per-pipe telemetry
	type baseAgg struct {
		txPkts, rxPkts, txErr, rxErr uint64
		alive, total                 int
	}
	agg := make(map[string]*baseAgg)

	for _, p := range m.paths {
		state := "down"
		if p.alive && (p.conn != nil || p.stripeConn != nil) {
			state = "up"
		}
		m.logger.Infof(
			"path telemetry name=%s state=%s tx_pkts=%d rx_pkts=%d tx_err=%d rx_err=%d fails=%d cooldown_until=%s last_up=%s last_down=%s",
			p.cfg.Name,
			state,
			p.txPackets,
			p.rxPackets,
			p.txErrors,
			p.rxErrors,
			p.consecutiveFails,
			formatTime(p.cooldownUntil),
			formatTime(p.lastUp),
			formatTime(p.lastDown),
		)

		// Aggregate by base path (for multi-pipe summary)
		base := p.cfg.BasePath
		if base == "" {
			base = p.cfg.Name
		}
		a := agg[base]
		if a == nil {
			a = &baseAgg{}
			agg[base] = a
		}
		a.txPkts += p.txPackets
		a.rxPkts += p.rxPackets
		a.txErr += p.txErrors
		a.rxErr += p.rxErrors
		a.total++
		if p.alive && (p.conn != nil || p.stripeConn != nil) {
			a.alive++
		}
	}

	// Log aggregate per base path (only when pipes > 1)
	for base, a := range agg {
		if a.total > 1 {
			m.logger.Infof(
				"path aggregate base=%s pipes=%d/%d tx_pkts=%d rx_pkts=%d tx_err=%d rx_err=%d",
				base, a.alive, a.total, a.txPkts, a.rxPkts, a.txErr, a.rxErr,
			)
		}
	}

	classes := make([]string, 0, len(m.classTx))
	for className := range m.classTx {
		classes = append(classes, className)
	}
	sort.Strings(classes)
	for _, className := range classes {
		c := m.classTx[className]
		if c == nil {
			continue
		}
		m.logger.Infof(
			"class telemetry class=%s tx_pkts=%d tx_err=%d tx_dups=%d",
			className,
			c.txPackets,
			c.txErrors,
			c.txDuplicates,
		)
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func (m *multipathConn) closeAll(code quic.ApplicationErrorCode, reason string) {
	// Collect references under lock, then close outside the lock.
	// This prevents deadlock with recvLoop → onPathError which also
	// needs m.mu. Closing stripe/QUIC connections unblocks those goroutines.
	m.mu.Lock()
	type toClose struct {
		stripe *stripeClientConn
		conn   quic.Connection
		udp    *net.UDPConn
	}
	var items []toClose
	for _, p := range m.paths {
		items = append(items, toClose{
			stripe: p.stripeConn,
			conn:   p.conn,
			udp:    p.udpConn,
		})
		// Nil out refs so onPathError won't double-close
		p.stripeConn = nil
		p.conn = nil
		p.dc = nil
		p.udpConn = nil
		p.alive = false
	}
	m.mu.Unlock()

	// Close outside lock — these calls unblock recvLoop goroutines
	for _, item := range items {
		if item.stripe != nil {
			_ = item.stripe.Close()
		}
		if item.conn != nil {
			_ = item.conn.CloseWithError(code, reason)
		}
		if item.udp != nil {
			_ = item.udp.Close()
		}
	}
}

func (m *multipathConn) snapshotDataplaneConfig() DataplaneConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneDataplaneConfig(m.cfg.Dataplane)
}

func (m *multipathConn) applyDataplaneConfig(dp DataplaneConfig) error {
	normalizeDataplaneConfig(&dp, m.cfg.MultipathPolicy)
	if err := validateDataplaneConfig(dp, m.cfg.MultipathPaths); err != nil {
		return err
	}
	compiled, err := compileDataplaneConfig(dp)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.cfg.Dataplane = cloneDataplaneConfig(dp)
	m.dataplane = compiled
	for className := range compiled.classes {
		if _, ok := m.classTx[className]; !ok {
			m.classTx[className] = &trafficClassCounters{}
		}
	}
	m.mu.Unlock()

	m.logger.Infof("dataplane policy applied classes=%d classifiers=%d", len(dp.Classes), len(dp.Classifiers))
	return nil
}

func (m *multipathConn) reloadDataplaneFromFile() error {
	path := strings.TrimSpace(m.cfg.DataplaneConfigFile)
	if path == "" {
		return fmt.Errorf("dataplane_config_file not configured")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dp := DataplaneConfig{}
	if err := yaml.Unmarshal(b, &dp); err != nil {
		return err
	}
	return m.applyDataplaneConfig(dp)
}

func cloneDataplaneConfig(in DataplaneConfig) DataplaneConfig {
	out := DataplaneConfig{
		DefaultClass: in.DefaultClass,
	}
	if in.Classes != nil {
		out.Classes = make(map[string]DataplaneClassPolicy, len(in.Classes))
		for name, policy := range in.Classes {
			copyPolicy := policy
			copyPolicy.PreferredPaths = append([]string(nil), policy.PreferredPaths...)
			copyPolicy.ExcludedPaths = append([]string(nil), policy.ExcludedPaths...)
			out.Classes[name] = copyPolicy
		}
	}
	if len(in.Classifiers) > 0 {
		out.Classifiers = make([]DataplaneClassifierRule, 0, len(in.Classifiers))
		for _, rule := range in.Classifiers {
			copyRule := rule
			copyRule.SrcCIDRs = append([]string(nil), rule.SrcCIDRs...)
			copyRule.DstCIDRs = append([]string(nil), rule.DstCIDRs...)
			copyRule.SrcPorts = append([]string(nil), rule.SrcPorts...)
			copyRule.DstPorts = append([]string(nil), rule.DstPorts...)
			copyRule.DSCP = append([]int(nil), rule.DSCP...)
			out.Classifiers = append(out.Classifiers, copyRule)
		}
	}
	return out
}

func startControlAPI(ctx context.Context, cfg *Config, mp *multipathConn, logger *Logger) (func(), error) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !authorizeControlAPI(w, r, cfg) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"role":             cfg.Role,
			"multipath_enabled": cfg.MultipathEnabled,
			"tun_name":         cfg.TunName,
		})
	})

	mux.HandleFunc("/dataplane", func(w http.ResponseWriter, r *http.Request) {
		if !authorizeControlAPI(w, r, cfg) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{
				"dataplane": mp.snapshotDataplaneConfig(),
			})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	})

	mux.HandleFunc("/dataplane/validate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !authorizeControlAPI(w, r, cfg) {
			return
		}

		dp, err := decodeDataplaneFromRequest(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		normalizeDataplaneConfig(&dp, cfg.MultipathPolicy)
		if err := validateDataplaneConfig(dp, cfg.MultipathPaths); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/dataplane/apply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !authorizeControlAPI(w, r, cfg) {
			return
		}

		dp, err := decodeDataplaneFromRequest(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := mp.applyDataplaneConfig(dp); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/dataplane/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !authorizeControlAPI(w, r, cfg) {
			return
		}
		if err := mp.reloadDataplaneFromFile(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	server := &http.Server{
		Addr:    cfg.ControlAPIListen,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		logger.Infof("control api listening addr=%s", cfg.ControlAPIListen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Errorf("control api stopped err=%v", err)
		}
	}()

	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}, nil
}

func authorizeControlAPI(w http.ResponseWriter, r *http.Request, cfg *Config) bool {
	if cfg.ControlAPIAuthToken == "" {
		return true
	}
	expected := "Bearer " + cfg.ControlAPIAuthToken
	if r.Header.Get("Authorization") != expected {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}
	return true
}

func decodeDataplaneFromRequest(r *http.Request) (DataplaneConfig, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		return DataplaneConfig{}, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return DataplaneConfig{}, fmt.Errorf("empty request body")
	}

	dp := DataplaneConfig{}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.Contains(ct, "json") {
		if err := json.Unmarshal(body, &dp); err != nil {
			return DataplaneConfig{}, err
		}
		return dp, nil
	}
	if strings.Contains(ct, "yaml") || strings.Contains(ct, "yml") {
		if err := yaml.Unmarshal(body, &dp); err != nil {
			return DataplaneConfig{}, err
		}
		return dp, nil
	}

	if err := json.Unmarshal(body, &dp); err == nil {
		return dp, nil
	}
	if err := yaml.Unmarshal(body, &dp); err != nil {
		return DataplaneConfig{}, fmt.Errorf("payload must be JSON or YAML")
	}
	return dp, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func runTunnel(ctx context.Context, cfg *Config, conn datagramConn, logger *Logger) error {
	tun, err := water.New(water.Config{DeviceType: water.TUN, PlatformSpecificParams: water.PlatformSpecificParams{Name: cfg.TunName}})
	if err != nil {
		return err
	}
	var tunCloseOnce sync.Once
	closeTun := func() { tunCloseOnce.Do(func() { tun.Close() }) }
	defer closeTun()

	return runTunnelWithTUN(ctx, conn, tun, closeTun, logger)
}

func runTunnelWithTUN(ctx context.Context, conn datagramConn, tun *water.Interface, closeTun func(), logger *Logger) error {

	errCh := make(chan error, 2)

	// Close TUN device when context is cancelled to unblock tun.Read goroutine.
	// This runs BEFORE the deferred tun.Close() in the caller, ensuring the
	// read goroutine doesn't stay blocked during shutdown.
	go func() {
		<-ctx.Done()
		closeTun()
	}()

	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := tun.Read(buf)
			if err != nil {
				// Check context first — if shutting down, exit silently
				if ctx.Err() != nil {
					return
				}
				errCh <- err
				return
			}
			// Check context before sending — don't attempt write on closed conn
			if ctx.Err() != nil {
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			if err := conn.SendDatagram(pkt); err != nil {
				if ctx.Err() != nil {
					return
				}
				errCh <- err
				return
			}
			logger.Debugf("TX %d bytes", n)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		default:
		}
		pkt, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			return err
		}
		if _, err := tun.Write(pkt); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		logger.Debugf("RX %d bytes", len(pkt))
	}
}

// expandMultipathPipes expands path configs with pipes > 1 into N individual
// path entries. Each pipe gets a unique name (e.g. "wan5.0", "wan5.1") but
// shares the same bind IP, remote address, priority, and weight.
// Each pipe will open its own UDP socket (different source port), creating
// separate UDP sessions that bypass per-session traffic shaping (e.g. Starlink).
func expandMultipathPipes(paths []MultipathPathConfig, cfg *Config, logger *Logger) []MultipathPathConfig {
	var expanded []MultipathPathConfig
	for _, p := range paths {
		effectiveTransport := resolvePathTransport(p, cfg, logger)

		// Stripe transport: do NOT expand pipes — stripeClientConn manages its
		// own N UDP sockets internally. The path stays as a single entry.
		if effectiveTransport == "stripe" {
			p.BasePath = p.Name
			if p.Pipes <= 0 {
				p.Pipes = cfg.StarlinkDefaultPipes
				if p.Pipes <= 0 {
					p.Pipes = 4
				}
			}
			logger.Infof("stripe path=%s pipes=%d (managed internally)", p.Name, p.Pipes)
			expanded = append(expanded, p)
			continue
		}

		// QUIC transport: expand pipes as before
		pipes := p.Pipes
		// Auto-detect Starlink if configured and pipes not explicitly set
		if pipes <= 0 && cfg.DetectStarlink {
			if detectStarlink(p.BindIP, logger) {
				pipes = cfg.StarlinkDefaultPipes
				if pipes <= 0 {
					pipes = 4
				}
				logger.Infof("starlink detected path=%s auto_pipes=%d", p.Name, pipes)
			}
		}
		if pipes <= 1 {
			p.BasePath = p.Name
			expanded = append(expanded, p)
			continue
		}
		logger.Infof("expanding path=%s into %d pipes", p.Name, pipes)
		for i := 0; i < pipes; i++ {
			ep := p
			ep.Name = fmt.Sprintf("%s.%d", p.Name, i)
			ep.BasePath = p.Name
			ep.Pipes = 1
			expanded = append(expanded, ep)
		}
	}
	return expanded
}

// resolvePathTransport determines the effective transport mode for a path.
// Returns "stripe" for Starlink paths or "quic" (default).
// Priority: explicit per-path → global starlink_transport → auto-detect.
func resolvePathTransport(p MultipathPathConfig, cfg *Config, logger *Logger) string {
	// Explicit per-path transport
	if p.Transport != "" && p.Transport != "auto" {
		return p.Transport
	}
	// Global starlink_transport setting
	if cfg.StarlinkTransport == "stripe" {
		return "stripe"
	}
	// Auto-detect: if detect_starlink is enabled, check rDNS/CGNAT
	if p.Transport == "auto" || cfg.DetectStarlink {
		if detectStarlink(p.BindIP, logger) {
			logger.Infof("starlink auto-detected for path=%s → stripe transport", p.Name)
			return "stripe"
		}
	}
	return "quic"
}

// detectStarlink checks if a network interface is connected via Starlink by
// performing a reverse DNS lookup on the interface's WAN IP.
// Starlink IPs resolve to *.starlinkisp.net PTR records.
// Uses the interface name extracted from bind_ip (e.g. "if:enp7s7") to
// obtain the external IP via a DNS resolver bound to that interface.
func detectStarlink(bindIP string, logger *Logger) bool {
	ip, err := resolveBindIP(bindIP)
	if err != nil {
		logger.Debugf("starlink detect: cannot resolve %s: %v", bindIP, err)
		return false
	}

	// First check: CGNAT range 100.64.0.0/10 is commonly used by Starlink
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	cgnat := net.IPNet{IP: net.ParseIP("100.64.0.0"), Mask: net.CIDRMask(10, 32)}
	isCGNAT := cgnat.Contains(parsed)

	// Try reverse DNS on the local IP (Starlink assigns IPs with PTR records)
	names, err := net.LookupAddr(ip)
	if err == nil {
		for _, name := range names {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "starlinkisp.net") || strings.Contains(lower, "starlink") {
				logger.Debugf("starlink detect: positive via rDNS %s → %s", ip, name)
				return true
			}
		}
	}

	// Fallback: try to obtain WAN IP via DNS (dig-style) and check rDNS on that
	wanIP := getWANIPViaDNS(ip, logger)
	if wanIP != "" {
		wanNames, err := net.LookupAddr(wanIP)
		if err == nil {
			for _, name := range wanNames {
				lower := strings.ToLower(name)
				if strings.Contains(lower, "starlinkisp.net") || strings.Contains(lower, "starlink") {
					logger.Debugf("starlink detect: positive via WAN rDNS %s → %s", wanIP, name)
					return true
				}
			}
		}
	}

	// Heuristic: CGNAT range is a strong indicator (most Starlink installations)
	if isCGNAT {
		logger.Debugf("starlink detect: CGNAT range match %s (heuristic positive)", ip)
		return true
	}

	logger.Debugf("starlink detect: negative for %s", ip)
	return false
}

// getWANIPViaDNS obtains the external (WAN) IP for traffic exiting via a
// specific local IP by using OpenDNS myip resolution.
// This is equivalent to: dig +short myip.opendns.com @resolver1.opendns.com -b <localIP>
func getWANIPViaDNS(localIP string, logger *Logger) string {
	laddr := net.ParseIP(localIP)
	if laddr == nil {
		return ""
	}
	dialer := &net.Dialer{
		LocalAddr: &net.UDPAddr{IP: laddr},
		Timeout:   3 * time.Second,
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, "udp", "208.67.222.222:53") // resolver1.opendns.com
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := resolver.LookupHost(ctx, "myip.opendns.com")
	if err != nil || len(ips) == 0 {
		logger.Debugf("starlink detect: WAN IP lookup failed via %s: %v", localIP, err)
		return ""
	}
	logger.Debugf("starlink detect: WAN IP via %s = %s", localIP, ips[0])
	return ips[0]
}

func resolveBindIP(value string) (string, error) {
	if strings.HasPrefix(value, "if:") {
		ifName := strings.TrimPrefix(value, "if:")
		iface, err := net.InterfaceByName(ifName)
		if err != nil {
			return "", err
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				continue
			}
			if ip.IsLoopback() {
				continue
			}
			return ip.String(), nil
		}
		return "", fmt.Errorf("no ipv4 found on %s", ifName)
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return "", fmt.Errorf("invalid bind_ip: %s", value)
	}
	return value, nil
}

func loadServerTLSConfig(cfg *Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"mpquic-ip"},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func loadClientTLSConfig(cfg *Config) (*tls.Config, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: cfg.TLSInsecureSkipVerify,
		ServerName:         cfg.TLSServerName,
		NextProtos:         []string{"mpquic-ip"},
		MinVersion:         tls.VersionTLS13,
	}
	if cfg.TLSCAFile != "" {
		caPEM, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("failed loading tls_ca_file: %s", cfg.TLSCAFile)
		}
		tlsConf.RootCAs = roots
	}
	return tlsConf, nil
}

func compileDataplaneConfig(dp DataplaneConfig) (compiledDataplane, error) {
	out := compiledDataplane{
		defaultClass: dp.DefaultClass,
		classes:      make(map[string]DataplaneClassPolicy, len(dp.Classes)),
	}

	for className, policy := range dp.Classes {
		out.classes[className] = policy
	}

	out.classifiers = make([]compiledClassifierRule, 0, len(dp.Classifiers))
	for _, rule := range dp.Classifiers {
		srcCIDRs, err := parseCIDRs(rule.SrcCIDRs)
		if err != nil {
			return compiledDataplane{}, err
		}
		dstCIDRs, err := parseCIDRs(rule.DstCIDRs)
		if err != nil {
			return compiledDataplane{}, err
		}
		srcPorts, err := parsePortRanges(rule.SrcPorts)
		if err != nil {
			return compiledDataplane{}, err
		}
		dstPorts, err := parsePortRanges(rule.DstPorts)
		if err != nil {
			return compiledDataplane{}, err
		}
		dscp := make(map[uint8]struct{}, len(rule.DSCP))
		for _, value := range rule.DSCP {
			dscp[uint8(value)] = struct{}{}
		}

		out.classifiers = append(out.classifiers, compiledClassifierRule{
			name:      rule.Name,
			className: rule.ClassName,
			protocol:  rule.Protocol,
			srcCIDRs:  srcCIDRs,
			dstCIDRs:  dstCIDRs,
			srcPorts:  srcPorts,
			dstPorts:  dstPorts,
			dscp:      dscp,
		})
	}

	return out, nil
}

func parseCIDRs(values []string) ([]netip.Prefix, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(values))
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(v)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", v, err)
		}
		out = append(out, prefix)
	}
	return out, nil
}

func parsePortRanges(values []string) ([]portRange, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]portRange, 0, len(values))
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if strings.Contains(v, "-") {
			parts := strings.SplitN(v, "-", 2)
			start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil {
				return nil, fmt.Errorf("invalid port range start %q", v)
			}
			end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid port range end %q", v)
			}
			if start < 1 || start > 65535 || end < 1 || end > 65535 || end < start {
				return nil, fmt.Errorf("invalid port range %q", v)
			}
			out = append(out, portRange{from: uint16(start), to: uint16(end)})
			continue
		}

		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid port value %q", v)
		}
		out = append(out, portRange{from: uint16(port), to: uint16(port)})
	}
	return out, nil
}

func (r compiledClassifierRule) matches(meta packetMeta) bool {
	if r.protocol != "" && r.protocol != meta.protocol {
		return false
	}
	if len(r.srcCIDRs) > 0 && !matchAddrPrefixes(meta.srcAddr, r.srcCIDRs) {
		return false
	}
	if len(r.dstCIDRs) > 0 && !matchAddrPrefixes(meta.dstAddr, r.dstCIDRs) {
		return false
	}
	if len(r.srcPorts) > 0 {
		if !meta.hasPorts || !matchPortRanges(meta.srcPort, r.srcPorts) {
			return false
		}
	}
	if len(r.dstPorts) > 0 {
		if !meta.hasPorts || !matchPortRanges(meta.dstPort, r.dstPorts) {
			return false
		}
	}
	if len(r.dscp) > 0 {
		if _, ok := r.dscp[meta.dscp]; !ok {
			return false
		}
	}
	return true
}

func matchAddrPrefixes(addr netip.Addr, prefixes []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func matchPortRanges(port uint16, ranges []portRange) bool {
	for _, r := range ranges {
		if port >= r.from && port <= r.to {
			return true
		}
	}
	return false
}

func parsePacketMeta(pkt []byte) (packetMeta, bool) {
	if len(pkt) < 1 {
		return packetMeta{}, false
	}

	version := pkt[0] >> 4
	switch version {
	case 4:
		if len(pkt) < 20 {
			return packetMeta{}, false
		}
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl {
			return packetMeta{}, false
		}

		src := netip.AddrFrom4([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]})
		dst := netip.AddrFrom4([4]byte{pkt[16], pkt[17], pkt[18], pkt[19]})
		meta := packetMeta{
			protocol: ipProtocolName(pkt[9]),
			srcAddr:  src,
			dstAddr:  dst,
			dscp:     pkt[1] >> 2,
		}
		if (meta.protocol == "tcp" || meta.protocol == "udp") && len(pkt) >= ihl+4 {
			meta.srcPort = binary.BigEndian.Uint16(pkt[ihl : ihl+2])
			meta.dstPort = binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
			meta.hasPorts = true
		}
		return meta, true

	case 6:
		if len(pkt) < 40 {
			return packetMeta{}, false
		}

		var srcArr, dstArr [16]byte
		copy(srcArr[:], pkt[8:24])
		copy(dstArr[:], pkt[24:40])
		trafficClass := ((pkt[0] & 0x0f) << 4) | (pkt[1] >> 4)
		meta := packetMeta{
			protocol: ipProtocolName(pkt[6]),
			srcAddr:  netip.AddrFrom16(srcArr),
			dstAddr:  netip.AddrFrom16(dstArr),
			dscp:     trafficClass >> 2,
		}
		if (meta.protocol == "tcp" || meta.protocol == "udp") && len(pkt) >= 44 {
			meta.srcPort = binary.BigEndian.Uint16(pkt[40:42])
			meta.dstPort = binary.BigEndian.Uint16(pkt[42:44])
			meta.hasPorts = true
		}
		return meta, true
	}

	return packetMeta{}, false
}

func ipProtocolName(proto uint8) string {
	switch proto {
	case 6:
		return "tcp"
	case 17:
		return "udp"
	case 1:
		return "icmp"
	case 58:
		return "icmpv6"
	default:
		return strconv.Itoa(int(proto))
	}
}
