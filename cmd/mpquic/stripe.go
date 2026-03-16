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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/reedsolomon"
	"github.com/songgao/water"
	"golang.org/x/net/ipv4"
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
	stripeNACK        uint8 = 0x05
	stripeXOR_REPAIR  uint8 = 0x06

	// Header: magic(2) + ver(1) + type(1) + session(4) + groupSeq(4) + shardIdx(1) + groupDataN(1) + dataLen(2) = 16
	stripeHdrLen = 16

	// Defaults
	stripeDefaultDataShards   = 10
	stripeDefaultParityShards = 2
	stripeMaxPayload          = 1500
	stripeFlushInterval       = 5 * time.Millisecond
	stripeKeepaliveInterval   = 5 * time.Second
	stripeSessionTimeout      = 30 * time.Second
	stripeBatchSize           = 8 // recvmmsg batch size (matches quic-go)
	stripeSocketBufSize       = 7 << 20 // 7 MB per socket (matches quic-go)
	stripeGCInterval          = 10 * time.Second
	stripeRegisterRetries     = 3
	stripeRegisterDelay       = 500 * time.Millisecond

	// Adaptive FEC: loss threshold to enable/disable parity
	adaptiveFECLossThreshold uint8 = 2   // enable parity when peer loss > 2%
	adaptiveFECCooldown            = 15 * time.Second // stay at M>0 for at least this long after peer loss
)

// ─── Pacing (Token Bucket) ────────────────────────────────────────────────
// stripePacer is a token-bucket rate limiter that prevents burst-induced
// retransmits by spreading UDP writes over time. NOT thread-safe — must be
// called under the caller's TX mutex (txMu).

const (
	stripePacerBurstMs  = 2     // burst window in milliseconds
	stripePacerMinBurst = 32768 // 32 KB minimum burst
)

type stripePacer struct {
	rateBPS    float64   // target rate in bytes/second
	burstBytes float64   // max token accumulation
	tokens     float64   // current available bytes (may go negative)
	lastRefill time.Time // last refill timestamp
}

func newStripePacer(rateMbps int) *stripePacer {
	if rateMbps <= 0 {
		return nil
	}
	rateBPS := float64(rateMbps) * 1e6 / 8.0
	burst := rateBPS * float64(stripePacerBurstMs) / 1000.0
	if burst < stripePacerMinBurst {
		burst = stripePacerMinBurst
	}
	return &stripePacer{
		rateBPS:    rateBPS,
		burstBytes: burst,
		tokens:     burst,
		lastRefill: time.Now(),
	}
}

// pace blocks until sufficient tokens are available to send `bytes` bytes.
// Must be called under the session/connection TX mutex.
func (p *stripePacer) pace(bytes int) {
	if p == nil {
		return
	}
	now := time.Now()
	elapsed := now.Sub(p.lastRefill).Seconds()
	p.lastRefill = now

	// Refill tokens based on elapsed time.
	p.tokens += elapsed * p.rateBPS
	if p.tokens > p.burstBytes {
		p.tokens = p.burstBytes
	}

	// Consume tokens.
	p.tokens -= float64(bytes)

	// If in deficit, sleep until tokens would be replenished.
	if p.tokens < 0 {
		deficit := -p.tokens
		sleepDur := time.Duration(deficit / p.rateBPS * float64(time.Second))
		if sleepDur > time.Microsecond {
			time.Sleep(sleepDur)
			// The sleep covered the deficit — reset tokens to zero so
			// subsequent calls don't re-sleep for the same debt.
			p.tokens = 0
			p.lastRefill = time.Now()
		}
		p.tokens = 0
	}
}

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
	tunIPU32   uint32 // TUN IP as uint32 for periodic re-register

	dataK   int
	parityM int
	enc     reedsolomon.Encoder // nil if parityM == 0

	// Adaptive FEC
	fecMode   string // "always", "adaptive", "off"
	adaptiveM int32  // atomic: current TX parity M (0..parityM)

	// XOR FEC (sliding-window, alternative to RS)
	fecType string          // "rs" (default) or "xor"
	xorTx   *xorFECSender   // XOR TX accumulator (nil if fecType != "xor")
	xorRx   *xorFECReceiver // XOR RX recovery buffer (nil if fecType != "xor")

	// Pacing
	pacer *stripePacer // TX rate limiter (nil = disabled)

	// Hybrid ARQ
	arqTx *arqTxBuf     // TX retransmit buffer (nil = ARQ disabled)
	arqRx *arqRxTracker // RX gap detector + NACK generator

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

	// RX loss tracking (measures loss on data FROM server → used to tell server to adjust its TX M)
	rxSeqHighest   uint64 // atomic: highest GroupSeq seen (M=0 loss detection)
	rxDirectCount  uint64 // atomic: data shards delivered via deliverDataDirect
	rxFECGroups    uint64 // atomic: total FEC groups completed (M>0)
	// fecRecov is also used (existing field)

	// Peer-reported loss (loss on data WE send, reported BY server → we adjust our TX M)
	peerLossRate uint32 // atomic: 0-100
	lastPeerLoss int64  // atomic: unix-nano of last nonzero peer loss report

	// Loss computation: previous window values (updated each keepalive cycle)
	rxLossPrevSeqHigh    uint64
	rxLossPrevDirectCnt  uint64
	rxLossPrevFECRecov   uint64
	rxLossPrevFECGroups  uint64

	// GSO TX batch (UDP Generic Segmentation Offload).
	// Per-pipe accumulation: encrypted wire packets are buffered and flushed
	// as one sendmsg with UDP_SEGMENT, reducing syscall overhead by N×.
	gsoEnabled  bool
	gsoDisabled uint32          // atomic: 1 = runtime fallback (NIC lacks TX csum)
	gsoBufs     []gsoTxPipeBuf  // one per pipe

	// Kernel TX pacing (SO_TXTIME + sch_fq).
	// Each pipe socket has SO_TXTIME enabled; per-sendmsg SCM_TXTIME cmsg
	// carries an EDT (Earliest Departure Time) so sch_fq holds the packet
	// until that instant.  Replaces the software stripePacer with nanosecond
	// kernel-level precision.
	txtimeEnabled bool               // SO_TXTIME probed OK on first pipe
	txtimeEDT     []int64            // per-pipe next EDT (ns, CLOCK_MONOTONIC)
	txtimeGapNs   int64              // inter-packet gap (ns) derived from pacing rate

	// Stats (atomic)
	txPkts   uint64
	rxPkts   uint64
	txBytes  uint64
	rxBytes  uint64
	fecRecov uint64
	lastRx   int64 // unix-nano timestamp of last received packet (atomic)

	closeCh   chan struct{}
	closeOnce sync.Once
	logger    *Logger

	txCipher *stripeCipher // client→server encryption
	rxCipher *stripeCipher // server→client decryption

	securityDecryptFail uint64
}

// gsoTxPipeBuf accumulates encrypted wire packets for a single pipe.
// All segments must be the same size (the kernel splits at segSize boundaries).
// If a packet of different size arrives, the buffer is flushed first.
type gsoTxPipeBuf struct {
	buf     []byte
	count   int
	segSize int
}

// SecurityStats returns the decrypt failure counter.
func (scc *stripeClientConn) SecurityStats() uint64 {
	return atomic.LoadUint64(&scc.securityDecryptFail)
}


// newStripeClientConn creates a stripe transport for a single multipath path.
// It opens N UDP sockets on the specified interface, all pointed at the server's
// stripe port. Each socket = one Starlink session = one ~80 Mbps allocation.
// Each path gets a unique session ID so FEC groups stay within a single path's
// pipes, while the server connectionTable balances TX across multiple sessions.
// setStripeSocketBuffers sets OS-level read and write buffer sizes on a
// UDP socket to 7 MB (matching quic-go). Large buffers prevent kernel-level
// packet drops during burst arrivals on Starlink (where jitter spikes can
// pause delivery for 50+ ms, then deliver a burst of hundreds of packets).
func setStripeSocketBuffers(conn *net.UDPConn, logger *Logger) {
	if err := conn.SetReadBuffer(stripeSocketBufSize); err != nil {
		logger.Errorf("stripe: SetReadBuffer(%d): %v", stripeSocketBufSize, err)
	}
	if err := conn.SetWriteBuffer(stripeSocketBufSize); err != nil {
		logger.Errorf("stripe: SetWriteBuffer(%d): %v", stripeSocketBufSize, err)
	}
}

// bindPipeToDevice sets SO_BINDTODEVICE on a UDP socket so that all
// outgoing packets are forced through the named interface, bypassing
// source-based policy routing. Requires CAP_NET_RAW or root.
func bindPipeToDevice(conn *net.UDPConn, ifname string) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("SyscallConn: %w", err)
	}
	var sysErr error
	if err := rawConn.Control(func(fd uintptr) {
		sysErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifname)
	}); err != nil {
		return fmt.Errorf("Control: %w", err)
	}
	return sysErr
}

