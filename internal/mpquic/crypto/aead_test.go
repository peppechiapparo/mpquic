package crypto_test

import (
	"bytes"
	"testing"

	crypto "mpquic/internal/mpquic/crypto"
)

// TestAESGCMProvider_Name verifica il nome del provider.
func TestAESGCMProvider_Name(t *testing.T) {
	var p crypto.AESGCMProvider
	if got := p.Name(); got != "AES-256-GCM" {
		t.Errorf("Name() = %q, want %q", got, "AES-256-GCM")
	}
}

// TestAESGCMProvider_KeySize verifica che il provider riporti 32 byte.
func TestAESGCMProvider_KeySize(t *testing.T) {
	var p crypto.AESGCMProvider
	if got := p.KeySize(); got != 32 {
		t.Errorf("KeySize() = %d, want 32", got)
	}
}

// TestAESGCMProvider_NonceSize verifica che il provider riporti 12 byte.
func TestAESGCMProvider_NonceSize(t *testing.T) {
	var p crypto.AESGCMProvider
	if got := p.NonceSize(); got != 12 {
		t.Errorf("NonceSize() = %d, want 12", got)
	}
}

// TestAESGCMProvider_NewAEAD_InvalidKeyLengths verifica che chiavi non da 32 byte vengano rifiutate.
func TestAESGCMProvider_NewAEAD_InvalidKeyLengths(t *testing.T) {
	var p crypto.AESGCMProvider
	for _, size := range []int{0, 1, 16, 24, 31, 33, 64} {
		key := make([]byte, size)
		_, err := p.NewAEAD(key)
		if err == nil {
			t.Errorf("NewAEAD(%d-byte key): expected error, got nil", size)
		}
	}
}

// TestAESGCMProvider_NewAEAD_ValidKey verifica che una chiave da 32 byte sia accettata.
func TestAESGCMProvider_NewAEAD_ValidKey(t *testing.T) {
	var p crypto.AESGCMProvider
	key := make([]byte, 32)
	aead, err := p.NewAEAD(key)
	if err != nil {
		t.Fatalf("NewAEAD(32-byte key): unexpected error: %v", err)
	}
	if aead == nil {
		t.Fatal("NewAEAD returned nil AEAD")
	}
	if aead.NonceSize() != 12 {
		t.Errorf("AEAD.NonceSize() = %d, want %d", aead.NonceSize(), 12)
	}
	if aead.Overhead() != 16 {
		t.Errorf("AEAD.Overhead() = %d, want %d", aead.Overhead(), 16)
	}
}

// TestAESGCMProvider_EncryptDecrypt verifica encrypt→decrypt round-trip.
// Usa un test vector semplice (chiave e plaintext noti).
func TestAESGCMProvider_EncryptDecrypt(t *testing.T) {
	var p crypto.AESGCMProvider

	// 32-byte key: tutti zeri (test vector banale ma determinista)
	key := make([]byte, 32)
	aead, err := p.NewAEAD(key)
	if err != nil {
		t.Fatalf("NewAEAD: %v", err)
	}

	nonce := make([]byte, 12) // tutti zeri
	aad := []byte("stripes-aad-test")
	plaintext := []byte("hello stripes crypto layer")

	// Encrypt
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	if len(ciphertext) != len(plaintext)+16 {
		t.Errorf("ciphertext len = %d, want %d", len(ciphertext), len(plaintext)+16)
	}

	// Decrypt
	recovered, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(recovered, plaintext) {
		t.Errorf("recovered %q, want %q", recovered, plaintext)
	}
}

// TestAESGCMProvider_AuthFailure verifica che un ciphertext tamperato fallisca.
func TestAESGCMProvider_AuthFailure(t *testing.T) {
	var p crypto.AESGCMProvider
	key := make([]byte, 32)
	aead, err := p.NewAEAD(key)
	if err != nil {
		t.Fatalf("NewAEAD: %v", err)
	}

	nonce := make([]byte, 12)
	aad := []byte("aad")
	plaintext := []byte("data")

	ciphertext := aead.Seal(nil, nonce, plaintext, aad)

	// Tamper: flip un bit nel ciphertext
	ciphertext[0] ^= 0xFF

	_, err = aead.Open(nil, nonce, ciphertext, aad)
	if err == nil {
		t.Error("Open should have failed on tampered ciphertext, but returned nil error")
	}
}

// TestAESGCMProvider_AADMismatch verifica che un AAD diverso causi fallimento autenticazione.
func TestAESGCMProvider_AADMismatch(t *testing.T) {
	var p crypto.AESGCMProvider
	key := make([]byte, 32)
	aead, err := p.NewAEAD(key)
	if err != nil {
		t.Fatalf("NewAEAD: %v", err)
	}

	nonce := make([]byte, 12)
	plaintext := []byte("data")

	ciphertext := aead.Seal(nil, nonce, plaintext, []byte("correct-aad"))

	_, err = aead.Open(nil, nonce, ciphertext, []byte("wrong-aad"))
	if err == nil {
		t.Error("Open should have failed on AAD mismatch, but returned nil error")
	}
}
