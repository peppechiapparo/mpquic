package main

import (
	"encoding/binary"
	"testing"
)

// ─── Wire Protocol Tests ──────────────────────────────────────────────────

func TestStripeHdrEncodeDecode(t *testing.T) {
	original := stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeDATA,
		Session:    0xDEADBEEF,
		GroupSeq:   42,
		ShardIdx:   7,
		GroupDataN: 10,
		DataLen:    1400,
	}

	buf := make([]byte, stripeHdrLen)
	encodeStripeHdr(buf, &original)

	decoded, ok := decodeStripeHdr(buf)
	if !ok {
		t.Fatal("decodeStripeHdr failed")
	}

	if decoded != original {
		t.Errorf("mismatch:\n  got  %+v\n  want %+v", decoded, original)
	}
}

func TestStripeHdrBadMagic(t *testing.T) {
	buf := make([]byte, stripeHdrLen)
	binary.BigEndian.PutUint16(buf[0:2], 0xFFFF) // wrong magic
	_, ok := decodeStripeHdr(buf)
	if ok {
		t.Error("expected decode to fail with bad magic")
	}
}

func TestStripeHdrTooShort(t *testing.T) {
	buf := make([]byte, stripeHdrLen-1)
	_, ok := decodeStripeHdr(buf)
	if ok {
		t.Error("expected decode to fail with short buffer")
	}
}

// ─── FEC Group Tests ──────────────────────────────────────────────────────

func TestFECGroupAddShard(t *testing.T) {
	grp := newFECGroup(4, 2) // K=4 data, M=2 parity

	// Add 3 data shards — not yet decodable
	for i := 0; i < 3; i++ {
		decodable := grp.addShard(i, []byte{byte(i), 1, 2, 3})
		if decodable {
			t.Errorf("should not be decodable with %d shards", i+1)
		}
	}

	// Add 4th shard — now decodable (K=4 received)
	decodable := grp.addShard(3, []byte{3, 1, 2, 3})
	if !decodable {
		t.Error("should be decodable with K=4 shards")
	}
}

func TestFECGroupDuplicateShard(t *testing.T) {
	grp := newFECGroup(2, 1)
	grp.addShard(0, []byte{0, 1})
	grp.addShard(0, []byte{0, 1}) // duplicate
	if grp.received != 1 {
		t.Errorf("duplicate shard should not increase received count, got %d", grp.received)
	}
}

func TestFECGroupOutOfRange(t *testing.T) {
	grp := newFECGroup(2, 1)
	grp.addShard(-1, []byte{0})
	grp.addShard(3, []byte{0}) // beyond K+M
	if grp.received != 0 {
		t.Errorf("out of range shards should not be counted, got %d", grp.received)
	}
}

// ─── Helper Tests ─────────────────────────────────────────────────────────