func newStripeClientConn(ctx context.Context, cfg *Config, pathCfg MultipathPathConfig, keys *stripeKeyMaterial, logger *Logger) (*stripeClientConn, error) {
	pipes := pathCfg.Pipes
	if pipes <= 1 {
		pipes = 4
	}

	bindIP, err := resolveBindIP(pathCfg.BindIP)
	if err != nil {
		return nil, fmt.Errorf("stripe: resolve bind: %w", err)
	}

	// Extract interface name for SO_BINDTODEVICE (e.g. "if:enp7s7" → "enp7s7")
	var ifName string
	if strings.HasPrefix(pathCfg.BindIP, "if:") {
		ifName = strings.TrimPrefix(pathCfg.BindIP, "if:")
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

	fecMode := cfg.StripeFECMode
	if fecMode == "" {
		fecMode = "always"
	}

	fecType := cfg.StripeFECType
	if fecType == "" {
		fecType = "rs"
	}
	fecWindow := cfg.StripeFECWindow
	if fecWindow <= 0 {
		fecWindow = xorFECDefaultWindow
	}

	// In "off" mode, force M=0 and skip encoder creation.
	// In "adaptive" mode, create encoder but start with adaptiveM=0.
	// When fecType="xor", RS encoder is still created (kept as fallback)
	// but the M=0 fast path is used for data with XOR repair alongside.
	var enc reedsolomon.Encoder
	if fecMode == "off" {
		parityM = 0
	} else if fecType == "xor" {
		// XOR mode: RS encoder not needed, force M=0 so data goes through fast path.
		// XOR repair packets are generated separately.
		parityM = 0
	} else if parityM > 0 {
		enc, err = reedsolomon.New(dataK, parityM)
		if err != nil {
			return nil, fmt.Errorf("stripe: FEC encoder: %w", err)
		}
	}

	var initialAdaptiveM int32
	if fecMode == "adaptive" {
		initialAdaptiveM = 0 // start with no parity
	} else {
		initialAdaptiveM = int32(parityM)
	}

	// Create AES-256-GCM ciphers from TLS-exported key material
	txCipher, err := newStripeCipher(keys.c2sKey)
	if err != nil {
		return nil, fmt.Errorf("stripe: TX cipher: %w", err)
	}
	rxCipher, err := newStripeCipher(keys.s2cKey)
	if err != nil {
		return nil, fmt.Errorf("stripe: RX cipher: %w", err)
	}

	scc := &stripeClientConn{
		serverAddr: serverAddr,
		sessionID:  sessionID,
		tunIPU32:   ipToUint32(tunIP),
		dataK:      dataK,
		parityM:    parityM,
		enc:        enc,
		fecMode:    fecMode,
		fecType:    fecType,
		txGroup:    make([][]byte, 0, dataK),
		rxCh:       make(chan []byte, 512),
		rxGroups:   make(map[uint32]*fecGroup),
		closeCh:    make(chan struct{}),
		logger:     logger,
		txCipher:   txCipher,
		rxCipher:   rxCipher,
	}
	atomic.StoreInt32(&scc.adaptiveM, initialAdaptiveM)
	atomic.StoreInt64(&scc.lastRx, time.Now().UnixNano())
	scc.pacer = newStripePacer(cfg.StripePacingRate)

	// XOR FEC: create sender/receiver when fec_type=xor and not off
	if fecType == "xor" && fecMode != "off" {
		scc.xorTx = newXorFECSender(fecWindow)
		scc.xorRx = newXorFECReceiver(fecWindow)
	}

	if cfg.StripeARQ {
		scc.arqTx = &arqTxBuf{}
		scc.arqRx = newArqRxTracker()
	}

	// Open N UDP sockets bound to the same interface
	for i := 0; i < pipes; i++ {
		laddr := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
		conn, err := net.ListenUDP("udp4", laddr)
		if err != nil {
			scc.Close()
			return nil, fmt.Errorf("stripe: listen pipe %d: %w", i, err)
		}
		setStripeSocketBuffers(conn, logger)
		if ifName != "" {
			if err := bindPipeToDevice(conn, ifName); err != nil {
				logger.Errorf("stripe: SO_BINDTODEVICE pipe %d to %s: %v", i, ifName, err)
				// non-fatal: proceed without device binding
			}
		}
		scc.pipes = append(scc.pipes, conn)
		logger.Infof("stripe pipe %d: local=%s → remote=%s dev=%s", i, conn.LocalAddr(), serverAddr, ifName)
	}

	// Probe GSO (UDP_SEGMENT) support on the first pipe.
	// If supported, allocate per-pipe accumulation buffers for batch TX.
	if !cfg.StripeDisableGSO && len(scc.pipes) > 0 && stripeGSOProbe(scc.pipes[0]) {
		scc.gsoEnabled = true
		scc.gsoBufs = make([]gsoTxPipeBuf, len(scc.pipes))
		for i := range scc.gsoBufs {
			scc.gsoBufs[i].buf = make([]byte, 0, 65536)
		}
	}

	// Probe SO_TXTIME kernel pacing on the first pipe.
	// If supported, enable SO_TXTIME on ALL pipes and compute inter-packet gap.
	// Kernel pacing replaces the software stripePacer with nanosecond-precision
	// sch_fq scheduling, eliminating burst-induced retransmits.
	if cfg.StripePacingRate > 0 && len(scc.pipes) > 0 && stripeTxtimeProbe(scc.pipes[0]) {
		numPipes := len(scc.pipes)
		rateBytesPerPipe := uint64(cfg.StripePacingRate) * 1e6 / 8 / uint64(numPipes)
		// Typical shard: stripeHdrLen + 2 + MTU + AES-GCM overhead ≈ 1402 bytes
		scc.txtimeGapNs = int64(float64(1402*8) / (float64(cfg.StripePacingRate) * 1e6 / float64(numPipes)) * 1e9)
		scc.txtimeEDT = make([]int64, numPipes)
		allOK := true
		for i, pipe := range scc.pipes {
			if err := stripeTxtimeSetup(pipe, rateBytesPerPipe); err != nil {
				logger.Errorf("stripe: SO_TXTIME pipe %d failed: %v (disabling kernel pacing)", i, err)
				allOK = false
				break
			}
		}
		if allOK {
			scc.txtimeEnabled = true
			// Kernel pacing supersedes the software token-bucket pacer.
			scc.pacer = nil
		}
	}

	// Register each pipe with the server (with retries for NAT traversal)
	var totalSendOK int
	for retry := 0; retry < stripeRegisterRetries; retry++ {
		// Check context before each retry round
		if ctx.Err() != nil {
			scc.Close()
			return nil, ctx.Err()
		}
		for i, pipe := range scc.pipes {
			regPayload := make([]byte, 6)
			binary.BigEndian.PutUint32(regPayload[0:4], ipToUint32(tunIP))
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
			pkt = stripeEncrypt(scc.txCipher, pkt)

			if _, err := pipe.WriteToUDP(pkt, serverAddr); err != nil {
				logger.Errorf("stripe: register pipe %d attempt %d failed: %v", i, retry, err)
			} else {
				totalSendOK++
			}
		}
		if retry < stripeRegisterRetries-1 {
			select {
			case <-ctx.Done():
				scc.Close()
				return nil, ctx.Err()
			case <-time.After(stripeRegisterDelay):
			}
		}
	}
	if totalSendOK == 0 {
		scc.Close()
		return nil, fmt.Errorf("stripe: all register sends failed (0/%d×%d)", len(scc.pipes), stripeRegisterRetries)
	}

	// Start recv goroutines
	for i, pipe := range scc.pipes {
		go scc.recvPipeLoop(ctx, i, pipe)
	}

	// Start keepalive
	go scc.keepaliveLoop(ctx)

	// Start ARQ NACK generation loop if enabled
	if scc.arqRx != nil {
		go scc.arqNackLoop(ctx)
	}

	// Flush timer for partial FEC groups
	scc.txTimer = time.AfterFunc(stripeFlushInterval, scc.flushTxGroup)

	pacingStr := "off"
	if scc.txtimeEnabled {
		pacingStr = fmt.Sprintf("kernel@%dMbps(gap=%dns)", cfg.StripePacingRate, scc.txtimeGapNs)
	} else if cfg.StripePacingRate > 0 {
		pacingStr = fmt.Sprintf("sw@%dMbps", cfg.StripePacingRate)
	}
	arqStr := "off"
	if cfg.StripeARQ {
		arqStr = "on"
	}
	gsoStr := "off"
	if scc.gsoEnabled {
		gsoStr = "on"
	}
	txtimeStr := "off"
	if scc.txtimeEnabled {
		txtimeStr = "on"
	}
	fecStr := fmt.Sprintf("FEC=%d+%d mode=%s type=%s", dataK, parityM, fecMode, fecType)
	if fecType == "xor" && scc.xorTx != nil {
		fecStr = fmt.Sprintf("FEC=xor W=%d mode=%s", scc.xorTx.window, fecMode)
	}
	logger.Infof("stripe client ready: session=%08x pipes=%d %s pacing=%s arq=%s gso=%s txtime=%s server=%s encrypted=AES-256-GCM",
		sessionID, len(scc.pipes), fecStr, pacingStr, arqStr, gsoStr, txtimeStr, serverAddr)

	return scc, nil
}

// SendDatagram queues an IP packet for FEC-encoded striped transmission.
// Implements datagramConn interface.
func (scc *stripeClientConn) SendDatagram(pkt []byte) error {
	scc.txMu.Lock()
	defer scc.txMu.Unlock()

	effectiveM := scc.getEffectiveM()

	// ── M=0 fast path: send each packet directly, no grouping/padding/parity ──
	if effectiveM == 0 {
		seq := atomic.AddUint32(&scc.txSeq, 1) - 1

		shardData := make([]byte, 2+len(pkt))
		binary.BigEndian.PutUint16(shardData[0:2], uint16(len(pkt)))
		copy(shardData[2:], pkt)

		wirePkt := stripeEncryptShard(scc.txCipher, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripeDATA,
			Session:    scc.sessionID,
			GroupSeq:   seq,
			ShardIdx:   0,
			GroupDataN: 1, // signals RX to deliver directly (< K)
			DataLen:    uint16(len(pkt)),
		}, shardData)

		// ARQ: store plaintext in retransmit buffer before sending
		if scc.arqTx != nil {
			scc.arqTx.store(seq, shardData, uint16(len(pkt)))
		}

		if scc.pacer != nil {
			scc.pacer.pace(len(wirePkt))
		}
		idx := atomic.AddUint32(&scc.txPipe, 1) - 1
		pipeIdx := int(idx) % len(scc.pipes)
		if scc.gsoEnabled && atomic.LoadUint32(&scc.gsoDisabled) == 0 {
			scc.gsoAccumLocked(pipeIdx, wirePkt)
		} else {
			scc.writePacedUDP(pipeIdx, wirePkt)
		}

		atomic.AddUint64(&scc.txPkts, 1)
		atomic.AddUint64(&scc.txBytes, uint64(len(pkt)))

		// XOR FEC: feed source to accumulator, emit repair when window completes.
		if scc.xorTx != nil {
			if repair, firstSeq, ok := scc.xorTx.addSource(seq, shardData); ok {
				scc.sendXorRepairLocked(firstSeq, uint8(scc.xorTx.window), repair)
			}
		}

		return nil
	}

	// ── M>0 path: accumulate in txGroup, FEC encode when full ──
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
//
// Hot path — optimised to minimise heap allocations:
//   - data shards alias txGroup entries when all are the same length (zero-copy);
//   - stripeEncryptShard builds the wire packet in 1 allocation (header+crypto+payload).
func (scc *stripeClientConn) sendFECGroupLocked() {
	K := len(scc.txGroup)
	if K == 0 {
		return
	}

	groupSeq := scc.txGrpSeq

	// Find max shard size for FEC alignment.
	maxLen := 0
	for _, s := range scc.txGroup {
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}

	// Build padded data shards. When all shards are the same length (typical
	// for MTU-sized IP packets) we alias the txGroup entries directly, saving
	// K heap allocations.
	shards := make([][]byte, K)
	allSameLen := true
	for _, s := range scc.txGroup {
		if len(s) != maxLen {
			allSameLen = false
			break
		}
	}
	if allSameLen {
		for i, s := range scc.txGroup {
			shards[i] = s
		}
	} else {
		for i, s := range scc.txGroup {
			padded := make([]byte, maxLen)
			copy(padded, s)
			shards[i] = padded
		}
	}

	// Compute FEC parity for full groups only.
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

	// Send data shards round-robin across pipes (1 alloc per shard).
	groupDataN := uint8(K)
	gsoActive := scc.gsoEnabled && atomic.LoadUint32(&scc.gsoDisabled) == 0
	for i, shard := range shards {
		wirePkt := stripeEncryptShard(scc.txCipher, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripeDATA,
			Session:    scc.sessionID,
			GroupSeq:   groupSeq,
			ShardIdx:   uint8(i),
			GroupDataN: groupDataN,
			DataLen:    binary.BigEndian.Uint16(scc.txGroup[i][:2]),
		}, shard)
		if scc.pacer != nil {
			scc.pacer.pace(len(wirePkt))
		}
		idx := atomic.AddUint32(&scc.txPipe, 1) - 1
		pipeIdx := int(idx) % len(scc.pipes)
		if gsoActive {
			scc.gsoAccumLocked(pipeIdx, wirePkt)
		} else {
			scc.writePacedUDP(pipeIdx, wirePkt)
		}
	}

	// Send parity shards (1 alloc per shard).
	for i, shard := range parityShards {
		wirePkt := stripeEncryptShard(scc.txCipher, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripePARITY,
			Session:    scc.sessionID,
			GroupSeq:   groupSeq,
			ShardIdx:   uint8(K + i),
			GroupDataN: groupDataN,
			DataLen:    0,
		}, shard)
		if scc.pacer != nil {
			scc.pacer.pace(len(wirePkt))
		}
		idx := atomic.AddUint32(&scc.txPipe, 1) - 1
		pipeIdx := int(idx) % len(scc.pipes)
		if gsoActive {
			scc.gsoAccumLocked(pipeIdx, wirePkt)
		} else {
			scc.writePacedUDP(pipeIdx, wirePkt)
		}
	}

	scc.txGroup = scc.txGroup[:0]
}

// sendXorRepairLocked encrypts and sends an XOR FEC repair packet.
// Caller must hold txMu.
func (scc *stripeClientConn) sendXorRepairLocked(firstSeq uint32, window uint8, repairData []byte) {
	wirePkt := stripeEncryptShard(scc.txCipher, &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeXOR_REPAIR,
		Session:    scc.sessionID,
		GroupSeq:   firstSeq,
		ShardIdx:   0,
		GroupDataN: window,
		DataLen:    0,
	}, repairData)
	if scc.pacer != nil {
		scc.pacer.pace(len(wirePkt))
	}
	idx := atomic.AddUint32(&scc.txPipe, 1) - 1
	pipeIdx := int(idx) % len(scc.pipes)
	if scc.gsoEnabled && atomic.LoadUint32(&scc.gsoDisabled) == 0 {
		scc.gsoAccumLocked(pipeIdx, wirePkt)
	} else {
		scc.writePacedUDP(pipeIdx, wirePkt)
	}
}

