package main

// stripe_fec_rs_interleaved.go — Systematic Reed-Solomon with interleaving.
//
// Design: data packets are sent IMMEDIATELY (M=0 fast path, zero added latency).
// In parallel, each packet is accumulated in one of D interleave groups via
// round-robin (seq % depth). When a group has K data shards, RS-encode produces
// M parity shards that are emitted as stripeRS_IL_PARITY packets.
//
// A burst of up to D consecutive lost packets hits D different groups, each
// losing at most 1 shard → RS(K,1) recovers all of them.
//
// Reference: wangyu-/UDPspeeder, xtaci/kcp-go.

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/reedsolomon"
)

// ─── Constants ────────────────────────────────────────────────────────────

const (
	rsilDefaultK     = 4    // data shards per interleave generation
	rsilDefaultM     = 1    // parity shards per interleave generation
	rsilDefaultDepth = 4    // number of interleave groups (burst tolerance)
	rsilRxRingSize   = 8192 // receiver ring buffer slots (power of 2)
	rsilRxRingMask   = rsilRxRingSize - 1
	rsilMaxPending   = 256  // max pending parity groups in RX
	rsilGroupTTL     = 5 * time.Second // discard stale groups
)

// ─── TX: Interleaved RS Sender ────────────────────────────────────────────

// rsilTxGroup is one interleave generation accumulator.
type rsilTxGroup struct {
	baseSeq uint32   // seq of first data shard in this generation
	shards  [][]byte // [K] data shard contents (2-byte len prefix + payload)
	seqs    []uint32 // [K] sequence numbers
	filled  int      // shards accumulated so far
	maxLen  int      // max shard byte length (for RS padding)
}

// rsilTx distributes source packets across D interleave groups and emits
// RS parity when each group is complete.
type rsilTx struct {
	K     int
	M     int
	depth int
	enc   reedsolomon.Encoder

	groups []rsilTxGroup // [depth] active generations
	genCnt []uint64      // [depth] generation counter (for logging)

	emitted uint64 // atomic: total parity shards emitted
}

// rsilParity is a single RS parity shard ready to send.
type rsilParity struct {
	BaseSeq  uint32 // first source seq in this generation
	ShardIdx uint8  // parity index (0..M-1)
	K        uint8  // data shards (for header GroupDataN)
	Data     []byte // parity payload (length = maxLen of padded data)
}

func newRSILTx(K, M, depth int) (*rsilTx, error) {
	enc, err := reedsolomon.New(K, M, reedsolomon.WithAutoGoroutines(K+M))
	if err != nil {
		return nil, err
	}
	groups := make([]rsilTxGroup, depth)
	for i := range groups {
		groups[i].shards = make([][]byte, K)
		groups[i].seqs = make([]uint32, K)
	}
	return &rsilTx{
		K:      K,
		M:      M,
		depth:  depth,
		enc:    enc,
		groups: groups,
		genCnt: make([]uint64, depth),
	}, nil
}

// addSource stores shardData (2-byte len prefix + payload) into the appropriate
// interleave group. Returns completed parity shards when a group fills up.
func (t *rsilTx) addSource(seq uint32, shardData []byte) []rsilParity {
	gIdx := int(seq) % t.depth
	if gIdx < 0 {
		gIdx += t.depth
	}
	g := &t.groups[gIdx]

	// Start a new generation if this is the first shard
	if g.filled == 0 {
		g.baseSeq = seq
		g.maxLen = 0
	}

	// Store shard data (copy to avoid aliasing TX buffer)
	slot := g.filled
	if slot >= t.K {
		// Safety: group already full (shouldn't happen with correct flow)
		return nil
	}
	data := make([]byte, len(shardData))
	copy(data, shardData)
	g.shards[slot] = data
	g.seqs[slot] = seq
	g.filled++
	if len(data) > g.maxLen {
		g.maxLen = len(data)
	}

	if g.filled < t.K {
		return nil // not yet complete
	}

	// Group complete — RS-encode
	parities := t.encodeGroup(g)

	// Reset group for next generation
	g.filled = 0
	for i := range g.shards {
		g.shards[i] = nil
	}
	t.genCnt[gIdx]++

	return parities
}

