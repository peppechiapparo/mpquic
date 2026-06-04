package crypto_test

import (
	"errors"
	"mpquic/internal/mpquic/crypto"
	"testing"
)

func TestNewKeyExchangeProvider_Performance(t *testing.T) {
	p, err := crypto.NewKeyExchangeProvider(crypto.ProfilePerformance)
	if err != nil {
		t.Fatalf("NewKeyExchangeProvider(ProfilePerformance): %v", err)
	}
	if _, ok := p.(*crypto.ClassicalKEXProvider); !ok {
		t.Fatalf("expected ClassicalKEXProvider, got %T", p)
	}
	if p.Name() != "X25519-HKDF-SHA256" {
		t.Fatalf("name=%q, want %q", p.Name(), "X25519-HKDF-SHA256")
	}
}

func TestNewKeyExchangeProvider_HybridSecurity(t *testing.T) {
	p, err := crypto.NewKeyExchangeProvider(crypto.ProfileHybridSecurity)
	if err != nil {
		t.Fatalf("NewKeyExchangeProvider(ProfileHybridSecurity): %v", err)
	}
	if _, ok := p.(*crypto.HybridKEXProvider); !ok {
		t.Fatalf("expected HybridKEXProvider, got %T", p)
	}
	if p.Name() != "X25519+ML-KEM-768-HKDF-SHA256" {
		t.Fatalf("name=%q, want %q", p.Name(), "X25519+ML-KEM-768-HKDF-SHA256")
	}
}

func TestNewKeyExchangeProvider_CustomProvider(t *testing.T) {
	p, err := crypto.NewKeyExchangeProvider(crypto.ProfileCustomProvider)
	if err == nil {
		t.Fatalf("expected error for ProfileCustomProvider, got nil")
	}
	if !errors.Is(err, crypto.ErrInvalidProfile) {
		t.Fatalf("expected ErrInvalidProfile, got %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil provider, got %v", p)
	}
}

func TestNewKeyExchangeProvider_UnknownProfile(t *testing.T) {
	p, err := crypto.NewKeyExchangeProvider(crypto.CryptoProfile("unknown"))
	if err == nil {
		t.Fatalf("expected error for unknown profile, got nil")
	}
	if !errors.Is(err, crypto.ErrInvalidProfile) {
		t.Fatalf("expected ErrInvalidProfile, got %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil provider, got %v", p)
	}
}
