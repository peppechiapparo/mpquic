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

// TestNewStripeCiphers_ShortQuicSecret_StripeCryptoEnabled verifica SEC-G04.
// Se StripeCryptoEnabled=true ma quicSecret < 64, newStripeCiphers deve
// ritornare un errore esplicito (non silent downgrade al path legacy).
func TestNewStripeCiphers_ShortQuicSecret_StripeCryptoEnabled(t *testing.T) {
	cfg := &Config{
		StripeCryptoEnabled: true,
	}

	// Crea una stripeKeyMaterial con quicSecret di lunghezza 32 (insufficiente)
	keys := &stripeKeyMaterial{
		quicSecret: make([]byte, 32), // Troppo corto: want >= 64
		c2sKey:     [32]byte{},
		s2cKey:     [32]byte{},
	}

	// newStripeCiphers deve ritornare errore, non fallback silenzioso
	tx, rx, err := newStripeCiphers(cfg, keys, false)
	if err == nil {
		t.Fatal("expected error for short quicSecret, got nil")
	}
	if tx != nil || rx != nil {
		t.Errorf("expected nil ciphers on error, got tx=%v rx=%v", tx, rx)
	}
	// Verifica che l'errore menziona la lunghezza della chiave (≥64 o >=64)
	if !contains(err.Error(), "quicSecret") || !contains(err.Error(), "64") {
		t.Errorf("error should mention 'quicSecret' and '64', got: %v", err)
	}
}

// TestNewStripeCiphers_ValidQuicSecret_StripeCryptoEnabled verifica che
// con StripeCryptoEnabled=true e quicSecret >= 64, la funzione non fallisce
// per lunghezza (può fallire per altri motivi, ma non per short secret).
func TestNewStripeCiphers_ValidQuicSecret_StripeCryptoEnabled(t *testing.T) {
	cfg := &Config{
		StripeCryptoEnabled: true,
	}

	// Crea una stripeKeyMaterial con quicSecret di lunghezza 64 (valida)
	keys := &stripeKeyMaterial{
		quicSecret: make([]byte, 64), // Valida
		c2sKey:     [32]byte{},
		s2cKey:     [32]byte{},
	}

	// newStripeCiphers non deve fallire per lunghezza quicSecret
	_, _, err := newStripeCiphers(cfg, keys, false)
	// Potrebbe fallire per altri motivi (es. cryptoSession setup), ma non per
	// "short quicSecret"
	if err != nil && contains(err.Error(), "need >=64") {
		t.Errorf("short quicSecret error should not occur with len=64, got: %v", err)
	}
}

// TestNewStripeCiphers_ShortQuicSecret_LegacyPath verifica che
// con StripeCryptoEnabled=false, anche quicSecret corto va bene (path legacy).
func TestNewStripeCiphers_ShortQuicSecret_LegacyPath(t *testing.T) {
	cfg := &Config{
		StripeCryptoEnabled: false, // Legacy path
	}

	// Anche con quicSecret corto, non deve fallire se siamo sul path legacy
	keys := &stripeKeyMaterial{
		quicSecret: make([]byte, 32),
		c2sKey:     [32]byte{},
		s2cKey:     [32]byte{},
	}

	// Sul path legacy, non deve controllare la lunghezza di quicSecret
	tx, rx, err := newStripeCiphers(cfg, keys, false)
	if err != nil {
		t.Fatalf("legacy path should not fail on short quicSecret: %v", err)
	}
	if tx == nil || rx == nil {
		t.Error("expected non-nil ciphers on legacy path")
	}
}

// contains è un helper per verificare se una stringa contiene una substring
func contains(s, substr string) bool {
	// Simple string search
	for i := 0; i < len(s)-len(substr)+1; i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
