package main

// stripe.go — UDP Striping + FEC transport layer for Starlink session bypass.
//
// Starlink applies per-session UDP traffic shaping (~80 Mbps per flow).
// This layer opens N raw UDP sockets ("pipes") on the same physical interface,
// each seen by Starlink as an independent session. Packets are distributed
// across pipes using round-robin with optional Reed-Solomon FEC for loss recovery.
//
// Unlike the multi-pipe QUIC approach (which suffered CC competition between
// N independent BBR instances), this layer has NO congestion control —
// rate limiting is naturally provided by the TCP senders inside the tunnel.
//
// Wire protocol:
//   [stripeHdr 16 bytes][shard payload (variable)]
//
// Integrates as a datagramConn: both stripeClientConn and stripeServerDC
// implement the datagramConn interface, so the existing multipath system
// uses them transparently.

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/reedsolomon"
	"github.com/songgao/water"
)

// ─── Constants ────────────────────────────────────────────────────────────

const (
	stripeMagic   uint16 = 0x5354 // "ST"
	stripeVersion uint8  = 1

	// Packet types
	stripeDATA      uint8 = 0x01
	stripePARITY    uint8 = 0x02
	stripeREGISTER  uint8 = 0x03
	stripeKEEPALIVE uint8 = 0x04

	// Header: magic(2) + ver(1) + type(1) + session(4) + groupSeq(4) + shardIdx(1) + groupDataN(1) + dataLen(2) = 16
	stripeHdrLen = 16

	// Defaults
	stripeDefaultDataShards   = 10
	stripeDefaultParityShards = 2
	stripeMaxPayload          = 1500
	stripeFlushInterval       = 5 * time.Millisecond
	stripeKeepaliveInterval   = 5 * time.Second
	stripeSessionTimeout      = 30 * time.Second
	stripeGCInterval          = 10 * time.Second
	stripeRegisterRetries     = 3
	stripeRegisterDelay       = 500 * time.Millisecond
)

// ─── Wire Protocol ────────────────────────────────────────────────────────

// stripeHdr is the 16-byte wire-format header for all stripe packets.
type stripeHdr struct {
	Magic      uint16 // 0x5354
	Version    uint8  // 1
	Type       uint8  // DATA / PARITY / REGISTER / KEEPALIVE
	Session    uint32 // TUN IP as uint32 (session identifier)
	GroupSeq   uint32 // sequence of first data shard in FEC group
	ShardIdx   uint8  // index within FEC group (0..K-1 data, K..K+M-1 parity)
	GroupDataN uint8  // number of data shards in this group (≤ K); 0 for PARITY
	DataLen    uint16 // actual IP packet length (before FEC padding)
}

func encodeStripeHdr(buf []byte, h *stripeHdr) {
	binary.BigEndian.PutUint16(buf[0:2], h.Magic)
	buf[2] = h.Version
	buf[3] = h.Type
	binary.BigEndian.PutUint32(buf[4:8], h.Session)
	binary.BigEndian.PutUint32(buf[8:12], h.GroupSeq)
	buf[12] = h.ShardIdx
	buf[13] = h.GroupDataN
	binary.BigEndian.PutUint16(buf[14:16], h.DataLen)
}

func decodeStripeHdr(buf []byte) (stripeHdr, bool) {
	if len(buf) < stripeHdrLen {
		return stripeHdr{}, false
	}
	h := stripeHdr{
		Magic:      binary.BigEndian.Uint16(buf[0:2]),
		Version:    buf[2],
		Type:       buf[3],
		Session:    binary.BigEndian.Uint32(buf[4:8]),
		GroupSeq:   binary.BigEndian.Uint32(buf[8:12]),
		ShardIdx:   buf[12],
		GroupDataN: buf[13],
		DataLen:    binary.BigEndian.Uint16(buf[14:16]),
	}
	if h.Magic != stripeMagic || h.Version != stripeVersion {
		return h, false
	}
	return h, true
}

// ─── FEC Group ────────────────────────────────────────────────────────────

type fecGroup struct {
	dataK    int
	parityM  int
	shards   [][]byte // [dataK + parityM] — nil = not received
	present  []bool
	received int
	maxLen   int
	created  time.Time
	delivered bool
}

func newFECGroup(k, m int) *fecGroup {
	total := k + m
	return &fecGroup{
		dataK:   k,
		parityM: m,
		shards:  make([][]byte, total),
		present: make([]bool, total),
		created: time.Now(),
	}
}

// addShard stores a received shard. Returns true when K shards are present (decodable).
func (g *fecGroup) addShard(idx int, data []byte) bool {
	if idx < 0 || idx >= len(g.shards) || g.present[idx] {
		return g.received >= g.dataK
	}
	shard := make([]byte, len(data))
	copy(shard, data)
	g.shards[idx] = shard
	g.present[idx] = true
	g.received++
	if len(data) > g.maxLen {
		g.maxLen = len(data)
	}
	return g.received >= g.dataK
}

// ─── Stripe Client Connection ────────────────────────────────────────────
// Implements datagramConn interface for use as multipathPathState.dc

