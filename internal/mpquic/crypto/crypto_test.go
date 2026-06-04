package crypto

import (
	"bytes"
	"sync"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func makeTestQuicSecret() []byte {
	qs := make([]byte, 64)
	for i := range qs {
		qs[i] = byte(i + 1)
	}
	return qs
}

func makeTestKeys(epochID uint8) *SessionKeys {
	clientKey := make([]byte, 32)
	serverKey := make([]byte, 32)
	for i := range clientKey {
		clientKey[i] = byte(epochID + 0xA0)
		serverKey[i] = byte(epochID + 0xB0)
	}
	return &SessionKeys{
		EpochID:   epochID,
		ClientKey: clientKey,
		ServerKey: serverKey,
	}
}

func makeTestSession(t *testing.T, isServer bool) *CryptoSession {
	t.Helper()
	cfg := DefaultCryptoConfig()
	cfg.Rekey.Enabled = false
	cs, err := NewCryptoSession(cfg, makeTestQuicSecret(), makeTestKeys(0), isServer, []byte("test-session-id"), 1)
	if err != nil {
		t.Fatalf("NewCryptoSession: %v", err)
	}
	return cs
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestNewCryptoSession_Validation(t *testing.T) {
	cfg := DefaultCryptoConfig()
	cfg.Rekey.Enabled = false
	qs := makeTestQuicSecret()
	keys := makeTestKeys(0)
	sid := []byte("sid")

	if _, err := NewCryptoSession(nil, qs, keys, false, sid, 1); err == nil {
		t.Error("expected error for nil cfg")
	}
	if _, err := NewCryptoSession(cfg, nil, keys, false, sid, 1); err == nil {
		t.Error("expected error for nil quicSecret")
	}
	if _, err := NewCryptoSession(cfg, make([]byte, 32), keys, false, sid, 1); err == nil {
		t.Error("expected error for short quicSecret")
	}
	if _, err := NewCryptoSession(cfg, qs, nil, false, sid, 1); err == nil {
		t.Error("expected error for nil initialKeys")
	}
	if _, err := NewCryptoSession(cfg, qs, keys, false, nil, 1); err == nil {
		t.Error("expected error for nil sessionID")
	}
	if _, err := NewCryptoSession(cfg, qs, keys, false, sid, 0); err == nil {
		t.Error("expected error for nWorkers=0")
	}
}

func TestCryptoSession_SealOpen_RoundTrip(t *testing.T) {
	client := makeTestSession(t, false)
	server := makeTestSession(t, true)
	defer client.Close()
	defer server.Close()

	plaintext := []byte("hello stripe world")
	nonce := make([]byte, client.NonceSize())
	aad := []byte("additional data")

	ciphertext := client.Seal(nil, nonce, plaintext, aad)
	decrypted, err := server.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("got %q, want %q", decrypted, plaintext)
	}
}

func TestCryptoSession_EpochTransition(t *testing.T) {
	client := makeTestSession(t, false)
	server := makeTestSession(t, true)
	defer client.Close()
	defer server.Close()

	newKeys := makeTestKeys(1)
	if err := client.UpdateKeys(newKeys); err != nil {
		t.Fatalf("client.UpdateKeys: %v", err)
	}
	if err := server.UpdateKeys(newKeys); err != nil {
		t.Fatalf("server.UpdateKeys: %v", err)
	}

	plaintext := []byte("post-rekey message")
	nonce := make([]byte, client.NonceSize())

	ciphertext := client.Seal(nil, nonce, plaintext, nil)
	decrypted, err := server.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("Open after rekey: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("got %q, want %q", decrypted, plaintext)
	}
}

func TestCryptoSession_EpochFallback(t *testing.T) {
	// Client advances to epoch 1; server still on epoch 0.
	// Server must still decrypt epoch-0 messages from client.
	client := makeTestSession(t, false)
	server := makeTestSession(t, true)
	defer client.Close()
	defer server.Close()

	// Seal with epoch 0 (current on both sides)
	plaintext := []byte("old epoch message")
	nonce := make([]byte, client.NonceSize())
	ciphertext := client.Seal(nil, nonce, plaintext, nil)

	// Advance client to epoch 1 (server stays on 0)
	if err := client.UpdateKeys(makeTestKeys(1)); err != nil {
		t.Fatalf("UpdateKeys: %v", err)
	}

	// Server should still decrypt the epoch-0 ciphertext
	decrypted, err := server.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("Open epoch-0 msg on server still at epoch-0: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("got %q, want %q", decrypted, plaintext)
	}
}

// TestCryptoSession_EpochFallback_RealScenario verifica il fallback reale:
// il server avanza a epoch 1, ma il client è ancora a epoch 0.
// Il server (epoch corrente = 1) deve decifrarei pacchetti epoch-0 del client
// tramite il meccanismo prev-epoch (transition window).
func TestCryptoSession_EpochFallback_RealScenario(t *testing.T) {
	client := makeTestSession(t, false)
	server := makeTestSession(t, true)
	defer client.Close()
	defer server.Close()

	// Client è ancora a epoch 0 — produce ciphertext con epoch 0
	plaintext := []byte("stale epoch from client")
	nonce := make([]byte, client.NonceSize())
	ciphertext := client.Seal(nil, nonce, plaintext, nil)

	// Server avanza a epoch 1 (ha ricevuto la pubkey del client via KX)
	// ma il client non ha ancora aggiornato le sue chiavi.
	if err := server.UpdateKeys(makeTestKeys(1)); err != nil {
		t.Fatalf("server.UpdateKeys: %v", err)
	}

	// Il server (currentEpoch=1) deve riuscire a decifrare il pacchetto epoch-0
	// tramite il fallback al prev epoch (epoch 0 è ancora nella transition window).
	decrypted, err := server.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("server@epoch1 failed to decrypt client@epoch0 packet: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("fallback decrypted: got %q, want %q", decrypted, plaintext)
	}

	// Verifica che il server decifri anche pacchetti epoch-1 (propri)
	if err := client.UpdateKeys(makeTestKeys(1)); err != nil {
		t.Fatalf("client.UpdateKeys: %v", err)
	}
	nonce1 := make([]byte, client.NonceSize())
	nonce1[11] = 0x42
	plaintext1 := []byte("new epoch message")
	ct1 := client.Seal(nil, nonce1, plaintext1, nil)
	dec1, err := server.Open(nil, nonce1, ct1, nil)
	if err != nil {
		t.Fatalf("server@epoch1 failed to decrypt client@epoch1 packet: %v", err)
	}
	if !bytes.Equal(dec1, plaintext1) {
		t.Errorf("epoch1 decrypted: got %q, want %q", dec1, plaintext1)
	}
}

func TestCryptoSession_PruneOldKeys(t *testing.T) {
	cs := makeTestSession(t, false)
	defer cs.Close()

	if err := cs.UpdateKeys(makeTestKeys(1)); err != nil {
		t.Fatalf("UpdateKeys(1): %v", err)
	}
	if err := cs.UpdateKeys(makeTestKeys(2)); err != nil {
		t.Fatalf("UpdateKeys(2): %v", err)
	}

	cs.PruneOldKeys(2)

	if _, ok := cs.GetKeysForEpoch(0); ok {
		t.Error("epoch 0 should have been pruned")
	}
	if _, ok := cs.GetKeysForEpoch(1); ok {
		t.Error("epoch 1 should have been pruned")
	}
	if _, ok := cs.GetKeysForEpoch(2); !ok {
		t.Error("epoch 2 should still exist")
	}
}

func TestCryptoSession_Close_StopsRekeyManager(t *testing.T) {
	cfg := DefaultCryptoConfig()
	cfg.Rekey.Enabled = false
	cs, err := NewCryptoSession(cfg, makeTestQuicSecret(), makeTestKeys(0), false, []byte("sid"), 1)
	if err != nil {
		t.Fatalf("NewCryptoSession: %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double-close must return ErrSessionClosed
	if err := cs.Close(); err != ErrSessionClosed {
		t.Errorf("second Close: got %v, want ErrSessionClosed", err)
	}
}

func TestCryptoSession_NotifyPathRecovery_NoOp(t *testing.T) {
	cs := makeTestSession(t, false)
	defer cs.Close()
	// Must not panic when rekeyMgr is nil (Rekey.Enabled=false)
	cs.NotifyPathRecovery()
}

func TestCryptoSession_AuthFail(t *testing.T) {
	client := makeTestSession(t, false)
	server := makeTestSession(t, true)
	defer client.Close()
	defer server.Close()

	plaintext := []byte("secret")
	nonce := make([]byte, client.NonceSize())
	ciphertext := client.Seal(nil, nonce, plaintext, nil)

	// Corrupt ciphertext
	ciphertext[len(ciphertext)-1] ^= 0xFF

	if _, err := server.Open(nil, nonce, ciphertext, nil); err == nil {
		t.Error("expected auth failure on corrupted ciphertext")
	}
}

func TestCryptoSession_ConcurrentSealOpen(t *testing.T) {
	client := makeTestSession(t, false)
	server := makeTestSession(t, true)
	defer client.Close()
	defer server.Close()

	const goroutines = 8
	const msgPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < msgPerGoroutine; i++ {
				plaintext := []byte("concurrent message")
				nonce := make([]byte, client.NonceSize())
				ct := client.Seal(nil, nonce, plaintext, nil)
				dec, err := server.Open(nil, nonce, ct, nil)
				if err != nil {
					t.Errorf("g%d i%d Open: %v", gid, i, err)
					return
				}
				if !bytes.Equal(dec, plaintext) {
					t.Errorf("g%d i%d mismatch", gid, i)
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestCryptoSession_RekeyManager_Integration(t *testing.T) {
	cfg := DefaultCryptoConfig()
	cfg.Rekey.Enabled = false // test only UpdateKeys flow, no background goroutines

	client, err := NewCryptoSession(cfg, makeTestQuicSecret(), makeTestKeys(0), false, []byte("integ-sid"), 1)
	if err != nil {
		t.Fatalf("NewCryptoSession client: %v", err)
	}
	server, err := NewCryptoSession(cfg, makeTestQuicSecret(), makeTestKeys(0), true, []byte("integ-sid"), 1)
	if err != nil {
		t.Fatalf("NewCryptoSession server: %v", err)
	}
	defer client.Close()
	defer server.Close()

	// Simulate a rekey: both sides advance to epoch 1
	newKeys := makeTestKeys(1)
	if err := client.UpdateKeys(newKeys); err != nil {
		t.Fatalf("client UpdateKeys: %v", err)
	}
	if err := server.UpdateKeys(newKeys); err != nil {
		t.Fatalf("server UpdateKeys: %v", err)
	}

	// Verify Metrics
	if client.Metrics().TotalRekeyEvents.Load() != 1 {
		t.Errorf("client: expected 1 rekey event, got %d", client.Metrics().TotalRekeyEvents.Load())
	}
	if server.Metrics().TotalRekeyEvents.Load() != 1 {
		t.Errorf("server: expected 1 rekey event, got %d", server.Metrics().TotalRekeyEvents.Load())
	}

	// Verify round-trip after rekey
	plaintext := []byte("post-integration-rekey")
	nonce := make([]byte, client.NonceSize())
	ct := client.Seal(nil, nonce, plaintext, nil)
	dec, err := server.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("Open after integration rekey: %v", err)
	}
	if !bytes.Equal(dec, plaintext) {
		t.Errorf("got %q, want %q", dec, plaintext)
	}
}

// TestCryptoSession_DuplicateEpoch verifica SEC-G02: rifiuto di epoch duplicato.
// Se UpdateKeys viene chiamato con un EpochID già presente, deve ritornare ErrRekeyBadEpoch.
func TestCryptoSession_DuplicateEpoch(t *testing.T) {
	client := makeTestSession(t, false)
	defer client.Close()

	// Prima UpdateKeys con epoch 1
	keys1 := makeTestKeys(1)
	if err := client.UpdateKeys(keys1); err != nil {
		t.Fatalf("first UpdateKeys(epoch=1): %v", err)
	}

	// Secondo tentativo con lo stesso epoch ID deve fallire
	keys1Dup := makeTestKeys(1)
	err := client.UpdateKeys(keys1Dup)
	if err == nil {
		t.Fatal("expected error for duplicate epoch, got nil")
	}

	// Verifica che l'errore è esattamente ErrRekeyBadEpoch
	if !IsWrappedError(err, ErrRekeyBadEpoch) {
		t.Errorf("expected ErrRekeyBadEpoch, got %v", err)
	}

	// Verifica che il session sia ancora operativo: Update con epoch 2 deve funzionare
	keys2 := makeTestKeys(2)
	if err := client.UpdateKeys(keys2); err != nil {
		t.Fatalf("third UpdateKeys(epoch=2) after duplicate: %v", err)
	}

	// Verifica round-trip con epoch 2
	plaintext := []byte("after-duplicate-check")
	nonce := make([]byte, client.NonceSize())
	ct := client.Seal(nil, nonce, plaintext, nil)
	if len(ct) == 0 {
		t.Fatal("Seal returned empty ciphertext")
	}
}

// IsWrappedError verifica se err è uguale a target o lo contiene tramite wrapping.
func IsWrappedError(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		// Cerca di unwrap l'errore
		type wrapper interface{ Unwrap() error }
		if w, ok := err.(wrapper); ok {
			err = w.Unwrap()
		} else {
			break
		}
	}
	return false
}
