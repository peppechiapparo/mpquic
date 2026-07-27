package crypto_test

import (
	"encoding/binary"
	"testing"

	"mpquic/internal/mpquic/crypto"
)

func TestContextualNonceManager_FormatAndStride(t *testing.T) {
	m, err := crypto.NewContextualNonceManager(4)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}
	if got, want := m.NonceSize(), 12; got != want {
		t.Fatalf("NonceSize() = %d, want %d", got, want)
	}

	n0, err := m.NextNonce(2)
	if err != nil {
		t.Fatalf("NextNonce: %v", err)
	}
	n1, err := m.NextNonce(2)
	if err != nil {
		t.Fatalf("NextNonce: %v", err)
	}

	if got, want := n0[0], byte(0); got != want {
		t.Fatalf("epoch byte n0[0] = %d, want %d", got, want)
	}
	for i := 1; i < 4; i++ {
		if n0[i] != 0 {
			t.Fatalf("reserved byte n0[%d] = 0x%02x, want 0x00", i, n0[i])
		}
	}

	seq0 := binary.BigEndian.Uint64(n0[4:])
	seq1 := binary.BigEndian.Uint64(n1[4:])
	if got, want := seq0, uint64(2); got != want {
		t.Fatalf("seq0 = %d, want %d", got, want)
	}
	if got, want := seq1, uint64(6); got != want {
		t.Fatalf("seq1 = %d, want %d (stride=4)", got, want)
	}
}

func TestContextualNonceManager_AdvanceEpoch(t *testing.T) {
	m, err := crypto.NewContextualNonceManager(2)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	n0, err := m.NextNonce(0)
	if err != nil {
		t.Fatalf("NextNonce: %v", err)
	}
	seq0 := binary.BigEndian.Uint64(n0[4:])
	if got, want := seq0, uint64(0); got != want {
		t.Fatalf("seq0 = %d, want %d", got, want)
	}
	if got, want := n0[0], byte(0); got != want {
		t.Fatalf("epoch byte n0[0] = %d, want %d", got, want)
	}

	if err := m.AdvanceEpoch(1); err != nil {
		t.Fatalf("AdvanceEpoch(1): %v", err)
	}
	if err := m.AdvanceEpoch(1); err == nil {
		t.Fatalf("AdvanceEpoch(1) again: expected error")
	}

	n1, err := m.NextNonce(0)
	if err != nil {
		t.Fatalf("NextNonce: %v", err)
	}
	if got, want := n1[0], byte(1); got != want {
		t.Fatalf("epoch byte n1[0] = %d, want %d", got, want)
	}
	seq1 := binary.BigEndian.Uint64(n1[4:])
	if got, want := seq1, uint64(2); got != want {
		t.Fatalf("seq1 = %d, want %d (counter must not reset)", got, want)
	}
}
package crypto_test

import (
	"sync"
	"testing"

	crypto "mpquic/internal/mpquic/crypto"
)

// TestContextualNonceManager_Sequential verifica che ogni worker produca nonce
// strettamente crescenti con step = nWorkers.
func TestContextualNonceManager_Sequential(t *testing.T) {
	const nWorkers = 4
	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	// Ogni worker deve produrre 100 nonce distinti e crescenti
	for w := uint(0); w < nWorkers; w++ {
		var prev [12]byte
		hasPrev := false
		for i := 0; i < 100; i++ {
			n, err := m.NextNonce(w)
			if err != nil {
				t.Fatalf("worker %d iter %d: %v", w, i, err)
			}
			if len(n) != 12 {
				t.Fatalf("worker %d: nonce len = %d, want 12", w, len(n))
			}
			if hasPrev {
				// I nonce devono essere strettamente crescenti (confronto lessicografico
				// corretto per big-endian 8B counter ai byte 4-11)
				if string(n[:]) <= string(prev[:]) {
					t.Errorf("worker %d iter %d: nonce not increasing", w, i)
				}
			}
			prev = n
			hasPrev = true
		}
	}
}

