package crypto

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type mockSession struct {
	mu          sync.Mutex
	keys        map[uint8]*SessionKeys
	updateCount int
	pruneCount  int
	updateErr   error
}

func newMockSession() *mockSession {
	return &mockSession{keys: make(map[uint8]*SessionKeys)}
}

func (m *mockSession) UpdateKeys(k *SessionKeys) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return m.updateErr
	}
	m.keys[k.EpochID] = k
	m.updateCount++
	return nil
}

func (m *mockSession) GetKeysForEpoch(epochID uint8) (*SessionKeys, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[epochID]
	return k, ok
}

func (m *mockSession) PruneOldKeys(oldest uint8) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.keys {
		if id < oldest {
			delete(m.keys, id)
		}
	}
	m.pruneCount++
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestNewRekeyManager_Validation(t *testing.T) {
	cfg := RekeyConfig{Enabled: true}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}
	sessionID := []byte{1, 2, 3}

	if _, err := NewRekeyManager(context.Background(), cfg, nil, sess, metrics, sessionID, 1); err == nil {
		t.Fatal("expected error for nil kex")
	}
	if _, err := NewRekeyManager(context.Background(), cfg, kex, nil, metrics, sessionID, 1); err == nil {
		t.Fatal("expected error for nil session")
	}
	if _, err := NewRekeyManager(context.Background(), cfg, kex, sess, nil, sessionID, 1); err == nil {
		t.Fatal("expected error for nil metrics")
	}
	if _, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, nil, 1); err == nil {
		t.Fatal("expected error for empty sessionID")
	}
}

func TestRekeyManager_StartStop(t *testing.T) {
	cfg := RekeyConfig{Enabled: true, IntervalSeconds: 0}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}

	rm, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, []byte{9, 9, 9}, 1)
	if err != nil {
		t.Fatalf("NewRekeyManager: %v", err)
	}
	rm.Start()
	time.Sleep(50 * time.Millisecond)

	rm.cancel()
	done := make(chan struct{})
	go func() {
		rm.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for RekeyManager goroutines")
	}

	rm.Stop()
}

func TestRekeyManager_NotifyPacketSent_Threshold(t *testing.T) {
	cfg := RekeyConfig{Enabled: true, MaxPackets: 10}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}

	rm, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, []byte{1}, 1)
	if err != nil {
		t.Fatalf("NewRekeyManager: %v", err)
	}
	rm.Start()
	defer rm.Stop()

	initialPub := rm.PublicKey()

	for i := 0; i < 10; i++ {
		rm.NotifyPacketSent(100)
	}

	// executeRekey rinnova la keypair locale (nessuna DeriveSessionKeys senza peer pub key).
	// Verifica che PublicKey() sia cambiata (keypair rinnovata) e lo stato sia tornato Idle.
	waitUntil(t, 500*time.Millisecond, func() bool {
		newPub := rm.PublicKey()
		return string(newPub) != string(initialPub)
	})
}

func TestRekeyManager_NotifyPathRecovery(t *testing.T) {
	cfg := RekeyConfig{Enabled: true, OnPathRecovery: true}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}

	rm, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, []byte{2}, 1)
	if err != nil {
		t.Fatalf("NewRekeyManager: %v", err)
	}
	rm.Start()
	defer rm.Stop()

	initialPub := rm.PublicKey()
	rm.NotifyPathRecovery()
	// Verifica che il keypair locale sia stato rinnovato (executeRekey completato).
	waitUntil(t, 500*time.Millisecond, func() bool {
		return string(rm.PublicKey()) != string(initialPub)
	})
}

