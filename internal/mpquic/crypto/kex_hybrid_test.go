package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func TestHybridKEX_GenerateKeyPair(t *testing.T) {
	p := NewHybridKEXProvider()
	pub, priv, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if len(pub) != hybridPubKeySize {
		t.Fatalf("pub size=%d, want %d", len(pub), hybridPubKeySize)
	}
	if len(priv) != hybridPrivKeySize {
		t.Fatalf("priv size=%d, want %d", len(priv), hybridPrivKeySize)
	}
}

func TestHybridKEX_ClientEncapsulate(t *testing.T) {
	p := NewHybridKEXProvider()
	servPub, _, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(server): %v", err)
	}

	clientLocalPriv, clientPeerKeyShare, err := p.ClientEncapsulate(servPub)
	if err != nil {
		t.Fatalf("ClientEncapsulate: %v", err)
	}
	if len(clientLocalPriv) != hybridClientPrivSize {
		t.Fatalf("clientLocalPriv size=%d, want %d", len(clientLocalPriv), hybridClientPrivSize)
	}
	if len(clientPeerKeyShare) != hybridPeerShareSize {
		t.Fatalf("clientPeerKeyShare size=%d, want %d", len(clientPeerKeyShare), hybridPeerShareSize)
	}
}

func TestHybridKEX_CrossDerivation(t *testing.T) {
	provider := NewHybridKEXProvider()
	servPub, servPriv, err := provider.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(server): %v", err)
	}

	clientLocalPriv, clientPeerKeyShare, err := provider.ClientEncapsulate(servPub)
	if err != nil {
		t.Fatalf("ClientEncapsulate: %v", err)
	}

	quicSecret := make([]byte, 32)
	sessionID := []byte("test-session-123")

	servKeys, err := provider.DeriveSessionKeys(quicSecret, servPriv, clientPeerKeyShare, sessionID)
	if err != nil {
		t.Fatalf("server DeriveSessionKeys: %v", err)
	}
	clientKeys, err := provider.DeriveSessionKeys(quicSecret, clientLocalPriv, servPub[:x25519KeySize], sessionID)
	if err != nil {
		t.Fatalf("client DeriveSessionKeys: %v", err)
	}

	if !bytes.Equal(servKeys.ClientKey, clientKeys.ClientKey) ||
		!bytes.Equal(servKeys.ServerKey, clientKeys.ServerKey) ||
		!bytes.Equal(servKeys.ClientIV, clientKeys.ClientIV) ||
		!bytes.Equal(servKeys.ServerIV, clientKeys.ServerIV) {
		t.Fatalf("hybrid cross derivation mismatch")
	}
}

func TestHybridKEX_EmptySessionID(t *testing.T) {
	p := NewHybridKEXProvider()
	servPub, servPriv, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(server): %v", err)
	}
	clientLocalPriv, clientPeerKeyShare, err := p.ClientEncapsulate(servPub)
	if err != nil {
		t.Fatalf("ClientEncapsulate: %v", err)
	}
	_, err = p.DeriveSessionKeys(nil, servPriv, clientPeerKeyShare, nil)
	if !errors.Is(err, ErrEmptySessionID) {
		t.Fatalf("expected ErrEmptySessionID, got %v", err)
	}
	_, err = p.DeriveSessionKeys(nil, clientLocalPriv, servPub[:32], nil)
	if !errors.Is(err, ErrEmptySessionID) {
		t.Fatalf("expected ErrEmptySessionID, got %v", err)
	}
}

func TestHybridKEX_InvalidLocalPrivKeySize(t *testing.T) {
	p := NewHybridKEXProvider()
	_, err := p.DeriveSessionKeys(nil, make([]byte, 63), make([]byte, 32), []byte("sess"))
	if !errors.Is(err, ErrInvalidKeySize) {
		t.Fatalf("expected ErrInvalidKeySize, got %v", err)
	}
}

func TestHybridKEX_ClientEncapsulate_InvalidKeySize(t *testing.T) {
	p := NewHybridKEXProvider()
	_, _, err := p.ClientEncapsulate(make([]byte, hybridPubKeySize-1))
	if !errors.Is(err, ErrInvalidKeySize) {
		t.Fatalf("expected ErrInvalidKeySize, got %v", err)
	}
}
