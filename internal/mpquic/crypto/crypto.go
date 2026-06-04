package crypto

import (
	"context"
	"crypto/cipher"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// zeroize azzera tutti i byte di b in memoria.
// runtime.KeepAlive(b) impedisce al GC di ottimizzare l'azzeramento.
// SEC-002: usare questa funzione ovunque venga azzerato materiale crittografico.
func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}

const (
	// epochWindowEntries è il numero massimo di epoch tenuti simultaneamente.
	epochWindowEntries = 4
	// epochTransitionWindow è la finestra temporale durante la quale
	// entrambi l'epoch corrente e il precedente sono validi per RX.
	epochTransitionWindow = 5 * time.Second
)

// epochEntry aggrega le chiavi operative e le AEAD per un singolo epoch.
type epochEntry struct {
	keys    *SessionKeys
	txAEAD  cipher.AEAD // AEAD per TX (client→server key per client, server→client per server)
	rxAEAD  cipher.AEAD // AEAD per RX (viceversa)
	addedAt time.Time
}

// CryptoSession è lo stato crittografico runtime di una sessione STRIPES.
// Implementa cipher.AEAD per integrazione diretta con stripeCipher.aead.
// Implementa RekeyableSession per integrazione con RekeyManager.
// Thread-safe.
type CryptoSession struct {
	cfg        *CryptoConfig
	aeadProv   AEADProvider
	kex        KeyExchangeProvider
	nonce      NonceManager
	quicSecret []byte // TLS Exporter 64B — statico per tutta la sessione
	sessionID  []byte
	isServer   bool
	metrics    *CryptoMetrics

	closed atomic.Bool
	ctx    context.Context
	cancel context.CancelFunc

	epochMu      sync.RWMutex
	epochs       map[uint8]*epochEntry
	currentEpoch atomic.Uint32 // low 8 bits = epoch attivo per TX; atomic per lock-free hot path

	rekeyMgr *RekeyManager // nil se cfg.Rekey.Enabled = false
}

// NewCryptoSession costruisce una CryptoSession completa e operativa.
func NewCryptoSession(
	cfg *CryptoConfig,
	quicSecret []byte,
	initialKeys *SessionKeys,
	isServer bool,
	sessionID []byte,
	nWorkers uint,
) (*CryptoSession, error) {
	if cfg == nil {
		return nil, ErrInvalidConfig
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("crypto: %w", err)
	}
	if len(quicSecret) < 64 {
		return nil, fmt.Errorf("%w: quicSecret must be at least 64 bytes, got %d", ErrKeyMaterial, len(quicSecret))
	}
	if initialKeys == nil {
		return nil, fmt.Errorf("%w: initialKeys must not be nil", ErrKeyMaterial)
	}
	if len(sessionID) == 0 {
		return nil, ErrEmptySessionID
	}
	if nWorkers == 0 {
		return nil, fmt.Errorf("crypto: nWorkers must be > 0")
	}

	aeadProv := NewAESGCMProvider()

	kex, err := NewKeyExchangeProvider(cfg.Profile)
	if err != nil {
		return nil, fmt.Errorf("crypto: kex init: %w", err)
	}

	nonceMgr, err := NewContextualNonceManager(nWorkers)
	if err != nil {
		return nil, fmt.Errorf("crypto: nonce manager: %w", err)
	}

	metrics := &CryptoMetrics{
		ActiveProfile: cfg.Profile,
	}

	qsCopy := make([]byte, 64)
	copy(qsCopy, quicSecret[:64])
	sidCopy := make([]byte, len(sessionID))
	copy(sidCopy, sessionID)

	ctx, cancel := context.WithCancel(context.Background())

	s := &CryptoSession{
		cfg:        cfg,
		aeadProv:   aeadProv,
		kex:        kex,
		nonce:      nonceMgr,
		quicSecret: qsCopy,
		sessionID:  sidCopy,
		isServer:   isServer,
		metrics:    metrics,
		ctx:        ctx,
		cancel:     cancel,
		epochs:     make(map[uint8]*epochEntry, epochWindowEntries),
	}

	if err := s.addEpochLocked(initialKeys); err != nil {
		cancel()
		return nil, fmt.Errorf("crypto: epoch init: %w", err)
	}
	s.currentEpoch.Store(uint32(initialKeys.EpochID))
	metrics.ActiveEpoch = initialKeys.EpochID

	if cfg.Rekey.Enabled {
		rm, err := NewRekeyManager(ctx, cfg.Rekey, kex, s, metrics, sidCopy, qsCopy, initialKeys.EpochID)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("crypto: rekey manager: %w", err)
		}
		s.rekeyMgr = rm
		rm.Start()
	}

	return s, nil
}