func TestRekeyManager_AntiFlap(t *testing.T) {
	cfg := RekeyConfig{Enabled: true, OnPathRecovery: true, AntiFlappingSeconds: 1}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}
	_ = sess

	rm, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, []byte{3}, 1)
	if err != nil {
		t.Fatalf("NewRekeyManager: %v", err)
	}
	rm.Start()
	defer rm.Stop()

	// Primo trigger: rinnova la keypair
	pub0 := rm.PublicKey()
	rm.NotifyPathRecovery()
	waitUntil(t, 500*time.Millisecond, func() bool {
		return string(rm.PublicKey()) != string(pub0)
	})
	pub1 := rm.PublicKey()

	// Secondo trigger entro la finestra anti-flap (50ms dopo il primo rekey)
	time.Sleep(50 * time.Millisecond)
	rm.NotifyPathRecovery()

	// La keypair NON deve cambiare subito (anti-flap)
	time.Sleep(100 * time.Millisecond)
	if string(rm.PublicKey()) != string(pub1) {
		t.Fatal("keypair changed within anti-flap window, expected no change")
	}

	// Dopo la scadenza dell'anti-flap (AntiFlappingSeconds=1), la keypair deve rinnovarsi
	waitUntil(t, 1500*time.Millisecond, func() bool {
		return string(rm.PublicKey()) != string(pub1)
	})
}

func TestRekeyManager_Disabled(t *testing.T) {
	cfg := RekeyConfig{Enabled: false, OnPathRecovery: true, MaxPackets: 1, IntervalSeconds: 1}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}

	rm, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, []byte{4}, 1)
	if err != nil {
		t.Fatalf("NewRekeyManager: %v", err)
	}
	rm.Start()
	defer rm.Stop()

	rm.NotifyPathRecovery()
	rm.NotifyPacketSent(1)
	time.Sleep(100 * time.Millisecond)

	if got := metrics.TotalRekeyEvents.Load(); got != 0 {
		t.Fatalf("TotalRekeyEvents=%d, want 0", got)
	}
}

func TestRekeyManager_InitiateRekey(t *testing.T) {
	cfg := RekeyConfig{Enabled: true}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}
	initialEpochID := uint8(7)

	rm, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, []byte{5}, initialEpochID)
	if err != nil {
		t.Fatalf("NewRekeyManager: %v", err)
	}
	defer rm.Stop()

	pubKey, _, err := kex.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	if err := rm.InitiateRekey(pubKey, initialEpochID+1); err != nil {
		t.Fatalf("InitiateRekey: %v", err)
	}

	sess.mu.Lock()
	uc := sess.updateCount
	sess.mu.Unlock()
	if uc != 1 {
		t.Fatalf("updateCount=%d, want 1", uc)
	}
}

func TestRekeyManager_InitiateRekey_BadEpoch(t *testing.T) {
	cfg := RekeyConfig{Enabled: true}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}
	initialEpochID := uint8(10)

	rm, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, []byte{6}, initialEpochID)
	if err != nil {
		t.Fatalf("NewRekeyManager: %v", err)
	}
	defer rm.Stop()

	pubKey, _, err := kex.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	err = rm.InitiateRekey(pubKey, initialEpochID+5)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrRekeyBadEpoch) {
		t.Fatalf("expected ErrRekeyBadEpoch, got %v", err)
	}
}

func TestRekeyManager_InitiateRekey_EmptyPubKey(t *testing.T) {
	cfg := RekeyConfig{Enabled: true}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}
	initialEpochID := uint8(15)

	rm, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, []byte{8}, initialEpochID)
	if err != nil {
		t.Fatalf("NewRekeyManager: %v", err)
	}
	defer rm.Stop()

	// Test with empty remotePubKey (SEC-002)
	err = rm.InitiateRekey([]byte{}, initialEpochID+1)
	if err == nil {
		t.Fatal("expected error for empty remotePubKey")
	}
	if !errors.Is(err, ErrKeyMaterial) {
		t.Fatalf("expected ErrKeyMaterial, got %v", err)
	}
}

func TestRekeyManager_StopDuringRekey(t *testing.T) {
	cfg := RekeyConfig{Enabled: true, MaxPackets: 1}
	kex := NewClassicalKEXProvider()
	sess := newMockSession()
	metrics := &CryptoMetrics{}

	rm, err := NewRekeyManager(context.Background(), cfg, kex, sess, metrics, []byte{7}, 1)
	if err != nil {
		t.Fatalf("NewRekeyManager: %v", err)
	}
	rm.Start()

	rm.NotifyPacketSent(1)

	done := make(chan struct{})
	go func() {
		rm.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() timeout (possible deadlock)")
	}
}
