package crypto_test

import (
	"sync"
	"testing"

	"mpquic/internal/mpquic/crypto"
)

// TestContextualNonceManager_Sequential verifica che i nonce siano crescenti
// e unici per ciascun worker (strategia step=nWorkers).
func TestContextualNonceManager_Sequential(t *testing.T) {
	const nWorkers = 4
	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	// worker 0: 0, 4, 8, ...  — counter inizia a 0
	for i := 0; i < 5; i++ {
		n, err := m.NextNonce(0)
		if err != nil {
			t.Fatalf("worker 0 iter %d: %v", i, err)
		}
		want := uint64(i) * nWorkers
		// Gli ultimi 8 byte (big-endian) devono codificare want
		got := uint64(n[4])<<56 | uint64(n[5])<<48 | uint64(n[6])<<40 | uint64(n[7])<<32 |
			uint64(n[8])<<24 | uint64(n[9])<<16 | uint64(n[10])<<8 | uint64(n[11])
		if got != want {
			t.Errorf("worker 0 iter %d: seq=%d, want=%d", i, got, want)
		}
	}

	// worker 1: 1, 5, 9, ...
	for i := 0; i < 5; i++ {
		n, err := m.NextNonce(1)
		if err != nil {
			t.Fatalf("worker 1 iter %d: %v", i, err)
		}
		want := uint64(1) + uint64(i)*nWorkers
		got := uint64(n[4])<<56 | uint64(n[5])<<48 | uint64(n[6])<<40 | uint64(n[7])<<32 |
			uint64(n[8])<<24 | uint64(n[9])<<16 | uint64(n[10])<<8 | uint64(n[11])
		if got != want {
			t.Errorf("worker 1 iter %d: seq=%d, want=%d", i, got, want)
		}
	}
}

// TestContextualNonceManager_PerWorkerUnique verifica che due worker non
// producano mai lo stesso nonce (spazi disgiunti).
func TestContextualNonceManager_PerWorkerUnique(t *testing.T) {
	const nWorkers = 2
	const iters = 1000
	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	seen0 := make(map[[12]byte]struct{}, iters)
	seen1 := make(map[[12]byte]struct{}, iters)
	for i := 0; i < iters; i++ {
		n0, _ := m.NextNonce(0)
		n1, _ := m.NextNonce(1)
		seen0[n0] = struct{}{}
		seen1[n1] = struct{}{}
		if n0 == n1 {
			t.Fatalf("nonce collision between worker 0 and 1 at iter %d: %x", i, n0)
		}
	}
	if len(seen0) != iters {
		t.Errorf("worker 0: got %d unique nonces, want %d", len(seen0), iters)
	}
	if len(seen1) != iters {
		t.Errorf("worker 1: got %d unique nonces, want %d", len(seen1), iters)
	}
}

// TestContextualNonceManager_Concurrency verifica unicità in contesto concorrente.
func TestContextualNonceManager_Concurrency(t *testing.T) {
	const nWorkers = 8
	const goroutines = 1000
	const noncePerGoroutine = 100

	m, err := crypto.NewContextualNonceManager(nWorkers)
	if err != nil {
		t.Fatalf("NewContextualNonceManager: %v", err)
	}

	var mu sync.Mutex
	seen := make(map[[12]byte]struct{}, goroutines*noncePerGoroutine)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(gid int) {
			defer wg.Done()
			workerID := uint(gid % nWorkers)
			for j := 0; j < noncePerGoroutine; j++ {
				n, err := m.NextNonce(workerID)
				if err != nil {
					t.Errorf("goroutine %d NextNonce: %v", gid, err)
					return
				}
				mu.Lock()
				if _, dup := seen[n]; dup {
					t.Errorf("NONCE REUSE: goroutine %d, nonce %x", gid, n)
				}
				seen[n] = struct{}{}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	t.Logf("total unique nonces generated: %d", len(seen))
}

// TestContextualNonceManager_AdvanceEpoch verifica la monotonicità dell'epoch.
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

	// Tenta di restare a epoch 1: ERRORE atteso (non strettamente maggiore)
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
//  1. cross-epoch uniqueness: nessun nonce in epoch 1 collide con epoch 0
//  2. within-epoch uniqueness: nessun duplicato dentro lo stesso epoch
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
			n, err := m.NextNonce(w)
			if err != nil {
				t.Fatalf("epoch0 worker %d iter %d: %v", w, i, err)
			}
			epoch0[n] = struct{}{}
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
			n, err := m.NextNonce(w)
			if err != nil {
				t.Fatalf("epoch1 worker %d iter %d: %v", w, i, err)
			}

			// Verifica cross-epoch: il nonce non deve essere in epoch0
			if _, dup := epoch0[n]; dup {
				t.Errorf("NONCE REUSE: epoch1 worker %d iter %d nonce %x già presente in epoch0", w, i, n)
			}
			// Verifica within-epoch
			if _, dup := epoch1[n]; dup {
				t.Errorf("NONCE REUSE: epoch1 worker %d iter %d nonce %x duplicato", w, i, n)
			}
			epoch1[n] = struct{}{}
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
