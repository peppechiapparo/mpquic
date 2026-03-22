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
	"encoding/binary"
	"sync"
	"time"
)

// ─── Constants ────────────────────────────────────────────────────────────

// stripePktPool provides reusable packet buffers for the server RX → TUN path.
// Avoids per-packet allocation in the handleDataShard → rxCh → tunWriter pipeline.
// Buffers are MTU-sized; resliced to actual packet length on Get.
var stripePktPool = sync.Pool{
	New: func() any {
		b := make([]byte, stripeMaxPayload)
		return b
	},
}

func getPktBuf(n int) []byte {
	b := stripePktPool.Get().([]byte)
	if cap(b) >= n {
		return b[:n]
	}
	// Rare: oversized packet, allocate fresh.
	return make([]byte, n)
}

func putPktBuf(b []byte) {
	if cap(b) >= stripeMaxPayload {
		stripePktPool.Put(b[:cap(b)])
	}
}

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

