package main

// pipe_health_test.go — flow-affinity con maschera di pipe sane (TS-031):
// un flusso non deve mai essere hashato su una pipe fuori maschera, la
// scelta deve essere deterministica per hash, e la maschera vuota deve
// degradare a "tutte candidabili" (mai azzerare il TX).

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func newAffinityConn(nPipes int) *stripeClientConn {
	return &stripeClientConn{
		flowAffinity:      true,
		pipes:             make([]*net.UDPConn, nPipes),
		pipeLastRx:        make([]int64, nPipes),
		keepaliveInterval: time.Second,
	}
}

// pacchetto IPv4+TCP minimo con porte fissate, per innerFlowHash
func fakeTCPPacket(srcPort, dstPort byte) []byte {
	pkt := make([]byte, 24)
	pkt[0] = 0x45
	pkt[9] = 6 // TCP
	pkt[21] = srcPort
	pkt[23] = dstPort
	return pkt
}

func TestDataPipeIdxRespectsHealthMask(t *testing.T) {
	scc := newAffinityConn(4)
	atomic.StoreUint32(&scc.pipeHealthyMask, 0b1010) // sane solo 1 e 3

	for p := byte(0); p < 64; p++ {
		idx := scc.dataPipeIdx(fakeTCPPacket(p, p+1))
		if idx != 1 && idx != 3 {
			t.Fatalf("porta %d: idx = %d, fuori dalla maschera {1,3}", p, idx)
		}
	}
}

func TestDataPipeIdxDeterministicPerFlow(t *testing.T) {
	scc := newAffinityConn(4)
	atomic.StoreUint32(&scc.pipeHealthyMask, 0b1111)

	pkt := fakeTCPPacket(10, 20)
	first := scc.dataPipeIdx(pkt)
	for i := 0; i < 20; i++ {
		if got := scc.dataPipeIdx(pkt); got != first {
			t.Fatalf("stesso flusso su pipe diverse: %d poi %d", first, got)
		}
	}
}

func TestRefreshPipeHealthMask(t *testing.T) {
	scc := newAffinityConn(3)
	now := time.Now().UnixNano()
	atomic.StoreInt64(&scc.pipeLastRx[0], now)
	atomic.StoreInt64(&scc.pipeLastRx[1], now-int64(10*time.Second)) // marcia
	atomic.StoreInt64(&scc.pipeLastRx[2], now)

	scc.refreshPipeHealthMask()
	if mask := atomic.LoadUint32(&scc.pipeHealthyMask); mask != 0b101 {
		t.Fatalf("mask = %03b, want 101", mask)
	}

	// Tutte marce → maschera piena (mai azzerare il TX)
	for i := range scc.pipeLastRx {
		atomic.StoreInt64(&scc.pipeLastRx[i], now-int64(time.Minute))
	}
	scc.refreshPipeHealthMask()
	if mask := atomic.LoadUint32(&scc.pipeHealthyMask); mask != 0b111 {
		t.Fatalf("mask con tutte marce = %03b, want 111 (degradazione, non blackout)", mask)
	}
}