func (scc *stripeClientConn) flushTxGroup() {
	// Don't flush if connection is closing/closed
	select {
	case <-scc.closeCh:
		return
	default:
	}
	scc.txMu.Lock()
	defer scc.txMu.Unlock()
	if len(scc.txGroup) > 0 {
		scc.sendFECGroupLocked()
	}
	// Flush partial XOR window (emit repair for accumulated sources).
	if scc.xorTx != nil {
		if repair, firstSeq, window, ok := scc.xorTx.flush(); ok {
			scc.sendXorRepairLocked(firstSeq, uint8(window), repair)
		}
	}
	// Flush any GSO-accumulated packets from the FEC group above.
	scc.gsoFlushAllLocked()
	scc.resetFlushTimer()
}

func (scc *stripeClientConn) resetFlushTimer() {
	if scc.txTimer != nil {
		scc.txTimer.Reset(stripeFlushInterval)
	}
}

// getEffectiveM returns the current parity shard count based on FEC mode.
func (scc *stripeClientConn) getEffectiveM() int {
	switch scc.fecMode {
	case "off":
		return 0
	case "adaptive":
		return int(atomic.LoadInt32(&scc.adaptiveM))
	default: // "always"
		return scc.parityM
	}
}

// ─── Client GSO TX (UDP Generic Segmentation Offload) ─────────────────────
//
// Instead of N individual WriteToUDP syscalls per pipe, GSO concatenates
// encrypted wire packets into one contiguous buffer and sends them in a
// single sendmsg with UDP_SEGMENT ancillary data. The kernel splits the
// buffer at segment boundaries, producing N individual UDP datagrams.
// This reduces TX syscall overhead by up to N× on the client hot path.
//
// All GSO methods require txMu to be held by the caller.

// gsoAccumLocked appends an encrypted wire packet to the per-pipe GSO buffer.
// If the new packet's size differs from the current segment size, the buffer
// is flushed first (GSO requires uniform segment sizes).
func (scc *stripeClientConn) gsoAccumLocked(pipeIdx int, wirePkt []byte) {
	gb := &scc.gsoBufs[pipeIdx]
	if gb.count > 0 && len(wirePkt) != gb.segSize {
		scc.gsoFlushPipeLocked(pipeIdx)
	}
	if gb.count == 0 {
		gb.segSize = len(wirePkt)
	}
	gb.buf = append(gb.buf, wirePkt...)
	gb.count++
}

// gsoFlushPipeLocked sends all accumulated segments for one pipe.
// Single-segment buffers use WriteMsgUDP (possibly with SCM_TXTIME).
// Multi-segment buffers use WriteMsgUDP with UDP_SEGMENT cmsg (+ SCM_TXTIME).
// On EIO (NIC lacks TX checksum offload), GSO is permanently disabled
// and packets are resent individually.
func (scc *stripeClientConn) gsoFlushPipeLocked(pipeIdx int) {
	gb := &scc.gsoBufs[pipeIdx]
	if gb.count == 0 {
		return
	}
	pipe := scc.pipes[pipeIdx]

	if gb.count == 1 {
		// Single segment — no GSO overhead.
		if scc.txtimeEnabled {
			edt := scc.txtimeNextEDT(pipeIdx, 1)
			oob := stripeTxtimeBuildOOB(edt)
			_, _, _ = pipe.WriteMsgUDP(gb.buf, oob, scc.serverAddr)
		} else {
			_, _ = pipe.WriteToUDP(gb.buf, scc.serverAddr)
		}
	} else {
		// GSO: single sendmsg, kernel splits at segSize boundaries.
		oob := stripeGSOBuildOOB(uint16(gb.segSize))
		if scc.txtimeEnabled {
			edt := scc.txtimeNextEDT(pipeIdx, gb.count)
			oob = stripeTxtimeAppendOOB(oob, edt)
		}
		_, _, err := pipe.WriteMsgUDP(gb.buf, oob, scc.serverAddr)
		if err != nil && stripeGSOIsError(err) {
			atomic.StoreUint32(&scc.gsoDisabled, 1)
			scc.logger.Errorf("stripe: GSO sendmsg returned EIO — disabling GSO, falling back to per-packet TX")
			// Resend all segments individually.
			for off := 0; off < len(gb.buf); off += gb.segSize {
				end := off + gb.segSize
				if end > len(gb.buf) {
					end = len(gb.buf)
				}
				_, _ = pipe.WriteToUDP(gb.buf[off:end], scc.serverAddr)
			}
		}
	}

	gb.buf = gb.buf[:0]
	gb.count = 0
	gb.segSize = 0
}

// txtimeNextEDT returns the next Earliest Departure Time for a pipe and
// advances the per-pipe EDT counter by numPkts * txtimeGapNs.
// Ensures the EDT is never in the past (clamps to now + small delta).
// Caller must hold txMu.
func (scc *stripeClientConn) txtimeNextEDT(pipeIdx int, numPkts int) int64 {
	now := monoNowNs()
	edt := scc.txtimeEDT[pipeIdx]
	if edt < now {
		edt = now + 1000 // 1 µs ahead to avoid immediate delivery
	}
	scc.txtimeEDT[pipeIdx] = edt + int64(numPkts)*scc.txtimeGapNs
	return edt
}

// writePacedUDP sends a single packet via a pipe, using SCM_TXTIME when
// kernel pacing is active — otherwise falls back to plain WriteToUDP.
// Caller must hold txMu.
func (scc *stripeClientConn) writePacedUDP(pipeIdx int, pkt []byte) {
	pipe := scc.pipes[pipeIdx]
	if scc.txtimeEnabled {
		edt := scc.txtimeNextEDT(pipeIdx, 1)
		oob := stripeTxtimeBuildOOB(edt)
		_, _, _ = pipe.WriteMsgUDP(pkt, oob, scc.serverAddr)
	} else {
		_, _ = pipe.WriteToUDP(pkt, scc.serverAddr)
	}
}

// gsoFlushAllLocked flushes all per-pipe GSO buffers. Caller must hold txMu.
func (scc *stripeClientConn) gsoFlushAllLocked() {
	if !scc.gsoEnabled {
		return
	}
	for i := range scc.gsoBufs {
		scc.gsoFlushPipeLocked(i)
	}
}

// FlushTxBatch flushes all per-pipe GSO buffers. Thread-safe.
// Called by drainSendCh after batch-draining the send channel.
// Implements txBatcher interface.
func (scc *stripeClientConn) FlushTxBatch() {
	scc.txMu.Lock()
	scc.gsoFlushAllLocked()
	scc.txMu.Unlock()
}

// getEffectiveM returns the current parity shard count for a server session.
func (sdc *stripeServerDC) getEffectiveM() int {
	sess := sdc.session
	switch sess.fecMode {
	case "off":
		return 0
	case "adaptive":
		return int(atomic.LoadInt32(&sess.adaptiveM))
	default: // "always"
		return sess.parityM
	}
}

// ─── TX Batch (sendmmsg) ──────────────────────────────────────────────────
// Server-side batch TX: accumulates encrypted wire packets and flushes them
// via ipv4.PacketConn.WriteBatch (sendmmsg), reducing per-packet syscall
// overhead by up to 8×. All batch methods require txMu to be held.

// txBatchAddLocked enqueues a wire packet for batched transmission.
// Auto-flushes when the batch reaches stripeBatchSize.
// When kernel pacing (SO_TXTIME) is active, each message gets an SCM_TXTIME
// cmsg with the session's next EDT, spreading the batch over time.
// Caller must hold sess.txMu.
func (sdc *stripeServerDC) txBatchAddLocked(wirePkt []byte, addr *net.UDPAddr) {
	sess := sdc.session
	n := sess.txBatchN
	sess.txBatchMsgs[n].Buffers[0] = wirePkt
	sess.txBatchMsgs[n].Addr = addr
	if sess.txtimeEnabled {
		edt := sdc.txtimeNextEDT(1)
		sess.txBatchMsgs[n].OOB = stripeTxtimeBuildOOB(edt)
	}
	sess.txBatchN = n + 1
	if sess.txBatchN >= stripeBatchSize {
		sdc.txBatchFlushLocked()
	}
}

// txtimeNextEDT returns the next EDT for the session and advances the counter.
// Caller must hold sess.txMu.
func (sdc *stripeServerDC) txtimeNextEDT(numPkts int) int64 {
	sess := sdc.session
	now := monoNowNs()
	edt := sess.txtimeEDT
	if edt < now {
		edt = now + 1000 // 1 µs ahead
	}
	sess.txtimeEDT = edt + int64(numPkts)*sess.txtimeGapNs
	return edt
}

// txBatchFlushLocked sends all accumulated messages via sendmmsg.
// Caller must hold sess.txMu.
func (sdc *stripeServerDC) txBatchFlushLocked() {
	sess := sdc.session
	if sess.txBatchN == 0 {
		return
	}
	_, _ = sess.txBatchPC.WriteBatch(sess.txBatchMsgs[:sess.txBatchN], 0)
	// Clear OOB references to avoid stale SCM_TXTIME on reused slots.
	if sess.txtimeEnabled {
		for i := 0; i < sess.txBatchN; i++ {
			sess.txBatchMsgs[i].OOB = nil
		}
	}
	sess.txBatchN = 0
}

// FlushTxBatch flushes any pending TX batch. Thread-safe.
// Called by drainSendCh after batch-draining the send channel.
// Implements txBatcher interface.
func (sdc *stripeServerDC) FlushTxBatch() {
	sess := sdc.session
	sess.txMu.Lock()
	sdc.txBatchFlushLocked()
	sess.txMu.Unlock()
}

// sendToPipe encrypts and sends a pre-built stripe packet to a pipe (round-robin).
// Used only by low-frequency paths (keepalive, register); the FEC TX hot path
// calls stripeEncryptShard directly to avoid the intermediate cleartext buffer.
func (scc *stripeClientConn) sendToPipe(pkt []byte) {
	idx := atomic.AddUint32(&scc.txPipe, 1) - 1
	pipe := scc.pipes[int(idx)%len(scc.pipes)]
	pkt = stripeEncrypt(scc.txCipher, pkt)
	_, _ = pipe.WriteToUDP(pkt, scc.serverAddr)
}

// ─── Client RX internals ──────────────────────────────────────────────────

func (scc *stripeClientConn) recvPipeLoop(ctx context.Context, pipeIdx int, conn *net.UDPConn) {
	// ── Batch RX: use recvmmsg to read up to stripeBatchSize packets per syscall ──
	pc := ipv4.NewPacketConn(conn)
	msgs := make([]ipv4.Message, stripeBatchSize)
	for i := range msgs {
		msgs[i].Buffers = make([][]byte, 1)
		msgs[i].Buffers[0] = make([]byte, 65535)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-scc.closeCh:
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		numMsgs, err := pc.ReadBatch(msgs, 0)
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

		for mi := 0; mi < numMsgs; mi++ {
			n := msgs[mi].N
			if n < stripeHdrLen {
				continue
			}

			raw := msgs[mi].Buffers[0][:n]
			if scc.rxCipher != nil {
				decrypted, decOK := stripeDecryptPkt(scc.rxCipher.aead, raw)
				if !decOK {
					count := atomic.AddUint64(&scc.securityDecryptFail, 1)
					if count <= 3 || count%1000 == 0 {
						scc.logger.Errorf("stripe: pipe %d decrypt FAILED (total=%d pktLen=%d)", pipeIdx, count, n)
					}
					continue
				}
				raw = decrypted
			}

			hdr, ok := decodeStripeHdr(raw)
			if !ok {
				continue
			}

			payload := raw[stripeHdrLen:]

			atomic.StoreInt64(&scc.lastRx, time.Now().UnixNano())

			switch hdr.Type {
			case stripeDATA:
				scc.handleRxShard(hdr, payload, false)
			case stripePARITY:
				scc.handleRxShard(hdr, payload, true)
			case stripeKEEPALIVE:
				// Server keepalive response — read peer loss rate if present
				if len(payload) >= 1 {
					peerLoss := uint32(payload[0])
					atomic.StoreUint32(&scc.peerLossRate, peerLoss)
					if peerLoss > 0 {
						atomic.StoreInt64(&scc.lastPeerLoss, time.Now().UnixNano())
					}
				}
			case stripeNACK:
				scc.handleNack(hdr, payload)
			case stripeXOR_REPAIR:
				scc.handleXorRepair(hdr, payload)
			}
		}
	}
}