// TestContextualNonceManager_PerWorkerUnique verifica che worker diversi
// non producano mai lo stesso nonce (sequenze non sovrapposte).
func TestContextualNonceManager_PerWorkerUnique(t *testing.T) {
	const nWorkers = 4
	const nPerWorker = 500
	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	seen := make(map[[12]byte]struct{}, nWorkers*nPerWorker)
	for w := uint(0); w < nWorkers; w++ {
		for i := 0; i < nPerWorker; i++ {
			n, err := m.NextNonce(w)
			if err != nil {
				t.Fatalf("worker %d iter %d: %v", w, i, err)
			}
			key := n
			if _, dup := seen[key]; dup {
				t.Errorf("worker %d iter %d: duplicate nonce", w, i)
			}
			seen[key] = struct{}{}
		}
	}
}

// TestContextualNonceManager_Concurrency verifica zero collisioni con 1000 goroutine
// concorrenti che generano nonce da worker casuali.
func TestContextualNonceManager_Concurrency(t *testing.T) {
	const nWorkers = 8
	const nGoroutines = 1000
	const nPerGoroutine = 100

	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	type result struct {
		workerID uint
		nonces   [][12]byte
	}

	results := make([]result, nGoroutines)
	var wg sync.WaitGroup
	wg.Add(nGoroutines)

	for g := 0; g < nGoroutines; g++ {
		go func(g int) {
			defer wg.Done()
			workerID := uint(g % nWorkers)
			results[g].workerID = workerID
			results[g].nonces = make([][12]byte, nPerGoroutine)
			for i := 0; i < nPerGoroutine; i++ {
				n, err := m.NextNonce(workerID)
				if err != nil {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
				results[g].nonces[i] = n
			}
		}(g)
	}
	wg.Wait()

	// Verifica zero collisioni nell'insieme completo
	seen := make(map[[12]byte]struct{}, nGoroutines*nPerGoroutine)
	for g := 0; g < nGoroutines; g++ {
		for i, nonce := range results[g].nonces {
			if _, dup := seen[nonce]; dup {
				t.Errorf("goroutine %d idx %d: duplicate nonce %x", g, i, nonce)
			}
			seen[nonce] = struct{}{}
		}
	}
	t.Logf("verified %d unique nonces across %d goroutines", len(seen), nGoroutines)
}

// TestContextualNonceManager_AdvanceEpoch verifica il monotonicità dell'epoch (SEC-004).
func TestContextualNonceManager_AdvanceEpoch(t *testing.T) {
	m, err := crypto.NewContextualNonceManager(2)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	// Avanza da epoch 0 → 1: OK
	if err := m.AdvanceEpoch(1); err != nil {
		t.Fatalf("AdvanceEpoch(1): unexpected error: %v", err)
	}

	// Tenta di tornare a epoch 0: ERRORE atteso
	if err := m.AdvanceEpoch(0); err == nil {
		t.Error("AdvanceEpoch(0) with current=1: expected error, got nil")
	}

	// Tenta di restare a epoch 1: ERRORE atteso (non stricty greater)
	if err := m.AdvanceEpoch(1); err == nil {
		t.Error("AdvanceEpoch(1) with current=1: expected error, got nil")
	}

	// Avanza a epoch 5 (salto): OK
	if err := m.AdvanceEpoch(5); err != nil {
		t.Fatalf("AdvanceEpoch(5): unexpected error: %v", err)
	}

	// Dopo AdvanceEpoch, il primo byte del nonce deve riflettere la nuova epoch
	n, err := m.NextNonce(0)
	if err != nil {
		t.Fatalf("NextNonce after epoch advance: %v", err)
	}
	if n[0] != 5 {
		t.Errorf("nonce[0] = %d after AdvanceEpoch(5), want 5", n[0])
	}
}

// TestContextualNonceManager_EpochIsolation verifica che nonce di epoch diverse
// siano sempre distinguibili (anche se il counter è uguale dopo il reset).
func TestContextualNonceManager_EpochIsolation(t *testing.T) {
	m, err := crypto.NewContextualNonceManager(1)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	// Genera nonce in epoch 0
	n0, _ := m.NextNonce(0)
	epoch0 := n0[0]

	// Avanza a epoch 1 e genera lo stesso nonce (counter resettato a 0)
	if err := m.AdvanceEpoch(1); err != nil {
		t.Fatalf("AdvanceEpoch(1): %v", err)
	}
	n1, _ := m.NextNonce(0)
	epoch1 := n1[0]

	if epoch0 == epoch1 {
		t.Errorf("epoch bytes are equal (%d) after AdvanceEpoch — nonce isolation broken", epoch0)
	}
	// I contatori possono essere uguali (entrambi 0 dopo reset), ma epoch ≠ → nonce ≠
	if string(n0[:]) == string(n1[:]) {
		t.Error("nonces are identical across epoch boundary — isolation broken")
	}
}

// TestContextualNonceManager_InvalidWorker verifica errore per workerID fuori range.
func TestContextualNonceManager_InvalidWorker(t *testing.T) {
	m, err := crypto.NewContextualNonceManager(4)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}
	if _, err := m.NextNonce(4); err == nil {
		t.Error("NextNonce(4) with nWorkers=4: expected error, got nil")
	}
	if _, err := m.NextNonce(100); err == nil {
		t.Error("NextNonce(100): expected error, got nil")
	}
}