type stripeClientConn struct {
	pipes      []*net.UDPConn
	serverAddr *net.UDPAddr
	sessionID  uint32

	dataK   int
	parityM int
	enc     reedsolomon.Encoder // nil if parityM == 0

	// TX state
	txSeq    uint32 // atomic: next data sequence number
	txPipe   uint32 // atomic: round-robin pipe selector
	txGroup  [][]byte
	txGrpSeq uint32
	txMu     sync.Mutex
	txTimer  *time.Timer

	// RX state
	rxCh     chan []byte // decoded IP packets delivered here
	rxGroups map[uint32]*fecGroup
	rxMu     sync.Mutex

	// Stats (atomic)
	txPkts   uint64
	rxPkts   uint64
	txBytes  uint64
	rxBytes  uint64
	fecRecov uint64

	closeCh   chan struct{}
	closeOnce sync.Once
	logger    *Logger
}

// newStripeClientConn creates a stripe transport for a single multipath path.
// It opens N UDP sockets on the specified interface, all pointed at the server's
// stripe port. Each socket = one Starlink session = one ~80 Mbps allocation.
func newStripeClientConn(ctx context.Context, cfg *Config, pathCfg MultipathPathConfig, logger *Logger) (*stripeClientConn, error) {
	pipes := pathCfg.Pipes
	if pipes <= 1 {
		pipes = 4
	}

	bindIP, err := resolveBindIP(pathCfg.BindIP)
	if err != nil {
		return nil, fmt.Errorf("stripe: resolve bind: %w", err)
	}

	remoteHost := pathCfg.RemoteAddr
	if remoteHost == "" {
		remoteHost = cfg.RemoteAddr
	}
	remotePort := pathCfg.RemotePort
	if remotePort == 0 {
		remotePort = cfg.RemotePort
	}

	stripePort := cfg.StripePort
	if stripePort == 0 {
		stripePort = remotePort + 1000
	}

	serverAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(remoteHost, fmt.Sprintf("%d", stripePort)))
	if err != nil {
		return nil, fmt.Errorf("stripe: resolve remote: %w", err)
	}

	tunIP, err := parseTUNIP(cfg.TunCIDR)
	if err != nil {
		return nil, fmt.Errorf("stripe: parse tun cidr: %w", err)
	}
	sessionID := pathSessionID(tunIP, pathCfg.Name)

	dataK := cfg.StripeDataShards
	if dataK <= 0 {
		dataK = stripeDefaultDataShards
	}
	parityM := cfg.StripeParityShards
	if parityM < 0 {
		parityM = stripeDefaultParityShards
	}

	var enc reedsolomon.Encoder
	if parityM > 0 {
		enc, err = reedsolomon.New(dataK, parityM)
		if err != nil {
			return nil, fmt.Errorf("stripe: FEC encoder: %w", err)
		}
	}

	scc := &stripeClientConn{
		serverAddr: serverAddr,
		sessionID:  sessionID,
		dataK:      dataK,
		parityM:    parityM,
		enc:        enc,
		txGroup:    make([][]byte, 0, dataK),
		rxCh:       make(chan []byte, 512),
		rxGroups:   make(map[uint32]*fecGroup),
		closeCh:    make(chan struct{}),
		logger:     logger,
	}

	// Open N UDP sockets bound to the same interface
	for i := 0; i < pipes; i++ {
		laddr := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
		conn, err := net.ListenUDP("udp", laddr)
		if err != nil {
			scc.Close()
			return nil, fmt.Errorf("stripe: listen pipe %d: %w", i, err)
		}
		scc.pipes = append(scc.pipes, conn)
		logger.Infof("stripe pipe %d: local=%s → remote=%s", i, conn.LocalAddr(), serverAddr)
	}

	// Register each pipe with the server (with retries for NAT traversal)
	for retry := 0; retry < stripeRegisterRetries; retry++ {
		for i, pipe := range scc.pipes {
			regPayload := make([]byte, 6)
			binary.BigEndian.PutUint32(regPayload[0:4], sessionID)
			regPayload[4] = uint8(i)
			regPayload[5] = uint8(len(scc.pipes))

			pkt := make([]byte, stripeHdrLen+len(regPayload))
			encodeStripeHdr(pkt, &stripeHdr{
				Magic:   stripeMagic,
				Version: stripeVersion,
				Type:    stripeREGISTER,
				Session: sessionID,
				DataLen: uint16(len(regPayload)),
			})
			copy(pkt[stripeHdrLen:], regPayload)

			if _, err := pipe.WriteToUDP(pkt, serverAddr); err != nil {
				logger.Errorf("stripe: register pipe %d attempt %d failed: %v", i, retry, err)
			}
		}
		if retry < stripeRegisterRetries-1 {
			time.Sleep(stripeRegisterDelay)
		}
	}

	// Start recv goroutines
	for i, pipe := range scc.pipes {
		go scc.recvPipeLoop(ctx, i, pipe)
	}

	// Start keepalive
	go scc.keepaliveLoop(ctx)

	// Flush timer for partial FEC groups
	scc.txTimer = time.AfterFunc(stripeFlushInterval, scc.flushTxGroup)

	logger.Infof("stripe client ready: session=%08x pipes=%d FEC=%d+%d server=%s",
		sessionID, len(scc.pipes), dataK, parityM, serverAddr)

	return scc, nil
}

