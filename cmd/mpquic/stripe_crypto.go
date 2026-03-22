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

// stripeEncrypt encrypts a stripe packet in-place-friendly fashion.
// The header stays in cleartext as AAD; the payload is AES-GCM encrypted.
// Output wire format: [hdr 16B][seq 8B][ciphertext + 16B GCM tag]
//
// If sc is nil the packet is returned unmodified (no encryption configured).
//
// Hot path — optimised for 1 heap allocation per call.
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

	// AAD on stack — no heap allocation.
	var aad [stripeHdrLen + stripeCryptoSeqLen]byte
	copy(aad[:stripeHdrLen], hdr)
	binary.BigEndian.PutUint64(aad[stripeHdrLen:], seq)

	// Single allocation: [hdr 16][seq 8][ciphertext+tag].
	outLen := stripeHdrLen + stripeCryptoSeqLen + len(payload) + sc.aead.Overhead()
	out := make([]byte, stripeHdrLen+stripeCryptoSeqLen, outLen)
	copy(out, aad[:])
	// Seal appends ciphertext+tag after header+seq.
	out = sc.aead.Seal(out, nonce[:], payload, aad[:])
	return out
}

// stripeEncryptShard builds a stripe packet from header fields + shard payload
// and encrypts it in a single allocation. Used by the FEC TX path to avoid the
// intermediate [hdr][payload] buffer that stripeEncrypt would then re-copy.
//
// Returns the encrypted wire packet ready for sendto(). If sc is nil, returns
// the cleartext packet with 0 extra allocations beyond the packet itself.
func stripeEncryptShard(sc *stripeCipher, hdr *stripeHdr, shard []byte) []byte {
	if sc == nil {
		pkt := make([]byte, stripeHdrLen+len(shard))
		encodeStripeHdr(pkt, hdr)
		copy(pkt[stripeHdrLen:], shard)
		return pkt
	}

	seq := atomic.AddUint64(&sc.txNonce, 1) - 1

	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)

	// Build AAD on the stack: [encoded_header 16][seq 8].
	var aad [stripeHdrLen + stripeCryptoSeqLen]byte
	encodeStripeHdr(aad[:stripeHdrLen], hdr)
	binary.BigEndian.PutUint64(aad[stripeHdrLen:], seq)

	// Single allocation for the entire wire packet.
	outLen := stripeHdrLen + stripeCryptoSeqLen + len(shard) + sc.aead.Overhead()
	out := make([]byte, stripeHdrLen+stripeCryptoSeqLen, outLen)
	copy(out, aad[:])
	out = sc.aead.Seal(out, nonce[:], shard, aad[:])
	return out
}

// stripeEncryptShardReuse is like stripeEncryptShard but reuses a caller-provided
// output buffer when capacity is sufficient (zero-alloc hot path).
// The returned slice shares backing memory with *buf. Caller must not retain
// the result beyond the next call with the same buf (it will be overwritten).
// Used on the client TX M=0 fast path where gsoAccumLocked/writePacedUDP
// copy the wire packet before returning.
func stripeEncryptShardReuse(sc *stripeCipher, hdr *stripeHdr, shard []byte, buf *[]byte) []byte {
	if sc == nil {
		pkt := make([]byte, stripeHdrLen+len(shard))
		encodeStripeHdr(pkt, hdr)
		copy(pkt[stripeHdrLen:], shard)
		return pkt
	}

	seq := atomic.AddUint64(&sc.txNonce, 1) - 1

	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)

	var aad [stripeHdrLen + stripeCryptoSeqLen]byte
	encodeStripeHdr(aad[:stripeHdrLen], hdr)
	binary.BigEndian.PutUint64(aad[stripeHdrLen:], seq)

	outLen := stripeHdrLen + stripeCryptoSeqLen + len(shard) + sc.aead.Overhead()
	if cap(*buf) < outLen {
		*buf = make([]byte, outLen)
	}
	out := (*buf)[:stripeHdrLen+stripeCryptoSeqLen]
	copy(out, aad[:])
	out = sc.aead.Seal(out, nonce[:], shard, aad[:])
	return out
}

// stripeDecryptPkt decrypts an AES-GCM encrypted stripe packet.
// Returns the reconstructed cleartext [hdr][payload] on success.
//
// Hot path — optimised for 1 heap allocation per call.
func stripeDecryptPkt(aead cipher.AEAD, pkt []byte) ([]byte, bool) {
	minLen := stripeHdrLen + stripeCryptoSeqLen + aead.Overhead()
	if len(pkt) < minLen {
		return nil, false
	}

	aadEnd := stripeHdrLen + stripeCryptoSeqLen
	seq := binary.BigEndian.Uint64(pkt[stripeHdrLen:aadEnd])
	ciphertext := pkt[aadEnd:]

	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)

	// Single allocation: [hdr 16][plaintext].
	ptLen := len(ciphertext) - aead.Overhead()
	out := make([]byte, stripeHdrLen, stripeHdrLen+ptLen)
	copy(out, pkt[:stripeHdrLen])
	var err error
	out, err = aead.Open(out, nonce[:], ciphertext, pkt[:aadEnd])
	if err != nil {
		return nil, false
	}
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
