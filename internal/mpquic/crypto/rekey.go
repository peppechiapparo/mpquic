package crypto

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type rekeyState int

const (
	stateIdle rekeyState = iota
	stateTriggered
	stateAntiFlapCooldown
	stateTransitioning
	stateCompleting
)

type rekeyTrigger struct {
	reason  string
	isEvent bool
}

type RekeyableSession interface {
	UpdateKeys(newKeys *SessionKeys) error
	GetKeysForEpoch(epochID uint8) (*SessionKeys, bool)
	PruneOldKeys(oldestValidEpoch uint8)
}

const defaultTransitionWindow = 5 * time.Second

type RekeyManager struct {
	config      RekeyConfig
	kexProvider KeyExchangeProvider
	session     RekeyableSession
	metrics     *CryptoMetrics
	sessionID   []byte

	mu             sync.Mutex
	state          rekeyState
	lastRekeyTime  time.Time
	packetsSince   uint64
	bytesSince     uint64
	currentEpochID uint8
	localPrivKey   []byte // segreto, non esporre mai direttamente
	localPubKey    []byte // protetto da mu, leggere via PublicKey()

	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	triggerChan chan rekeyTrigger
}

func NewRekeyManager(
	ctx context.Context,
	cfg RekeyConfig,
	kex KeyExchangeProvider,
	session RekeyableSession,
	metrics *CryptoMetrics,
	sessionID []byte,
	initialEpochID uint8,
) (*RekeyManager, error) {
	if kex == nil {
		return nil, ErrMissingProvider
	}
	if session == nil {
		return nil, fmt.Errorf("%w: RekeyableSession must not be nil", ErrMissingProvider)
	}
	if metrics == nil {
		return nil, fmt.Errorf("%w: CryptoMetrics must not be nil", ErrMissingProvider)
	}
	if len(sessionID) == 0 {
		return nil, ErrEmptySessionID
	}

	pubKey, privKey, err := kex.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("rekey: initial key generation: %w", err)
	}

	childCtx, cancel := context.WithCancel(ctx)
	rm := &RekeyManager{
		config:         cfg,
		kexProvider:    kex,
		session:        session,
		metrics:        metrics,
		sessionID:      sessionID,
		state:          stateIdle,
		lastRekeyTime:  time.Time{},
		currentEpochID: initialEpochID,
		localPrivKey:   privKey,
		localPubKey:    pubKey,
		ctx:            childCtx,
		cancel:         cancel,
		triggerChan:    make(chan rekeyTrigger, 4),
	}
	return rm, nil
}

func (rm *RekeyManager) Start() {
	rm.wg.Add(1)
	go rm.run()
}

func (rm *RekeyManager) Stop() {
	rm.cancel()
	rm.wg.Wait()
}

// PublicKey restituisce una copia della chiave pubblica effimera locale.
// Thread-safe. Il chiamante deve inviare questa chiave al peer prima di
// completare il rekey tramite InitiateRekey.
func (rm *RekeyManager) PublicKey() []byte {
	rm.mu.Lock()
	cp := make([]byte, len(rm.localPubKey))
	copy(cp, rm.localPubKey)
	rm.mu.Unlock()
	return cp
}

func (rm *RekeyManager) run() {
	defer rm.wg.Done()

	tickInterval := time.Duration(rm.config.IntervalSeconds) * time.Second
	if !rm.config.Enabled || rm.config.IntervalSeconds <= 0 {
		tickInterval = 24 * time.Hour
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-rm.ctx.Done():
			return
		case <-ticker.C:
			if rm.config.Enabled {
				rm.sendTrigger(rekeyTrigger{reason: "interval"})
			}
		case trigger := <-rm.triggerChan:
			rm.handleTrigger(trigger)
		}
	}
}

func (rm *RekeyManager) sendTrigger(t rekeyTrigger) {
	select {
	case rm.triggerChan <- t:
	default:
	}
}

func (rm *RekeyManager) handleTrigger(t rekeyTrigger) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if !rm.config.Enabled {
		return
	}

	if rm.state == stateTransitioning {
		return
	}

	rm.state = stateTriggered

	antiFlap := time.Duration(rm.config.AntiFlappingSeconds) * time.Second
	if antiFlap > 0 && !rm.lastRekeyTime.IsZero() &&
		time.Since(rm.lastRekeyTime) < antiFlap {

		rm.state = stateAntiFlapCooldown
		retryAt := rm.lastRekeyTime.Add(antiFlap)
		time.AfterFunc(time.Until(retryAt), func() {
			if rm.ctx.Err() != nil {
				return
			}
			rm.sendTrigger(rekeyTrigger{reason: "anti-flap-retry"})
		})
		return
	}

	rm.state = stateTransitioning
	rm.wg.Add(1)
	go rm.executeRekey(t.reason)
}

