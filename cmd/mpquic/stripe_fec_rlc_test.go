package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func rlcTestShard(payload string) []byte {
	buf := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(payload)))
	copy(buf[2:], []byte(payload))
	return buf
}

func TestRLCRecoversSingleMissingShard(t *testing.T) {
	tx := newRLCFECSender(4)
	tx.setStride(1)
	rx := newRLCFECReceiver(4)
	var repair1, repair2 []byte
	var first1, first2 uint32
	var count1, count2 int

	for i, payload := range []string{"pkt-1", "pkt-2", "pkt-3", "pkt-4", "pkt-5"} {
		repair, firstSeq, count, ok := tx.addSource(uint32(i+1), rlcTestShard(payload))
		if i == 3 || i == 4 {
			if !ok {
				t.Fatalf("expected repair at step %d", i)
			}
			if i == 3 {
				repair1, first1, count1 = repair, firstSeq, count
			} else {
				repair2, first2, count2 = repair, firstSeq, count
			}
		}
	}

	for seq, payload := range map[uint32]string{1: "pkt-1", 2: "pkt-2", 4: "pkt-4", 5: "pkt-5"} {
		rx.storeShard(seq, rlcTestShard(payload))
	}
	got := rx.addRepair(first1, count1, repair1)
	if len(got) != 1 {
		t.Fatalf("expected one recovered packet, got %d", len(got))
	}
	if got[0].seq != 3 {
		t.Fatalf("expected seq 3, got %d", got[0].seq)
	}
	if !bytes.Equal(got[0].pkt, []byte("pkt-3")) {
		t.Fatalf("unexpected payload %q", string(got[0].pkt))
	}
	if got2 := rx.addRepair(first2, count2, repair2); len(got2) != 0 {
		t.Fatalf("unexpected extra recovery after second repair: %d", len(got2))
	}
}

func TestRLCRecoversTwoMissingShardsWithTwoEquations(t *testing.T) {
	tx := newRLCFECSender(3)
	tx.setStride(1)
	rx := newRLCFECReceiver(3)

	type repairInfo struct {
		repair []byte
		first  uint32
		count  int
	}
	var repairs []repairInfo
	for i, payload := range []string{"a", "b", "c", "d"} {
		repair, firstSeq, count, ok := tx.addSource(uint32(i+1), rlcTestShard(payload))
		if ok {
			repairs = append(repairs, repairInfo{repair: repair, first: firstSeq, count: count})
		}
	}
	if len(repairs) < 2 {
		t.Fatalf("expected at least two repairs, got %d", len(repairs))
	}

	rx.storeShard(1, rlcTestShard("a"))
	rx.storeShard(4, rlcTestShard("d"))
	if got := rx.addRepair(repairs[0].first, repairs[0].count, repairs[0].repair); len(got) != 0 {
		t.Fatalf("unexpected recovery after first equation: %d", len(got))
	}
	got := rx.addRepair(repairs[1].first, repairs[1].count, repairs[1].repair)
	if len(got) != 2 {
		t.Fatalf("expected two recovered packets, got %d", len(got))
	}
	seen := map[uint32]string{}
	for _, pkt := range got {
		seen[pkt.seq] = string(pkt.pkt)
	}
	if seen[2] != "b" || seen[3] != "c" {
		t.Fatalf("unexpected recovered set: %#v", seen)
	}
}