package main

// edt_pacing_test.go — invarianti del pacing EDT (review trasporto TS-031):
// [I1] il debito EDT non supera mai l'orizzonte; [I2] anche i pacchetti
// esenti addebitano byte al budget; [I3] l'esenzione vale solo per ACK puri.

import (
	"sync/atomic"
	"testing"
)

func newPacedConn(rateMbit int) *stripeClientConn {
	scc := &stripeClientConn{txtimeEnabled: true}
	scc.txtimeGapNs = int64(float64(stripePacedRefBytes*8) / (float64(rateMbit) * 1e6) * 1e9)
	return scc
}

func TestEDTDebtNeverExceedsHorizon(t *testing.T) {
	scc := newPacedConn(2) // gap enorme: 5.6ms per shard pieno
	for i := 0; i < 5000; i++ {
		_ = scc.txtimeNextEDT(0, stripePacedRefBytes)
	}
	debt := scc.txtimeEDT - monoNowNs()
	if debt > stripeEDTHorizonNs {
		t.Fatalf("debito EDT %d ns oltre l'orizzonte %d", debt, stripeEDTHorizonNs)
	}
	if atomic.LoadUint64(&scc.edtClamped) == 0 {
		t.Fatal("con offerta >> rate il clamp deve scattare")
	}
}

func TestEDTChargeAdvancesBudget(t *testing.T) {
	scc := newPacedConn(100)
	before := scc.txtimeNextEDT(0, stripePacedRefBytes)
	scc.txtimeChargeLocked(stripePacedRefBytes)
	after := scc.txtimeNextEDT(0, stripePacedRefBytes)
	if after <= before {
		t.Fatal("i byte esenti devono avanzare il budget EDT")
	}
	if atomic.LoadUint64(&scc.pacedExemptByte) != stripePacedRefBytes {
		t.Fatal("contatore byte esenti non aggiornato")
	}
}

func tcpPkt(payload int, flags byte) []byte {
	ihl, dataOff := 20, 20
	pkt := make([]byte, ihl+dataOff+payload)
	pkt[0] = 0x45
	total := len(pkt)
	pkt[2], pkt[3] = byte(total>>8), byte(total)
	pkt[9] = 6
	pkt[ihl+12] = byte(dataOff/4) << 4
	pkt[ihl+13] = flags
	return pkt
}

func TestIsPureAck(t *testing.T) {
	cases := []struct {
		name string
		pkt  []byte
		want bool
	}{
		{"ack puro", tcpPkt(0, 0x10), true},
		{"ack con payload", tcpPkt(100, 0x10), false},
		{"syn", tcpPkt(0, 0x02), false},
		{"fin", tcpPkt(0, 0x11), false},
		{"rst", tcpPkt(0, 0x04), false},
		{"udp", func() []byte { p := tcpPkt(0, 0x10); p[9] = 17; return p }(), false},
		{"troppo corto", make([]byte, 20), false},
	}
	for _, c := range cases {
		if got := isPureAck(c.pkt); got != c.want {
			t.Errorf("%s: isPureAck = %v, want %v", c.name, got, c.want)
		}
	}
}