// executeRekey gestisce il lato locale di un rekey spontaneo (trigger-initiated).
// Rinnova la coppia di chiavi effimere locali e aggiorna lastRekeyTime per l'anti-flap.
// NON chiama DeriveSessionKeys né UpdateKeys: un KEX interattivo richiede la pub key
// del peer, che arriva separatamente tramite InitiateRekey.
// La Fase G collegherà il segnale di "keypair rinnovata" all'invio della nuova LocalPubKey
// al peer via il canale di controllo STRIPES.
func (rm *RekeyManager) executeRekey(reason string) {
	_ = reason
	defer rm.wg.Done()

	newPubKey, newPrivKey, err := rm.kexProvider.GenerateKeyPair()
	if err != nil {
		rm.metrics.TotalKEXFailures.Add(1)
		rm.mu.Lock()
		rm.state = stateIdle
		rm.mu.Unlock()
		return
	}

	rm.mu.Lock()
	// Azzera la vecchia chiave privata (forward secrecy)
	zeroize(rm.localPrivKey)
	rm.localPrivKey = newPrivKey
	newPubKeyCopy := make([]byte, len(newPubKey))
	copy(newPubKeyCopy, newPubKey)
	rm.localPubKey = newPubKeyCopy
	// Aggiorna lastRekeyTime per il calcolo anti-flap
	rm.lastRekeyTime = time.Now()
	// SEC-003: reset counter per evitare trigger churn continuo dopo soglia raggiunta
	rm.packetsSince = 0
	rm.bytesSince = 0
	rm.state = stateIdle
	rm.mu.Unlock()
}

func (rm *RekeyManager) NotifyPacketSent(packetSizeBytes int) {
	if !rm.config.Enabled {
		return
	}
	if packetSizeBytes < 0 {
		return
	}
	rm.mu.Lock()
	rm.packetsSince++
	rm.bytesSince += uint64(packetSizeBytes)
	packets := rm.packetsSince
	bytes := rm.bytesSince
	state := rm.state
	rm.mu.Unlock()

	if state != stateIdle {
		return
	}
	if (rm.config.MaxPackets > 0 && packets >= rm.config.MaxPackets) ||
		(rm.config.MaxBytes > 0 && bytes >= rm.config.MaxBytes) {
		rm.sendTrigger(rekeyTrigger{reason: "threshold", isEvent: false})
	}
}

func (rm *RekeyManager) NotifyPathRecovery() {
	if !rm.config.Enabled || !rm.config.OnPathRecovery {
		return
	}
	rm.sendTrigger(rekeyTrigger{reason: "path-recovery", isEvent: true})
}

func (rm *RekeyManager) InitiateRekey(remotePubKey []byte, expectedNextEpochID uint8) error {
	// SEC-002: validazione input al boundary
	if len(remotePubKey) == 0 {
		return fmt.Errorf("%w: remotePubKey is empty", ErrKeyMaterial)
	}
	rm.mu.Lock()
	if !rm.config.Enabled {
		rm.mu.Unlock()
		return ErrRekeyDisabled
	}
	if rm.state == stateTransitioning || rm.state == stateCompleting {
		rm.mu.Unlock()
		return ErrRekeyInProgress
	}
	nextEpoch := rm.currentEpochID + 1
	if expectedNextEpochID != nextEpoch {
		rm.mu.Unlock()
		return fmt.Errorf("%w: got %d, expected %d", ErrRekeyBadEpoch, expectedNextEpochID, nextEpoch)
	}

	sessionIDCopy := make([]byte, len(rm.sessionID))
	copy(sessionIDCopy, rm.sessionID)
	// SEC-001: copia difensiva di localPrivKey per evitare che zeroize() in caso
	// di errore corrompa rm.localPrivKey (slice condivisa con rm.localPrivKey).
	localPrivKey := make([]byte, len(rm.localPrivKey))
	copy(localPrivKey, rm.localPrivKey)
	rm.state = stateTransitioning
	rm.mu.Unlock()

	newPubKey, newPrivKey, err := rm.kexProvider.GenerateKeyPair()
	if err != nil {
		rm.metrics.TotalKEXFailures.Add(1)
		rm.mu.Lock()
		rm.state = stateIdle
		rm.mu.Unlock()
		return fmt.Errorf("rekey: GenerateKeyPair: %w", err)
	}

	newKeys, err := rm.kexProvider.DeriveSessionKeys(
		remotePubKey,
		localPrivKey,
		remotePubKey,
		sessionIDCopy,
	)
	zeroize(localPrivKey)
	if err != nil {
		rm.metrics.TotalKEXFailures.Add(1)
		zeroize(newPrivKey)
		rm.mu.Lock()
		rm.state = stateIdle
		rm.mu.Unlock()
		return fmt.Errorf("rekey: DeriveSessionKeys: %w", err)
	}

	newKeys.EpochID = expectedNextEpochID
	if err := rm.session.UpdateKeys(newKeys); err != nil {
		rm.metrics.TotalKEXFailures.Add(1)
		zeroize(newPrivKey)
		rm.mu.Lock()
		rm.state = stateIdle
		rm.mu.Unlock()
		return fmt.Errorf("rekey: UpdateKeys: %w", err)
	}

	rm.metrics.TotalKEXCompleted.Add(1)
	rm.metrics.TotalRekeyEvents.Add(1)

	epochActivated := expectedNextEpochID
	rm.mu.Lock()
	rm.currentEpochID = expectedNextEpochID
	rm.lastRekeyTime = time.Now()
	rm.packetsSince = 0
	rm.bytesSince = 0
	zeroize(rm.localPrivKey)
	rm.localPrivKey = newPrivKey
	newPubKeyCopy := make([]byte, len(newPubKey))
	copy(newPubKeyCopy, newPubKey)
	rm.localPubKey = newPubKeyCopy
	rm.metrics.ActiveEpoch = expectedNextEpochID
	rm.state = stateCompleting
	rm.mu.Unlock()

	time.AfterFunc(defaultTransitionWindow, func() {
		if rm.ctx.Err() != nil {
			return
		}
		rm.session.PruneOldKeys(epochActivated)
		rm.mu.Lock()
		if rm.state == stateCompleting {
			rm.state = stateIdle
		}
		rm.mu.Unlock()
	})

	return nil
}
