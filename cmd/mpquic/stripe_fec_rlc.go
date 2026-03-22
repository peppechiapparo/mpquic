package main

import (
	"encoding/binary"
	"sort"
	"sync"
	"sync/atomic"
)

const (
	rlcSeedLen       = 4
	rlcRxMinCapacity = 512
	rlcRxMaxCapacity = 8192
	rlcMaxRepairs    = 256
	rlcMaxUnknowns   = 32
)

var (
	rlcGFExp [512]byte
	rlcGFLog [256]byte
)

func init() {
	var x byte = 1
	for i := 0; i < 255; i++ {
		rlcGFExp[i] = x
		rlcGFLog[x] = byte(i)
		x = rlcGFMultiplyNoTable(x, 0x02)
	}
	for i := 255; i < len(rlcGFExp); i++ {
		rlcGFExp[i] = rlcGFExp[i-255]
	}
}

func rlcGFMultiplyNoTable(a, b byte) byte {
	var res byte
	for b > 0 {
		if b&1 != 0 {
			res ^= a
		}
		hi := a & 0x80
		a <<= 1
		if hi != 0 {
			a ^= 0x1d
		}
		b >>= 1
	}
	return res
}

func rlcGFMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return rlcGFExp[int(rlcGFLog[a])+int(rlcGFLog[b])]
}

func rlcGFInv(a byte) byte {
	if a == 0 {
		return 0
	}
	return rlcGFExp[255-int(rlcGFLog[a])]
}

func rlcCoeff(seed uint32, idx int) byte {
	v := seed ^ (uint32(idx+1) * 0x9e3779b1)
	v ^= v >> 16
	v *= 0x7feb352d
	v ^= v >> 15
	return byte(v%255 + 1)
}

type rlcSourceShard struct {
	seq  uint32
	data []byte
}

type rlcFECSender struct {
	window int
	stride int

	history         []rlcSourceShard
	sinceLastRepair int
	nextSeed        uint32

	emitted uint64
}

func newRLCFECSender(window int) *rlcFECSender {
	if window <= 0 {
		window = xorFECDefaultWindow
	}
	stride := window / 2
	if stride < 1 {
		stride = 1
	}
	return &rlcFECSender{
		window:  window,
		stride:  stride,
		history: make([]rlcSourceShard, 0, window),
		nextSeed: 1,
	}
}

func (r *rlcFECSender) setStride(stride int) {
	if stride < 1 {
		stride = 1
	}
	if stride > r.window {
		stride = r.window
	}
	r.stride = stride
}

func (r *rlcFECSender) stats() (window, stride, historyLen int) {
	return r.window, r.stride, len(r.history)
}

func (r *rlcFECSender) addSource(seq uint32, shardData []byte) (repair []byte, firstSeq uint32, count int, ok bool) {
	dataCopy := make([]byte, len(shardData))
	copy(dataCopy, shardData)
	r.history = append(r.history, rlcSourceShard{seq: seq, data: dataCopy})
	if len(r.history) > r.window {
		copy(r.history, r.history[1:])
		r.history = r.history[:r.window]
	}
	r.sinceLastRepair++
	if len(r.history) < r.window || r.sinceLastRepair < r.stride {
		return nil, 0, 0, false
	}
	repair, firstSeq, count = r.buildRepairLocked(len(r.history))
	r.sinceLastRepair = 0
	atomic.AddUint64(&r.emitted, 1)
	return repair, firstSeq, count, true
}

func (r *rlcFECSender) flush() (repair []byte, firstSeq uint32, count int, ok bool) {
	if len(r.history) == 0 {
		return nil, 0, 0, false
	}
	count = len(r.history)
	if len(r.history) >= r.window && r.sinceLastRepair > 0 && r.sinceLastRepair < r.window {
		count = r.sinceLastRepair
	}
	repair, firstSeq, count = r.buildRepairLocked(count)
	r.sinceLastRepair = 0
	atomic.AddUint64(&r.emitted, 1)
	return repair, firstSeq, count, true
}

func (r *rlcFECSender) buildRepairLocked(count int) (repair []byte, firstSeq uint32, outCount int) {
	if count <= 0 || count > len(r.history) {
		return nil, 0, 0
	}
	start := len(r.history) - count
	seed := r.nextSeed
	r.nextSeed++
	firstSeq = r.history[start].seq
	outCount = count
	maxLen := 0
	for i := start; i < len(r.history); i++ {
		if l := len(r.history[i].data); l > maxLen {
			maxLen = l
		}
	}
	repair = make([]byte, rlcSeedLen+maxLen)
	binary.BigEndian.PutUint32(repair[:rlcSeedLen], seed)
	acc := repair[rlcSeedLen:]
	for i := start; i < len(r.history); i++ {
		coeff := rlcCoeff(seed, i-start)
		for j := 0; j < len(r.history[i].data); j++ {
			acc[j] ^= rlcGFMul(coeff, r.history[i].data[j])
		}
	}
	return repair, firstSeq, outCount
}

type rlcRepairEquation struct {
	firstSeq uint32
	window   int
	seed     uint32
	coded    []byte
}