func (scc *stripeClientConn) handleRxShard(hdr stripeHdr, payload []byte, isParity bool) {
	// ── Adaptive FEC: track RX sequence for loss detection ──
	if !isParity {
		// Update highest GroupSeq seen (atomic CAS loop)
		for {
			old := atomic.LoadUint64(&scc.rxSeqHighest)
			newSeq := uint64(hdr.GroupSeq)
			if newSeq <= old {
				break
			}
			if atomic.CompareAndSwapUint64(&scc.rxSeqHighest, old, newSeq) {
				break
			}
		}
	}

	// Partial group (fewer than K data shards) — deliver data directly, no FEC
	if !isParity && int(hdr.GroupDataN) < scc.dataK {
		// ARQ: mark this sequence as received for gap detection;
		// if already received (duplicate from ARQ retransmit), skip TUN delivery.
		if scc.arqRx != nil {
			if !scc.arqRx.markReceived(hdr.GroupSeq) {
				scc.arqRx.addDupFiltered(1)
				return // dedup: already delivered
			}
			// Gap-filling: if this seq is below the previously seen highest,
			// it was out-of-order or a NACK-retransmit that filled a gap.
			if uint64(hdr.GroupSeq) < atomic.LoadUint64(&scc.rxSeqHighest) {
				scc.arqRx.addRetxReceived(1)
			}
		}
		scc.deliverDataDirect(hdr, payload)
		// XOR FEC: store source shard for potential recovery.
		if scc.xorRx != nil {
			scc.xorRx.storeShard(hdr.GroupSeq, payload)
		}
		return
	}

	if scc.enc == nil || scc.parityM == 0 {
		// No FEC configured — deliver data directly
		if !isParity {
			scc.deliverDataDirect(hdr, payload)
			// XOR FEC: store source shard for potential recovery.
			if scc.xorRx != nil {
				scc.xorRx.storeShard(hdr.GroupSeq, payload)
			}
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

	atomic.AddUint64(&scc.rxDirectCount, 1)

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
		atomic.AddUint64(&scc.rxFECGroups, 1)
		return
	}

	// Need FEC reconstruction — snapshot and pad shards under lock.
	shards := make([][]byte, len(grp.shards))
	for i := range grp.shards {
		if grp.shards[i] != nil {
			padded := make([]byte, grp.maxLen)
			copy(padded, grp.shards[i])
			shards[i] = padded
		}
	}
	delete(scc.rxGroups, groupSeq)
	scc.rxMu.Unlock()

	if err := scc.enc.Reconstruct(shards); err != nil {
		scc.logger.Debugf("stripe: FEC reconstruct failed group=%d: %v", groupSeq, err)
		return
	}

	atomic.AddUint64(&scc.fecRecov, 1)
	atomic.AddUint64(&scc.rxFECGroups, 1)

	scc.rxMu.Lock()
	grp.shards = shards
	scc.deliverGroupData(grp)
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

// handleXorRepair processes a received XOR FEC repair packet.
// Attempts to recover a single missing source packet from the window.
func (scc *stripeClientConn) handleXorRepair(hdr stripeHdr, payload []byte) {
	if scc.xorRx == nil || len(payload) == 0 {
		return
	}
	W := int(hdr.GroupDataN)
	if W <= 0 {
		return
	}
	pkt, recoveredSeq, ok := scc.xorRx.tryRecover(hdr.GroupSeq, W, payload)
	if !ok || pkt == nil {
		return
	}
	// Deliver recovered IP packet.
	atomic.AddUint64(&scc.fecRecov, 1)
	// ARQ: mark recovered seq as received so it won't be NACKed.
	if scc.arqRx != nil {
		scc.arqRx.markReceived(recoveredSeq)
		scc.arqRx.addRetxReceived(1)
	}
	select {
	case scc.rxCh <- pkt:
	case <-scc.closeCh:
	default:
	}
}

// ─── Client keepalive ─────────────────────────────────────────────────────

func (scc *stripeClientConn) keepaliveLoop(ctx context.Context) {
	ticker := time.NewTicker(stripeKeepaliveInterval)
	defer ticker.Stop()
	var tickCount int
	for {
		select {
		case <-ctx.Done():
			return
		case <-scc.closeCh:
			return
		case <-ticker.C:
			tickCount++

			// Check for session timeout — server may have restarted
			last := time.Unix(0, atomic.LoadInt64(&scc.lastRx))
			if time.Since(last) > stripeSessionTimeout {
				scc.logger.Errorf("stripe: session %08x timeout (no rx for %v), closing",
					scc.sessionID, time.Since(last).Round(time.Second))
				_ = scc.Close()
				return
			}

			// ── Compute RX loss for this window ──
			rxLoss := scc.computeRxLoss()

			// ── Update our TX M based on peer's loss report ──
			scc.updateAdaptiveM()

			// ── Periodic re-register (every 30s) for self-healing ──
			// If the server lost pipe addresses (re-key race, GC, etc.),
			// this ensures pipe mappings are refreshed without a full restart.
			if tickCount%6 == 0 {
				for i, pipe := range scc.pipes {
					regPayload := make([]byte, 6)
					binary.BigEndian.PutUint32(regPayload[0:4], scc.tunIPU32)
					regPayload[4] = uint8(i)
					regPayload[5] = uint8(len(scc.pipes))
					pkt := make([]byte, stripeHdrLen+len(regPayload))
					encodeStripeHdr(pkt, &stripeHdr{
						Magic:   stripeMagic,
						Version: stripeVersion,
						Type:    stripeREGISTER,
						Session: scc.sessionID,
						DataLen: uint16(len(regPayload)),
					})
					copy(pkt[stripeHdrLen:], regPayload)
					pkt = stripeEncrypt(scc.txCipher, pkt)
					_, _ = pipe.WriteToUDP(pkt, scc.serverAddr)
				}
			}

			for i, pipe := range scc.pipes {
				// Keepalive payload: [pipe_index: 1B][rx_loss_pct: 1B]
				pkt := make([]byte, stripeHdrLen+2)
				encodeStripeHdr(pkt, &stripeHdr{
					Magic:   stripeMagic,
					Version: stripeVersion,
					Type:    stripeKEEPALIVE,
					Session: scc.sessionID,
				})
				pkt[stripeHdrLen] = byte(i)
				pkt[stripeHdrLen+1] = rxLoss
				pkt = stripeEncrypt(scc.txCipher, pkt)
				_, _ = pipe.WriteToUDP(pkt, scc.serverAddr)
			}
		}
	}
}

// ─── Adaptive FEC: loss computation and M adjustment (client) ─────────────

// computeRxLoss computes the client-side RX loss rate (loss on data FROM server)
// over the last keepalive window. Returns loss percentage 0-100.
//
// Two detection modes:
//   - M>0 (FEC active): ratio of groups that needed reconstruction vs total groups.
//   - M=0 (no FEC): sequence-gap based — compare highest seq seen vs packets received.
//     This is essential for adaptive FEC bootstrap: without it, M=0→M>0 transition
//     never happens because loss is never detected (chicken-and-egg problem).
func (scc *stripeClientConn) computeRxLoss() uint8 {
	fecRecov := atomic.LoadUint64(&scc.fecRecov)
	fecGroups := atomic.LoadUint64(&scc.rxFECGroups)

	dFECRecov := fecRecov - scc.rxLossPrevFECRecov
	dFECGroups := fecGroups - scc.rxLossPrevFECGroups

	scc.rxLossPrevFECRecov = fecRecov
	scc.rxLossPrevFECGroups = fecGroups

	// Prefer FEC-group based loss when available (more accurate)
	if dFECGroups > 10 {
		rate := dFECRecov * 100 / dFECGroups
		if rate > 100 {
			rate = 100
		}
		return uint8(rate)
	}

	// M=0 fallback: sequence-gap based loss detection.
	// expected = delta in highest seq seen, received = delta in direct-delivered packets.
	seqHigh := atomic.LoadUint64(&scc.rxSeqHighest)
	directCnt := atomic.LoadUint64(&scc.rxDirectCount)

	dSeq := seqHigh - scc.rxLossPrevSeqHigh
	dRecv := directCnt - scc.rxLossPrevDirectCnt

	scc.rxLossPrevSeqHigh = seqHigh
	scc.rxLossPrevDirectCnt = directCnt

	// Need a minimum sample size to avoid false positives from jitter/reordering
	if dSeq > 20 && dRecv <= dSeq {
		lost := dSeq - dRecv
		rate := lost * 100 / dSeq
		if rate > 100 {
			rate = 100
		}
		return uint8(rate)
	}

	return 0
}

// updateAdaptiveM adjusts our TX parity M based on peer's loss feedback.
// Called every keepalive interval.
func (scc *stripeClientConn) updateAdaptiveM() {
	if scc.fecMode != "adaptive" || scc.parityM == 0 {
		return
	}

	peerLoss := atomic.LoadUint32(&scc.peerLossRate)
	currentM := atomic.LoadInt32(&scc.adaptiveM)
	lastLoss := time.Unix(0, atomic.LoadInt64(&scc.lastPeerLoss))

	if peerLoss > uint32(adaptiveFECLossThreshold) {
		// Significant loss reported by peer → enable full parity
		if currentM == 0 {
			atomic.StoreInt32(&scc.adaptiveM, int32(scc.parityM))
			scc.logger.Infof("adaptive FEC: TX M=0→%d (peer reports %d%% loss)", scc.parityM, peerLoss)
		}
	} else if peerLoss == 0 && currentM > 0 {
		// No loss from peer — disable parity after cooldown
		if time.Since(lastLoss) > adaptiveFECCooldown {
			atomic.StoreInt32(&scc.adaptiveM, 0)
			scc.logger.Infof("adaptive FEC: TX M=%d→0 (no peer loss for %v)", currentM, time.Since(lastLoss).Round(time.Second))
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
	// GC XOR FEC receiver buffer.
	if scc.xorRx != nil {
		scc.xorRx.gc()
	}
}

// ─── Client ARQ: NACK handler + generation loop ──────────────────────────

// handleNack processes a NACK from the server requesting retransmission of
// packets the server sent to us. This happens when the *server* detects gaps
// in what *we* sent. We look up our TX ring buffer and retransmit.
func (scc *stripeClientConn) handleNack(hdr stripeHdr, payload []byte) {
	if scc.arqTx == nil {
		return
	}
	baseSeq, bitmap, ok := decodeNackPayload(payload)
	if !ok || bitmap == 0 {
		return
	}

	var retxCount int
	for bit := uint32(0); bit < 64; bit++ {
		if bitmap&(1<<bit) == 0 {
			continue
		}
		seq := baseSeq + bit
		shardData, dataLen, found := scc.arqTx.lookup(seq)
		if !found {
			continue
		}
		// Re-encrypt with fresh nonce and send on round-robin pipe
		wirePkt := stripeEncryptShard(scc.txCipher, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripeDATA,
			Session:    scc.sessionID,
			GroupSeq:   seq,
			ShardIdx:   0,
			GroupDataN: 1,
			DataLen:    dataLen,
		}, shardData)
		idx := atomic.AddUint32(&scc.txPipe, 1) - 1
		pipe := scc.pipes[int(idx)%len(scc.pipes)]
		_, _ = pipe.WriteToUDP(wirePkt, scc.serverAddr)
		retxCount++
	}

	if retxCount > 0 {
		scc.logger.Debugf("stripe ARQ: retransmitted %d packets (base=%d)", retxCount, baseSeq)
	}
}

// arqNackLoop periodically checks for gaps in received sequences and sends
// NACK packets to the server requesting retransmission.
func (scc *stripeClientConn) arqNackLoop(ctx context.Context) {
	ticker := time.NewTicker(arqNackInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scc.closeCh:
			return
		case <-ticker.C:
			if scc.arqRx == nil {
				continue
			}
			// Rate limit: max 1 NACK per RTT (~30ms) to avoid flooding
			if !scc.arqRx.canSendNack() {
				continue
			}
			baseSeq, bitmap, count := scc.arqRx.getMissing()
			if count == 0 {
				continue
			}
			// Build and send NACK packet
			pkt := make([]byte, stripeHdrLen+arqNackPayloadLen)
			encodeStripeHdr(pkt, &stripeHdr{
				Magic:   stripeMagic,
				Version: stripeVersion,
				Type:    stripeNACK,
				Session: scc.sessionID,
			})
			encodeNackPayload(pkt[stripeHdrLen:], baseSeq, bitmap)
			pkt = stripeEncrypt(scc.txCipher, pkt)
			// Send on first active pipe
			if len(scc.pipes) > 0 {
				_, _ = scc.pipes[0].WriteToUDP(pkt, scc.serverAddr)
			}
			scc.arqRx.addNacksSent(1)
			scc.arqRx.recordNackSent()
			scc.logger.Debugf("stripe ARQ: NACK sent base=%d count=%d", baseSeq, count)
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
	txCipher   *stripeCipher // server→client encryption
	rxCipher   *stripeCipher // client→server decryption

	// FEC
	dataK   int
	parityM int
	enc     reedsolomon.Encoder

	// Adaptive FEC
	fecMode   string // "always", "adaptive", "off"
	adaptiveM int32  // atomic: current TX parity M (0..parityM)

	// XOR FEC (sliding-window, alternative to RS)
	fecType string          // "rs" (default) or "xor"
	xorTx   *xorFECSender   // XOR TX accumulator (nil if fecType != "xor")
	xorRx   *xorFECReceiver // XOR RX recovery buffer (nil if fecType != "xor")

	// Pacing
	pacer *stripePacer // TX rate limiter (nil = disabled)

	// TUN multiqueue: per-session write fd for parallel TUN writes.
	// Opened with IFF_MULTI_QUEUE on the same device, so each tunWriter
	// goroutine writes to its own kernel queue without contention.
	tunFd *water.Interface

	// Hybrid ARQ
	arqTx *arqTxBuf     // TX retransmit buffer (nil = ARQ disabled)
	arqRx *arqRxTracker // RX gap detector + NACK generator

	// RX (client → server): FEC decode → TUN
	rxGroups map[uint32]*fecGroup
	rxMu     sync.Mutex
	rxCh     chan []byte // decoded IP packets delivered to tunWriter

	// RX loss tracking (measures loss on data FROM client → reported to client so it adjusts TX M)
	rxSeqHighest   uint64 // atomic
	rxDirectCount  uint64 // atomic
	rxFECGroups    uint64 // atomic
	rxFECRecov     uint64 // atomic

	// Peer-reported loss (loss on data WE send, reported BY client → we adjust our TX M)
	peerLossRate uint32 // atomic: 0-100
	lastPeerLoss int64  // atomic: unix-nano of last nonzero peer loss report

	// Loss computation: previous window values
	rxLossPrevSeqHigh    uint64
	rxLossPrevDirectCnt  uint64
	rxLossPrevFECRecov   uint64
	rxLossPrevFECGroups  uint64

	// TX (server → client): FEC encode + stripe
	txSeq    uint32 // atomic
	txPipe   uint32 // atomic
	txGroup       [][]byte
	txGrpSeq      uint32
	txActivePipes []*net.UDPAddr // cached non-nil pipes, rebuilt on REGISTER (under txMu)
	txMu          sync.Mutex
	txTimer       *time.Timer

	// TX batch (sendmmsg) — reduces per-packet syscall overhead by ~8×.
	// All fields protected by txMu.
	txBatchPC   *ipv4.PacketConn // wraps server listener for WriteBatch
	txBatchMsgs []ipv4.Message   // pre-allocated message slots
	txBatchN    int              // messages in current batch

	// Kernel TX pacing (SO_TXTIME + sch_fq) — per-session EDT tracking.
	// The server socket has SO_TXTIME set once; each message in the sendmmsg
	// batch carries its own SCM_TXTIME cmsg with the next EDT for this session.
	txtimeEnabled bool   // inherited from stripeServer
	txtimeEDT     int64  // next EDT (ns, CLOCK_MONOTONIC) — protected by txMu
	txtimeGapNs   int64  // inter-packet gap derived from pacing rate

	// Metrics (atomic, zero-alloc counters for /metrics and /api/v1/stats)
	txPkts     uint64 // IP packets sent to client
	txBytes    uint64 // IP payload bytes sent to client
	rxPkts     uint64 // IP packets received from client
	rxBytes    uint64 // IP payload bytes received from client
	fecEncoded uint64 // FEC groups encoded (TX)
	createdAt  time.Time

	lastActivity time.Time
	lastTxDrop   time.Time // rate-limit for TX drop logging
	logger       *Logger

	securityDecryptFail uint64
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

	effectiveM := sdc.getEffectiveM()

	// ── M=0 fast path: send each packet directly, no grouping/padding/parity ──
	if effectiveM == 0 {
		activePipes := sess.txActivePipes
		if len(activePipes) == 0 {
			// Rate-limited log: at most once per second per session
			now := time.Now()
			if now.Sub(sess.lastTxDrop) > time.Second {
				sess.lastTxDrop = now
				if sess.logger != nil {
					sess.logger.Infof("stripe: session %08x TX drop (no active pipes), pktLen=%d", sess.sessionID, len(pkt))
				}
			}
			return nil
		}

		seq := atomic.AddUint32(&sess.txSeq, 1) - 1

		shardData := make([]byte, 2+len(pkt))
		binary.BigEndian.PutUint16(shardData[0:2], uint16(len(pkt)))
		copy(shardData[2:], pkt)

		wirePkt := stripeEncryptShard(sess.txCipher, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripeDATA,
			Session:    sess.sessionID,
			GroupSeq:   seq,
			ShardIdx:   0,
			GroupDataN: 1, // signals RX to deliver directly (< K)
			DataLen:    uint16(len(pkt)),
		}, shardData)

		// ARQ: store plaintext in retransmit buffer before sending
		if sess.arqTx != nil {
			sess.arqTx.store(seq, shardData, uint16(len(pkt)))
		}

		if sess.pacer != nil {
			sess.pacer.pace(len(wirePkt))
		}
		pipeIdx := int(atomic.AddUint32(&sess.txPipe, 1)-1) % len(activePipes)
		sdc.txBatchAddLocked(wirePkt, activePipes[pipeIdx])
		atomic.AddUint64(&sess.txPkts, 1)
		atomic.AddUint64(&sess.txBytes, uint64(len(pkt)))

		// XOR FEC: feed source to accumulator, emit repair when window completes.
		if sess.xorTx != nil {
			if repair, firstSeq, ok := sess.xorTx.addSource(seq, shardData); ok {
				sdc.sendXorRepairLocked(firstSeq, uint8(sess.xorTx.window), repair, activePipes)
			}
		}

		return nil
	}

	// ── M>0 path: accumulate in txGroup, FEC encode when full ──
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

	atomic.AddUint64(&sess.txPkts, 1)
	atomic.AddUint64(&sess.txBytes, uint64(len(pkt)))
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
//
// Hot path — mirrors client-side optimisations: zero-copy shard aliasing + single-alloc
// stripeEncryptShard to minimise per-packet heap churn.
func (sdc *stripeServerDC) sendFECGroupLocked() {
	sess := sdc.session
	K := len(sess.txGroup)
	if K == 0 {
		return
	}

	activePipes := sess.txActivePipes
	if len(activePipes) == 0 {
		now := time.Now()
		if now.Sub(sess.lastTxDrop) > time.Second {
			sess.lastTxDrop = now
			if sess.logger != nil {
				sess.logger.Infof("stripe: session %08x FEC TX drop (no active pipes), shards=%d", sess.sessionID, K)
			}
		}
		sess.txGroup = sess.txGroup[:0]
		return
	}

	groupSeq := sess.txGrpSeq

	// Find max shard size.
	maxLen := 0
	for _, s := range sess.txGroup {
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}

	// Build data shards. Zero-copy alias when all are the same length.
	shards := make([][]byte, K)
	allSameLen := true
	for _, s := range sess.txGroup {
		if len(s) != maxLen {
			allSameLen = false
			break
		}
	}
	if allSameLen {
		for i, s := range sess.txGroup {
			shards[i] = s
		}
	} else {
		for i, s := range sess.txGroup {
			padded := make([]byte, maxLen)
			copy(padded, s)
			shards[i] = padded
		}
	}

	// Compute FEC parity for full groups.
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

	// Send data shards round-robin across client pipes (1 alloc per shard).
	for i, shard := range shards {
		wirePkt := stripeEncryptShard(sess.txCipher, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripeDATA,
			Session:    sess.sessionID,
			GroupSeq:   groupSeq,
			ShardIdx:   uint8(i),
			GroupDataN: groupDataN,
			DataLen:    binary.BigEndian.Uint16(sess.txGroup[i][:2]),
		}, shard)

		if sess.pacer != nil {
			sess.pacer.pace(len(wirePkt))
		}
		pipeIdx := int(atomic.AddUint32(&sess.txPipe, 1)-1) % len(activePipes)
		sdc.txBatchAddLocked(wirePkt, activePipes[pipeIdx])
	}

	// Send parity shards (1 alloc per shard).
	for i, shard := range parityShards {
		wirePkt := stripeEncryptShard(sess.txCipher, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripePARITY,
			Session:    sess.sessionID,
			GroupSeq:   groupSeq,
			ShardIdx:   uint8(K + i),
			GroupDataN: groupDataN,
			DataLen:    0,
		}, shard)

		if sess.pacer != nil {
			sess.pacer.pace(len(wirePkt))
		}
		pipeIdx := int(atomic.AddUint32(&sess.txPipe, 1)-1) % len(activePipes)
		sdc.txBatchAddLocked(wirePkt, activePipes[pipeIdx])
	}

	sess.txGroup = sess.txGroup[:0]
	atomic.AddUint64(&sess.fecEncoded, 1)
}

// sendXorRepairLocked encrypts and sends an XOR FEC repair packet (server side).
// Caller must hold sess.txMu.
func (sdc *stripeServerDC) sendXorRepairLocked(firstSeq uint32, window uint8, repairData []byte, activePipes []*net.UDPAddr) {
	sess := sdc.session
	wirePkt := stripeEncryptShard(sess.txCipher, &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeXOR_REPAIR,
		Session:    sess.sessionID,
		GroupSeq:   firstSeq,
		ShardIdx:   0,
		GroupDataN: window,
		DataLen:    0,
	}, repairData)
	if sess.pacer != nil {
		sess.pacer.pace(len(wirePkt))
	}
	pipeIdx := int(atomic.AddUint32(&sess.txPipe, 1)-1) % len(activePipes)
	sdc.txBatchAddLocked(wirePkt, activePipes[pipeIdx])
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

	tun            *water.Interface // primary fd (fallback if multiqueue unavailable)
	tunName        string           // TUN device name for opening multiqueue fds
	tunMultiQueue  bool             // true if TUN was opened with IFF_MULTI_QUEUE
	ct             *connectionTable
	dataK   int
	parityM int
	fecMode    string // "always", "adaptive", "off"
	fecType    string // "rs" (default), "xor"
	fecWindow  int    // XOR window W (only used when fecType=xor)
	pacingRate int    // Mbps per session (0 = disabled)
	arqEnabled bool   // Hybrid ARQ enabled
	txtimeEnabled bool // SO_TXTIME probed OK on listener socket
	logger     *Logger
	closeCh    chan struct{}

	pendingKeys *stripePendingKeys

	securityDecryptFail uint64
}

// newStripeServer creates and starts the server-side stripe listener.
func newStripeServer(cfg *Config, tun *water.Interface, tunMultiQueue bool, ct *connectionTable, pendingKeys *stripePendingKeys, logger *Logger) (*stripeServer, error) {
	tunName := cfg.TunName
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
	setStripeSocketBuffers(conn, logger)

	dataK := cfg.StripeDataShards
	if dataK <= 0 {
		dataK = stripeDefaultDataShards
	}
	parityM := cfg.StripeParityShards
	if parityM < 0 {
		parityM = stripeDefaultParityShards
	}

	fecMode := cfg.StripeFECMode
	if fecMode == "" {
		fecMode = "always"
	}
	fecType := cfg.StripeFECType
	if fecType == "" {
		fecType = "rs"
	}
	fecWindow := cfg.StripeFECWindow
	if fecWindow <= 0 {
		fecWindow = xorFECDefaultWindow
	}
	if fecMode == "off" {
		parityM = 0
	} else if fecType == "xor" {
		// XOR mode: RS not needed, force M=0 so data goes through fast path.
		parityM = 0
	}

	ss := &stripeServer{
		conn:       conn,
		sessions:   make(map[uint32]*stripeSession),
		addrToSess: make(map[string]uint32),
		tun:           tun,
		tunName:       tunName,
		tunMultiQueue: tunMultiQueue,
		ct:            ct,
		dataK:      dataK,
		parityM:    parityM,
		fecMode:    fecMode,
		fecType:    fecType,
		fecWindow:  fecWindow,
		pacingRate: cfg.StripePacingRate,
		arqEnabled: cfg.StripeARQ,
		logger:     logger,
		closeCh:    make(chan struct{}),
		pendingKeys: pendingKeys,
	}

	// Probe SO_TXTIME on the server listener socket.
	// If supported, enable kernel pacing — each session's sendmmsg batch
	// messages will carry SCM_TXTIME with per-session EDT timestamps.
	if cfg.StripePacingRate > 0 && stripeTxtimeProbe(conn) {
		// Rate per session: the actual rate is set when the session is created.
		// Here we just enable SO_TXTIME on the socket (rate=0 → skip SO_MAX_PACING_RATE,
		// rely on per-packet SCM_TXTIME EDT instead).
		if err := stripeTxtimeSetup(conn, 0); err != nil {
			logger.Errorf("stripe server: SO_TXTIME setup failed: %v (using software pacer)", err)
		} else {
			ss.txtimeEnabled = true
		}
	}

	pacingStr := "off"
	if ss.txtimeEnabled {
		pacingStr = fmt.Sprintf("kernel@%dMbps/sess", cfg.StripePacingRate)
	} else if cfg.StripePacingRate > 0 {
		pacingStr = fmt.Sprintf("sw@%dMbps", cfg.StripePacingRate)
	}
	arqStr := "off"
	if cfg.StripeARQ {
		arqStr = "on"
	}
	txtimeStr := "off"
	if ss.txtimeEnabled {
		txtimeStr = "on"
	}
	fecStr := fmt.Sprintf("FEC=%d+%d mode=%s type=%s", dataK, parityM, fecMode, fecType)
	if fecType == "xor" {
		fecStr = fmt.Sprintf("FEC=xor W=%d mode=%s", fecWindow, fecMode)
	}
	logger.Infof("stripe server listening on %s, %s pacing=%s arq=%s txtime=%s encrypted=AES-256-GCM", listenAddr, fecStr, pacingStr, arqStr, txtimeStr)
	return ss, nil
}

// Run is the main receive loop of the stripe server. Call in a goroutine.
// Uses recvmmsg (via ipv4.PacketConn.ReadBatch) to read up to stripeBatchSize
// UDP datagrams per syscall, reducing per-packet overhead on the hot path.
func (ss *stripeServer) Run(ctx context.Context) {
	// Periodic GC for stale sessions and incomplete FEC groups
	go ss.gcLoop(ctx)

	// ── Batch RX: use recvmmsg to read multiple packets per syscall ──
	pc := ipv4.NewPacketConn(ss.conn)
	msgs := make([]ipv4.Message, stripeBatchSize)
	for i := range msgs {
		msgs[i].Buffers = make([][]byte, 1)
		msgs[i].Buffers[0] = make([]byte, 65535)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ss.closeCh:
			return
		default:
		}

		ss.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		numMsgs, err := pc.ReadBatch(msgs, 0)
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

		for mi := 0; mi < numMsgs; mi++ {
			from, ok := msgs[mi].Addr.(*net.UDPAddr)
			if !ok {
				continue
			}
			ss.processIncomingPacket(msgs[mi].Buffers[0][:msgs[mi].N], from)
		}
	}
}