// SendDatagram queues an IP packet for FEC-encoded striped transmission.
// Implements datagramConn interface.
func (scc *stripeClientConn) SendDatagram(pkt []byte) error {
	scc.txMu.Lock()
	defer scc.txMu.Unlock()

	seq := atomic.AddUint32(&scc.txSeq, 1) - 1
	if len(scc.txGroup) == 0 {
		scc.txGrpSeq = seq
	}

	// Store as [2-byte length prefix][payload] for FEC alignment
	shardData := make([]byte, 2+len(pkt))
	binary.BigEndian.PutUint16(shardData[0:2], uint16(len(pkt)))
	copy(shardData[2:], pkt)
	scc.txGroup = append(scc.txGroup, shardData)

	if len(scc.txGroup) >= scc.dataK {
		scc.sendFECGroupLocked()
	}
	scc.resetFlushTimer()

	atomic.AddUint64(&scc.txPkts, 1)
	atomic.AddUint64(&scc.txBytes, uint64(len(pkt)))
	return nil
}

// ReceiveDatagram waits for a decoded IP packet from the receive pipeline.
// Implements datagramConn interface.
func (scc *stripeClientConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-scc.closeCh:
		return nil, fmt.Errorf("stripe: connection closed")
	case pkt := <-scc.rxCh:
		atomic.AddUint64(&scc.rxPkts, 1)
		atomic.AddUint64(&scc.rxBytes, uint64(len(pkt)))
		return pkt, nil
	}
}

// Close shuts down the stripe connection and all its UDP sockets.
func (scc *stripeClientConn) Close() error {
	scc.closeOnce.Do(func() {
		close(scc.closeCh)
		if scc.txTimer != nil {
			scc.txTimer.Stop()
		}
		for _, pipe := range scc.pipes {
			_ = pipe.Close()
		}
	})
	return nil
}

// ─── Client TX internals ──────────────────────────────────────────────────

// sendFECGroupLocked encodes the accumulated data shards with FEC parity
// and sends all shards round-robin across pipes. Caller must hold txMu.
func (scc *stripeClientConn) sendFECGroupLocked() {
	K := len(scc.txGroup)
	if K == 0 {
		return
	}

	groupSeq := scc.txGrpSeq

	// Find max shard size for FEC alignment
	maxLen := 0
	for _, s := range scc.txGroup {
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}

	// Create padded data shards
	shards := make([][]byte, K)
	for i, s := range scc.txGroup {
		padded := make([]byte, maxLen)
		copy(padded, s)
		shards[i] = padded
	}

	// Compute FEC parity for full groups only
	var parityShards [][]byte
	if scc.enc != nil && K == scc.dataK {
		total := K + scc.parityM
		allShards := make([][]byte, total)
		copy(allShards[:K], shards)
		for i := K; i < total; i++ {
			allShards[i] = make([]byte, maxLen)
		}
		if err := scc.enc.Encode(allShards); err != nil {
			scc.logger.Errorf("stripe: FEC encode error: %v", err)
		} else {
			parityShards = allShards[K:]
		}
	}

	// Send data shards round-robin across pipes
	groupDataN := uint8(K)
	for i, shard := range shards {
		pkt := make([]byte, stripeHdrLen+len(shard))
		encodeStripeHdr(pkt, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripeDATA,
			Session:    scc.sessionID,
			GroupSeq:   groupSeq,
			ShardIdx:   uint8(i),
			GroupDataN: groupDataN,
			DataLen:    binary.BigEndian.Uint16(scc.txGroup[i][:2]),
		})
		copy(pkt[stripeHdrLen:], shard)
		scc.sendToPipe(pkt)
	}

	// Send parity shards
	for i, shard := range parityShards {
		pkt := make([]byte, stripeHdrLen+len(shard))
		encodeStripeHdr(pkt, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripePARITY,
			Session:    scc.sessionID,
			GroupSeq:   groupSeq,
			ShardIdx:   uint8(K + i),
			GroupDataN: groupDataN,
			DataLen:    0,
		})
		copy(pkt[stripeHdrLen:], shard)
		scc.sendToPipe(pkt)
	}

	scc.txGroup = scc.txGroup[:0]
}

func (scc *stripeClientConn) flushTxGroup() {
	scc.txMu.Lock()
	defer scc.txMu.Unlock()
	if len(scc.txGroup) > 0 {
		scc.sendFECGroupLocked()
	}
	scc.resetFlushTimer()
}

func (scc *stripeClientConn) resetFlushTimer() {
	if scc.txTimer != nil {
		scc.txTimer.Reset(stripeFlushInterval)
	}
}

func (scc *stripeClientConn) sendToPipe(pkt []byte) {
	idx := atomic.AddUint32(&scc.txPipe, 1) - 1
	pipe := scc.pipes[int(idx)%len(scc.pipes)]
	_, _ = pipe.WriteToUDP(pkt, scc.serverAddr)
}

// ─── Client RX internals ──────────────────────────────────────────────────