// addEpochLocked crea le AEAD per una nuova epoch e le registra nella map.
func (s *CryptoSession) addEpochLocked(keys *SessionKeys) error {
	if keys == nil {
		return fmt.Errorf("%w: keys are nil", ErrKeyMaterial)
	}

	// SEC-G02: rifiuta sovrascrittura di un'epoch già presente (replay/confusion attack).
	if _, exists := s.epochs[keys.EpochID]; exists {
		return fmt.Errorf("%w: epoch %d already exists", ErrRekeyBadEpoch, keys.EpochID)
	}

	var txKey, rxKey []byte
	if s.isServer {
		txKey = keys.ServerKey
		rxKey = keys.ClientKey
	} else {
		txKey = keys.ClientKey
		rxKey = keys.ServerKey
	}

	if len(txKey) != s.aeadProv.KeySize() {
		return fmt.Errorf("%w: txKey len=%d, expected %d", ErrInvalidKeySize, len(txKey), s.aeadProv.KeySize())
	}
	if len(rxKey) != s.aeadProv.KeySize() {
		return fmt.Errorf("%w: rxKey len=%d, expected %d", ErrInvalidKeySize, len(rxKey), s.aeadProv.KeySize())
	}

	txAEAD, err := s.aeadProv.NewAEAD(txKey)
	if err != nil {
		return fmt.Errorf("crypto: txAEAD epoch %d: %w", keys.EpochID, err)
	}
	rxAEAD, err := s.aeadProv.NewAEAD(rxKey)
	if err != nil {
		return fmt.Errorf("crypto: rxAEAD epoch %d: %w", keys.EpochID, err)
	}

	keysCopy := &SessionKeys{EpochID: keys.EpochID}
	if len(keys.ClientKey) > 0 {
		keysCopy.ClientKey = make([]byte, len(keys.ClientKey))
		copy(keysCopy.ClientKey, keys.ClientKey)
	}
	if len(keys.ServerKey) > 0 {
		keysCopy.ServerKey = make([]byte, len(keys.ServerKey))
		copy(keysCopy.ServerKey, keys.ServerKey)
	}
	if len(keys.ClientIV) > 0 {
		keysCopy.ClientIV = make([]byte, len(keys.ClientIV))
		copy(keysCopy.ClientIV, keys.ClientIV)
	}
	if len(keys.ServerIV) > 0 {
		keysCopy.ServerIV = make([]byte, len(keys.ServerIV))
		copy(keysCopy.ServerIV, keys.ServerIV)
	}

	s.epochs[keys.EpochID] = &epochEntry{
		keys:    keysCopy,
		txAEAD:  txAEAD,
		rxAEAD:  rxAEAD,
		addedAt: time.Now(),
	}
	return nil
}

// ── cipher.AEAD interface ──────────────────────────────────────────────────

// NonceSize implementa cipher.AEAD. Ritorna sempre 12 (GCM standard).
func (s *CryptoSession) NonceSize() int { return 12 }

// Overhead implementa cipher.AEAD. Ritorna sempre 16 (AES-GCM tag).
func (s *CryptoSession) Overhead() int { return 16 }

// Seal implementa cipher.AEAD.Seal.
//
// Il chiamante (stripeEncrypt*) fornisce un nonce [12]byte con nonce[0]=0 e
// nonce[4:12] = seq big-endian. Seal usa una copia interna del nonce in cui
// inietta currentEpoch nel byte 0 — il buffer originale del chiamante non
// viene mai modificato (invariante cipher.AEAD).
//
// SEC-FAILSAFE: se l'epoch corrente non è presente (invariante violato —
// non deve accadere in normale operazione), Seal ritorna dst invariato e
// incrementa il contatore di encryption failures. Il chiamante (stripeEncrypt)
// riceverà un output senza payload cifrato — il pacchetto sarà scartato.
func (s *CryptoSession) Seal(dst, nonce, plaintext, additionalData []byte) []byte {
	epochID := uint8(s.currentEpoch.Load())

	// Copia difensiva del nonce: Seal NON muta il buffer del chiamante.
	var txNonce [12]byte
	copy(txNonce[:], nonce)
	txNonce[0] = epochID // epoch nel byte 0 (i byte 1-3 sono reserved)

	s.epochMu.RLock()
	entry, ok := s.epochs[epochID]
	s.epochMu.RUnlock()

	if !ok {
		// Invariante violato: non produrre ciphertext con AEAD sconosciuta.
		s.metrics.TotalAuthFailures.Add(1) // misuse dell'AEAD = auth concern
		return dst
	}

	out := entry.txAEAD.Seal(dst, txNonce[:], plaintext, additionalData)
	s.metrics.TotalEncryptions.Add(1)

	if s.rekeyMgr != nil {
		s.rekeyMgr.NotifyPacketSent(len(plaintext) + 16)
	}
	return out
}