// processIncomingPacket handles decrypt and dispatch of a single raw packet
// received from the batch-read loop. raw must not be retained after return
// (batch buffers are reused across ReadBatch calls).
func (ss *stripeServer) processIncomingPacket(raw []byte, from *net.UDPAddr) {
	n := len(raw)
	if n < stripeHdrLen {
		return
	}

	// Parse header (always cleartext — used as AAD in GCM)
	hdr, ok := decodeStripeHdr(raw)
	if !ok {
		return
	}

	// Decrypt: look up session or pending key
	var payload []byte
	sess := ss.lookupSession(hdr.Session, from)
	if sess != nil && sess.rxCipher != nil {
		decrypted, decOK := stripeDecryptPkt(sess.rxCipher.aead, raw)
		if !decOK {
			// Decrypt failed with current key. Check if client re-keyed
			// (new KX stored in pendingKeys). If so, update ciphers in-place.
			km := ss.pendingKeys.Get(hdr.Session)
			if km != nil {
				tmpCipher, err := newStripeCipher(km.c2sKey)
				if err == nil {
					decrypted2, decOK2 := stripeDecryptPkt(tmpCipher.aead, raw)
					if decOK2 {
						// Re-key succeeded — update session ciphers
						newTx, errTx := newStripeCipher(km.s2cKey)
						if errTx == nil {
							sess.rxCipher = tmpCipher
							// Update txCipher under txMu to prevent data race
							// with concurrent SendDatagram reads.
							sess.txMu.Lock()
							sess.txCipher = newTx
							sess.txActivePipes = nil
							sess.txGroup = sess.txGroup[:0]
							atomic.StoreUint32(&sess.txSeq, 0)
							atomic.StoreUint32(&sess.txPipe, 0)
							sess.txMu.Unlock()

							// Consume the pending key so it doesn't trigger
							// spurious re-keys on future decrypt failures.
							ss.pendingKeys.Delete(hdr.Session)

							ss.logger.Infof("stripe: session %08x re-keyed in-place peer=%s", hdr.Session, sess.peerIP)

							// Re-key means client restarted with new TLS session.
							// Reset ARQ / FEC RX state: the new client starts
							// at txSeq=0 but the old arqRx.base holds the
							// previous client's highest seq, causing all new
							// packets to be rejected as "too old" duplicates.
							if sess.arqRx != nil {
								sess.arqRx = newArqRxTracker()
							}
							if sess.arqTx != nil {
								sess.arqTx = &arqTxBuf{}
							}
							atomic.StoreUint64(&sess.rxSeqHighest, 0)
							atomic.StoreUint64(&sess.rxDirectCount, 0)
							atomic.StoreUint64(&sess.rxFECGroups, 0)
							atomic.StoreUint64(&sess.rxFECRecov, 0)
							sess.rxMu.Lock()
							sess.rxGroups = make(map[uint32]*fecGroup)
							sess.rxMu.Unlock()

							// Clear stale pipe addresses so return traffic doesn't
							// go to dead NAT endpoints from the previous connection.
							ss.mu.Lock()
							for addr, sid := range ss.addrToSess {
								if sid == hdr.Session {
									delete(ss.addrToSess, addr)
								}
							}
							for i := range sess.pipes {
								sess.pipes[i] = nil
							}
							sess.registered = 0
							ss.mu.Unlock()

							// Re-register in connectionTable so the pathConn
							// gets a fresh lastRecv timestamp and drain goroutine.
							// Without this, dispatch() may consider the path stale
							// (lastRecv from before client restart) and skip it,
							// causing return traffic to be silently dropped.
							sdc := &stripeServerDC{session: sess, conn: ss.conn}
							_, cancel := context.WithCancel(context.Background())
							remoteID := fmt.Sprintf("stripe:%08x", hdr.Session)
							ss.ct.registerStripe(sess.peerIP, remoteID, sdc, cancel)
							ss.logger.Infof("stripe: session %08x re-registered in connectionTable", hdr.Session)

							payload = decrypted2[stripeHdrLen:]
							goto dispatch
						}
					}
				}
			}
			atomic.AddUint64(&sess.securityDecryptFail, 1)
			atomic.AddUint64(&ss.securityDecryptFail, 1)
			return
		}
		payload = decrypted[stripeHdrLen:]
	} else if sess == nil {
		// Unknown session — try pre-negotiated key from QUIC KX
		km := ss.pendingKeys.Get(hdr.Session)
		if km == nil {
			return // no key yet; client will retry
		}
		if hdr.Type != stripeREGISTER {
			return // only REGISTER can create sessions
		}
		tmpCipher, err := newStripeCipher(km.c2sKey)
		if err != nil {
			return
		}
		decrypted, decOK := stripeDecryptPkt(tmpCipher.aead, raw)
		if !decOK {
			atomic.AddUint64(&ss.securityDecryptFail, 1)
			return
		}
		payload = decrypted[stripeHdrLen:]
	} else {
		// Session exists but no cipher (shouldn't happen)
		return
	}

dispatch:
	switch hdr.Type {
	case stripeREGISTER:
		ss.handleRegister(hdr, payload, from)
	case stripeDATA:
		ss.handleDataShard(hdr, payload, from)
	case stripePARITY:
		ss.handleParityShard(hdr, payload, from)
	case stripeKEEPALIVE:
		ss.handleKeepalive(hdr, payload, from)
	case stripeNACK:
		ss.handleNack(hdr, payload, from)
	case stripeXOR_REPAIR:
		ss.handleXorRepairServer(hdr, payload, from)
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
		// Create FEC encoder unless mode is "off" or fecType is "xor".
		// In adaptive mode, encoder is needed when M dynamically switches from 0 to parityM.
		if ss.fecMode != "off" && ss.fecType != "xor" && ss.parityM > 0 {
			var err error
			enc, err = reedsolomon.New(ss.dataK, ss.parityM)
			if err != nil {
				ss.logger.Errorf("stripe server: FEC init session %08x: %v", sessionID, err)
				return
			}
		}

		rxCh := make(chan []byte, 512)

		// Get pre-negotiated keys from QUIC TLS Exporter
		km := ss.pendingKeys.Get(sessionID)
		if km == nil {
			ss.logger.Errorf("stripe: register for session %08x with no negotiated key", sessionID)
			return
		}
		txCipher, err := newStripeCipher(km.s2cKey)
		if err != nil {
			ss.logger.Errorf("stripe: TX cipher init session %08x: %v", sessionID, err)
			return
		}
		rxCipher, err := newStripeCipher(km.c2sKey)
		if err != nil {
			ss.logger.Errorf("stripe: RX cipher init session %08x: %v", sessionID, err)
			return
		}

		sess = &stripeSession{
			sessionID:    sessionID,
			peerIP:       peerIP,
			pipes:        make([]*net.UDPAddr, totalPipes),
			totalPipes:   totalPipes,
			txCipher:     txCipher,
			rxCipher:     rxCipher,
			dataK:        ss.dataK,
			parityM:      ss.parityM,
			fecMode:      ss.fecMode,
			fecType:      ss.fecType,
			enc:          enc,
			rxGroups:     make(map[uint32]*fecGroup),
			rxCh:         rxCh,
			txGroup:      make([][]byte, 0, ss.dataK),
			lastActivity: time.Now(),
			createdAt:    time.Now(),
			logger:       ss.logger,
		}
		// Initialize XOR FEC sender/receiver when fec_type=xor and not off.
		if ss.fecType == "xor" && ss.fecMode != "off" {
			sess.xorTx = newXorFECSender(ss.fecWindow)
			sess.xorRx = newXorFECReceiver(ss.fecWindow)
		}
		// Set initial adaptive M
		if ss.fecMode == "adaptive" {
			atomic.StoreInt32(&sess.adaptiveM, 0) // start with no parity
		} else if ss.fecMode == "off" {
			atomic.StoreInt32(&sess.adaptiveM, 0)
		} else {
			atomic.StoreInt32(&sess.adaptiveM, int32(ss.parityM))
		}
		sess.pacer = newStripePacer(ss.pacingRate)
		if ss.txtimeEnabled && ss.pacingRate > 0 {
			sess.txtimeEnabled = true
			// Inter-packet gap (ns) = pkt_size * 8 / rate_bps * 1e9
			// Typical shard ≈ 1402 bytes. Rate is per-session (all pipes).
			sess.txtimeGapNs = int64(float64(1402*8) / (float64(ss.pacingRate) * 1e6) * 1e9)
			// Kernel pacing supersedes software pacer.
			sess.pacer = nil
		}
		if ss.arqEnabled {
			sess.arqTx = &arqTxBuf{}
			sess.arqRx = newArqRxTracker()
		}

		// Initialize TX batch (sendmmsg) — pre-allocate message slots to avoid
		// per-call allocations on the hot path.
		sess.txBatchPC = ipv4.NewPacketConn(ss.conn)
		sess.txBatchMsgs = make([]ipv4.Message, stripeBatchSize)
		for i := range sess.txBatchMsgs {
			sess.txBatchMsgs[i].Buffers = make([][]byte, 1)
		}

		ss.sessions[sessionID] = sess

		// Create server-to-client datagramConn and register in connectionTable
		sdc := &stripeServerDC{session: sess, conn: ss.conn}
		sess.txTimer = time.AfterFunc(stripeFlushInterval, func() {
			sess.txMu.Lock()
			if len(sess.txGroup) > 0 {
				sdc.sendFECGroupLocked()
			}
			// Flush partial XOR window (emit repair for accumulated sources).
			if sess.xorTx != nil && len(sess.txActivePipes) > 0 {
				if repair, firstSeq, window, ok := sess.xorTx.flush(); ok {
					sdc.sendXorRepairLocked(firstSeq, uint8(window), repair, sess.txActivePipes)
				}
			}
			sdc.txBatchFlushLocked() // flush any partial batch from FEC timer
			sdc.resetFlushTimer()
			sess.txMu.Unlock()
		})

		_, cancel := context.WithCancel(context.Background())
		ss.ct.registerStripe(peerIP, fmt.Sprintf("stripe:%08x", sessionID), sdc, cancel)
		ss.logger.Infof("stripe session created: peer=%s session=%08x pipes=%d", peerIP, sessionID, totalPipes)

		// Open per-session TUN fd via IFF_MULTI_QUEUE for parallel writes.
		// Each tunWriter goroutine gets its own kernel queue, avoiding
		// contention on a single fd shared by all sessions.
		//
		// IMPORTANT: with IFF_MULTI_QUEUE the kernel distributes RX packets
		// (kernel → userspace) across ALL open fds via hash-based queue
		// selection. Every per-session fd MUST have a reader goroutine,
		// otherwise packets routed to that fd's queue are stuck forever.
		if ss.tunMultiQueue {
			tunFd, tunErr := water.New(water.Config{
				DeviceType: water.TUN,
				PlatformSpecificParams: water.PlatformSpecificParams{
					Name:       ss.tunName,
					MultiQueue: true,
				},
			})
			if tunErr != nil {
				ss.logger.Errorf("stripe: multiqueue TUN fd for session %08x: %v (using shared fd)", sessionID, tunErr)
				sess.tunFd = ss.tun
			} else {
				sess.tunFd = tunFd
				ss.logger.Infof("stripe: multiqueue TUN fd opened for session %08x", sessionID)
				// Start reader for per-session fd so RX packets routed
				// to this queue by the kernel are dispatched correctly.
				go ss.tunFdReader(sess)
			}
		} else {
			sess.tunFd = ss.tun
		}

		// Start goroutine writing decoded packets to TUN
		go ss.tunWriter(sess)

		// Start ARQ NACK generation loop if enabled
		if sess.arqRx != nil {
			go ss.startArqNackLoop(context.Background(), sess)
		}
	} else {
		// Session exists — detect client reconnect (address change or pipe
		// count change) and reset pipe state so stale NAT addresses are purged.
		needReset := totalPipes != sess.totalPipes
		if !needReset && pipeIdx >= 0 && pipeIdx < len(sess.pipes) && sess.pipes[pipeIdx] != nil {
			if sess.pipes[pipeIdx].String() != from.String() {
				needReset = true
			}
		}
		if needReset {
			ss.logger.Infof("stripe session %08x: reconnect detected (pipes %d→%d), resetting pipe state",
				sessionID, sess.totalPipes, totalPipes)
			// Remove all old addr→session mappings
			for addr, sid := range ss.addrToSess {
				if sid == sessionID {
					delete(ss.addrToSess, addr)
				}
			}
			// Resize or clear pipes slice
			if totalPipes != sess.totalPipes {
				sess.pipes = make([]*net.UDPAddr, totalPipes)
				sess.totalPipes = totalPipes
			} else {
				for i := range sess.pipes {
					sess.pipes[i] = nil
				}
			}
			sess.registered = 0
			sess.txMu.Lock()
			sess.txActivePipes = nil
			sess.txMu.Unlock()

			// Reset ARQ / FEC state: new client starts at txSeq=0
			if sess.arqRx != nil {
				sess.arqRx = newArqRxTracker()
			}
			if sess.arqTx != nil {
				sess.arqTx = &arqTxBuf{}
			}
			atomic.StoreUint64(&sess.rxSeqHighest, 0)
			sess.rxMu.Lock()
			sess.rxGroups = make(map[uint32]*fecGroup)
			sess.rxMu.Unlock()

			// Re-register in connectionTable with fresh pathConn
			sdc := &stripeServerDC{session: sess, conn: ss.conn}
			_, cancel := context.WithCancel(context.Background())
			remoteID := fmt.Sprintf("stripe:%08x", sessionID)
			ss.ct.registerStripe(sess.peerIP, remoteID, sdc, cancel)
		}
	}

	if pipeIdx >= 0 && pipeIdx < len(sess.pipes) {
		sess.pipes[pipeIdx] = from
		sess.registered++
		ss.addrToSess[from.String()] = sessionID

		// Rebuild cached active pipes under txMu for thread-safe TX access
		sess.txMu.Lock()
		ap := make([]*net.UDPAddr, 0, len(sess.pipes))
		for _, p := range sess.pipes {
			if p != nil {
				ap = append(ap, p)
			}
		}
		sess.txActivePipes = ap
		sess.txMu.Unlock()

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
	reply = stripeEncrypt(sess.txCipher, reply)
	_, _ = ss.conn.WriteToUDP(reply, from)
}

// tunFdReader reads IP packets from a per-session multiqueue TUN fd and
// dispatches them via the connectionTable. With IFF_MULTI_QUEUE the kernel
// distributes RX packets across all open fds; without a reader on each fd,
// packets routed to unread queues are silently stuck. This goroutine exits
// when sess.tunFd is closed (during session GC or server shutdown).
func (ss *stripeServer) tunFdReader(sess *stripeSession) {
	buf := make([]byte, 65535)
	var lastDispatchFail time.Time
	for {
		n, err := sess.tunFd.Read(buf)
		if err != nil {
			// Normal exit when fd is closed during session cleanup.
			return
		}
		pkt := buf[:n]
		if len(pkt) < 20 {
			continue
		}
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
		if !ss.ct.dispatch(dstIP, pktCopy) {
			now := time.Now()
			if now.Sub(lastDispatchFail) > time.Second {
				lastDispatchFail = now
				ss.logger.Infof("stripe: per-session fd dispatch failed for dst=%s", dstIP)
			}
		}
	}
}

func (ss *stripeServer) tunWriter(sess *stripeSession) {
	remoteID := fmt.Sprintf("stripe:%08x", sess.sessionID)

	// writePkt writes a single IP packet to the TUN device and learns routes.
	// touchPath/learnRoute are called only on the first packet of each batch
	// to reduce mutex contention (touchPath does RLock + string compare + time
	// check per call; learnRoute does RLock + map lookup per call).
	writePkt := func(pkt []byte, doTouch bool) {
		if doTouch {
			ss.ct.touchPath(sess.peerIP, remoteID)
		}
		if len(pkt) >= 20 {
			version := pkt[0] >> 4
			if version == 4 {
				srcIP := netip.AddrFrom4([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]})
				if srcIP != sess.peerIP {
					ss.ct.learnRoute(srcIP, sess.peerIP)
				}
			}
		}
		if _, err := sess.tunFd.Write(pkt); err != nil {
			ss.logger.Errorf("stripe: TUN write error: %v", err)
		}
	}

	// Batch-drain: blocking receive one packet, then non-blocking drain any
	// additional queued packets. The goroutine stays running for the entire
	// batch, reducing park/unpark scheduling overhead (~11% of CPU).
	// touchPath is called only on the first packet of each batch.
	for pkt := range sess.rxCh {
		writePkt(pkt, true) // first packet: touchPath + learnRoute
		// Non-blocking drain
		drain := true
		for drain {
			select {
			case pkt2, ok := <-sess.rxCh:
				if !ok {
					return
				}
				writePkt(pkt2, false) // subsequent: learnRoute only
			default:
				drain = false
			}
		}
	}
}