func (scc *stripeClientConn) recvPipeLoop(ctx context.Context, pipeIdx int, conn *net.UDPConn) {
	buf := make([]byte, 65535)
	for {
		select {
		case <-ctx.Done():
			return
		case <-scc.closeCh:
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-scc.closeCh:
				return
			default:
			}
			scc.logger.Debugf("stripe: pipe %d recv error: %v", pipeIdx, err)
			continue
		}

		if n < stripeHdrLen {
			continue
		}

		hdr, ok := decodeStripeHdr(buf[:n])
		if !ok {
			continue
		}

		payload := buf[stripeHdrLen:n]

		switch hdr.Type {
		case stripeDATA:
			scc.handleRxShard(hdr, payload, false)
		case stripePARITY:
			scc.handleRxShard(hdr, payload, true)
		case stripeKEEPALIVE:
			// Server keepalive response — good, NAT mapping is alive
		}
	}
}

func (scc *stripeClientConn) handleRxShard(hdr stripeHdr, payload []byte, isParity bool) {
	// Partial group (fewer than K data shards) — deliver data directly, no FEC
	if !isParity && int(hdr.GroupDataN) < scc.dataK {
		scc.deliverDataDirect(hdr, payload)
		return
	}

	if scc.enc == nil || scc.parityM == 0 {
		// No FEC configured — deliver data directly
		if !isParity {
			scc.deliverDataDirect(hdr, payload)
		}
		return
	}

	// FEC mode: accumulate shards in group
	scc.rxMu.Lock()
	grp := scc.rxGroups[hdr.GroupSeq]
	if grp == nil {
		grp = newFECGroup(scc.dataK, scc.parityM)
		scc.rxGroups[hdr.GroupSeq] = grp
	}
	decodable := grp.addShard(int(hdr.ShardIdx), payload)
	scc.rxMu.Unlock()

	if decodable {
		scc.decodeAndDeliver(hdr.GroupSeq, grp)
	}
}

func (scc *stripeClientConn) deliverDataDirect(hdr stripeHdr, payload []byte) {
	if hdr.DataLen == 0 || len(payload) < 2+int(hdr.DataLen) {
		return
	}
	pkt := make([]byte, hdr.DataLen)
	copy(pkt, payload[2:2+hdr.DataLen])

	select {
	case scc.rxCh <- pkt:
	case <-scc.closeCh:
	default:
		// Drop if buffer full (backpressure)
	}
}

func (scc *stripeClientConn) decodeAndDeliver(groupSeq uint32, grp *fecGroup) {
	scc.rxMu.Lock()
	if grp.delivered {
		scc.rxMu.Unlock()
		return
	}
	grp.delivered = true

	// Check if all data shards are present (no reconstruction needed)
	allPresent := true
	for i := 0; i < grp.dataK; i++ {
		if !grp.present[i] {
			allPresent = false
			break
		}
	}

	if allPresent {
		scc.deliverGroupData(grp)
		delete(scc.rxGroups, groupSeq)
		scc.rxMu.Unlock()
		return
	}

	// Need FEC reconstruction — pad shards to maxLen
	for i := range grp.shards {
		if grp.shards[i] != nil && len(grp.shards[i]) < grp.maxLen {
			padded := make([]byte, grp.maxLen)
			copy(padded, grp.shards[i])
			grp.shards[i] = padded
		}
	}
	scc.rxMu.Unlock()

	if err := scc.enc.Reconstruct(grp.shards); err != nil {
		scc.logger.Debugf("stripe: FEC reconstruct failed group=%d: %v", groupSeq, err)
		return
	}

	atomic.AddUint64(&scc.fecRecov, 1)

	scc.rxMu.Lock()
	scc.deliverGroupData(grp)
	delete(scc.rxGroups, groupSeq)
	scc.rxMu.Unlock()
}

// deliverGroupData extracts IP packets from decoded data shards and pushes to rxCh.
// Caller must hold rxMu.
func (scc *stripeClientConn) deliverGroupData(grp *fecGroup) {
	for i := 0; i < grp.dataK; i++ {
		if grp.shards[i] == nil || len(grp.shards[i]) < 2 {
			continue
		}
		dataLen := binary.BigEndian.Uint16(grp.shards[i][:2])
		if dataLen == 0 || int(dataLen)+2 > len(grp.shards[i]) {
			continue
		}
		pkt := make([]byte, dataLen)
		copy(pkt, grp.shards[i][2:2+dataLen])
		select {
		case scc.rxCh <- pkt:
		case <-scc.closeCh:
			return
		default:
			// Drop if full
		}
	}
}

// ─── Client keepalive ─────────────────────────────────────────────────────

func (scc *stripeClientConn) keepaliveLoop(ctx context.Context) {
	ticker := time.NewTicker(stripeKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scc.closeCh:
			return
		case <-ticker.C:
			for _, pipe := range scc.pipes {
				pkt := make([]byte, stripeHdrLen)
				encodeStripeHdr(pkt, &stripeHdr{
					Magic:   stripeMagic,
					Version: stripeVersion,
					Type:    stripeKEEPALIVE,
					Session: scc.sessionID,
				})
				_, _ = pipe.WriteToUDP(pkt, scc.serverAddr)
			}
		}
	}
}

// ─── Client RX group GC ──────────────────────────────────────────────────

