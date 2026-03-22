package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/reedsolomon"
	"golang.org/x/net/ipv4"
)

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
	xorTx     *xorFECSender   // XOR TX accumulator (nil if fecType != "xor")
	xorRx     *xorFECReceiver // XOR RX recovery buffer (nil if fecType != "xor")
	xorActive int32           // atomic: 1=emit XOR repairs, 0=skip (adaptive gate)

	// Pacing
	pacer *stripePacer // TX rate limiter (nil = disabled)

	// Hybrid ARQ
	arqTx *arqTxBuf     // TX retransmit buffer (nil = ARQ disabled)
	arqRx *arqRxTracker // RX gap detector + NACK generator

	// TX state
	txSeq      uint32 // atomic: next data sequence number
	txPipe     uint32 // atomic: round-robin pipe selector
	txGroup    [][]byte
	txGrpSeq   uint32
	txMu       sync.Mutex
	txTimer    *time.Timer
	txShardBuf []byte // reusable M=0 shard buffer (under txMu, avoids alloc/pkt)
	txEncBuf   []byte // reusable encrypt output buffer (under txMu, client only)

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
	rxLossPrevXorRecov   uint64
	rxLossPrevXorUnrecov uint64

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
		// Adaptive: start XOR off (no repairs until loss > threshold);
		// always: start XOR on.
		if fecMode == "adaptive" {
			atomic.StoreInt32(&scc.xorActive, 0)
		} else {
			atomic.StoreInt32(&scc.xorActive, 1)
		}
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

	// Start dynamic pacing adaptation (Step 4.29)
	if scc.txtimeEnabled && cfg.StripePacingRate > 0 {
		go scc.dynamicPacingLoop(ctx, cfg.StripePacingRate)
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

		// Reuse TX shard buffer (safe: encrypt copies, arqTx copies, xorTx XORs in-place).
		need := 2 + len(pkt)
		if cap(scc.txShardBuf) < need {
			scc.txShardBuf = make([]byte, need)
		} else {
			scc.txShardBuf = scc.txShardBuf[:need]
		}
		binary.BigEndian.PutUint16(scc.txShardBuf[0:2], uint16(len(pkt)))
		copy(scc.txShardBuf[2:], pkt)
		shardData := scc.txShardBuf

		// Reuse encrypt output buffer (safe: gsoAccum copies, kernel copies).
		wirePkt := stripeEncryptShardReuse(scc.txCipher, &stripeHdr{
			Magic:      stripeMagic,
			Version:    stripeVersion,
			Type:       stripeDATA,
			Session:    scc.sessionID,
			GroupSeq:   seq,
			ShardIdx:   0,
			GroupDataN: 1, // signals RX to deliver directly (< K)
			DataLen:    uint16(len(pkt)),
		}, shardData, &scc.txEncBuf)

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
		// In adaptive mode, only emit repairs when xorActive=1 (peer loss > threshold).
		if scc.xorTx != nil && atomic.LoadInt32(&scc.xorActive) == 1 {
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
	if scc.xorTx != nil && atomic.LoadInt32(&scc.xorActive) == 1 {
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
	// Count recovery in rxDirectCount so seq-gap loss estimator sees NET loss
	// (after FEC), not GROSS loss. Without this, recovered packets are invisible
	// to gap detection, inflating perceived loss and keeping the adaptive gate ON
	// in a positive feedback loop.
	// ARQ: mark recovered seq as received so it won't be NACKed.
	if scc.arqRx != nil {
		if !scc.arqRx.markReceived(recoveredSeq) {
			scc.arqRx.addDupFiltered(1)
			return
		}
		scc.arqRx.addRetxReceived(1)
	}
	atomic.AddUint64(&scc.rxDirectCount, 1)
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

	// XOR FEC Anti-Waste (Step 4.28): 
	// If burst loss prevents XOR from recovering anything, suspend it.
	if scc.fecType == "xor" && scc.xorRx != nil {
		xorRecov := atomic.LoadUint64(&scc.xorRx.recovered)
		xorUnrecov := atomic.LoadUint64(&scc.xorRx.unrecoverable)
		dXorRecov := xorRecov - scc.rxLossPrevXorRecov
		dXorUnrecov := xorUnrecov - scc.rxLossPrevXorUnrecov
		
		scc.rxLossPrevXorRecov = xorRecov
		scc.rxLossPrevXorUnrecov = xorUnrecov

		// If we had multi-loss windows but NO successful recoveries, XOR is failing.
		if dXorUnrecov > 5 && dXorRecov == 0 {
			// Return a sentinel value so the server suspends XOR.
			return 255
		}
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
	if scc.fecMode != "adaptive" {
		return
	}

	peerLoss := atomic.LoadUint32(&scc.peerLossRate)
	lastLoss := time.Unix(0, atomic.LoadInt64(&scc.lastPeerLoss))

	// Step 4.28 Anti-waste sentinel: Server says "your XOR is useless"
	if peerLoss == 255 {
		if scc.xorTx != nil && atomic.LoadInt32(&scc.xorActive) == 1 {
			atomic.StoreInt32(&scc.xorActive, 0)
			scc.logger.Infof("adaptive XOR FEC: SUSPENDED (anti-waste: server reports 0 recoveries)")
		}
		// Reset peerLossRate to 0 locally so we don't repeatedly trigger or get stuck
		atomic.StoreUint32(&scc.peerLossRate, 0)
		return
	}

	// ── RS adaptive (parityM > 0) ──
	if scc.parityM > 0 {
		currentM := atomic.LoadInt32(&scc.adaptiveM)
		if peerLoss > uint32(adaptiveFECLossThreshold) {
			if currentM == 0 {
				atomic.StoreInt32(&scc.adaptiveM, int32(scc.parityM))
				scc.logger.Infof("adaptive FEC: TX M=0→%d (peer reports %d%% loss)", scc.parityM, peerLoss)
			}
		} else if peerLoss == 0 && currentM > 0 {
			if time.Since(lastLoss) > adaptiveFECCooldown {
				atomic.StoreInt32(&scc.adaptiveM, 0)
				scc.logger.Infof("adaptive FEC: TX M=%d→0 (no peer loss for %v)", currentM, time.Since(lastLoss).Round(time.Second))
			}
		}
	}

	// ── XOR adaptive (xorTx != nil) ──
	if scc.xorTx != nil {
		currentXor := atomic.LoadInt32(&scc.xorActive)
		if peerLoss > uint32(adaptiveFECLossThreshold) {
			if currentXor == 0 {
				atomic.StoreInt32(&scc.xorActive, 1)
				scc.logger.Infof("adaptive XOR FEC: ON (peer reports %d%% loss)", peerLoss)
			}
		} else if peerLoss == 0 && currentXor == 1 {
			if time.Since(lastLoss) > adaptiveFECCooldown {
				atomic.StoreInt32(&scc.xorActive, 0)
				scc.logger.Infof("adaptive XOR FEC: OFF (no peer loss for %v)", time.Since(lastLoss).Round(time.Second))
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

// dynamicPacingLoop dynamically adjusts the pacing rate (txtimeGapNs) based on
// current throughput and RTT to avoid queue buildup when bandwidth drops.
func (scc *stripeClientConn) dynamicPacingLoop(ctx context.Context, baseRate int) {
ticker := time.NewTicker(200 * time.Millisecond)
defer ticker.Stop()

// Target base pacing converted to nanoseconds
baseNs := int64(1000000000 / baseRate)

for {
select {
case <-ctx.Done():
return
case <-ticker.C:
// Minimal dynamic pacing: 
// In Phase 4d we scale pacing off RTT jitter & loss.
// Currently this is a base implementation avoiding panic.
// TODO: Add real scaling based on srtt and ewma bandwidth
targetNs := baseNs

// Increase pacing gap slightly if loss is high
loss := atomic.LoadUint32(&scc.peerLossRate)
if loss > 0 && loss < 255 {
targetNs += targetNs * int64(loss) / 100 // up to 2x slower
}

atomic.StoreInt64(&scc.txtimeGapNs, targetNs)
}
}
}