// encodeGroup performs RS encoding on a complete group and returns M parity shards.
func (t *rsilTx) encodeGroup(g *rsilTxGroup) []rsilParity {
	maxLen := g.maxLen
	if maxLen == 0 {
		return nil
	}

	// Build shard array: K data (padded) + M parity (allocated)
	total := t.K + t.M
	shards := make([][]byte, total)
	for i := 0; i < t.K; i++ {
		padded := make([]byte, maxLen)
		copy(padded, g.shards[i])
		shards[i] = padded
	}
	for i := t.K; i < total; i++ {
		shards[i] = make([]byte, maxLen)
	}

	if err := t.enc.Encode(shards); err != nil {
		return nil
	}

	// Extract parity shards
	parities := make([]rsilParity, t.M)
	for i := 0; i < t.M; i++ {
		parities[i] = rsilParity{
			BaseSeq:  g.baseSeq,
			ShardIdx: uint8(i),
			K:        uint8(t.K),
			Data:     shards[t.K+i],
		}
	}
	atomic.AddUint64(&t.emitted, uint64(t.M))
	return parities
}

// flush emits parity for any partially-filled groups (e.g., on timer or close).
// Partial groups are padded with zero shards.
func (t *rsilTx) flush() []rsilParity {
	var all []rsilParity
	for gIdx := range t.groups {
		g := &t.groups[gIdx]
		if g.filled == 0 {
			continue
		}

		// Pad remaining slots with empty shards
		for i := g.filled; i < t.K; i++ {
			g.shards[i] = make([]byte, g.maxLen)
			g.seqs[i] = 0
		}
		g.filled = t.K

		parities := t.encodeGroup(g)
		all = append(all, parities...)

		g.filled = 0
		for i := range g.shards {
			g.shards[i] = nil
		}
		t.genCnt[gIdx]++
	}
	return all
}

// stats returns (totalEmitted).
func (t *rsilTx) stats() uint64 {
	return atomic.LoadUint64(&t.emitted)
}

// ─── RX: Interleaved RS Receiver ──────────────────────────────────────────

// rsilRxSlot stores a received data shard in the ring buffer.
type rsilRxSlot struct {
	seq   uint32
	data  []byte // [2-byte len prefix][payload]
	valid bool
}

// rsilRxGroup tracks received shards (data + parity) for one interleave generation.
type rsilRxGroup struct {
	baseSeq  uint32
	K        int
	depth    int
	shards   [][]byte // [K+M] — nil = missing
	present  []bool   // [K+M]
	received int
	maxLen   int       // max shard byte length for padding
	created  time.Time
}

// rsilRx decodes interleaved RS FEC: looks up stored data shards and combines
// with received parity to reconstruct missing data.
type rsilRx struct {
	K     int
	M     int
	depth int
	enc   reedsolomon.Encoder

	mu   sync.Mutex
	ring []rsilRxSlot // [rsilRxRingSize] circular buffer of data shards

	pendingGroups map[uint64]*rsilRxGroup // genKey → pending group

	// Stats (atomic)
	recovered    uint64
	attempts     uint64
	successes    uint64
	insufficient uint64
}

func newRSILRx(K, M, depth int) (*rsilRx, error) {
	enc, err := reedsolomon.New(K, M, reedsolomon.WithAutoGoroutines(K+M))
	if err != nil {
		return nil, err
	}
	return &rsilRx{
		K:             K,
		M:             M,
		depth:         depth,
		enc:           enc,
		ring:          make([]rsilRxSlot, rsilRxRingSize),
		pendingGroups: make(map[uint64]*rsilRxGroup),
	}, nil
}

// genKey returns a unique key for an interleave generation.
// Uses baseSeq which is unique per generation.
func rsilGenKey(baseSeq uint32) uint64 {
	return uint64(baseSeq)
}

// storeShard records a received data shard in the ring buffer.
// Called for every DATA packet in the M=0 fast path.
func (r *rsilRx) storeShard(seq uint32, data []byte) {
	r.mu.Lock()
	idx := seq & rsilRxRingMask
	slot := &r.ring[idx]
	// Store copy to avoid aliasing
	if cap(slot.data) >= len(data) {
		slot.data = slot.data[:len(data)]
	} else {
		slot.data = make([]byte, len(data))
	}
	copy(slot.data, data)
	slot.seq = seq
	slot.valid = true
	r.mu.Unlock()
}

// rsilRecoveredPkt is a recovered data packet.
type rsilRecoveredPkt struct {
	Seq uint32
	Pkt []byte // IP packet (without 2-byte length prefix)
}