// TestBuildAADv1_Format verifica che BuildAADv1 produca i 24 byte corretti.
func TestBuildAADv1_Format(t *testing.T) {
	hdr := []byte{
		0x53, 0x54, 0x01, 0x01, // Magic, Ver, Type
		0x00, 0x00, 0x00, 0x01, // Session
		0x00, 0x00, 0x00, 0x2A, // GroupSeq
		0x00, 0x01, 0x00, 0xF0, // ShardIdx, GroupDataN, DataLen
	}
	seq := uint64(0xDEADBEEFCAFEBABE)
	aad := crypto.BuildAADv1(hdr, seq)

	if len(aad) != 24 {
		t.Fatalf("BuildAADv1 len = %d, want 24", len(aad))
	}
	for i, b := range hdr {
		if aad[i] != b {
			t.Errorf("aad[%d] = 0x%02x, want 0x%02x", i, aad[i], b)
		}
	}
	// Ultimi 8 byte = seq big-endian
	if aad[16] != 0xDE || aad[23] != 0xBE {
		t.Errorf("seq bytes wrong: %x", aad[16:])
	}
}

// TestBuildAADv2_Format verifica che BuildAADv2 produca i 24 byte corretti.
func TestBuildAADv2_Format(t *testing.T) {
	f := crypto.AADv2Fields{
		EpochID:      3,
		PathPipeID:   0x0102,
		TrafficClass: 0x04,
		Flags:        0x00,
		FECGroupID:   0x0506,
		SequenceNum:  0x0102030405060708,
		SessionIDLow: 0x090A0B0C0D0E0F10,
	}
	aad := crypto.BuildAADv2(f)
	if len(aad) != 24 {
		t.Fatalf("BuildAADv2 len = %d, want 24", len(aad))
	}
	if aad[0] != 0x02 {
		t.Errorf("aad[0] (version) = 0x%02x, want 0x02", aad[0])
	}
	if aad[1] != 3 {
		t.Errorf("aad[1] (epoch) = %d, want 3", aad[1])
	}
}

// TestDetectAADVersion verifica il rilevamento automatico della versione.
func TestDetectAADVersion(t *testing.T) {
	// 0x53 = high byte di Magic 0x5354 → v1
	if v := crypto.DetectAADVersion(0x53); v != crypto.AADVersionV1 {
		t.Errorf("DetectAADVersion(0x53) = %d, want 1", v)
	}
	// 0x02 = AADVersionV2
	if v := crypto.DetectAADVersion(0x02); v != crypto.AADVersionV2 {
		t.Errorf("DetectAADVersion(0x02) = %d, want 2", v)
	}
	// default → v1
	if v := crypto.DetectAADVersion(0xFF); v != crypto.AADVersionV1 {
		t.Errorf("DetectAADVersion(0xFF) = %d, want 1 (default)", v)
	}
}

// TestBuildAADv1_ShortHdr documenta che hdr < 16 byte produce zero-padding.
// In produzione non si verifica (encodeStripeHdr produce sempre 16 byte).
func TestBuildAADv1_ShortHdr(t *testing.T) {
	hdr := []byte{0x53, 0x54} // solo 2 byte
	aad := crypto.BuildAADv1(hdr, 0)
	if len(aad) != 24 {
		t.Fatalf("BuildAADv1 len = %d, want 24", len(aad))
	}
	// byte 2-15 devono essere zero (zero-padded da hdr corto)
	for i := 2; i < 16; i++ {
		if aad[i] != 0 {
			t.Errorf("aad[%d] = 0x%02x, want 0x00 (zero-padded)", i, aad[i])
		}
	}
	// byte 16-23 (seq=0) devono essere zero
	for i := 16; i < 24; i++ {
		if aad[i] != 0 {
			t.Errorf("aad[%d] = 0x%02x, want 0x00 (seq=0)", i, aad[i])
		}
	}
}
package crypto
package crypto_test

