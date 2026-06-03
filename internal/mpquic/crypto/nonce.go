package crypto

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
)

// NonceManager gestisce la generazione di nonce unici e non riutilizzabili.
// Le implementazioni devono essere lock-free nel hot path (NextNonce).
type NonceManager interface {
	// NextNonce restituisce il prossimo nonce per il workerID specificato.
	NextNonce(workerID uint) ([12]byte, error)

	// AdvanceEpoch avanza l'epoch a newEpoch.
	// Chiamare SOLO dopo che la nuova chiave è già attiva — mai durante la cifratura.
	AdvanceEpoch(newEpoch uint8) error

	// NonceSize restituisce la dimensione del nonce in byte (sempre 12).
	NonceSize() int
}

// ContextualNonceManager è un'implementazione lock-free di NonceManager.
//
// Strategia per-worker step=nWorkers:
//
//	worker 0 → 0, N, 2N, 3N, ...
//	worker 1 → 1, N+1, 2N+1, ...
//	worker w → w, w+N, w+2N, ...
//
// Questa strategia elimina la contesa mutex sull'hot path TX: ogni worker ha
// il proprio contatore atomico e non condivide mai contatori con altri worker.
//
// Nonce wire format (12 byte):
//
//	[epoch:1][reserved:3][counter:8 big-endian]
//
// L'epoch nel nonce distingue sessioni crittografiche diverse (dopo rekey),
// rendendo impossibile il riutilizzo accidentale di nonce tra chiavi diverse.
type ContextualNonceManager struct {
	nWorkers uint
	epoch    atomic.Uint32 // solo i byte bassi 8 bit sono usati come epoch

	// counters[w] è il contatore atomico per il worker w.
	// Inizializzato a w; incrementato di nWorkers ad ogni NextNonce.
	// Monotonicamente crescente — mai resettato (SEC-004).
	counters []atomic.Uint64

	// exhausted è un latch fail-secure (SEC-001).
	// Una volta impostato a true (wrap-around del contatore), NextNonce
	// continuerà a ritornare ErrNonceExhausted fino al rekey.
	exhausted atomic.Bool
}

// NewContextualNonceManager crea un ContextualNonceManager per nWorkers worker.
// nWorkers deve essere > 0.
func NewContextualNonceManager(nWorkers uint) (*ContextualNonceManager, error) {
	if nWorkers == 0 {
		return nil, fmt.Errorf("nonce: nWorkers must be > 0")
	}
	m := &ContextualNonceManager{
		nWorkers: nWorkers,
		counters: make([]atomic.Uint64, nWorkers),
	}
	for w := uint(0); w < nWorkers; w++ {
		m.counters[w].Store(uint64(w))
	}
	return m, nil
}

// NextNonce genera il prossimo nonce per il workerID specificato.
//
// Hot path — lock-free, zero allocazioni heap.
// Ritorna un [12]byte per valore (stack-allocated): nessun aliasing,
// il caller possiede il buffer e può usarlo senza sincronizzazione.
// Restituisce ErrNonceExhausted se il contatore ha fatto wrap-around (estremamente
// improbabile: richiede 2^64/nWorkers pacchetti per worker prima del rekey).
// SEC-001: dopo wrap, il latch exhausted impedisce qualsiasi rigenerazione.
func (m *ContextualNonceManager) NextNonce(workerID uint) ([12]byte, error) {
	if workerID >= m.nWorkers {
		return [12]byte{}, fmt.Errorf("nonce: workerID %d out of range [0, %d)", workerID, m.nWorkers)
	}

	// SEC-001: latch fail-secure — una volta esaurito, non rigenerare mai seq bassi.
	if m.exhausted.Load() {
		return [12]byte{}, ErrNonceExhausted
	}

	step := uint64(m.nWorkers)

	// Incrementa atomicamente di nWorkers e ottieni il valore precedente (seq).
	newVal := m.counters[workerID].Add(step)
	if newVal < step {
		// Wrap-around rilevato: imposta il latch fail-secure e ritorna errore.
		m.exhausted.Store(true)
		return [12]byte{}, ErrNonceExhausted
	}
	seq := newVal - step // valore prima dell'incremento

	epoch := uint8(m.epoch.Load())

	// Costruisce il nonce sul stack (no heap alloc).
	// Wire format: [epoch:1][reserved:3][counter:8 big-endian]
	var nonce [12]byte
	nonce[0] = epoch
	// nonce[1..3] = 0 (reserved, già zero da zero-value)
	binary.BigEndian.PutUint64(nonce[4:], seq)
	return nonce, nil
}

// AdvanceEpoch avanza l'epoch a newEpoch.
//
// SEC-004: newEpoch deve essere strettamente maggiore dell'epoch corrente.
// Implementa CAS per thread-safety senza mutex.
//
// Limite: l'epoch è un uint8 — max 255 rekey per sessione.
// Superato questo limite, AdvanceEpoch restituirà sempre errore.
// In pratica (1 rekey/ora) = 10+ anni prima di esaurimento.
//
// Sicurezza durante la transizione:
//
//	I contatori per-worker NON vengono resettati: continuano monotonicamente.
//	Questo evita race che possono causare nonce reuse dopo una transizione.
//	Il byte epoch nel nonce distingue già le sessioni crittografiche:
//	(epoch diversa = chiave diversa = spazio nonce separato).
func (m *ContextualNonceManager) AdvanceEpoch(newEpoch uint8) error {
	for {
		cur := m.epoch.Load()
		curEpoch := uint8(cur)
		if newEpoch <= curEpoch {
			return fmt.Errorf("nonce: new epoch %d must be > current epoch %d (SEC-004 monotonic enforcement)",
				newEpoch, curEpoch)
		}
		if m.epoch.CompareAndSwap(cur, uint32(newEpoch)) {
			// I counter NON vengono resettati: il byte epoch nel nonce distingue
			// già le sessioni crittografiche e mantiene unici i nonce per (epoch,key).
			return nil
		}
	}
}

// NonceSize restituisce sempre 12 (GCM standard nonce size).
func (m *ContextualNonceManager) NonceSize() int { return 12 }
