package crypto

import (
	"fmt"
	"runtime"
	"sync/atomic"
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
	// Azzerare le chiavi in memoria (SEC-002)
	if s.keys != nil {
		zeroize(s.keys.ClientKey)
		zeroize(s.keys.ServerKey)
		zeroize(s.keys.ClientIV)
		zeroize(s.keys.ServerIV)
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