import (
	"sync"
	"testing"

	crypto "mpquic/internal/mpquic/crypto"
)

// TestContextualNonceManager_Sequential verifica che ogni worker produca nonce
// strettamente crescenti con step = nWorkers.
func TestContextualNonceManager_Sequential(t *testing.T) {
	const nWorkers = 4
	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	// Ogni worker deve produrre 100 nonce distinti e crescenti
	for w := uint(0); w < nWorkers; w++ {
		var prev [12]byte
		hasPrev := false
		for i := 0; i < 100; i++ {
			n, err := m.NextNonce(w)
			if err != nil {
				t.Fatalf("worker %d iter %d: %v", w, i, err)
			}
			if len(n) != 12 {
				t.Fatalf("worker %d: nonce len = %d, want 12", w, len(n))
			}
			if hasPrev {
				// I nonce devono essere strettamente crescenti (confronto lessicografico
				// corretto per big-endian 8B counter ai byte 4-11)
				if string(n[:]) <= string(prev[:]) {
					t.Errorf("worker %d iter %d: nonce not increasing", w, i)
				}
			}
			prev = n
			hasPrev = true
		}
	}
}

// TestContextualNonceManager_PerWorkerUnique verifica che worker diversi
// non producano mai lo stesso nonce (sequenze non sovrapposte).
func TestContextualNonceManager_PerWorkerUnique(t *testing.T) {
	const nWorkers = 4
	const nPerWorker = 500
	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	seen := make(map[[12]byte]struct{}, nWorkers*nPerWorker)
	for w := uint(0); w < nWorkers; w++ {
		for i := 0; i < nPerWorker; i++ {
			if string(n0[:]) == string(n1[:]) {
			if err != nil {
				t.Fatalf("worker %d iter %d: %v", w, i, err)
			}
			key := n
			if _, dup := seen[key]; dup {
				t.Errorf("worker %d iter %d: duplicate nonce", w, i)
			}
			seen[key] = struct{}{}
		}
	}
}

// TestContextualNonceManager_Concurrency verifica zero collisioni con 1000 goroutine
// concorrenti che generano nonce da worker casuali.
func TestContextualNonceManager_Concurrency(t *testing.T) {
	const nWorkers = 8
	const nGoroutines = 1000
	const nPerGoroutine = 100

	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	type result struct {
		workerID uint
		nonces   [][12]byte
	}

	results := make([]result, nGoroutines)
	var wg sync.WaitGroup
	wg.Add(nGoroutines)

	for g := 0; g < nGoroutines; g++ {
		go func(g int) {
			defer wg.Done()
			workerID := uint(g % nWorkers)
			results[g].workerID = workerID
			results[g].nonces = make([][12]byte, nPerGoroutine)
			for i := 0; i < nPerGoroutine; i++ {
				n, err := m.NextNonce(workerID)
				if err != nil {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
						results[g].nonces[i] = n
			}
		}(g)
	}
	wg.Wait()

	// Verifica zero collisioni nell'insieme completo
	seen := make(map[[12]byte]struct{}, nGoroutines*nPerGoroutine)
	for g := 0; g < nGoroutines; g++ {
		for i, nonce := range results[g].nonces {
			if _, dup := seen[nonce]; dup {
				t.Errorf("goroutine %d idx %d: duplicate nonce %x", g, i, nonce)
			}
			seen[nonce] = struct{}{}
		}
	}
	t.Logf("verified %d unique nonces across %d goroutines", len(seen), nGoroutines)
}

// TestContextualNonceManager_AdvanceEpoch verifica il monotonicità dell'epoch (SEC-004).
func TestContextualNonceManager_AdvanceEpoch(t *testing.T) {
	m, err := crypto.NewContextualNonceManager(2)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	// Avanza da epoch 0 → 1: OK
	if err := m.AdvanceEpoch(1); err != nil {
		t.Fatalf("AdvanceEpoch(1): unexpected error: %v", err)
	}

	// Tenta di tornare a epoch 0: ERRORE atteso
	if err := m.AdvanceEpoch(0); err == nil {
		t.Error("AdvanceEpoch(0) with current=1: expected error, got nil")
	}

	// Tenta di restare a epoch 1: ERRORE atteso (non stricty greater)
	if err := m.AdvanceEpoch(1); err == nil {
		t.Error("AdvanceEpoch(1) with current=1: expected error, got nil")
	}

	// Avanza a epoch 5 (salto): OK
	if err := m.AdvanceEpoch(5); err != nil {
		t.Fatalf("AdvanceEpoch(5): unexpected error: %v", err)
	}

	// Dopo AdvanceEpoch, il primo byte del nonce deve riflettere la nuova epoch
	n, err := m.NextNonce(0)
	if err != nil {
		t.Fatalf("NextNonce after epoch advance: %v", err)
	}
	if n[0] != 5 {
		t.Errorf("nonce[0] = %d after AdvanceEpoch(5), want 5", n[0])
	}
}

// TestContextualNonceManager_EpochIsolation verifica:
//  1) cross-epoch uniqueness: nessun nonce in epoch 1 collide con epoch 0
//  2) within-epoch uniqueness: nessun duplicato dentro lo stesso epoch
func TestContextualNonceManager_EpochIsolation(t *testing.T) {
	const nWorkers = 2
	const nPerEpoch = 200

	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	// Fase 1: raccoglie nonce in epoch 0
	epoch0 := make(map[[12]byte]struct{}, nWorkers*nPerEpoch)
	for i := 0; i < nPerEpoch; i++ {
		for w := uint(0); w < nWorkers; w++ {
			n0Arr, err := m.NextNonce(0)
			n0 := n0Arr[:]
			if err != nil {
				t.Fatalf("epoch0 worker %d iter %d: %v", w, i, err)
			}
			var key [12]byte
			copy(key[:], n)
			epoch0[key] = struct{}{}
		}
	}

	// Avanza epoch
	if err := m.AdvanceEpoch(1); err != nil {
		t.Fatalf("AdvanceEpoch(1): %v", err)
	}

	// Fase 2: raccoglie nonce in epoch 1 e verifica cross-epoch uniqueness
	epoch1 := make(map[[12]byte]struct{}, nWorkers*nPerEpoch)
	for i := 0; i < nPerEpoch; i++ {
		for w := uint(0); w < nWorkers; w++ {
			n1Arr, err := m.NextNonce(1)
			n1 := n1Arr[:]
			if err != nil {
				t.Fatalf("epoch1 worker %d iter %d: %v", w, i, err)
			}
			var key [12]byte
			copy(key[:], n)

			// Verifica cross-epoch: il nonce non deve essere in epoch0
			if _, dup := epoch0[key]; dup {
				t.Errorf("NONCE REUSE: epoch1 worker %d iter %d nonce %x già presente in epoch0", w, i, key)
			}
			// Verifica within-epoch
			if _, dup := epoch1[key]; dup {
				t.Errorf("NONCE REUSE: epoch1 worker %d iter %d nonce %x duplicato", w, i, key)
			}
			epoch1[key] = struct{}{}
		}
	}

	t.Logf("epoch0=%d nonces, epoch1=%d nonces, no cross-epoch collisions", len(epoch0), len(epoch1))
}

// TestContextualNonceManager_InvalidWorker verifica errore per workerID fuori range.
func TestContextualNonceManager_InvalidWorker(t *testing.T) {
	m, err := crypto.NewContextualNonceManager(4)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}
	if _, err := m.NextNonce(4); err == nil {
		t.Error("NextNonce(4) with nWorkers=4: expected error, got nil")
	}
	if _, err := m.NextNonce(100); err == nil {
		t.Error("NextNonce(100): expected error, got nil")
	}
}

// TestBuildAADv1_Format verifica che BuildAADv1 produca i 24 byte corretti.
func TestBuildAADv1_Format(t *testing.T) {
	hdr := []byte{
		0x53, 0x54, 0x01, 0x01, // Magic, Ver, Type
		0x00, 0x00, 0x00, 0x01, // Session
		0x00, 0x00, 0x00, 0x2A, // GroupSeq
		0x00, 0x01, 0x00, 0xF0, // ShardIdx, GroupDataN, DataLen
	}
	seq := uint64(0xDEADBEEFCAFEBABE)
	aad := crypto.BuildAADv1(hdr, seq)

	if len(aad) != 24 {
		t.Fatalf("BuildAADv1 len = %d, want 24", len(aad))
	}
	for i, b := range hdr {
		if aad[i] != b {
			t.Errorf("aad[%d] = 0x%02x, want 0x%02x", i, aad[i], b)
		}
	}
	// Ultimi 8 byte = seq big-endian
	if aad[16] != 0xDE || aad[23] != 0xBE {
		t.Errorf("seq bytes wrong: %x", aad[16:])
	}
}

// TestBuildAADv1_ShortHdr documenta che hdr < 16 byte produce zero-padding.
// In produzione non si verifica (encodeStripeHdr produce sempre 16 byte).
func TestBuildAADv1_ShortHdr(t *testing.T) {
	hdr := []byte{0x53, 0x54} // solo 2 byte
	aad := crypto.BuildAADv1(hdr, 0)
	if len(aad) != 24 {
		t.Fatalf("BuildAADv1 len = %d, want 24", len(aad))
	}
	// I byte 2-15 devono essere zero (zero-padded)
	for i := 2; i < 16; i++ {
		if aad[i] != 0 {
			t.Errorf("aad[%d] = 0x%02x, want 0x00 (zero-padded)", i, aad[i])
		}
	}
	// I byte 16-23 (seq=0) devono essere zero
	for i := 16; i < 24; i++ {
		if aad[i] != 0 {
			t.Errorf("aad[%d] = 0x%02x, want 0x00 (seq=0)", i, aad[i])
		}
	}
}
			t.Errorf("aad[%d] = 0x%02x, want 0x00 (zero-padded)", i, aad[i])
		}
	}
}