func TestParseTUNIP(t *testing.T) {
	tests := []struct {
		input string
		want  string
		err   bool
	}{
		{"10.200.17.1/30", "10.200.17.1", false},
		{"10.200.17.1", "10.200.17.1", false},
		{"192.168.1.0/24", "192.168.1.0", false},
		{"not-an-ip", "", true},
	}

	for _, tt := range tests {
		ip, err := parseTUNIP(tt.input)
		if tt.err {
			if err == nil {
				t.Errorf("parseTUNIP(%q): expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTUNIP(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if ip.String() != tt.want {
			t.Errorf("parseTUNIP(%q) = %q, want %q", tt.input, ip.String(), tt.want)
		}
	}
}

func TestIPToUint32(t *testing.T) {
	tests := []struct {
		ip   string
		want uint32
	}{
		{"10.200.17.1", 0x0AC81101},
		{"0.0.0.0", 0},
		{"255.255.255.255", 0xFFFFFFFF},
	}

	for _, tt := range tests {
		ip, _ := parseTUNIP(tt.ip)
		got := ipToUint32(ip)
		if got != tt.want {
			t.Errorf("ipToUint32(%s) = 0x%08X, want 0x%08X", tt.ip, got, tt.want)
		}
	}
}

func TestStripeHdrRegisterPacket(t *testing.T) {
	// Verify a complete register packet can be encoded and decoded
	sessionID := ipToUint32(func() []byte {
		ip, _ := parseTUNIP("10.200.17.1")
		return ip.To4()
	}())
	_ = sessionID // just verify it doesn't panic

	regPayload := make([]byte, 6)
	binary.BigEndian.PutUint32(regPayload[0:4], 0x0AC81101)
	regPayload[4] = 2 // pipeIdx
	regPayload[5] = 4 // totalPipes

	pkt := make([]byte, stripeHdrLen+len(regPayload))
	encodeStripeHdr(pkt, &stripeHdr{
		Magic:   stripeMagic,
		Version: stripeVersion,
		Type:    stripeREGISTER,
		Session: 0x0AC81101,
		DataLen: uint16(len(regPayload)),
	})
	copy(pkt[stripeHdrLen:], regPayload)

	hdr, ok := decodeStripeHdr(pkt)
	if !ok {
		t.Fatal("failed to decode register packet")
	}
	if hdr.Type != stripeREGISTER {
		t.Errorf("type = %d, want %d", hdr.Type, stripeREGISTER)
	}
	if hdr.Session != 0x0AC81101 {
		t.Errorf("session = 0x%08X, want 0x0AC81101", hdr.Session)
	}

	// Verify payload
	payload := pkt[stripeHdrLen:]
	gotIP := binary.BigEndian.Uint32(payload[0:4])
	if gotIP != 0x0AC81101 {
		t.Errorf("register IP = 0x%08X, want 0x0AC81101", gotIP)
	}
	if payload[4] != 2 {
		t.Errorf("pipeIdx = %d, want 2", payload[4])
	}
	if payload[5] != 4 {
		t.Errorf("totalPipes = %d, want 4", payload[5])
	}
}

func TestPathSessionID_UniquePerPath(t *testing.T) {
	ip, _ := parseTUNIP("10.200.17.1")

	sid1 := pathSessionID(ip, "wan5")
	sid2 := pathSessionID(ip, "wan6")

	if sid1 == sid2 {
		t.Errorf("pathSessionID should differ for wan5 vs wan6, both = 0x%08X", sid1)
	}

	// Same path name → same session ID
	sid1b := pathSessionID(ip, "wan5")
	if sid1 != sid1b {
		t.Errorf("pathSessionID should be deterministic: 0x%08X != 0x%08X", sid1, sid1b)
	}
}

// ─── AES-256-GCM Encryption Tests ────────────────────────────────────────

func TestStripeDeriveKeys(t *testing.T) {
	material := make([]byte, 64)
	for i := range material {
		material[i] = byte(i)
	}

	km, err := stripeDeriveKeys(material)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify keys are different
	if km.c2sKey == km.s2cKey {
		t.Fatal("c2s and s2c keys should differ")
	}

	// Verify deterministic
	km2, _ := stripeDeriveKeys(material)
	if km.c2sKey != km2.c2sKey || km.s2cKey != km2.s2cKey {
		t.Fatal("stripeDeriveKeys should be deterministic")
	}

	// Too short input
	_, err = stripeDeriveKeys(make([]byte, 32))
	if err == nil {
		t.Fatal("expected error for short input")
	}
}

func TestStripeEncryptDecrypt(t *testing.T) {
	var key [32]byte
	for i := range key {
		key[i] = byte(i + 1)
	}

	txCipher, err := newStripeCipher(key)
	if err != nil {
		t.Fatalf("newStripeCipher failed: %v", err)
	}
	rxCipher, err := newStripeCipher(key)
	if err != nil {
		t.Fatalf("newStripeCipher failed: %v", err)
	}

	// Build a test packet
	pkt := make([]byte, stripeHdrLen+20)
	encodeStripeHdr(pkt, &stripeHdr{
		Magic:      stripeMagic,
		Version:    stripeVersion,
		Type:       stripeDATA,
		Session:    0xDEADBEEF,
		GroupSeq:   42,
		ShardIdx:   3,
		GroupDataN: 10,
		DataLen:    20,
	})
	for i := stripeHdrLen; i < len(pkt); i++ {
		pkt[i] = byte(i)
	}

	// Encrypt
	encrypted := stripeEncrypt(txCipher, pkt)
	if len(encrypted) != len(pkt)+stripeCryptoOverhead {
		t.Fatalf("encrypted len: got=%d want=%d", len(encrypted), len(pkt)+stripeCryptoOverhead)
	}

	// Header must be preserved in cleartext
	hdr, ok := decodeStripeHdr(encrypted)
	if !ok {
		t.Fatal("cleartext header should be decodable")
	}
	if hdr.Session != 0xDEADBEEF {
		t.Errorf("session mismatch: got=0x%08X want=0xDEADBEEF", hdr.Session)
	}

	// Decrypt
	decrypted, ok := stripeDecryptPkt(rxCipher.aead, encrypted)
	if !ok {
		t.Fatal("decrypt failed")
	}
	if len(decrypted) != len(pkt) {
		t.Fatalf("decrypted len: got=%d want=%d", len(decrypted), len(pkt))
	}

	// Payload must match original
	for i := stripeHdrLen; i < len(pkt); i++ {
		if decrypted[i] != pkt[i] {
			t.Fatalf("payload mismatch at byte %d: got=%d want=%d", i, decrypted[i], pkt[i])
		}
	}

	// Tamper → must fail
	encrypted[len(encrypted)-1] ^= 0x01
	_, ok = stripeDecryptPkt(rxCipher.aead, encrypted)
	if ok {
		t.Fatal("expected decrypt to fail after tamper")
	}
}

func TestStripeEncryptNilCipher(t *testing.T) {
	pkt := make([]byte, stripeHdrLen+4)
	encodeStripeHdr(pkt, &stripeHdr{Magic: stripeMagic, Version: stripeVersion, Type: stripeDATA})
	result := stripeEncrypt(nil, pkt)
	if len(result) != len(pkt) {
		t.Fatal("nil cipher should return packet unchanged")
	}
}

func TestStripeEncryptSequenceUniqueness(t *testing.T) {
	var key [32]byte
	key[0] = 0xFF
	sc, _ := newStripeCipher(key)

	pkt := make([]byte, stripeHdrLen+4)
	encodeStripeHdr(pkt, &stripeHdr{Magic: stripeMagic, Version: stripeVersion, Type: stripeDATA})

	enc1 := stripeEncrypt(sc, pkt)
	enc2 := stripeEncrypt(sc, pkt)

	// Same plaintext, different nonces → different ciphertexts
	if string(enc1) == string(enc2) {
		t.Fatal("sequential encryptions should produce different ciphertexts")
	}
}