// addParity processes a received RS_IL_PARITY packet. Looks up stored data shards,
// attempts RS reconstruction, and returns any recovered packets.
//
// Parameters:
//   - baseSeq: first source seq of the interleave generation (from header GroupSeq)
//   - parityIdx: parity shard index (0..M-1, from header ShardIdx)
//   - K: data shards per group (from header GroupDataN)
//   - parityData: raw parity shard payload
func (r *rsilRx) addParity(baseSeq uint32, parityIdx int, K int, parityData []byte) []rsilRecoveredPkt {
	if K <= 0 || parityIdx < 0 || parityIdx >= r.M || len(parityData) == 0 {
		return nil
	}

	atomic.AddUint64(&r.attempts, 1)
	maxLen := len(parityData) // parity length = maxLen of padded data shards

	r.mu.Lock()
	defer r.mu.Unlock()

	// Get or create pending group
	key := rsilGenKey(baseSeq)
	grp := r.pendingGroups[key]
	if grp == nil {
		total := K + r.M
		grp = &rsilRxGroup{
			baseSeq: baseSeq,
			K:       K,
			depth:   r.depth,
			shards:  make([][]byte, total),
			present: make([]bool, total),
			created: time.Now(),
		}
		r.pendingGroups[key] = grp
	}

	// Store parity shard
	pIdx := K + parityIdx
	if pIdx < len(grp.shards) && !grp.present[pIdx] {
		parCopy := make([]byte, len(parityData))
		copy(parCopy, parityData)
		grp.shards[pIdx] = parCopy
		grp.present[pIdx] = true
		grp.received++
		if len(parityData) > grp.maxLen {
			grp.maxLen = len(parityData)
		}
	}

	// Look up data shards from ring buffer
	for i := 0; i < K; i++ {
		if grp.present[i] {
			continue // already have this data shard
		}
		seq := baseSeq + uint32(i*r.depth)
		idx := seq & rsilRxRingMask
		slot := &r.ring[idx]
		if slot.valid && slot.seq == seq {
			padded := make([]byte, maxLen)
			copy(padded, slot.data)
			grp.shards[i] = padded
			grp.present[i] = true
			grp.received++
			if len(slot.data) > grp.maxLen {
				grp.maxLen = len(slot.data)
			}
		}
	}

	// Check if we can decode
	total := K + r.M
	if grp.received < K {
		atomic.AddUint64(&r.insufficient, 1)
		return nil
	}

	// Find missing data shards (these will be reconstructed)
	var missingData []int
	for i := 0; i < K; i++ {
		if !grp.present[i] {
			missingData = append(missingData, i)
		}
	}
	if len(missingData) == 0 {
		// All data present, no reconstruction needed
		delete(r.pendingGroups, key)
		return nil
	}

	// Pad all present shards to maxLen, set missing to nil
	for i := 0; i < total; i++ {
		if grp.present[i] {
			if len(grp.shards[i]) < maxLen {
				padded := make([]byte, maxLen)
				copy(padded, grp.shards[i])
				grp.shards[i] = padded
			}
		} else {
			grp.shards[i] = nil
		}
	}

	// RS Reconstruct
	if err := r.enc.Reconstruct(grp.shards); err != nil {
		delete(r.pendingGroups, key)
		return nil
	}

	// Extract recovered data packets
	var recovered []rsilRecoveredPkt
	for _, di := range missingData {
		shard := grp.shards[di]
		if len(shard) < 2 {
			continue
		}
		dataLen := int(shard[0])<<8 | int(shard[1])
		if dataLen == 0 || dataLen+2 > len(shard) {
			continue
		}
		pkt := make([]byte, dataLen)
		copy(pkt, shard[2:2+dataLen])
		seq := baseSeq + uint32(di*r.depth)
		recovered = append(recovered, rsilRecoveredPkt{Seq: seq, Pkt: pkt})
	}

	delete(r.pendingGroups, key)
	atomic.AddUint64(&r.recovered, uint64(len(recovered)))
	if len(recovered) > 0 {
		atomic.AddUint64(&r.successes, 1)
	}
	return recovered
}

// gc removes stale pending groups. Called periodically.
func (r *rsilRx) gc() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for key, grp := range r.pendingGroups {
		if now.Sub(grp.created) > rsilGroupTTL {
			delete(r.pendingGroups, key)
		}
	}
}

// stats returns (recovered, attempts, successes, insufficient).
func (r *rsilRx) stats() (uint64, uint64, uint64, uint64) {
	return atomic.LoadUint64(&r.recovered),
		atomic.LoadUint64(&r.attempts),
		atomic.LoadUint64(&r.successes),
		atomic.LoadUint64(&r.insufficient)
}