// TestBuildAADv2_Format verifica che BuildAADv2 produca i 24 byte corretti.
func TestBuildAADv2_Format(t *testing.T) {
	f := crypto.AADv2Fields{
		EpochID:      3,
		PathPipeID:   0x0102,
		TrafficClass: 0x04,
		Flags:        0x00,
		FECGroupID:   0x0506,
		SequenceNum:  0x0102030405060708,
		SessionIDLow: 0x090A0B0C0D0E0F10,
	}
	aad := crypto.BuildAADv2(f)
	if len(aad) != 24 {
		t.Fatalf("BuildAADv2 len = %d, want 24", len(aad))
	}
	if aad[0] != 0x02 {
		t.Errorf("aad[0] version = 0x%02x, want 0x02", aad[0])
	}
	if aad[1] != 3 {
		t.Errorf("aad[1] epoch = %d, want 3", aad[1])
	}
	if aad[2] != 0x01 || aad[3] != 0x02 {
		t.Errorf("path_pipe_id wrong: %02x%02x", aad[2], aad[3])
	}
	if aad[4] != 0x04 {
		t.Errorf("traffic_class = 0x%02x, want 0x04", aad[4])
	}
	if aad[5] != 0x00 {
		t.Errorf("flags = 0x%02x, want 0x00", aad[5])
	}
	if aad[6] != 0x05 || aad[7] != 0x06 {
		t.Errorf("fec_group_id wrong: %02x%02x", aad[6], aad[7])
	}
	// SequenceNum bytes 8-15
	if aad[8] != 0x01 || aad[15] != 0x08 {
		t.Errorf("seq_num wrong: %x", aad[8:16])
	}
	// SessionIDLow bytes 16-23
	if aad[16] != 0x09 || aad[23] != 0x10 {
		t.Errorf("session_id_low wrong: %x", aad[16:24])
	}
}

// TestDetectAADVersion verifica il rilevamento automatico della versione.
func TestDetectAADVersion(t *testing.T) {
	// 0x53 = high byte di Magic 0x5354 → v1
	if v := crypto.DetectAADVersion(0x53); v != crypto.AADVersionV1 {
		t.Errorf("DetectAADVersion(0x53) = %d, want 1", v)
	}
	// 0x02 = AADVersionV2
	if v := crypto.DetectAADVersion(0x02); v != crypto.AADVersionV2 {
		t.Errorf("DetectAADVersion(0x02) = %d, want 2", v)
	}
	// default → v1
	if v := crypto.DetectAADVersion(0xFF); v != crypto.AADVersionV1 {
		t.Errorf("DetectAADVersion(0xFF) = %d, want 1 (default)", v)
	}
}