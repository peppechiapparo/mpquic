package crypto

import (
	"errors"
	"testing"
)

func TestNewKeyExchangeProvider_Performance(t *testing.T) {
	p, err := NewKeyExchangeProvider(ProfilePerformance)
	if err != nil {
		t.Fatalf("NewKeyExchangeProvider(ProfilePerformance): %v", err)
	}
	if _, ok := p.(*ClassicalKEXProvider); !ok {
		t.Fatalf("expected ClassicalKEXProvider, got %T", p)
	}
	if p.Name() != classicalKEXName {
		t.Fatalf("name=%q, want %q", p.Name(), classicalKEXName)
	}
}

func TestNewKeyExchangeProvider_HybridSecurity(t *testing.T) {
	p, err := NewKeyExchangeProvider(ProfileHybridSecurity)
	if err != nil {
		t.Fatalf("NewKeyExchangeProvider(ProfileHybridSecurity): %v", err)
	}
	if _, ok := p.(*HybridKEXProvider); !ok {
		t.Fatalf("expected HybridKEXProvider, got %T", p)
	}
	if p.Name() != hybridKEXName {
		t.Fatalf("name=%q, want %q", p.Name(), hybridKEXName)
	}
}

func TestNewKeyExchangeProvider_CustomProvider(t *testing.T) {
	p, err := NewKeyExchangeProvider(ProfileCustomProvider)
	if err == nil {
		t.Fatalf("expected error for ProfileCustomProvider, got nil")
	}
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil provider, got %v", p)
	}
}

func TestNewKeyExchangeProvider_UnknownProfile(t *testing.T) {
	p, err := NewKeyExchangeProvider(CryptoProfile("unknown"))
	if err == nil {
		t.Fatalf("expected error for unknown profile, got nil")
	}
	if !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("expected ErrInvalidProfile, got %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil provider, got %v", p)
	}
}
