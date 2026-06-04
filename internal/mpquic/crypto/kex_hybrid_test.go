package crypto_test

import (
	"bytes"
	"errors"
	"mpquic/internal/mpquic/crypto"
	"testing"
)

func TestHybridKEX_GenerateKeyPair(t *testing.T) {
	p := crypto.NewHybridKEXProvider()
	pub, priv, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if len(pub) != 1216 {
		t.Fatalf("pub size=%d, want %d", len(pub), 1216)
	}
	if len(priv) != 96 {
		t.Fatalf("priv size=%d, want %d", len(priv), 96)
	}
}

func TestHybridKEX_ClientEncapsulate(t *testing.T) {
	p := crypto.NewHybridKEXProvider()
	servPub, _, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(server): %v", err)
	}

	clientLocalPriv, clientPeerKeyShare, err := p.ClientEncapsulate(servPub)
	if err != nil {
		t.Fatalf("ClientEncapsulate: %v", err)
	}
	if len(clientLocalPriv) != 64 {
		t.Fatalf("clientLocalPriv size=%d, want %d", len(clientLocalPriv), 64)
	}
	if len(clientPeerKeyShare) != 1120 {
		t.Fatalf("clientPeerKeyShare size=%d, want %d", len(clientPeerKeyShare), 1120)
	}
}

func TestHybridKEX_CrossDerivation(t *testing.T) {
	provider := crypto.NewHybridKEXProvider()
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
	clientKeys, err := provider.DeriveSessionKeys(quicSecret, clientLocalPriv, servPub[:32], sessionID)
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
	p := crypto.NewHybridKEXProvider()
	servPub, servPriv, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(server): %v", err)
	}
	clientLocalPriv, clientPeerKeyShare, err := p.ClientEncapsulate(servPub)
	if err != nil {
		t.Fatalf("ClientEncapsulate: %v", err)
	}
	_, err = p.DeriveSessionKeys(nil, servPriv, clientPeerKeyShare, nil)
	if !errors.Is(err, crypto.ErrEmptySessionID) {
		t.Fatalf("expected ErrEmptySessionID, got %v", err)
	}
	_, err = p.DeriveSessionKeys(nil, clientLocalPriv, servPub[:32], nil)
	if !errors.Is(err, crypto.ErrEmptySessionID) {
		t.Fatalf("expected ErrEmptySessionID, got %v", err)
	}
}

func TestHybridKEX_InvalidLocalPrivKeySize(t *testing.T) {
	p := crypto.NewHybridKEXProvider()
	_, err := p.DeriveSessionKeys(nil, make([]byte, 63), make([]byte, 32), []byte("sess"))
	if !errors.Is(err, crypto.ErrInvalidKeySize) {
		t.Fatalf("expected ErrInvalidKeySize, got %v", err)
	}
}

func TestHybridKEX_ClientEncapsulate_InvalidKeySize(t *testing.T) {
	p := crypto.NewHybridKEXProvider()
	_, _, err := p.ClientEncapsulate(make([]byte, 1215))
	if !errors.Is(err, crypto.ErrInvalidKeySize) {
		t.Fatalf("expected ErrInvalidKeySize, got %v", err)
	}
}
