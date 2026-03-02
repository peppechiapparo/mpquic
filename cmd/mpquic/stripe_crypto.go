package main

// stripe_crypto.go — AES-256-GCM encryption for stripe transport.
//
// Key derivation uses QUIC TLS 1.3 Exporter (RFC 5705 / RFC 8446 §7.5):
//
//  1. Client dials a temporary QUIC connection to the server (ALPN "mpquic-stripe-kx")
//  2. Both sides call ExportKeyingMaterial("mpquic-stripe-v1", sessionID_bytes, 64)
//  3. First 32 bytes = client→server AES-256 key, next 32 = server→client key
//  4. All stripe UDP packets are AES-256-GCM encrypted with the derived keys
//
// Wire format (encrypted packets):
//
//	[stripeHdr 16B — cleartext, used as AAD][8B sequence counter][ciphertext + 16B GCM tag]
//
// The 16-byte header remains in cleartext so the server can identify the session
// and look up the decryption key. It is authenticated (AAD) but not encrypted.
// Per-packet overhead: 24 bytes (8 seq + 16 tag) — vs 20 bytes for the old MAC.

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	// QUIC ALPN protocol for stripe key exchange.
	stripeKXALPN = "mpquic-stripe-kx"

	// TLS Exporter label (both sides must use the same label + context).
	stripeKXLabel = "mpquic-stripe-v1"

	// Wire overhead constants.
	stripeCryptoSeqLen   = 8  // explicit 8-byte sequence number
	stripeCryptoTagLen   = 16 // AES-GCM authentication tag
	stripeCryptoOverhead = stripeCryptoSeqLen + stripeCryptoTagLen // 24 bytes total
)

// ── Key material ──────────────────────────────────────────────────────────

// stripeKeyMaterial holds derived AES-256-GCM keys for a stripe session.
type stripeKeyMaterial struct {
	c2sKey [32]byte // client → server
	s2cKey [32]byte // server → client
}

// stripeDeriveKeys splits 64 bytes of TLS-exported material into c2s / s2c keys.
func stripeDeriveKeys(exported []byte) (*stripeKeyMaterial, error) {
	if len(exported) < 64 {
		return nil, fmt.Errorf("stripe: exported key material too short (%d < 64)", len(exported))
	}
	km := &stripeKeyMaterial{}
	copy(km.c2sKey[:], exported[:32])
	copy(km.s2cKey[:], exported[32:64])
	return km, nil
}

// ── Pending keys store (server side) ──────────────────────────────────────

// stripePendingKeys is a thread-safe store for keys that have been negotiated
// via QUIC TLS Exporter but not yet associated with a stripe session (i.e. the
// REGISTER packet has not arrived yet on the UDP port).
type stripePendingKeys struct {
	mu   sync.RWMutex
	keys map[uint32]*stripeKeyMaterial
}

func newStripePendingKeys() *stripePendingKeys {
	return &stripePendingKeys{keys: make(map[uint32]*stripeKeyMaterial)}
}

func (pk *stripePendingKeys) Store(sessionID uint32, km *stripeKeyMaterial) {
	pk.mu.Lock()
	pk.keys[sessionID] = km
	pk.mu.Unlock()
}

func (pk *stripePendingKeys) Get(sessionID uint32) *stripeKeyMaterial {
	pk.mu.RLock()
	km := pk.keys[sessionID]
	pk.mu.RUnlock()
	return km
}

func (pk *stripePendingKeys) Delete(sessionID uint32) {
	pk.mu.Lock()
	delete(pk.keys, sessionID)
	pk.mu.Unlock()
}

// ── Cipher ────────────────────────────────────────────────────────────────

// stripeCipher wraps an AES-256-GCM AEAD with a monotonic TX nonce counter.
// Safe for concurrent use (txNonce is atomic, AEAD is goroutine-safe).
type stripeCipher struct {
	aead    cipher.AEAD
	txNonce uint64 // atomic: next TX sequence number
}

// newStripeCipher creates an AES-256-GCM cipher from a 32-byte key.
func newStripeCipher(key [32]byte) (*stripeCipher, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("stripe: AES init: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("stripe: GCM init: %w", err)
	}
	return &stripeCipher{aead: aead}, nil
}

// ── Encrypt / Decrypt ─────────────────────────────────────────────────────

// stripeEncrypt encrypts a stripe packet.
// The header stays in cleartext as AAD; the payload is AES-GCM encrypted.
// Output wire format: [hdr 16B][seq 8B][ciphertext + 16B GCM tag]
//
// If sc is nil the packet is returned unmodified (no encryption configured).
func stripeEncrypt(sc *stripeCipher, pkt []byte) []byte {
	if sc == nil || len(pkt) < stripeHdrLen {
		return pkt
	}
	hdr := pkt[:stripeHdrLen]
	payload := pkt[stripeHdrLen:]

	// Monotonic sequence number — unique per (key, direction).
	seq := atomic.AddUint64(&sc.txNonce, 1) - 1

	// 12-byte GCM nonce: [4 zero bytes][8-byte seq].
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)

	// AAD = [header 16B][seq 8B]  — authenticated but not encrypted.
	aad := make([]byte, stripeHdrLen+stripeCryptoSeqLen)
	copy(aad, hdr)
	binary.BigEndian.PutUint64(aad[stripeHdrLen:], seq)

	ciphertext := sc.aead.Seal(nil, nonce[:], payload, aad)

	out := make([]byte, len(aad)+len(ciphertext))
	copy(out, aad)
	copy(out[len(aad):], ciphertext)
	return out
}

// stripeDecryptPkt decrypts an AES-GCM encrypted stripe packet.
// Returns the reconstructed cleartext [hdr][payload] on success.
func stripeDecryptPkt(aead cipher.AEAD, pkt []byte) ([]byte, bool) {
	minLen := stripeHdrLen + stripeCryptoSeqLen + aead.Overhead()
	if len(pkt) < minLen {
		return nil, false
	}

	aad := pkt[:stripeHdrLen+stripeCryptoSeqLen]
	seq := binary.BigEndian.Uint64(pkt[stripeHdrLen : stripeHdrLen+stripeCryptoSeqLen])
	ciphertext := pkt[stripeHdrLen+stripeCryptoSeqLen:]

	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)

	plaintext, err := aead.Open(nil, nonce[:], ciphertext, aad)
	if err != nil {
		return nil, false
	}

	out := make([]byte, stripeHdrLen+len(plaintext))
	copy(out[:stripeHdrLen], pkt[:stripeHdrLen])
	copy(out[stripeHdrLen:], plaintext)
	return out, true
}

// ── Helpers ───────────────────────────────────────────────────────────────

// stripeComputeSessionID computes the unique session ID for a (TUN IP, path) pair.
func stripeComputeSessionID(cfg *Config, pathName string) (uint32, error) {
	tunIP, err := parseTUNIP(cfg.TunCIDR)
	if err != nil {
		return 0, fmt.Errorf("stripe: parse tun cidr: %w", err)
	}
	return pathSessionID(tunIP, pathName), nil
}