// gcRxGroups removes stale incomplete FEC groups to prevent memory leaks.
func (scc *stripeClientConn) gcRxGroups() {
	scc.rxMu.Lock()
	defer scc.rxMu.Unlock()
	now := time.Now()
	for seq, grp := range scc.rxGroups {
		if now.Sub(grp.created) > 2*time.Second {
			// Deliver whatever we have and discard
			if !grp.delivered {
				grp.delivered = true
				scc.deliverGroupData(grp)
			}
			delete(scc.rxGroups, seq)
		}
	}
}

// ─── Stripe Server ────────────────────────────────────────────────────────

// stripeSession holds per-client state on the server side.
type stripeSession struct {
	sessionID  uint32
	peerIP     netip.Addr
	pipes      []*net.UDPAddr // client pipe addresses, filled by REGISTER
	totalPipes int
	registered int

	// FEC
	dataK   int
	parityM int
	enc     reedsolomon.Encoder

	// RX (client → server): FEC decode → TUN
	rxGroups map[uint32]*fecGroup
	rxMu     sync.Mutex
	rxCh     chan []byte // decoded IP packets delivered to tunWriter

	// TX (server → client): FEC encode + stripe
	txSeq    uint32 // atomic
	txPipe   uint32 // atomic
	txGroup  [][]byte
	txGrpSeq uint32
	txMu     sync.Mutex
	txTimer  *time.Timer

	lastActivity time.Time
	logger       *Logger
}

// stripeServerDC implements datagramConn for the server→client return path.
// It is registered in the connectionTable so that dispatch() can route
// TUN packets back to the stripe client.
type stripeServerDC struct {
	session *stripeSession
	conn    *net.UDPConn // server's UDP listener socket
}

// SendDatagram FEC-encodes and sends an IP packet to the client via stripe.
func (sdc *stripeServerDC) SendDatagram(pkt []byte) error {
	sess := sdc.session
	sess.txMu.Lock()
	defer sess.txMu.Unlock()

	seq := atomic.AddUint32(&sess.txSeq, 1) - 1
	if len(sess.txGroup) == 0 {
		sess.txGrpSeq = seq
	}

	shardData := make([]byte, 2+len(pkt))
	binary.BigEndian.PutUint16(shardData[0:2], uint16(len(pkt)))
	copy(shardData[2:], pkt)
	sess.txGroup = append(sess.txGroup, shardData)

	if len(sess.txGroup) >= sess.dataK {
		sdc.sendFECGroupLocked()
	}
	sdc.resetFlushTimer()

	return nil
}

// ReceiveDatagram reads from the session's decoded packet channel.
// Not typically used for server stripe (tunWriter reads rxCh directly),
// but provided for datagramConn interface completeness.
func (sdc *stripeServerDC) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case pkt, ok := <-sdc.session.rxCh:
		if !ok {
			return nil, fmt.Errorf("stripe session closed")
		}
		return pkt, nil
	}
}

// sendFECGroupLocked encodes + sends accumulated shards. Caller must hold sess.txMu.
func (sdc *stripeServerDC) sendFECGroupLocked() {
	sess := sdc.session
	K := len(sess.txGroup)
	if K == 0 {
		return
	}

	// Collect active pipe addresses
	activePipes := make([]*net.UDPAddr, 0, len(sess.pipes))
	for _, p := range sess.pipes {
		if p != nil {
			activePipes = append(activePipes, p)
		}
	}
	if len(activePipes) == 0 {
		sess.txGroup = sess.txGroup[:0]
		return
	}

	groupSeq := sess.txGrpSeq

	// Find max shard size
	maxLen := 0
	for _, s := range sess.txGroup {
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}

	// Pad data shards
	shards := make([][]byte, K)
	for i, s := range sess.txGroup {
		padded := make([]byte, maxLen)
		copy(padded, s)
		shards[i] = padded
	}

	// Compute FEC parity for full groups
	var parityShards [][]byte
	if sess.enc != nil && K == sess.dataK {
		total := K + sess.parityM
		allShards := make([][]byte, total)
		copy(allShards[:K], shards)
		for i := K; i < total; i++ {
			allShards[i] = make([]byte, maxLen)
		}
		if err := sess.enc.Encode(allShards); err != nil {
			sess.logger.Errorf("stripe server: FEC encode error: %v", err)
		} else {
			parityShards = allShards[K:]
		}
	}

	groupDataN := uint8(K)

	// Send data shards round-robin across client pipes
	for i, shard := range shards {
		pkt := make([]byte, stripeHdrLen+len(shard))
		encodeStripeHdr(pkt, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripeDATA,
			Session:    sess.sessionID,
			GroupSeq:   groupSeq,
			ShardIdx:   uint8(i),
			GroupDataN: groupDataN,
			DataLen:    binary.BigEndian.Uint16(sess.txGroup[i][:2]),
		})
		copy(pkt[stripeHdrLen:], shard)

		pipeIdx := int(atomic.AddUint32(&sess.txPipe, 1)-1) % len(activePipes)
		_, _ = sdc.conn.WriteToUDP(pkt, activePipes[pipeIdx])
	}

	// Send parity shards
	for i, shard := range parityShards {
		pkt := make([]byte, stripeHdrLen+len(shard))
		encodeStripeHdr(pkt, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripePARITY,
			Session:    sess.sessionID,
			GroupSeq:   groupSeq,
			ShardIdx:   uint8(K + i),
			GroupDataN: groupDataN,
			DataLen:    0,
		})
		copy(pkt[stripeHdrLen:], shard)

		pipeIdx := int(atomic.AddUint32(&sess.txPipe, 1)-1) % len(activePipes)
		_, _ = sdc.conn.WriteToUDP(pkt, activePipes[pipeIdx])
	}

	sess.txGroup = sess.txGroup[:0]
}