type rlcRecoveredPacket struct {
	seq uint32
	pkt []byte
}

type rlcFECReceiver struct {
	window   int
	capacity int

	mu      sync.Mutex
	ring    []xorRxSlot
	repairs []rlcRepairEquation
	hiSeq   uint32

	recovered      uint64
	decodeFailures uint64
	repairsRecv    uint64
}

func newRLCFECReceiver(window int) *rlcFECReceiver {
	if window <= 0 {
		window = xorFECDefaultWindow
	}
	cap := window * xorRxBufKeepWindows
	if cap < rlcRxMinCapacity {
		cap = rlcRxMinCapacity
	}
	ring := make([]xorRxSlot, cap)
	for i := range ring {
		ring[i].data = make([]byte, 0, stripeMaxPayload+2)
	}
	return &rlcFECReceiver{window: window, capacity: cap, ring: ring}
}

func (r *rlcFECReceiver) stats() (window, capacity int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.window, r.capacity
}

func (r *rlcFECReceiver) ensureCapacity(minCapacity int) bool {
	if minCapacity < rlcRxMinCapacity {
		minCapacity = rlcRxMinCapacity
	}
	if minCapacity > rlcRxMaxCapacity {
		minCapacity = rlcRxMaxCapacity
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if minCapacity <= r.capacity {
		return false
	}
	newCap := r.capacity * 2
	if newCap < minCapacity {
		newCap = minCapacity
	}
	if newCap > rlcRxMaxCapacity {
		newCap = rlcRxMaxCapacity
	}
	newRing := make([]xorRxSlot, newCap)
	for i := range newRing {
		newRing[i].data = make([]byte, 0, stripeMaxPayload+2)
	}
	for i := range r.ring {
		slot := &r.ring[i]
		if !slot.valid {
			continue
		}
		idx := int(slot.seq % uint32(newCap))
		dst := &newRing[idx]
		dst.data = append(dst.data[:0], slot.data...)
		dst.seq = slot.seq
		dst.valid = true
	}
	r.ring = newRing
	r.capacity = newCap
	return true
}

func (r *rlcFECReceiver) storeShard(seq uint32, data []byte) {
	idx := int(seq % uint32(r.capacity))
	r.mu.Lock()
	slot := &r.ring[idx]
	if cap(slot.data) >= len(data) {
		slot.data = slot.data[:len(data)]
	} else {
		slot.data = make([]byte, len(data))
	}
	copy(slot.data, data)
	slot.seq = seq
	slot.valid = true
	if seq > r.hiSeq {
		r.hiSeq = seq
	}
	r.dropSatisfiedRepairsLocked()
	r.mu.Unlock()
}

func (r *rlcFECReceiver) lookupShardLocked(seq uint32) ([]byte, bool) {
	idx := int(seq % uint32(r.capacity))
	slot := &r.ring[idx]
	if slot.valid && slot.seq == seq {
		return slot.data, true
	}
	return nil, false
}

func (r *rlcFECReceiver) addRepair(firstSeq uint32, window int, payload []byte) []rlcRecoveredPacket {
	if len(payload) <= rlcSeedLen || window <= 0 {
		return nil
	}
	seed := binary.BigEndian.Uint32(payload[:rlcSeedLen])
	coded := make([]byte, len(payload)-rlcSeedLen)
	copy(coded, payload[rlcSeedLen:])

	r.mu.Lock()
	r.repairs = append(r.repairs, rlcRepairEquation{firstSeq: firstSeq, window: window, seed: seed, coded: coded})
	if len(r.repairs) > rlcMaxRepairs {
		r.repairs = r.repairs[len(r.repairs)-rlcMaxRepairs:]
	}
	atomic.AddUint64(&r.repairsRecv, 1)
	recovered := r.tryDecodeLocked()
	r.mu.Unlock()
	return recovered
}

func (r *rlcFECReceiver) tryDecodeLocked() []rlcRecoveredPacket {
	var recovered []rlcRecoveredPacket
	for {
		missingSeqs := r.collectMissingSeqsLocked()
		if len(missingSeqs) == 0 || len(missingSeqs) > rlcMaxUnknowns {
			return recovered
		}
		seqToCol := make(map[uint32]int, len(missingSeqs))
		for i, seq := range missingSeqs {
			seqToCol[seq] = i
		}

		rows, rhs := r.buildSystemLocked(missingSeqs, seqToCol)
		if len(rows) < len(missingSeqs) {
			return recovered
		}
		solution, ok := rlcSolveSystem(rows, rhs, len(missingSeqs))
		if !ok {
			atomic.AddUint64(&r.decodeFailures, 1)
			return recovered
		}

		newRecovered := 0
		for i, seq := range missingSeqs {
			shard := solution[i]
			if len(shard) < 2 {
				continue
			}
			dataLen := binary.BigEndian.Uint16(shard[:2])
			if dataLen == 0 || int(dataLen)+2 > len(shard) {
				continue
			}
			if _, ok := r.lookupShardLocked(seq); ok {
				continue
			}
			idx := int(seq % uint32(r.capacity))
			slot := &r.ring[idx]
			if cap(slot.data) >= len(shard) {
				slot.data = slot.data[:len(shard)]
			} else {
				slot.data = make([]byte, len(shard))
			}
			copy(slot.data, shard)
			slot.seq = seq
			slot.valid = true
			pkt := make([]byte, dataLen)
			copy(pkt, shard[2:2+dataLen])
			recovered = append(recovered, rlcRecoveredPacket{seq: seq, pkt: pkt})
			atomic.AddUint64(&r.recovered, 1)
			newRecovered++
		}
		if newRecovered == 0 {
			return recovered
		}
		r.dropSatisfiedRepairsLocked()
	}
}

func (r *rlcFECReceiver) collectMissingSeqsLocked() []uint32 {
	set := make(map[uint32]struct{})
	for _, eq := range r.repairs {
		for i := 0; i < eq.window; i++ {
			seq := eq.firstSeq + uint32(i)
			if _, ok := r.lookupShardLocked(seq); !ok {
				set[seq] = struct{}{}
			}
		}
	}
	seqs := make([]uint32, 0, len(set))
	for seq := range set {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs
}

func (r *rlcFECReceiver) buildSystemLocked(missingSeqs []uint32, seqToCol map[uint32]int) ([][]byte, [][]byte) {
	rows := make([][]byte, 0, len(r.repairs))
	rhs := make([][]byte, 0, len(r.repairs))
	for _, eq := range r.repairs {
		row := make([]byte, len(missingSeqs))
		residual := make([]byte, len(eq.coded))
		copy(residual, eq.coded)
		hasUnknown := false
		for i := 0; i < eq.window; i++ {
			seq := eq.firstSeq + uint32(i)
			coeff := rlcCoeff(eq.seed, i)
			if shard, ok := r.lookupShardLocked(seq); ok {
				for j := 0; j < len(shard) && j < len(residual); j++ {
					residual[j] ^= rlcGFMul(coeff, shard[j])
				}
				continue
			}
			col, ok := seqToCol[seq]
			if !ok {
				continue
			}
			row[col] ^= coeff
			hasUnknown = true
		}
		if hasUnknown {
			rows = append(rows, row)
			rhs = append(rhs, residual)
		}
	}
	return rows, rhs
}

func (r *rlcFECReceiver) dropSatisfiedRepairsLocked() {
	if len(r.repairs) == 0 {
		return
	}
	filtered := r.repairs[:0]
	for _, eq := range r.repairs {
		keep := false
		for i := 0; i < eq.window; i++ {
			seq := eq.firstSeq + uint32(i)
			if _, ok := r.lookupShardLocked(seq); !ok {
				keep = true
				break
			}
		}
		if keep {
			filtered = append(filtered, eq)
		}
	}
	r.repairs = filtered
}

func rlcSolveSystem(rows [][]byte, rhs [][]byte, cols int) ([][]byte, bool) {
	rowCount := len(rows)
	maxLen := 0
	for _, v := range rhs {
		if len(v) > maxLen {
			maxLen = len(v)
		}
	}
	mat := make([][]byte, rowCount)
	out := make([][]byte, rowCount)
	for i := range rows {
		mat[i] = append([]byte(nil), rows[i]...)
		out[i] = make([]byte, maxLen)
		copy(out[i], rhs[i])
	}
	pivotRow := 0
	pivotForCol := make([]int, cols)
	for i := range pivotForCol {
		pivotForCol[i] = -1
	}
	for col := 0; col < cols && pivotRow < rowCount; col++ {
		pivot := -1
		for r := pivotRow; r < rowCount; r++ {
			if mat[r][col] != 0 {
				pivot = r
				break
			}
		}
		if pivot == -1 {
			continue
		}
		mat[pivotRow], mat[pivot] = mat[pivot], mat[pivotRow]
		out[pivotRow], out[pivot] = out[pivot], out[pivotRow]
		inv := rlcGFInv(mat[pivotRow][col])
		for c := col; c < cols; c++ {
			mat[pivotRow][c] = rlcGFMul(mat[pivotRow][c], inv)
		}
		for j := 0; j < maxLen; j++ {
			out[pivotRow][j] = rlcGFMul(out[pivotRow][j], inv)
		}
		for r := 0; r < rowCount; r++ {
			if r == pivotRow || mat[r][col] == 0 {
				continue
			}
			factor := mat[r][col]
			for c := col; c < cols; c++ {
				mat[r][c] ^= rlcGFMul(factor, mat[pivotRow][c])
			}
			for j := 0; j < maxLen; j++ {
				out[r][j] ^= rlcGFMul(factor, out[pivotRow][j])
			}
		}
		pivotForCol[col] = pivotRow
		pivotRow++
	}
	for _, prow := range pivotForCol {
		if prow == -1 {
			return nil, false
		}
	}
	solution := make([][]byte, cols)
	for col, prow := range pivotForCol {
		solution[col] = append([]byte(nil), out[prow]...)
	}
	return solution, true
}