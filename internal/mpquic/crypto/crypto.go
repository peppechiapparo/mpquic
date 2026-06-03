package crypto

import (
	"fmt"
	"sync/atomic"
)

// CryptoSession rappresenta lo stato crittografico runtime di una sessione STRIPES.
// È il punto unico di accesso del data plane al sottosistema crypto.
type CryptoSession struct {
	cfg     *CryptoConfig
	aead    AEADProvider
	kex     KeyExchangeProvider
	nonce   NonceManager
	keys    *SessionKeys
	metrics *CryptoMetrics
	closed  atomic.Bool
}

// NewCryptoSession crea una nuova CryptoSession dalla configurazione.
// Restituisce errore se la configurazione non è valida.
// Le implementazioni concrete di aead, kex e nonce vengono selezionate
// in base al profilo configurato.
//
// In Fase A, NewCryptoSession restituisce sempre ErrInvalidProfile perché
// nessun provider concreto è ancora registrato — questo è atteso.
func NewCryptoSession(cfg *CryptoConfig) (*CryptoSession, error) {
	if cfg == nil {
		return nil, ErrInvalidConfig
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("crypto: %w", err)
	}
	// TODO(fase-B): selezionare AEADProvider in base a cfg.Profile
	// TODO(fase-D): selezionare KeyExchangeProvider in base a cfg.Profile
	// TODO(fase-C): inizializzare NonceManager
	return nil, ErrInvalidProfile
}

// Close chiude la sessione e azzera il materiale crittografico in memoria.
func (s *CryptoSession) Close() error {
	if s == nil {
		return ErrSessionClosed
	}
	if s.closed.Swap(true) {
		return ErrSessionClosed
	}
	// Azzerare le chiavi in memoria
	if s.keys != nil {
		for i := range s.keys.ClientKey {
			s.keys.ClientKey[i] = 0
		}
		for i := range s.keys.ServerKey {
			s.keys.ServerKey[i] = 0
		}
	}
	return nil
}

// Metrics restituisce un puntatore ai contatori Prometheus della sessione.
func (s *CryptoSession) Metrics() *CryptoMetrics {
	if s == nil {
		return nil
	}
	return s.metrics
}
