package crypto_test

import (
	"bytes"
	"errors"
	"mpquic/internal/mpquic/crypto"
	"testing"
)

func TestClassicalKEX_GenerateKeyPair(t *testing.T) {
	p := crypto.NewClassicalKEXProvider()
	pub1, priv1, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if len(pub1) != 32 {
		t.Fatalf("pub size=%d, want 32", len(pub1))
	}
	if len(priv1) != 32 {
		t.Fatalf("priv size=%d, want 32", len(priv1))
	}
	if bytes.Equal(pub1, priv1) {
		t.Fatalf("pub must differ from priv")
	}

	pub2, priv2, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(2): %v", err)
	}
	if bytes.Equal(pub1, pub2) && bytes.Equal(priv1, priv2) {
		t.Fatalf("two GenerateKeyPair calls should not return identical pair")
	}
}

func TestClassicalKEX_DeriveSessionKeys(t *testing.T) {
	p := crypto.NewClassicalKEXProvider()
	remotePub, _, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(remote): %v", err)
	}
	localPub, localPriv, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(local): %v", err)
	}
	_ = localPub

	quicSecret := make([]byte, 32)
	sessionID := []byte("sess-1")

	keys1, err := p.DeriveSessionKeys(quicSecret, localPriv, remotePub, sessionID)
	if err != nil {
		t.Fatalf("DeriveSessionKeys(1): %v", err)
	}
	keys2, err := p.DeriveSessionKeys(quicSecret, localPriv, remotePub, sessionID)
	if err != nil {
		t.Fatalf("DeriveSessionKeys(2): %v", err)
	}

	if !bytes.Equal(keys1.ClientKey, keys2.ClientKey) || !bytes.Equal(keys1.ServerKey, keys2.ServerKey) ||
		!bytes.Equal(keys1.ClientIV, keys2.ClientIV) || !bytes.Equal(keys1.ServerIV, keys2.ServerIV) {
		t.Fatalf("DeriveSessionKeys must be deterministic for same inputs")
	}

	if len(keys1.ClientKey) != 32 || len(keys1.ServerKey) != 32 || len(keys1.ClientIV) != 12 || len(keys1.ServerIV) != 12 {
		t.Fatalf("unexpected sizes: CK=%d SK=%d CIV=%d SIV=%d",
			len(keys1.ClientKey), len(keys1.ServerKey), len(keys1.ClientIV), len(keys1.ServerIV))
	}
}

func TestClassicalKEX_CrossDerivation(t *testing.T) {
	p := crypto.NewClassicalKEXProvider()
	clientPub, clientPriv, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(client): %v", err)
	}
	servPub, servPriv, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(server): %v", err)
	}

	quicSecret := make([]byte, 32)
	sessionID := []byte("test-session-123")

	clientKeys, err := p.DeriveSessionKeys(quicSecret, clientPriv, servPub, sessionID)
	if err != nil {
		t.Fatalf("client DeriveSessionKeys: %v", err)
	}
	servKeys, err := p.DeriveSessionKeys(quicSecret, servPriv, clientPub, sessionID)
	if err != nil {
		t.Fatalf("server DeriveSessionKeys: %v", err)
	}

	if !bytes.Equal(clientKeys.ClientKey, servKeys.ClientKey) ||
		!bytes.Equal(clientKeys.ServerKey, servKeys.ServerKey) ||
		!bytes.Equal(clientKeys.ClientIV, servKeys.ClientIV) ||
		!bytes.Equal(clientKeys.ServerIV, servKeys.ServerIV) {
		t.Fatalf("cross derivation mismatch")
	}
}

func TestClassicalKEX_EmptySessionID(t *testing.T) {
	p := crypto.NewClassicalKEXProvider()
	remotePub, _, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(remote): %v", err)
	}
	_, localPriv, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(local): %v", err)
	}
	_, err = p.DeriveSessionKeys(nil, localPriv, remotePub, nil)
	if !errors.Is(err, crypto.ErrEmptySessionID) {
		t.Fatalf("expected ErrEmptySessionID, got %v", err)
	}
}

func TestClassicalKEX_InvalidKeySize(t *testing.T) {
	p := crypto.NewClassicalKEXProvider()
	remotePub, _, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair(remote): %v", err)
	}
	badPriv := make([]byte, 31)
	_, err = p.DeriveSessionKeys(nil, badPriv, remotePub, []byte("sess"))
	if !errors.Is(err, crypto.ErrInvalidKeySize) {
		t.Fatalf("expected ErrInvalidKeySize, got %v", err)
	}
}
