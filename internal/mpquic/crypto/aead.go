package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
)

// AEADProvider astrae un cifrante AEAD (Authenticated Encryption with Associated Data).
// Implementazioni devono essere thread-safe e produrre zero allocazioni nel hot path
// per le operazioni Seal/Open.
type AEADProvider interface {
	Name() string
	NewAEAD(key []byte) (cipher.AEAD, error)
	KeySize() int   // byte (es. 32 per AES-256)
	NonceSize() int // byte (es. 12 per GCM)
}

// AESGCMProvider implementa AEADProvider usando AES-256-GCM dalla stdlib Go.
// Thread-safe: NewAEAD crea istanze indipendenti.
// Allocazione-libera nel hot path: la AEAD restituita da NewAEAD usa AES-NI quando disponibile.
type AESGCMProvider struct{}

func NewAESGCMProvider() *AESGCMProvider { return &AESGCMProvider{} }

// Name restituisce il nome dell'algoritmo.
func (AESGCMProvider) Name() string { return "AES-256-GCM" }

// KeySize restituisce 32 (AES-256).
func (AESGCMProvider) KeySize() int { return 32 }

// NonceSize restituisce 12 (GCM standard).
func (AESGCMProvider) NonceSize() int { return 12 }

// NewAEAD crea un'istanza AES-256-GCM per la chiave fornita.
// key deve essere esattamente 32 byte.
func (AESGCMProvider) NewAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("aesgcm: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: %w", err)
	}
	return aead, nil
}