func (ss *stripeServer) handleDataShard(hdr stripeHdr, payload []byte, from *net.UDPAddr) {
	sess := ss.lookupSession(hdr.Session, from)
	if sess == nil {
		return
	}
	sess.lastActivity = time.Now()

	// ── Adaptive FEC: track RX sequence for loss detection ──
	for {
		old := atomic.LoadUint64(&sess.rxSeqHighest)
		newSeq := uint64(hdr.GroupSeq)
		if newSeq <= old {
			break
		}
		if atomic.CompareAndSwapUint64(&sess.rxSeqHighest, old, newSeq) {
			break
		}
	}

	// Partial group or no FEC: deliver directly
	if int(hdr.GroupDataN) < ss.dataK || ss.parityM == 0 || sess.enc == nil {
		// ARQ: mark this sequence as received for gap detection;
		// if already received (duplicate from ARQ retransmit), skip TUN delivery.
		if sess.arqRx != nil {
			if !sess.arqRx.markReceived(hdr.GroupSeq) {
				sess.arqRx.addDupFiltered(1)
				return // dedup: already delivered
			}
			// Gap-filling: if this seq is below the previously seen highest,
			// it was out-of-order or a NACK-retransmit that filled a gap.
			if uint64(hdr.GroupSeq) < atomic.LoadUint64(&sess.rxSeqHighest) {
				sess.arqRx.addRetxReceived(1)
			}
		}
		if hdr.DataLen > 0 && len(payload) >= 2+int(hdr.DataLen) {
			pkt := make([]byte, hdr.DataLen)
			copy(pkt, payload[2:2+hdr.DataLen])
			atomic.AddUint64(&sess.rxDirectCount, 1)
			atomic.AddUint64(&sess.rxPkts, 1)
			atomic.AddUint64(&sess.rxBytes, uint64(hdr.DataLen))
			// XOR FEC: store source shard for potential recovery.
			if sess.xorRx != nil {
				sess.xorRx.storeShard(hdr.GroupSeq, payload)
			}
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

// handleXorRepairServer processes an XOR FEC repair packet from a client.
func (ss *stripeServer) handleXorRepairServer(hdr stripeHdr, payload []byte, from *net.UDPAddr) {
	sess := ss.lookupSession(hdr.Session, from)
	if sess == nil || sess.xorRx == nil || len(payload) == 0 {
		return
	}
	sess.lastActivity = time.Now()

	W := int(hdr.GroupDataN)
	if W <= 0 {
		return
	}
	pkt, recoveredSeq, ok := sess.xorRx.tryRecover(hdr.GroupSeq, W, payload)
	if !ok || pkt == nil {
		return
	}
	// Deliver recovered IP packet.
	atomic.AddUint64(&sess.rxFECRecov, 1)
	atomic.AddUint64(&sess.rxPkts, 1)
	atomic.AddUint64(&sess.rxBytes, uint64(len(pkt)))
	// ARQ: mark recovered seq as received so it won't be NACKed.
	if sess.arqRx != nil {
		sess.arqRx.markReceived(recoveredSeq)
		sess.arqRx.addRetxReceived(1)
	}
	select {
	case sess.rxCh <- pkt:
	default:
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
		atomic.AddUint64(&sess.rxFECGroups, 1)
		return
	}

	// FEC reconstruction needed — snapshot and pad shards under lock.
	shards := make([][]byte, len(grp.shards))
	for i := range grp.shards {
		if grp.shards[i] != nil {
			padded := make([]byte, grp.maxLen)
			copy(padded, grp.shards[i])
			shards[i] = padded
		}
	}
	delete(sess.rxGroups, groupSeq)
	sess.rxMu.Unlock()

	if err := sess.enc.Reconstruct(shards); err != nil {
		ss.logger.Debugf("stripe server: FEC reconstruct failed group=%d: %v", groupSeq, err)
		return
	}

	atomic.AddUint64(&sess.rxFECRecov, 1)
	atomic.AddUint64(&sess.rxFECGroups, 1)

	sess.rxMu.Lock()
	grp.shards = shards
	ss.deliverGroupToTUN(sess, grp)
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
		atomic.AddUint64(&sess.rxPkts, 1)
		atomic.AddUint64(&sess.rxBytes, uint64(dataLen))
		select {
		case sess.rxCh <- pkt:
		default:
			// Drop if buffer full
		}
	}
}

// ─── Adaptive FEC: loss computation and M adjustment (server) ─────────────

// computeSessionRxLoss computes server-side RX loss for a session (loss on data FROM client).
// Returns loss percentage 0-100.
//
// Two detection modes:
//   - M>0 (FEC active): ratio of groups that needed reconstruction vs total groups.
//   - M=0 (no FEC): sequence-gap based — compare highest seq seen vs packets received.
//     This is essential for adaptive FEC bootstrap: without it, M=0→M>0 transition
//     never happens because loss is never detected (chicken-and-egg problem).
func (ss *stripeServer) computeSessionRxLoss(sess *stripeSession) uint8 {
	fecRecov := atomic.LoadUint64(&sess.rxFECRecov)
	fecGroups := atomic.LoadUint64(&sess.rxFECGroups)

	dFECRecov := fecRecov - sess.rxLossPrevFECRecov
	dFECGroups := fecGroups - sess.rxLossPrevFECGroups

	sess.rxLossPrevFECRecov = fecRecov
	sess.rxLossPrevFECGroups = fecGroups

	// Prefer FEC-group based loss when available (more accurate)
	if dFECGroups > 10 {
		rate := dFECRecov * 100 / dFECGroups
		if rate > 100 {
			rate = 100
		}
		return uint8(rate)
	}

	// M=0 fallback: sequence-gap based loss detection.
	seqHigh := atomic.LoadUint64(&sess.rxSeqHighest)
	directCnt := atomic.LoadUint64(&sess.rxDirectCount)

	dSeq := seqHigh - sess.rxLossPrevSeqHigh
	dRecv := directCnt - sess.rxLossPrevDirectCnt

	sess.rxLossPrevSeqHigh = seqHigh
	sess.rxLossPrevDirectCnt = directCnt

	if dSeq > 20 && dRecv <= dSeq {
		lost := dSeq - dRecv
		rate := lost * 100 / dSeq
		if rate > 100 {
			rate = 100
		}
		return uint8(rate)
	}

	return 0
}

// updateSessionAdaptiveM adjusts server TX parity M for a session based on
// client-reported loss (which is loss on data WE send TO the client).
func (ss *stripeServer) updateSessionAdaptiveM(sess *stripeSession) {
	if sess.fecMode != "adaptive" || sess.parityM == 0 {
		return
	}

	peerLoss := atomic.LoadUint32(&sess.peerLossRate)
	currentM := atomic.LoadInt32(&sess.adaptiveM)
	lastLoss := time.Unix(0, atomic.LoadInt64(&sess.lastPeerLoss))

	if peerLoss > uint32(adaptiveFECLossThreshold) {
		if currentM == 0 {
			atomic.StoreInt32(&sess.adaptiveM, int32(sess.parityM))
			ss.logger.Infof("adaptive FEC: server TX M=0→%d session=%08x (client reports %d%% loss)",
				sess.parityM, sess.sessionID, peerLoss)
		}
	} else if peerLoss == 0 && currentM > 0 {
		if time.Since(lastLoss) > adaptiveFECCooldown {
			atomic.StoreInt32(&sess.adaptiveM, 0)
			ss.logger.Infof("adaptive FEC: server TX M=%d→0 session=%08x (no client loss for %v)",
				currentM, sess.sessionID, time.Since(lastLoss).Round(time.Second))
		}
	}
}

func (ss *stripeServer) handleKeepalive(hdr stripeHdr, payload []byte, from *net.UDPAddr) {
	sess := ss.lookupSession(hdr.Session, from)
	if sess == nil {
		// Unknown session — don't reply so client detects timeout and reconnects
		return
	}
	sess.lastActivity = time.Now()

	// Update pipe address if the client included a pipe index byte.
	// This handles CGNAT rebind: the client's public IP:port changed,
	// so we update sess.pipes and addrToSess to the new source address.
	if len(payload) >= 1 {
		pipeIdx := int(payload[0])
		ss.mu.Lock()
		if pipeIdx >= 0 && pipeIdx < len(sess.pipes) {
			old := sess.pipes[pipeIdx]
			if old == nil || old.String() != from.String() {
				// Remove old addrToSess entry
				if old != nil {
					delete(ss.addrToSess, old.String())
				}
				sess.pipes[pipeIdx] = from
				ss.addrToSess[from.String()] = hdr.Session
				if old != nil {
					ss.logger.Infof("stripe: pipe %d/%d address updated %s → %s (session=%08x)",
						pipeIdx, len(sess.pipes), old, from, hdr.Session)
				}

				// Rebuild cached active pipes
				sess.txMu.Lock()
				ap := make([]*net.UDPAddr, 0, len(sess.pipes))
				for _, p := range sess.pipes {
					if p != nil {
						ap = append(ap, p)
					}
				}
				sess.txActivePipes = ap
				sess.txMu.Unlock()
			}
		}
		ss.mu.Unlock()
	}

	// Read client's RX loss report (byte 1) — this is loss on data WE sent to client.
	// We use it to adjust our TX parity M.
	if len(payload) >= 2 {
		peerLoss := uint32(payload[1])
		atomic.StoreUint32(&sess.peerLossRate, peerLoss)
		if peerLoss > 0 {
			atomic.StoreInt64(&sess.lastPeerLoss, time.Now().UnixNano())
		}
	}

	// Compute server-side RX loss (loss on data FROM client) and update adaptive M
	rxLoss := ss.computeSessionRxLoss(sess)
	ss.updateSessionAdaptiveM(sess)

	// Reply with server-measured RX loss (tells client about loss on data CLIENT sent)
	reply := make([]byte, stripeHdrLen+1)
	encodeStripeHdr(reply, &stripeHdr{
		Magic:   stripeMagic,
		Version: stripeVersion,
		Type:    stripeKEEPALIVE,
		Session: hdr.Session,
	})
	reply[stripeHdrLen] = rxLoss
	reply = stripeEncrypt(sess.txCipher, reply)
	_, _ = ss.conn.WriteToUDP(reply, from)
}

// ─── Server ARQ: NACK handler + generation ─────────────────────────────

// handleNack processes a NACK from a client requesting retransmission of
// packets we sent to that client. Look up our TX ring buffer and retransmit.
func (ss *stripeServer) handleNack(hdr stripeHdr, payload []byte, from *net.UDPAddr) {
	sess := ss.lookupSession(hdr.Session, from)
	if sess == nil || sess.arqTx == nil {
		return
	}
	baseSeq, bitmap, ok := decodeNackPayload(payload)
	if !ok || bitmap == 0 {
		return
	}
	sess.lastActivity = time.Now()

	// Use cached active pipes for round-robin retransmission
	sess.txMu.Lock()
	activePipes := sess.txActivePipes
	sess.txMu.Unlock()
	if len(activePipes) == 0 {
		return
	}

	var retxCount int
	for bit := uint32(0); bit < 64; bit++ {
		if bitmap&(1<<bit) == 0 {
			continue
		}
		seq := baseSeq + bit
		shardData, dataLen, found := sess.arqTx.lookup(seq)
		if !found {
			continue
		}
		// Re-encrypt with fresh nonce and send on round-robin pipe
		wirePkt := stripeEncryptShard(sess.txCipher, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripeDATA,
			Session:    sess.sessionID,
			GroupSeq:   seq,
			ShardIdx:   0,
			GroupDataN: 1,
			DataLen:    dataLen,
		}, shardData)
		pipeIdx := int(atomic.AddUint32(&sess.txPipe, 1)-1) % len(activePipes)
		_, _ = ss.conn.WriteToUDP(wirePkt, activePipes[pipeIdx])
		retxCount++
	}

	if retxCount > 0 {
		ss.logger.Debugf("stripe ARQ: retransmitted %d packets for session %08x (base=%d)", retxCount, sess.sessionID, baseSeq)
	}
}

// startArqNackLoop starts the NACK generation loop for a session.
// Called when a new session is created with ARQ enabled.
func (ss *stripeServer) startArqNackLoop(ctx context.Context, sess *stripeSession) {
	ticker := time.NewTicker(arqNackInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ss.closeCh:
			return
		case <-ticker.C:
			if sess.arqRx == nil {
				continue
			}
			// Rate limit: max 1 NACK per RTT (~30ms) to avoid flooding
			if !sess.arqRx.canSendNack() {
				continue
			}
			baseSeq, bitmap, count := sess.arqRx.getMissing()
			if count == 0 {
				continue
			}
			// Collect an active pipe address to send the NACK to the client
			var peerAddr *net.UDPAddr
			for _, p := range sess.pipes {
				if p != nil {
					peerAddr = p
					break
				}
			}
			if peerAddr == nil {
				continue
			}
			// Build and send NACK packet
			pkt := make([]byte, stripeHdrLen+arqNackPayloadLen)
			encodeStripeHdr(pkt, &stripeHdr{
				Magic:   stripeMagic,
				Version: stripeVersion,
				Type:    stripeNACK,
				Session: sess.sessionID,
			})
			encodeNackPayload(pkt[stripeHdrLen:], baseSeq, bitmap)
			pkt = stripeEncrypt(sess.txCipher, pkt)
			_, _ = ss.conn.WriteToUDP(pkt, peerAddr)
			sess.arqRx.addNacksSent(1)
			sess.arqRx.recordNackSent()
			ss.logger.Debugf("stripe ARQ: NACK sent to %s base=%d count=%d session=%08x", peerAddr, baseSeq, count, sess.sessionID)
		}
	}
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
			// Close per-session TUN fd (multiqueue) if it's not the shared fd
			if sess.tunFd != nil && sess.tunFd != ss.tun {
				sess.tunFd.Close()
			}
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
		// GC XOR FEC receiver buffer.
		if sess.xorRx != nil {
			sess.xorRx.gc()
		}
	}

	if df := atomic.LoadUint64(&ss.securityDecryptFail); df > 0 {
		ss.logger.Infof("stripe security metrics decrypt_fail=%d", df)
	}
}

// Close stops the stripe server.
func (ss *stripeServer) Close() error {
	close(ss.closeCh)
	// Close per-session multiqueue TUN fds
	ss.mu.RLock()
	for _, sess := range ss.sessions {
		if sess.tunFd != nil && sess.tunFd != ss.tun {
			sess.tunFd.Close()
		}
	}
	ss.mu.RUnlock()
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