func (sdc *stripeServerDC) resetFlushTimer() {
	sess := sdc.session
	if sess.txTimer != nil {
		sess.txTimer.Reset(stripeFlushInterval)
	}
}

// ─── Stripe Server Listener ──────────────────────────────────────────────

// stripeServer manages the server-side UDP listener for stripe connections.
// Multiple clients can connect; each is identified by session ID.
type stripeServer struct {
	conn       *net.UDPConn
	sessions   map[uint32]*stripeSession
	addrToSess map[string]uint32 // "IP:port" → sessionID
	mu         sync.RWMutex

	tun     *water.Interface
	ct      *connectionTable
	dataK   int
	parityM int
	logger  *Logger
	closeCh chan struct{}
}

// newStripeServer creates and starts the server-side stripe listener.
func newStripeServer(cfg *Config, tun *water.Interface, ct *connectionTable, logger *Logger) (*stripeServer, error) {
	bindIP, err := resolveBindIP(cfg.BindIP)
	if err != nil {
		return nil, fmt.Errorf("stripe server: resolve bind: %w", err)
	}

	stripePort := cfg.StripePort
	if stripePort == 0 {
		stripePort = cfg.RemotePort + 1000
	}

	listenAddr := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: stripePort}
	conn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("stripe server: listen %s: %w", listenAddr, err)
	}

	dataK := cfg.StripeDataShards
	if dataK <= 0 {
		dataK = stripeDefaultDataShards
	}
	parityM := cfg.StripeParityShards
	if parityM < 0 {
		parityM = stripeDefaultParityShards
	}

	ss := &stripeServer{
		conn:       conn,
		sessions:   make(map[uint32]*stripeSession),
		addrToSess: make(map[string]uint32),
		tun:        tun,
		ct:         ct,
		dataK:      dataK,
		parityM:    parityM,
		logger:     logger,
		closeCh:    make(chan struct{}),
	}

	logger.Infof("stripe server listening on %s, FEC=%d+%d", listenAddr, dataK, parityM)
	return ss, nil
}

// Run is the main receive loop of the stripe server. Call in a goroutine.
func (ss *stripeServer) Run(ctx context.Context) {
	// Periodic GC for stale sessions and incomplete FEC groups
	go ss.gcLoop(ctx)

	buf := make([]byte, 65535)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ss.closeCh:
			return
		default:
		}

		ss.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, from, err := ss.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-ss.closeCh:
				return
			default:
			}
			ss.logger.Errorf("stripe server: recv error: %v", err)
			continue
		}

		if n < stripeHdrLen {
			continue
		}

		hdr, ok := decodeStripeHdr(buf[:n])
		if !ok {
			continue
		}

		payload := make([]byte, n-stripeHdrLen)
		copy(payload, buf[stripeHdrLen:n])

		switch hdr.Type {
		case stripeREGISTER:
			ss.handleRegister(hdr, payload, from)
		case stripeDATA:
			ss.handleDataShard(hdr, payload, from)
		case stripePARITY:
			ss.handleParityShard(hdr, payload, from)
		case stripeKEEPALIVE:
			ss.handleKeepalive(hdr, from)
		}
	}
}

