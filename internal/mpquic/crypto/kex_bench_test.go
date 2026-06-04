package crypto_test

import (
	"mpquic/internal/mpquic/crypto"
	"testing"
)

var (
	benchQuicSecret = []byte("bench-quic-secret-32-bytes-padded")
	benchSessionID  = []byte("bench-session-id")
)

// BenchmarkClassicalKEX_Handshake misura il tempo completo di un handshake classico
// X25519: GenerateKeyPair (server) + GenerateKeyPair (client) + DeriveSessionKeys x2.
// Questo è l'overhead one-time per tunnel al momento della connessione.
func BenchmarkClassicalKEX_Handshake(b *testing.B) {
	p := crypto.NewClassicalKEXProvider()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		servPub, servPriv, err := p.GenerateKeyPair()
		if err != nil {
			b.Fatal(err)
		}
		clientPub, clientPriv, err := p.GenerateKeyPair()
		if err != nil {
			b.Fatal(err)
		}
		_, err = p.DeriveSessionKeys(benchQuicSecret, servPriv, clientPub, benchSessionID)
		if err != nil {
			b.Fatal(err)
		}
		_, err = p.DeriveSessionKeys(benchQuicSecret, clientPriv, servPub, benchSessionID)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHybridKEX_Handshake misura il tempo completo di un handshake ibrido
// X25519+ML-KEM-768: GenerateKeyPair (server) + ClientEncapsulate + DeriveSessionKeys x2.
// Questo è l'overhead one-time per tunnel al momento della connessione.
func BenchmarkHybridKEX_Handshake(b *testing.B) {
	p := crypto.NewHybridKEXProvider()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		servPub, servPriv, err := p.GenerateKeyPair()
		if err != nil {
			b.Fatal(err)
		}
		clientLocalPriv, clientPeerKeyShare, err := p.ClientEncapsulate(servPub)
		if err != nil {
			b.Fatal(err)
		}
		_, err = p.DeriveSessionKeys(benchQuicSecret, servPriv, clientPeerKeyShare, benchSessionID)
		if err != nil {
			b.Fatal(err)
		}
		_, err = p.DeriveSessionKeys(benchQuicSecret, clientLocalPriv, servPub[:32], benchSessionID)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClassicalKEX_DeriveOnly misura solo DeriveSessionKeys (senza keygen).
// Modella il costo quando le chiavi sono già note (es. rekeying).
func BenchmarkClassicalKEX_DeriveOnly(b *testing.B) {
	p := crypto.NewClassicalKEXProvider()
	servPub, servPriv, _ := p.GenerateKeyPair()
	clientPub, _, _ := p.GenerateKeyPair()
	_ = servPub
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := p.DeriveSessionKeys(benchQuicSecret, servPriv, clientPub, benchSessionID)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHybridKEX_DeriveOnly misura solo DeriveSessionKeys lato server (senza keygen/encapsulate).
func BenchmarkHybridKEX_DeriveOnly(b *testing.B) {
	p := crypto.NewHybridKEXProvider()
	servPub, servPriv, _ := p.GenerateKeyPair()
	clientLocalPriv, clientPeerKeyShare, _ := p.ClientEncapsulate(servPub)
	_ = clientLocalPriv
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := p.DeriveSessionKeys(benchQuicSecret, servPriv, clientPeerKeyShare, benchSessionID)
		if err != nil {
			b.Fatal(err)
		}
	}
}
