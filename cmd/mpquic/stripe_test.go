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