func (ss *stripeServer) handleRegister(hdr stripeHdr, payload []byte, from *net.UDPAddr) {
	if len(payload) < 6 {
		return
	}

	peerIP := netip.AddrFrom4([4]byte{payload[0], payload[1], payload[2], payload[3]})
	pipeIdx := int(payload[4])
	totalPipes := int(payload[5])
	sessionID := hdr.Session

	ss.mu.Lock()
	defer ss.mu.Unlock()

	sess, exists := ss.sessions[sessionID]
	if !exists {
		var enc reedsolomon.Encoder
		if ss.parityM > 0 {
			var err error
			enc, err = reedsolomon.New(ss.dataK, ss.parityM)
			if err != nil {
				ss.logger.Errorf("stripe server: FEC init session %08x: %v", sessionID, err)
				return
			}
		}

		rxCh := make(chan []byte, 512)
		sess = &stripeSession{
			sessionID:    sessionID,
			peerIP:       peerIP,
			pipes:        make([]*net.UDPAddr, totalPipes),
			totalPipes:   totalPipes,
			dataK:        ss.dataK,
			parityM:      ss.parityM,
			enc:          enc,
			rxGroups:     make(map[uint32]*fecGroup),
			rxCh:         rxCh,
			txGroup:      make([][]byte, 0, ss.dataK),
			lastActivity: time.Now(),
			logger:       ss.logger,
		}
		ss.sessions[sessionID] = sess

		// Create server-to-client datagramConn and register in connectionTable
		sdc := &stripeServerDC{session: sess, conn: ss.conn}
		sess.txTimer = time.AfterFunc(stripeFlushInterval, func() {
			sess.txMu.Lock()
			if len(sess.txGroup) > 0 {
				sdc.sendFECGroupLocked()
			}
			sdc.resetFlushTimer()
			sess.txMu.Unlock()
		})

		_, cancel := context.WithCancel(context.Background())
		ss.ct.registerStripe(peerIP, fmt.Sprintf("stripe:%08x", sessionID), sdc, cancel)
		ss.logger.Infof("stripe session created: peer=%s session=%08x pipes=%d", peerIP, sessionID, totalPipes)

		// Start goroutine writing decoded packets to TUN
		go ss.tunWriter(sess)
	}

	if pipeIdx >= 0 && pipeIdx < len(sess.pipes) {
		sess.pipes[pipeIdx] = from
		sess.registered++
		ss.addrToSess[from.String()] = sessionID
		ss.logger.Infof("stripe pipe registered: session=%08x pipe=%d/%d from=%s",
			sessionID, pipeIdx, totalPipes, from)
	}

	sess.lastActivity = time.Now()

	// Reply with keepalive to confirm (punches NAT)
	reply := make([]byte, stripeHdrLen)
	encodeStripeHdr(reply, &stripeHdr{
		Magic:   stripeMagic,
		Version: stripeVersion,
		Type:    stripeKEEPALIVE,
		Session: sessionID,
	})
	_, _ = ss.conn.WriteToUDP(reply, from)
}

func (ss *stripeServer) tunWriter(sess *stripeSession) {
	for pkt := range sess.rxCh {
		// Learn routes for return traffic (like runServerMultiConnTunnel does)
		if len(pkt) >= 20 {
			version := pkt[0] >> 4
			if version == 4 {
				srcIP := netip.AddrFrom4([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]})
				if srcIP != sess.peerIP {
					ss.ct.learnRoute(srcIP, sess.peerIP)
				}
			}
		}
		if _, err := ss.tun.Write(pkt); err != nil {
			ss.logger.Errorf("stripe: TUN write error: %v", err)
		}
	}
}

func (ss *stripeServer) handleDataShard(hdr stripeHdr, payload []byte, from *net.UDPAddr) {
	sess := ss.lookupSession(hdr.Session, from)
	if sess == nil {
		return
	}
	sess.lastActivity = time.Now()

	// Partial group or no FEC: deliver directly
	if int(hdr.GroupDataN) < ss.dataK || ss.parityM == 0 || sess.enc == nil {
		if hdr.DataLen > 0 && len(payload) >= 2+int(hdr.DataLen) {
			pkt := make([]byte, hdr.DataLen)
			copy(pkt, payload[2:2+hdr.DataLen])
			select {
			case sess.rxCh <- pkt:
			default:
			}
		}
		return
	}

	// FEC mode: accumulate
	sess.rxMu.Lock()
	grp := sess.rxGroups[hdr.GroupSeq]
	if grp == nil {
		grp = newFECGroup(sess.dataK, sess.parityM)
		sess.rxGroups[hdr.GroupSeq] = grp
	}
	decodable := grp.addShard(int(hdr.ShardIdx), payload)
	sess.rxMu.Unlock()

	if decodable {
		ss.decodeAndDeliver(sess, hdr.GroupSeq, grp)
	}
}

func (ss *stripeServer) handleParityShard(hdr stripeHdr, payload []byte, from *net.UDPAddr) {
	sess := ss.lookupSession(hdr.Session, from)
	if sess == nil || sess.enc == nil {
		return
	}
	sess.lastActivity = time.Now()

	sess.rxMu.Lock()
	grp := sess.rxGroups[hdr.GroupSeq]
	if grp == nil {
		grp = newFECGroup(sess.dataK, sess.parityM)
		sess.rxGroups[hdr.GroupSeq] = grp
	}
	decodable := grp.addShard(int(hdr.ShardIdx), payload)
	sess.rxMu.Unlock()

	if decodable {
		ss.decodeAndDeliver(sess, hdr.GroupSeq, grp)
	}
}

func (ss *stripeServer) decodeAndDeliver(sess *stripeSession, groupSeq uint32, grp *fecGroup) {
	sess.rxMu.Lock()
	if grp.delivered {
		sess.rxMu.Unlock()
		return
	}
	grp.delivered = true

	allPresent := true
	for i := 0; i < grp.dataK; i++ {
		if !grp.present[i] {
			allPresent = false
			break
		}
	}

	if allPresent {
		ss.deliverGroupToTUN(sess, grp)
		delete(sess.rxGroups, groupSeq)
		sess.rxMu.Unlock()
		return
	}

	// FEC reconstruction needed
	for i := range grp.shards {
		if grp.shards[i] != nil && len(grp.shards[i]) < grp.maxLen {
			padded := make([]byte, grp.maxLen)
			copy(padded, grp.shards[i])
			grp.shards[i] = padded
		}
	}
	sess.rxMu.Unlock()

	if err := sess.enc.Reconstruct(grp.shards); err != nil {
		ss.logger.Debugf("stripe server: FEC reconstruct failed group=%d: %v", groupSeq, err)
		return
	}

	sess.rxMu.Lock()
	ss.deliverGroupToTUN(sess, grp)
	delete(sess.rxGroups, groupSeq)
	sess.rxMu.Unlock()
}

