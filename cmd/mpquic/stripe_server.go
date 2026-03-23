package main

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
	"golang.org/x/net/ipv4"
)

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
	fecType   string          // "rs" (default) or "xor"
	xorTx     *xorFECSender   // XOR TX accumulator (nil if fecType != "xor")
	xorRx     *xorFECReceiver // XOR RX recovery buffer (nil if fecType != "xor")
	xorActive int32           // atomic: 1=emit XOR repairs, 0=skip (adaptive gate)
	rlcTx     *rlcFECSender   // RLC TX accumulator (nil if fecType != "rlc")
	rlcRx     *rlcFECReceiver // RLC RX decoder (nil if fecType != "rlc")
	rlcActive int32           // atomic: 1=emit RLC repairs, 0=skip (adaptive gate)

	// RS Interleaved FEC (always-on, small generations + interleaving)
	rsilTx *rsilTx // RS interleaved TX (nil unless fec_type=rs && interleave>0)
	rsilRx *rsilRx // RS interleaved RX decoder

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
	rxLossPrevXorRecov   uint64
	rxLossPrevXorUnrecov uint64

	// TX (server → client): FEC encode + stripe
	txSeq    uint32 // atomic
	txPipe   uint32 // atomic
	txGroup       [][]byte
	txGrpSeq      uint32
	txActivePipes []*net.UDPAddr // cached non-nil pipes, rebuilt on REGISTER (under txMu)
	txMu          sync.Mutex
	txTimer       *time.Timer
	txShardBuf    []byte // reusable M=0 shard buffer (under txMu, avoids alloc/pkt)

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

		// Reuse TX shard buffer (safe: encrypt copies, arqTx copies, xorTx XORs in-place).
		// Note: encrypt output is NOT reused server-side because txBatchAddLocked
		// stores a reference until flush (sendmmsg batch).
		need := 2 + len(pkt)
		if cap(sess.txShardBuf) < need {
			sess.txShardBuf = make([]byte, need)
		} else {
			sess.txShardBuf = sess.txShardBuf[:need]
		}
		binary.BigEndian.PutUint16(sess.txShardBuf[0:2], uint16(len(pkt)))
		copy(sess.txShardBuf[2:], pkt)
		shardData := sess.txShardBuf

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

		// Sliding-window FEC: feed source to accumulator, emit repair when window completes.
		if sess.xorTx != nil && atomic.LoadInt32(&sess.xorActive) == 1 {
			if repair, firstSeq, ok := sess.xorTx.addSource(seq, shardData); ok {
				sdc.sendXorRepairLocked(firstSeq, uint8(sess.xorTx.window), repair, activePipes)
			}
		}
		if sess.rlcTx != nil && atomic.LoadInt32(&sess.rlcActive) == 1 {
			if repair, firstSeq, count, ok := sess.rlcTx.addSource(seq, shardData); ok {
				sdc.sendRLCRepairLocked(firstSeq, uint8(count), repair, activePipes)
			}
		}

		// RS Interleaved FEC: feed source to interleave accumulator, emit parity when group fills.
		if sess.rsilTx != nil {
			parities := sess.rsilTx.addSource(seq, shardData)
			for _, p := range parities {
				sdc.sendRSILParityLocked(p, activePipes)
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

// sendRLCRepairLocked encrypts and sends a sliding-window RLC repair packet.
// Caller must hold sess.txMu.
func (sdc *stripeServerDC) sendRLCRepairLocked(firstSeq uint32, window uint8, repairData []byte, activePipes []*net.UDPAddr) {
	sess := sdc.session
	wirePkt := stripeEncryptShard(sess.txCipher, &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeRLC_REPAIR,
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

// sendRSILParityLocked sends a single RS interleaved parity shard. Caller must hold sess.txMu.
func (sdc *stripeServerDC) sendRSILParityLocked(p rsilParity, activePipes []*net.UDPAddr) {
	sess := sdc.session
	wirePkt := stripeEncryptShard(sess.txCipher, &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeRS_IL_PARITY,
		Session:    sess.sessionID,
		GroupSeq:   p.BaseSeq,
		ShardIdx:   p.ShardIdx,
		GroupDataN: p.K,
		DataLen:    uint16(len(p.Data)),
	}, p.Data)
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
	fecType    string // "rs" (default), "xor", "rlc"
	fecWindow  int    // sliding-window size W (used when fecType=xor|rlc)
	interleaveDepth int // RS interleave depth (0=block RS, >0=interleaved)
	rsilK           int // RS-IL data shards per generation (== dataK or default)
	rsilM           int // RS-IL parity shards per generation (original parityM or default)
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
	interleaveDepth := cfg.StripeFECInterleave
	// Compute RS-IL K/M from config before parityM is zeroed
	rsilK := dataK
	if rsilK <= 0 || rsilK > 255 {
		rsilK = rsilDefaultK
	}
	rsilM := cfg.StripeParityShards
	if rsilM <= 0 {
		rsilM = rsilDefaultM
	}
	if fecMode == "off" {
		parityM = 0
	} else if fecType == "xor" || fecType == "rlc" {
		// Sliding-window mode: RS not needed, force M=0 so data goes through fast path.
		parityM = 0
	} else if fecType == "rs" && interleaveDepth > 0 {
		// RS interleaved mode: data goes through M=0 fast path, parity emitted by rsilTx.
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
		interleaveDepth: interleaveDepth,
		rsilK:      rsilK,
		rsilM:      rsilM,
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
	} else if fecType == "rlc" {
		fecStr = fmt.Sprintf("FEC=rlc W=%d mode=%s", fecWindow, fecMode)
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
	case stripeRLC_REPAIR:
		ss.handleRLCRepairServer(hdr, payload, from)
	case stripeRS_IL_PARITY:
		ss.handleRSILParityServer(hdr, payload, from)
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
		// Create FEC encoder unless mode is "off" or fecType is a sliding-window codec.
		// In adaptive mode, encoder is needed when M dynamically switches from 0 to parityM.
		if ss.fecMode != "off" && ss.fecType != "xor" && ss.fecType != "rlc" && ss.parityM > 0 {
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
		// Initialize sliding-window FEC sender/receiver when fec_type=xor|rlc and not off.
		if ss.fecType == "xor" && ss.fecMode != "off" {
			sess.xorTx = newXorFECSender(ss.fecWindow)
			sess.xorRx = newXorFECReceiver(ss.fecWindow)
			// Adaptive: start XOR off; always: start XOR on.
			if ss.fecMode == "adaptive" {
				atomic.StoreInt32(&sess.xorActive, 0)
			} else {
				atomic.StoreInt32(&sess.xorActive, 1)
			}
		} else if ss.fecType == "rlc" && ss.fecMode != "off" {
			sess.rlcTx = newRLCFECSender(ss.fecWindow)
			sess.rlcRx = newRLCFECReceiver(ss.fecWindow)
			if ss.fecMode == "adaptive" {
				atomic.StoreInt32(&sess.rlcActive, 0)
			} else {
				atomic.StoreInt32(&sess.rlcActive, 1)
			}
		}
		// RS Interleaved FEC: create per-session TX/RX when fec_type=rs && interleave>0 and not off.
		if ss.fecType == "rs" && ss.interleaveDepth > 0 && ss.fecMode != "off" {
			rsilTxInst, err := newRSILTx(ss.rsilK, ss.rsilM, ss.interleaveDepth)
			if err != nil {
				ss.logger.Errorf("stripe: session %08x RS-IL TX encoder: %v", sessionID, err)
			} else {
				sess.rsilTx = rsilTxInst
			}
			rsilRxInst, err := newRSILRx(ss.rsilK, ss.rsilM, ss.interleaveDepth)
			if err != nil {
				ss.logger.Errorf("stripe: session %08x RS-IL RX decoder: %v", sessionID, err)
			} else {
				sess.rsilRx = rsilRxInst
			}
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
			if sess.xorTx != nil && atomic.LoadInt32(&sess.xorActive) == 1 && len(sess.txActivePipes) > 0 {
				if repair, firstSeq, window, ok := sess.xorTx.flush(); ok {
					sdc.sendXorRepairLocked(firstSeq, uint8(window), repair, sess.txActivePipes)
				}
			}
			if sess.rlcTx != nil && atomic.LoadInt32(&sess.rlcActive) == 1 && len(sess.txActivePipes) > 0 {
				if repair, firstSeq, window, ok := sess.rlcTx.flush(); ok {
					sdc.sendRLCRepairLocked(firstSeq, uint8(window), repair, sess.txActivePipes)
				}
			}
			// Flush partial RS interleaved groups.
			if sess.rsilTx != nil && len(sess.txActivePipes) > 0 {
				parities := sess.rsilTx.flush()
				for _, p := range parities {
					sdc.sendRSILParityLocked(p, sess.txActivePipes)
				}
			}
			sdc.txBatchFlushLocked() // flush any partial batch from FEC timer
			sdc.resetFlushTimer()
			sess.txMu.Unlock()
		})

		_, cancel := context.WithCancel(context.Background())
		ss.ct.registerStripe(peerIP, fmt.Sprintf("stripe:%08x", sessionID), sdc, cancel)
		fecStr := fmt.Sprintf("FEC=%d+%d mode=%s type=%s", ss.dataK, ss.parityM, ss.fecMode, ss.fecType)
		if sess.rsilTx != nil {
			fecStr = fmt.Sprintf("FEC=rs-il K=%d M=%d D=%d mode=%s", sess.rsilTx.K, sess.rsilTx.M, sess.rsilTx.depth, ss.fecMode)
		}
		ss.logger.Infof("stripe session created: peer=%s session=%08x pipes=%d %s", peerIP, sessionID, totalPipes, fecStr)

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

		go ss.dynamicPacingLoop(context.Background(), sess)
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
		putPktBuf(pkt)      // return pooled buffer after TUN write
		// Non-blocking drain
		drain := true
		for drain {
			select {
			case pkt2, ok := <-sess.rxCh:
				if !ok {
					return
				}
				writePkt(pkt2, false) // subsequent: learnRoute only
				putPktBuf(pkt2)
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
			pkt := getPktBuf(int(hdr.DataLen))
			copy(pkt, payload[2:2+hdr.DataLen])
			atomic.AddUint64(&sess.rxDirectCount, 1)
			atomic.AddUint64(&sess.rxPkts, 1)
			atomic.AddUint64(&sess.rxBytes, uint64(hdr.DataLen))
			// Sliding-window FEC: store source shard for potential recovery.
			if sess.xorRx != nil {
				sess.xorRx.storeShard(hdr.GroupSeq, payload)
			}
			if sess.rlcRx != nil {
				sess.rlcRx.storeShard(hdr.GroupSeq, payload)
			}
			// RS Interleaved: store for parity-based recovery.
			if sess.rsilRx != nil {
				sess.rsilRx.storeShard(hdr.GroupSeq, payload)
			}
			select {
			case sess.rxCh <- pkt:
			default:
				putPktBuf(pkt)
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
	// Count recovery in rxDirectCount so seq-gap loss estimator sees NET loss
	// (after FEC), not GROSS loss — prevents adaptive gate feedback loop.
	// ARQ: mark recovered seq as received so it won't be NACKed.
	if sess.arqRx != nil {
		if !sess.arqRx.markReceived(recoveredSeq) {
			sess.arqRx.addDupFiltered(1)
			return
		}
		sess.arqRx.addRetxReceived(1)
	}
	atomic.AddUint64(&sess.rxDirectCount, 1)
	select {
	case sess.rxCh <- pkt:
	default:
	}
}

// handleRLCRepairServer processes a sliding-window RLC repair packet from a client.
func (ss *stripeServer) handleRLCRepairServer(hdr stripeHdr, payload []byte, from *net.UDPAddr) {
	sess := ss.lookupSession(hdr.Session, from)
	if sess == nil || sess.rlcRx == nil || len(payload) <= rlcSeedLen {
		return
	}
	sess.lastActivity = time.Now()

	window := int(hdr.GroupDataN)
	if window <= 0 {
		return
	}
	recovered := sess.rlcRx.addRepair(hdr.GroupSeq, window, payload)
	for _, rp := range recovered {
		atomic.AddUint64(&sess.rxFECRecov, 1)
		atomic.AddUint64(&sess.rxPkts, 1)
		atomic.AddUint64(&sess.rxBytes, uint64(len(rp.pkt)))
		if sess.arqRx != nil {
			if !sess.arqRx.markReceived(rp.seq) {
				sess.arqRx.addDupFiltered(1)
				continue
			}
			sess.arqRx.addRetxReceived(1)
		}
		atomic.AddUint64(&sess.rxDirectCount, 1)
		pkt := getPktBuf(len(rp.pkt))
		copy(pkt, rp.pkt)
		select {
		case sess.rxCh <- pkt:
		default:
			putPktBuf(pkt)
		}
	}
}

// handleRSILParityServer processes a received RS interleaved parity packet from a client.
func (ss *stripeServer) handleRSILParityServer(hdr stripeHdr, payload []byte, from *net.UDPAddr) {
	sess := ss.lookupSession(hdr.Session, from)
	if sess == nil || sess.rsilRx == nil || len(payload) == 0 {
		return
	}
	sess.lastActivity = time.Now()

	K := int(hdr.GroupDataN)
	if K <= 0 {
		return
	}
	recovered := sess.rsilRx.addParity(hdr.GroupSeq, int(hdr.ShardIdx), K, payload)
	for _, rp := range recovered {
		atomic.AddUint64(&sess.rxFECRecov, 1)
		atomic.AddUint64(&sess.rxPkts, 1)
		atomic.AddUint64(&sess.rxBytes, uint64(len(rp.Pkt)))
		if sess.arqRx != nil {
			if !sess.arqRx.markReceived(rp.Seq) {
				sess.arqRx.addDupFiltered(1)
				continue
			}
			sess.arqRx.addRetxReceived(1)
		}
		atomic.AddUint64(&sess.rxDirectCount, 1)
		pkt := getPktBuf(len(rp.Pkt))
		copy(pkt, rp.Pkt)
		select {
		case sess.rxCh <- pkt:
		default:
			putPktBuf(pkt)
		}
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
		pkt := getPktBuf(int(dataLen))
		copy(pkt, grp.shards[i][2:2+dataLen])
		atomic.AddUint64(&sess.rxPkts, 1)
		atomic.AddUint64(&sess.rxBytes, uint64(dataLen))
		select {
		case sess.rxCh <- pkt:
		default:
			putPktBuf(pkt)
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

	// XOR FEC Anti-Waste (Step 4.28): 
	// If burst loss prevents XOR from recovering anything, suspend it.
	if sess.fecType == "xor" && sess.xorRx != nil {
		xorRecov := atomic.LoadUint64(&sess.xorRx.recovered)
		xorUnrecov := atomic.LoadUint64(&sess.xorRx.unrecoverable)
		dXorRecov := xorRecov - sess.rxLossPrevXorRecov
		dXorUnrecov := xorUnrecov - sess.rxLossPrevXorUnrecov
		
		sess.rxLossPrevXorRecov = xorRecov
		sess.rxLossPrevXorUnrecov = xorUnrecov

		// If we had multi-loss windows but NO successful recoveries, XOR is failing.
		if dXorUnrecov > 5 && dXorRecov == 0 {
			// Return a sentinel value so the peer suspends XOR.
			return 255
		}
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
	if sess.fecMode != "adaptive" {
		return
	}

	peerLoss := atomic.LoadUint32(&sess.peerLossRate)
	lastLoss := time.Unix(0, atomic.LoadInt64(&sess.lastPeerLoss))

	// Step 4.28 Anti-waste sentinel: Client says "your XOR is useless"
	if peerLoss == 255 {
		if sess.xorTx != nil && atomic.LoadInt32(&sess.xorActive) == 1 {
			atomic.StoreInt32(&sess.xorActive, 0)
			ss.logger.Infof("adaptive XOR FEC: SUSPENDED session=%08x (anti-waste: client reports 0 recoveries)", sess.sessionID)
		}
		// Reset peerLossRate to 0 locally so we don't repeatedly trigger or get stuck
		atomic.StoreUint32(&sess.peerLossRate, 0)
		return
	}

	// ── RS adaptive (parityM > 0) ──
	if sess.parityM > 0 {
		currentM := atomic.LoadInt32(&sess.adaptiveM)
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

	// ── XOR adaptive (xorTx != nil) ──
	if sess.xorTx != nil {
		currentXor := atomic.LoadInt32(&sess.xorActive)
		if peerLoss > uint32(adaptiveFECLossThreshold) {
			if currentXor == 0 {
				atomic.StoreInt32(&sess.xorActive, 1)
				ss.logger.Infof("adaptive XOR FEC: ON session=%08x (client reports %d%% loss)", sess.sessionID, peerLoss)
			}
		} else if peerLoss == 0 && currentXor == 1 {
			if time.Since(lastLoss) > adaptiveFECCooldown {
				atomic.StoreInt32(&sess.xorActive, 0)
				ss.logger.Infof("adaptive XOR FEC: OFF session=%08x (no client loss for %v)", sess.sessionID, time.Since(lastLoss).Round(time.Second))
			}
		}
	}

	if sess.rlcTx != nil {
		currentRLC := atomic.LoadInt32(&sess.rlcActive)
		if peerLoss > uint32(adaptiveFECLossThreshold) {
			if currentRLC == 0 {
				atomic.StoreInt32(&sess.rlcActive, 1)
				ss.logger.Infof("adaptive RLC FEC: ON session=%08x (client reports %d%% loss)", sess.sessionID, peerLoss)
			}
		} else if peerLoss == 0 && currentRLC == 1 {
			if time.Since(lastLoss) > adaptiveFECCooldown {
				atomic.StoreInt32(&sess.rlcActive, 0)
				ss.logger.Infof("adaptive RLC FEC: OFF session=%08x (no client loss for %v)", sess.sessionID, time.Since(lastLoss).Round(time.Second))
			}
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
	ss.tuneSessionXorRuntime(sess)
	ss.tuneSessionRLCRuntime(sess)

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

func (ss *stripeServer) tuneSessionXorRuntime(sess *stripeSession) {
	if sess.xorTx == nil && sess.xorRx == nil {
		return
	}

	var maxOOO uint32
	if sess.arqRx != nil {
		_, maxOOO, _ = sess.arqRx.dynamicStats()
	}
	peerLoss := atomic.LoadUint32(&sess.peerLossRate)

	if sess.xorRx != nil {
		window, _ := sess.xorRx.stats()
		desiredCap := xorRxMinCapacity
		if maxOOO > 0 {
			desiredCap = int(maxOOO*2) + window*8
		}
		_ = sess.xorRx.ensureCapacity(desiredCap)
	}

	if sess.xorTx != nil {
		window, _, _ := sess.xorTx.stats()
		stride := window / 2
		if stride < 1 {
			stride = 1
		}
		switch {
		case peerLoss >= 10 || maxOOO >= uint32(window*32):
			stride = 1
		case peerLoss >= 5 || maxOOO >= uint32(window*16):
			stride = window / 4
			if stride < 1 {
				stride = 1
			}
		}
		sess.xorTx.setStride(stride)
	}
}

func (ss *stripeServer) tuneSessionRLCRuntime(sess *stripeSession) {
	if sess.rlcTx == nil && sess.rlcRx == nil {
		return
	}

	var maxOOO uint32
	if sess.arqRx != nil {
		_, maxOOO, _ = sess.arqRx.dynamicStats()
	}
	peerLoss := atomic.LoadUint32(&sess.peerLossRate)

	if sess.rlcRx != nil {
		window, _ := sess.rlcRx.stats()
		desiredCap := rlcRxMinCapacity
		if maxOOO > 0 {
			desiredCap = int(maxOOO*2) + window*8
		}
		_ = sess.rlcRx.ensureCapacity(desiredCap)
	}

	if sess.rlcTx != nil {
		window, _, _ := sess.rlcTx.stats()
		stride := window / 2
		if stride < 1 {
			stride = 1
		}
		switch {
		case peerLoss >= 10 || maxOOO >= uint32(window*32):
			stride = 1
		case peerLoss >= 5 || maxOOO >= uint32(window*16):
			stride = window / 4
			if stride < 1 {
				stride = 1
			}
		}
		sess.rlcTx.setStride(stride)
	}
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

// dynamicPacingLoop adjusts session pacing speed based on reported peer loss.
func (ss *stripeServer) dynamicPacingLoop(ctx context.Context, sess *stripeSession) {
	if ss.pacingRate <= 0 {
		return // pacing disabled, nothing to do
	}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	// Initial pacing rate.
	baseNs := int64(1000000000 / ss.pacingRate)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			targetNs := baseNs
			// Read peer loss from keepalives.
			loss := atomic.LoadUint32(&sess.peerLossRate)
			if loss > 0 && loss < 255 {
				// E.g., at 10% loss (25), pacing slows by 25% to relieve congestion.
				targetNs += targetNs * int64(loss) / 100
			}
			atomic.StoreInt64(&sess.txtimeGapNs, targetNs)
		}
	}
}
