package main

import "encoding/binary"

// flowHash calcola un FNV-1a a 32 bit del 5-tuple del pacchetto IP interno
// (src, dst, proto, sport, dport). Ritorna ok=false quando il pacchetto non e'
// IPv4/IPv6 parsabile: in quel caso il chiamante ricade sul round-robin.
// Per i frammenti non-primi (niente porte) usa il 3-tuple: tutti i frammenti
// di un flusso restano comunque sulla stessa pipe.
//
// Scopo (TS-023/TS-024): con lo striping round-robin ogni flusso viene
// spalmato su tutte le pipe e arriva riordinato (~100% out-of-order misurato),
// TCP lo legge come loss e l'ARQ tempesta. Inchiodare ogni flusso a UNA pipe
// preserva l'ordine end-to-end senza toccare il protocollo wire.
func innerFlowHash(pkt []byte) (uint32, bool) {
	const prime = 16777619
	h := uint32(2166136261)
	mix := func(bs []byte) {
		for _, b := range bs {
			h ^= uint32(b)
			h *= prime
		}
	}
	if len(pkt) < 20 {
		return 0, false
	}
	switch pkt[0] >> 4 {
	case 4:
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl {
			return 0, false
		}
		proto := pkt[9]
		mix(pkt[12:20])
		h ^= uint32(proto)
		h *= prime
		fragOff := binary.BigEndian.Uint16(pkt[6:8]) & 0x1fff
		if fragOff == 0 && (proto == 6 || proto == 17) && len(pkt) >= ihl+4 {
			mix(pkt[ihl : ihl+4])
		}
		return h, true
	case 6:
		if len(pkt) < 40 {
			return 0, false
		}
		next := pkt[6]
		mix(pkt[8:40])
		h ^= uint32(next)
		h *= prime
		if (next == 6 || next == 17) && len(pkt) >= 44 {
			mix(pkt[40:44])
		}
		return h, true
	}
	return 0, false
}