// deliverGroupToTUN extracts IP packets from data shards and pushes to session.rxCh.
// Caller must hold sess.rxMu.
func (ss *stripeServer) deliverGroupToTUN(sess *stripeSession, grp *fecGroup) {
	for i := 0; i < grp.dataK; i++ {
		if grp.shards[i] == nil || len(grp.shards[i]) < 2 {
			continue
		}
		dataLen := binary.BigEndian.Uint16(grp.shards[i][:2])
		if dataLen == 0 || int(dataLen)+2 > len(grp.shards[i]) {
			continue
		}
		pkt := make([]byte, dataLen)
		copy(pkt, grp.shards[i][2:2+dataLen])
		select {
		case sess.rxCh <- pkt:
		default:
			// Drop if buffer full
		}
	}
}

func (ss *stripeServer) handleKeepalive(hdr stripeHdr, from *net.UDPAddr) {
	sess := ss.lookupSession(hdr.Session, from)
	if sess != nil {
		sess.lastActivity = time.Now()
	}
	// Reply
	reply := make([]byte, stripeHdrLen)
	encodeStripeHdr(reply, &stripeHdr{
		Magic:   stripeMagic,
		Version: stripeVersion,
		Type:    stripeKEEPALIVE,
		Session: hdr.Session,
	})
	_, _ = ss.conn.WriteToUDP(reply, from)
}

// lookupSession finds a session by header session ID or source address.
func (ss *stripeServer) lookupSession(sessionID uint32, from *net.UDPAddr) *stripeSession {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	// Try by source address first (faster, handles NAT)
	if sessID, ok := ss.addrToSess[from.String()]; ok {
		if sess, ok := ss.sessions[sessID]; ok {
			return sess
		}
	}
	// Fallback: by session ID from header
	return ss.sessions[sessionID]
}

// gcLoop periodically cleans up stale sessions and incomplete FEC groups.
func (ss *stripeServer) gcLoop(ctx context.Context) {
	ticker := time.NewTicker(stripeGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ss.closeCh:
			return
		case <-ticker.C:
			ss.runGC()
		}
	}
}

func (ss *stripeServer) runGC() {
	now := time.Now()

	ss.mu.Lock()
	defer ss.mu.Unlock()

	for sessID, sess := range ss.sessions {
		// Expire sessions inactive for > timeout
		if now.Sub(sess.lastActivity) > stripeSessionTimeout {
			ss.logger.Infof("stripe: expiring session %08x peer=%s (inactive %v)",
				sessID, sess.peerIP, now.Sub(sess.lastActivity).Round(time.Second))
			if sess.txTimer != nil {
				sess.txTimer.Stop()
			}
			close(sess.rxCh)
			// Remove addr→session mappings
			for addr, sid := range ss.addrToSess {
				if sid == sessID {
					delete(ss.addrToSess, addr)
				}
			}
			// Unregister from connectionTable
			ss.ct.unregisterConn(sess.peerIP, fmt.Sprintf("stripe:%08x", sessID))
			delete(ss.sessions, sessID)
			continue
		}

		// GC incomplete FEC groups older than 2s
		sess.rxMu.Lock()
		for seq, grp := range sess.rxGroups {
			if now.Sub(grp.created) > 2*time.Second {
				if !grp.delivered {
					grp.delivered = true
					ss.deliverGroupToTUN(sess, grp)
				}
				delete(sess.rxGroups, seq)
			}
		}
		sess.rxMu.Unlock()
	}
}

// Close stops the stripe server.
func (ss *stripeServer) Close() error {
	close(ss.closeCh)
	return ss.conn.Close()
}

// ─── Helpers ──────────────────────────────────────────────────────────────

// parseTUNIP extracts the IP address from a CIDR string like "10.200.17.1/30".
func parseTUNIP(cidr string) (net.IP, error) {
	host, _, err := net.ParseCIDR(cidr)
	if err != nil {
		ip := net.ParseIP(cidr)
		if ip == nil {
			return nil, fmt.Errorf("invalid TUN CIDR: %s", cidr)
		}
		return ip, nil
	}
	return host, nil
}

// ipToUint32 converts an IPv4 address to a uint32 for use as session ID.
func ipToUint32(ip net.IP) uint32 {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip4)
}

// pathSessionID generates a unique session ID per (TUN IP, path name) pair.
// This ensures that multiple stripe paths from the same client (e.g. wan5 and
// wan6) get distinct sessions on the server, so their pipes don't collide.
func pathSessionID(tunIP net.IP, pathName string) uint32 {
	base := ipToUint32(tunIP)
	h := fnv.New32a()
	h.Write([]byte(pathName))
	return base ^ h.Sum32()
}

// Ensure stripeClientConn implements io.Closer for clean shutdown.
var _ io.Closer = (*stripeClientConn)(nil)
