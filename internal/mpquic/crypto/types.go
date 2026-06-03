package crypto

import "sync/atomic"

// CryptoProfile è il tipo per i profili crittografici configurabili.
type CryptoProfile string

const (
	ProfilePerformance    CryptoProfile = "performance"
	ProfileHybridSecurity CryptoProfile = "hybrid_security"
	ProfileCustomProvider CryptoProfile = "custom_provider"
)

func (p CryptoProfile) String() string {
	return string(p)
}

// SessionKeys contiene le chiavi operative per una sessione crittografica.
type SessionKeys struct {
	ClientKey []byte // client→server AES-256 key (32 byte)
	ServerKey []byte // server→client AES-256 key (32 byte)
	ClientIV  []byte // client→server base IV (opzionale, 12 byte)
	ServerIV  []byte // server→client base IV (opzionale, 12 byte)
	EpochID   uint8  // epoch corrente (incrementa a ogni rekey)
}

// CryptoMetrics contiene i contatori per il monitoraggio Prometheus.
// Tutti i campi numerici devono essere aggiornati con sync/atomic.
type CryptoMetrics struct {
	TotalEncryptions        atomic.Uint64
	TotalDecryptions        atomic.Uint64
	TotalDecryptionFailures atomic.Uint64
	TotalAuthFailures       atomic.Uint64
	TotalRekeyEvents        atomic.Uint64
	TotalKEXCompleted       atomic.Uint64
	TotalKEXFailures        atomic.Uint64
	// Snapshot fields (non-atomic, letti solo da metrics collector)
	ActiveProfile CryptoProfile
	ActiveEpoch   uint8
}
