package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"testing"
)

// ─── Benchmarks: verify zero-alloc hot paths ──────────────────────────────

// BenchmarkGetPutPktBuf measures the packet buffer pool round-trip.
// Expected: 0 allocs/op after warmup.
func BenchmarkGetPutPktBuf(b *testing.B) {
	// Warm up the pool
	for i := 0; i < 64; i++ {
		putPktBuf(getPktBuf(1400))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf := getPktBuf(1400)
		buf[0] = 0x45 // simulate use
		putPktBuf(buf)
	}
}

// BenchmarkGetPutPktBuf_VaryingSize tests pool with varying packet sizes (typical MTU spread).
func BenchmarkGetPutPktBuf_VaryingSize(b *testing.B) {
	sizes := []int{64, 576, 1400, 1500}
	for i := 0; i < 64; i++ {
		putPktBuf(getPktBuf(sizes[i%len(sizes)]))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sz := sizes[i%len(sizes)]
		buf := getPktBuf(sz)
		buf[0] = 0x45
		putPktBuf(buf)
	}
}

// BenchmarkDispatchActiveStackArray confirms the stack-allocated [8]int
// active-paths array doesn't escape to heap for typical path counts (1-4).
func BenchmarkDispatchActiveStackArray(b *testing.B) {
	// Simulate the dispatch active-paths logic
	pathCount := 4
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var activeArr [8]int
		active := activeArr[:0]
		for j := 0; j < pathCount; j++ {
			active = append(active, j)
		}
		_ = active[i%len(active)]
	}
}

// BenchmarkTxShardBufReuse benchmarks the reusable shard buffer pattern vs fresh alloc.
func BenchmarkTxShardBufReuse(b *testing.B) {
	b.Run("fresh_alloc", func(b *testing.B) {
		pkt := make([]byte, 1400)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			shardData := make([]byte, 2+len(pkt))
			shardData[0] = byte(len(pkt) >> 8)
			shardData[1] = byte(len(pkt))
			copy(shardData[2:], pkt)
			_ = shardData
		}
	})

	b.Run("reuse_buf", func(b *testing.B) {
		pkt := make([]byte, 1400)
		var txShardBuf []byte
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			need := 2 + len(pkt)
			if cap(txShardBuf) < need {
				txShardBuf = make([]byte, need)
			} else {
				txShardBuf = txShardBuf[:need]
			}
			txShardBuf[0] = byte(len(pkt) >> 8)
			txShardBuf[1] = byte(len(pkt))
			copy(txShardBuf[2:], pkt)
			_ = txShardBuf
		}
	})
}

// TestStripeEncryptShardReuse_Correctness verifies that stripeEncryptShardReuse
// produces ciphertext that decrypts to the same plaintext as stripeEncryptShard.
func TestStripeEncryptShardReuse_Correctness(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)

	// Two independent ciphers (separate nonce counters)
	sc1 := &stripeCipher{aead: aead}
	sc2 := &stripeCipher{aead: aead}

	shard := make([]byte, 1402)
	for i := range shard {
		shard[i] = byte(i)
	}
	hdr := &stripeHdr{
		Magic: stripeMagic, Version: stripeVersion, Type: stripeDATA,
		Session: 0xAABBCCDD, GroupSeq: 42, ShardIdx: 0, GroupDataN: 1, DataLen: 1400,
	}

	// Encrypt with original function
	wire1 := stripeEncryptShard(sc1, hdr, shard)

	// Encrypt with reuse function
	var encBuf []byte
	wire2 := stripeEncryptShardReuse(sc2, hdr, shard, &encBuf)

	// Both should decrypt successfully. Since nonces differ, ciphertext differs,
	// but plaintext must match.
	pt1, ok1 := stripeDecryptPkt(aead, wire1)
	pt2, ok2 := stripeDecryptPkt(aead, wire2)
	if !ok1 || !ok2 {
		t.Fatalf("decrypt failed: ok1=%v ok2=%v", ok1, ok2)
	}
	// Skip header (16 bytes), compare payloads
	if len(pt1) != len(pt2) {
		t.Fatalf("length mismatch: %d vs %d", len(pt1), len(pt2))
	}
	for i := stripeHdrLen; i < len(pt1); i++ {
		if pt1[i] != pt2[i] {
			t.Fatalf("payload mismatch at byte %d: %02x vs %02x", i, pt1[i], pt2[i])
		}
	}
}

// BenchmarkEncryptShardReuse compares stripeEncryptShard (allocating) vs
// stripeEncryptShardReuse (zero-alloc after warmup) for 1400-byte payloads.
func BenchmarkEncryptShardReuse(b *testing.B) {
	key := make([]byte, 32)
	rand.Read(key)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	sc := &stripeCipher{aead: aead}
	shard := make([]byte, 1402) // 2-byte prefix + 1400 payload
	hdr := &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeDATA,
		Session:    0x12345678,
		GroupSeq:   0,
		ShardIdx:   0,
		GroupDataN: 1,
		DataLen:    1400,
	}

	b.Run("allocating", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			hdr.GroupSeq = uint32(i)
			_ = stripeEncryptShard(sc, hdr, shard)
		}
	})

	b.Run("reuse_buf", func(b *testing.B) {
		var encBuf []byte
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			hdr.GroupSeq = uint32(i)
			_ = stripeEncryptShardReuse(sc, hdr, shard, &encBuf)
		}
	})
}
