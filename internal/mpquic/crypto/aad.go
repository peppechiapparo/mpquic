package crypto

import "encoding/binary"

// AADVersion identifica il formato AAD usato nella cifratura STRIPES.
// La versione è negoziata tramite il campo aad_version nella configurazione YAML
// e deve essere identica su sender e receiver.
type AADVersion uint8

const (
	// AADVersionV1 è il formato legacy: [stripeHdr 16B][seq 8B] = 24 byte.
	// Compatibile con tutte le versioni di MPQUIC dalla v1.x.
	AADVersionV1 AADVersion = 1

	// AADVersionV2 è il formato esteso con binding crittografico rafforzato.
	// Wire layout (24 byte, packed big-endian):
	//   version(1) + epoch_id(1) + path_pipe_id(2) + traffic_class(1) +
	//   flags(1) + fec_group_id(2) + sequence_number(8) + session_id_low(8)
	// Attivabile con: aad_version: 2 nel file YAML di configurazione.
	// PREREQUISITO: sia sender che receiver devono essere aggiornati a v5.0+.
	AADVersionV2 AADVersion = 2
)

// AADv2Fields contiene i campi per costruire un AAD di versione 2.
// Tutti i campi sono cleartext e devono essere identici su sender e receiver.
type AADv2Fields struct {
	EpochID      uint8  // epoch crittografica corrente (da SessionKeys.EpochID)
	PathPipeID   uint16 // identificatore del path/pipe stripe (da stripeHdr.Session lower 16b)
	TrafficClass uint8  // classe di traffico DSCP (da stripeHdr.Type & 0x0F)
	Flags        uint8  // flag di sessione (riservato, deve essere 0 in v2.0)
	FECGroupID   uint16 // ID del gruppo FEC corrente (da stripeHdr.GroupSeq lower 16b)
	SequenceNum  uint64 // numero di sequenza monotono (da seq counter)
	SessionIDLow uint64 // parte bassa del session ID a 64 bit (da stripeHdr.Session)
}

// BuildAADv1 costruisce il buffer AAD in formato v1 (24 byte).
// PRECONDIZIONE: len(hdr) == 16 (stripeHdr encodificato). Se len(hdr) < 16,
// i byte mancanti vengono zero-padded silenziosamente — l'autenticazione GCM
// fallirà sul receiver. Chiamato sempre con hdr da encodeStripeHdr (16B esatti).
func BuildAADv1(hdr []byte, seq uint64) [24]byte {
	var aad [24]byte
	if len(hdr) != 16 {
		debugPanicf("crypto: BuildAADv1 precondition violated: len(hdr)=%d, want 16", len(hdr))
	}
	copy(aad[:16], hdr) // safe: copy gestisce hdr corto (zero-pad)
	binary.BigEndian.PutUint64(aad[16:], seq)
	return aad
}

// BuildAADv2 costruisce il buffer AAD in formato v2 (24 byte).
// Il primo byte è sempre 0x02 (AADVersionV2), distinguibile da v1 dove
// il primo byte è 0x53 (high byte di stripeMagic 0x5354).
//
// Layout: [version:1][epoch_id:1][path_pipe_id:2][traffic_class:1][flags:1]
//
//	[fec_group_id:2][sequence_number:8][session_id_low:8]
func BuildAADv2(f AADv2Fields) [24]byte {
	var aad [24]byte
	aad[0] = byte(AADVersionV2)
	aad[1] = f.EpochID
	binary.BigEndian.PutUint16(aad[2:4], f.PathPipeID)
	aad[4] = f.TrafficClass
	aad[5] = f.Flags
	binary.BigEndian.PutUint16(aad[6:8], f.FECGroupID)
	binary.BigEndian.PutUint64(aad[8:16], f.SequenceNum)
	binary.BigEndian.PutUint64(aad[16:24], f.SessionIDLow)
	return aad
}

// DetectAADVersion determina la versione AAD dal primo byte del pacchetto wire.
// In v1, il primo byte è 0x53 (high byte di Magic 0x5354).
// In v2, il primo byte è 0x02 (AADVersionV2).
// Restituisce AADVersionV1 per default (backward compat) se il byte è sconosciuto.
func DetectAADVersion(firstByte byte) AADVersion {
	switch firstByte {
	case 0x53: // high byte di stripeMagic (0x5354 "ST") → v1 legacy
		return AADVersionV1
	case byte(AADVersionV2):
		return AADVersionV2
	default:
		return AADVersionV1
	}
}
