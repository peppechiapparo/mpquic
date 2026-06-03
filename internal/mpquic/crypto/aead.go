package crypto

import "crypto/cipher"

// AEADProvider astrae un cifrante AEAD (Authenticated Encryption with Associated Data).
// Implementazioni devono essere thread-safe e produrre zero allocazioni nel hot path
// per le operazioni Seal/Open.
type AEADProvider interface {
	Name() string
	NewAEAD(key []byte) (cipher.AEAD, error)
	KeySize() int   // byte (es. 32 per AES-256)
	NonceSize() int // byte (es. 12 per GCM)
}
