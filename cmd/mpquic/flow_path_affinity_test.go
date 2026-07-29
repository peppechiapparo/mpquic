package main

// flow_path_affinity_test.go — path-stickiness per flusso in selectBestPath.
//
// A parità di punteggio tra più path, lo stesso flowHash deve restituire
// sempre lo stesso path (niente alternanza per-pacchetto), hash diversi
// devono poter distribuire su path diversi, e senza hash (flowOK=false)
// resta il comportamento round-robin storico.

import (
	"sync/atomic"
	"testing"
)

func TestSelectBestPathFlowStickySameHash(t *testing.T) {
	p0 := newPathState("wan5", 1)
	p1 := newPathState("wan6", 1)
	m := newTestMultipathConn(t, []*multipathPathState{p0, p1})

	const hash = uint32(0xdeadbeef)
	first, _ := m.selectBestPath(DataplaneClassPolicy{}, nil, hash, true)
	for i := 0; i < 50; i++ {
		idx, conn := m.selectBestPath(DataplaneClassPolicy{}, nil, hash, true)
		if idx != first {
			t.Fatalf("iterazione %d: idx = %d, want %d (stesso hash → stesso path)", i, idx, first)
		}
		if conn == nil {
			t.Fatal("conn = nil, want non-nil")
		}
	}
}

func TestSelectBestPathFlowHashDistributes(t *testing.T) {
	p0 := newPathState("wan5", 1)
	p1 := newPathState("wan6", 1)
	m := newTestMultipathConn(t, []*multipathPathState{p0, p1})

	seen := map[int]bool{}
	for h := uint32(0); h < 16; h++ {
		idx, _ := m.selectBestPath(DataplaneClassPolicy{}, nil, h, true)
		seen[idx] = true
	}
	if len(seen) < 2 {
		t.Fatalf("16 hash diversi finiti tutti su un solo path: %v", seen)
	}
}

func TestSelectBestPathFlowStickySkipsDegraded(t *testing.T) {
	p0 := newPathState("wan5", 1)
	p1 := newPathState("wan6", 1)
	m := newTestMultipathConn(t, []*multipathPathState{p0, p1})

	// Trova un hash che oggi mappa su p0, poi degrada p0: il flusso deve
	// re-mappare sull'unico candidato sano invece di morire sul path malato.
	var hash uint32
	for h := uint32(0); h < 64; h++ {
		if idx, _ := m.selectBestPath(DataplaneClassPolicy{}, nil, h, true); idx == 0 {
			hash = h
			break
		}
	}
	atomic.StoreUint32(&p0.degraded, 1)
	idx, _ := m.selectBestPath(DataplaneClassPolicy{}, nil, hash, true)
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (p0 degradato → rehash sul sano)", idx)
	}
}

func TestSelectBestPathNoHashKeepsRoundRobin(t *testing.T) {
	p0 := newPathState("wan5", 1)
	p1 := newPathState("wan6", 1)
	m := newTestMultipathConn(t, []*multipathPathState{p0, p1})

	a, _ := m.selectBestPath(DataplaneClassPolicy{}, nil, 0, false)
	b, _ := m.selectBestPath(DataplaneClassPolicy{}, nil, 0, false)
	if a == b {
		t.Fatalf("senza hash i pacchetti devono alternare i path a pari score: %d, %d", a, b)
	}
}

func TestSelectBestPathPriorityBeatsHash(t *testing.T) {
	p0 := newPathState("wan5", 2) // riserva
	p1 := newPathState("wan6", 1) // primario
	m := newTestMultipathConn(t, []*multipathPathState{p0, p1})

	for h := uint32(0); h < 16; h++ {
		idx, _ := m.selectBestPath(DataplaneClassPolicy{}, nil, h, true)
		if idx != 1 {
			t.Fatalf("hash %d: idx = %d, want 1 (la priorità vince sull'hash)", h, idx)
		}
	}
}