// Open implementa cipher.AEAD.Open.
func (s *CryptoSession) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	currentEpochID := uint8(s.currentEpoch.Load())

	s.epochMu.RLock()
	current := s.epochs[currentEpochID]
	var prev *epochEntry
	if currentEpochID > 0 {
		prev = s.epochs[currentEpochID-1]
	}
	s.epochMu.RUnlock()

	if current != nil {
		var nonceEpoch [12]byte
		copy(nonceEpoch[:], nonce)
		nonceEpoch[0] = currentEpochID
		out, err := current.rxAEAD.Open(dst, nonceEpoch[:], ciphertext, additionalData)
		if err == nil {
			s.metrics.TotalDecryptions.Add(1)
			return out, nil
		}
	}

	if prev != nil {
		var nonceEpoch [12]byte
		copy(nonceEpoch[:], nonce)
		nonceEpoch[0] = currentEpochID - 1
		out, err := prev.rxAEAD.Open(dst, nonceEpoch[:], ciphertext, additionalData)
		if err == nil {
			s.metrics.TotalDecryptions.Add(1)
			return out, nil
		}
	}

	s.metrics.TotalDecryptionFailures.Add(1)
	s.metrics.TotalAuthFailures.Add(1)
	return nil, ErrAuthFailed
}

// ── RekeyableSession interface ────────────────────────────────────────────

// UpdateKeys aggiunge una nuova epoch e avanza currentEpoch.
func (s *CryptoSession) UpdateKeys(newKeys *SessionKeys) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if newKeys == nil {
		return fmt.Errorf("%w: newKeys must not be nil", ErrKeyMaterial)
	}

	s.epochMu.Lock()
	if err := s.addEpochLocked(newKeys); err != nil {
		s.epochMu.Unlock()
		return err
	}
	s.epochMu.Unlock()

	s.currentEpoch.Store(uint32(newKeys.EpochID))
	s.metrics.ActiveEpoch = newKeys.EpochID
	s.metrics.TotalRekeyEvents.Add(1)

	if err := s.nonce.AdvanceEpoch(newKeys.EpochID); err != nil {
		_ = err
	}

	if newKeys.EpochID > 0 {
		oldestToKeep := newKeys.EpochID
		go func() {
			timer := time.NewTimer(epochTransitionWindow)
			defer timer.Stop()
			select {
			case <-timer.C:
				s.PruneOldKeys(oldestToKeep)
			case <-s.ctx.Done():
			}
		}()
	}

	return nil
}

// GetKeysForEpoch ritorna una copia difensiva delle SessionKeys per l'epochID.
func (s *CryptoSession) GetKeysForEpoch(epochID uint8) (*SessionKeys, bool) {
	s.epochMu.RLock()
	entry, ok := s.epochs[epochID]
	s.epochMu.RUnlock()
	if !ok {
		return nil, false
	}
	cp := &SessionKeys{EpochID: entry.keys.EpochID}
	if len(entry.keys.ClientKey) > 0 {
		cp.ClientKey = make([]byte, len(entry.keys.ClientKey))
		copy(cp.ClientKey, entry.keys.ClientKey)
	}
	if len(entry.keys.ServerKey) > 0 {
		cp.ServerKey = make([]byte, len(entry.keys.ServerKey))
		copy(cp.ServerKey, entry.keys.ServerKey)
	}
	return cp, true
}

// PruneOldKeys rimuove dalla mappa tutte le epoch con ID < oldestValidEpoch.
func (s *CryptoSession) PruneOldKeys(oldestValidEpoch uint8) {
	s.epochMu.Lock()
	defer s.epochMu.Unlock()

	for id, entry := range s.epochs {
		if id < oldestValidEpoch {
			if entry.keys != nil {
				zeroize(entry.keys.ClientKey)
				zeroize(entry.keys.ServerKey)
				zeroize(entry.keys.ClientIV)
				zeroize(entry.keys.ServerIV)
			}
			delete(s.epochs, id)
		}
	}
}

// ── Lifecycle ─────────────────────────────────────────────────────────────

// Close chiude la sessione, ferma il RekeyManager e azzera il materiale crittografico.
func (s *CryptoSession) Close() error {
	if s == nil {
		return ErrSessionClosed
	}
	if s.closed.Swap(true) {
		return ErrSessionClosed
	}

	if s.rekeyMgr != nil {
		s.rekeyMgr.Stop()
	}

	s.cancel()

	s.epochMu.Lock()
	for id, entry := range s.epochs {
		if entry.keys != nil {
			zeroize(entry.keys.ClientKey)
			zeroize(entry.keys.ServerKey)
			zeroize(entry.keys.ClientIV)
			zeroize(entry.keys.ServerIV)
		}
		delete(s.epochs, id)
	}
	s.epochMu.Unlock()

	zeroize(s.quicSecret)
	zeroize(s.sessionID)

	runtime.KeepAlive(s)
	return nil
}

// NotifyPathRecovery segnala al RekeyManager il recupero di un path STRIPES.
func (s *CryptoSession) NotifyPathRecovery() {
	if s == nil || s.closed.Load() {
		return
	}
	if s.rekeyMgr != nil {
		s.rekeyMgr.NotifyPathRecovery()
	}
}

// RekeyManager restituisce il RekeyManager della sessione.
func (s *CryptoSession) RekeyManager() *RekeyManager {
	return s.rekeyMgr
}

// Metrics restituisce il puntatore ai contatori Prometheus della sessione.
func (s *CryptoSession) Metrics() *CryptoMetrics {
	if s == nil {
		return nil
	}
	return s.metrics
}
