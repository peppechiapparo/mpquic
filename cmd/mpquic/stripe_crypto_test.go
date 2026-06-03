package main

import (
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

// TestStripeCryptoRoundtrip_Regression verifica che encrypt→decrypt sia corretto.
// Questo test definisce il comportamento atteso: qualsiasi refactoring deve passarlo.
func TestStripeCryptoRoundtrip_Regression(t *testing.T) {
	var key [32]byte
	for i := range key {
		key[i] = byte(i)
	}

	sc, err := newStripeCipher(key)
	if err != nil {
		t.Fatalf("newStripeCipher: %v", err)
	}

	// Build a minimal stripe packet: [16B header][payload]
	hdr := &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeDATA,
		Session:    0x12345678,
		GroupSeq:   1,
		ShardIdx:   0,
		GroupDataN: 1,
		DataLen:    4,
	}
	shard := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	wire := stripeEncryptShard(sc, hdr, shard)
	if len(wire) == 0 {
		t.Fatal("encrypted packet is empty")
	}

	decrypted, ok := stripeDecryptPkt(sc.aead, wire)
	if !ok {
		t.Fatal("decryption failed")
	}
	if len(decrypted) < stripeHdrLen+len(shard) {
		t.Fatalf("decrypted too short: %d", len(decrypted))
	}
	got := decrypted[stripeHdrLen:]
	if string(got) != string(shard) {
		t.Errorf("payload mismatch: got %x, want %x", got, shard)
	}
}

// BenchmarkStripeEncryptShard_Baseline misura il throughput PRIMA del refactoring.
// Questo benchmark va eseguito prima e dopo la migrazione per confronto.
func BenchmarkStripeEncryptShard_Baseline(b *testing.B) {
	var key [32]byte
	block, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCM(block)
	sc := &stripeCipher{aead: aead}

	hdr := &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeDATA,
		Session:    0xCAFEBABE,
		GroupDataN: 1,
		DataLen:    1400,
	}
	shard := make([]byte, 1400)
	var encBuf []byte

	b.SetBytes(1400)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = stripeEncryptShardReuse(sc, hdr, shard, &encBuf)
	}
}

// BenchmarkStripeDecryptPkt_Baseline misura la decifratura PRIMA del refactoring.
func BenchmarkStripeDecryptPkt_Baseline(b *testing.B) {
	var key [32]byte
	block, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCM(block)
	sc := &stripeCipher{aead: aead}

	hdr := &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeDATA,
		Session:    0xCAFEBABE,
		GroupDataN: 1,
		DataLen:    1400,
	}
	shard := make([]byte, 1400)
	wire := stripeEncryptShard(sc, hdr, shard)

	b.SetBytes(1400)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = stripeDecryptPkt(aead, wire)
	}
}

// BenchmarkStripeEncryptShard_WithProvider misura il throughput DOPO il refactoring.
// Deve essere ≤ baseline + 2% overhead.
func BenchmarkStripeEncryptShard_WithProvider(b *testing.B) {
	var key [32]byte
	sc, err := newStripeCipher(key) // ora usa AESGCMProvider internamente
	if err != nil {
		b.Fatal(err)
	}

	hdr := &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeDATA,
		Session:    0xCAFEBABE,
		GroupDataN: 1,
		DataLen:    1400,
	}
	shard := make([]byte, 1400)
	var encBuf []byte

	b.SetBytes(1400)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = stripeEncryptShardReuse(sc, hdr, shard, &encBuf)
	}
}
