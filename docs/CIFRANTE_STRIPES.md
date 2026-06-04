# CIFRANTE STRIPES — Specifica di Integrazione per Fornitore Esterno

**Progetto:** MPQUIC/STRIPES — Telespazio  
**Documento:** Partner Integration Specification  
**Versione:** 1.0 — 2026-06-04  
**Stato:** Released

---

## Indice

1. [Scopo e applicabilità](#1-scopo-e-applicabilità)
2. [Livelli di integrazione](#2-livelli-di-integrazione)
3. [Requisiti comuni a tutti i livelli](#3-requisiti-comuni-a-tutti-i-livelli)
4. [Interfacce Go — Livello A (AEAD only)](#4-interfacce-go--livello-a-aead-only)
5. [Interfacce Go — Livello B (KEX only)](#5-interfacce-go--livello-b-kex-only)
6. [Interfacce Go — Livello C (Full provider)](#6-interfacce-go--livello-c-full-provider)
7. [Formato AAD esteso (v2)](#7-formato-aad-esteso-v2)
8. [Profilo YAML custom_provider](#8-profilo-yaml-custom_provider)
9. [Compilazione e consegna del plugin](#9-compilazione-e-consegna-del-plugin)
10. [Checklist di consegna](#10-checklist-di-consegna)

---

## 1. Scopo e applicabilità

Il sistema MPQUIC/STRIPES espone una **Crypto Abstraction Layer** (CAL) che consente a un fornitore esterno di sostituire il cifrante interno (X25519 + ML-KEM-768 + AES-256-GCM) con una propria implementazione certificata, senza modificare il codice del data plane.

Il presente documento descrive le **interfacce Go obbligatorie**, il **formato AAD**, la **configurazione YAML**, le **modalità di compilazione** del plugin e i **requisiti di sicurezza** che il fornitore deve rispettare.

---

## 2. Livelli di integrazione

Il fornitore sceglie uno dei tre livelli di integrazione in base a cosa la propria soluzione certificata fornisce:

| Livello | Cosa fornisce il fornitore | STRIPES gestisce |
|---------|--------------------------|-----------------|
| **A — AEAD only** | `AEADProvider`: `Seal` e `Open` | KEX, KDF, nonce, sessione, epoch, AAD |
| **B — KEX only** | `KeyExchangeProvider`: `GenerateKeyPair` e `DeriveSessionKeys` | AEAD, AAD, nonce, sessione, pacchetti |
| **C — Full provider** | `ExternalCryptoAdapter` completo (KEX + AEAD + lifecycle) | Solo orchestrazione |

**Guida alla scelta:**

- Il fornitore ha un **cifrante simmetrico certificato** (es. HAIPE, NSA Suite B) ma non gestisce il KEX → **Livello A**
- Il fornitore ha un **algoritmo di key agreement certificato** (es. modulare post-quantum) ma si appoggia ad AES-GCM interno → **Livello B**
- Il fornitore fornisce una **suite criptografica completa** con lifecycle proprio → **Livello C**

---

## 3. Requisiti comuni a tutti i livelli

### 3.1 Requisiti obbligatori

1. **Thread safety**: tutti i metodi pubblici devono essere goroutine-safe.
2. **No I/O in hot path**: `Seal` / `Open` / `NewAEAD` non devono eseguire I/O durante il processing di un pacchetto.
3. **No key logging**: vietato scrivere chiavi, nonce, shared secret o materiale derivato in log/stdout/stderr/file.
4. **Gestione errori non-panic**: qualsiasi errore deve essere comunicato tramite `error`; `panic()` in produzione è proibito.
5. **Test vectors**: il fornitore deve consegnare test vectors verificabili (vedi §10).
6. **Cross-compilation**: il plugin deve compilare su `linux/amd64` e `linux/arm64`.
7. **Init idempotente**: `Init(configPath)` deve essere chiamabile una sola volta per istanza.

### 3.2 Comportamenti proibiti in hot path

| Comportamento | Motivo |
|--------------|--------|
| Allocazione heap (`make`, `new`, letterali slice/map) | Viola zero-alloc invariant del data plane |
| Lock globale (mutex non bounded) | Contention su percorso multi-goroutine |
| Syscall bloccante (read/write socket, file) | Latenza inaccettabile per pacchetti live |
| Log a qualsiasi livello | Key logging risk + latenza |
| `panic()` | Crash dell'intero processo STRIPES |

---

## 4. Interfacce Go — Livello A (AEAD only)

### 4.1 Interfaccia `AEADProvider`

```go
import "crypto/cipher"

// AEADProvider astrae un cifrante AEAD.
// Usato nel hot path per ogni pacchetto UDP.
// Le implementazioni DEVONO essere thread-safe e allocazione-minima.
type AEADProvider interface {
    // Name restituisce il nome dell'algoritmo, es. "VendorCipher-256-GCM".
    Name() string

    // NewAEAD crea un'istanza del cifrante AEAD per la chiave fornita.
    // key ha lunghezza uguale a KeySize().
    // Chiamato una sola volta per sessione/epoch.
    NewAEAD(key []byte) (cipher.AEAD, error)

    // KeySize restituisce la dimensione della chiave richiesta in byte.
    KeySize() int

    // NonceSize restituisce la dimensione del nonce in byte.
    // STRIPES usa 12 byte (GCM-standard).
    NonceSize() int
}
```

### 4.2 Nota sul nonce

STRIPES gestisce il nonce autonomamente:

```
nonce[12B]:
  byte[0]    = epoch_id (uint8)
  byte[1:12] = contatore monotono uint64 big-endian
```

Il fornitore Livello A non deve modificare la gestione del nonce.

---

## 5. Interfacce Go — Livello B (KEX only)

### 5.1 Interfaccia `KeyExchangeProvider`

```go
// KeyExchangeProvider astrae la logica di key exchange (classico o post-quantum).
// Usato una sola volta per sessione (handshake) e ad ogni rekey.
type KeyExchangeProvider interface {
    // Name restituisce il nome del provider, es. "VendorKEX-PQC-L3".
    Name() string

    // GenerateKeyPair genera una coppia di chiavi pubblica/privata per il KEX.
    GenerateKeyPair() (publicKey, privateKey []byte, err error)

    // DeriveSessionKeys calcola le SessionKeys a partire dal secret QUIC
    // e dalle chiavi pubbliche dei due peer.
    //
    // quicSecret: output di QUIC TLS Exporter (64 byte)
    //             derivato come: QUIC TLS Exporter("mpquic-stripe-v1", sessionID, 64)
    // localPrivKey: chiave privata locale
    // remotePubKey: chiave pubblica del peer
    // sessionID: identificatore univoco di sessione
    //
    // Output: SessionKeys (layout fisso: vedi struct sotto)
    DeriveSessionKeys(quicSecret, localPrivKey, remotePubKey []byte, sessionID []byte) (*SessionKeys, error)
}

// SessionKeys contiene le chiavi operative per una sessione.
// Il layout (88 byte totali) è OBBLIGATORIO indipendentemente dall'algoritmo KEX.
type SessionKeys struct {
    ClientKey []byte // client→server: chiave simmetrica (32 byte)
    ServerKey []byte // server→client: chiave simmetrica (32 byte)
    ClientIV  []byte // client→server: base IV (12 byte)
    ServerIV  []byte // server→client: base IV (12 byte)
    EpochID   uint8  // epoch corrente (propagato, non derivato dal fornitore)
}
```

### 5.2 Sub-interfaccia `KemProvider` (per KEX post-quantum asimmetrico)

```go
// KemProvider estende KeyExchangeProvider per algoritmi KEM (es. ML-KEM, Kyber).
// I provider DH classici implementano solo KeyExchangeProvider.
type KemProvider interface {
    KeyExchangeProvider

    // ClientEncapsulate prepara il materiale per il lato client del KEX KEM.
    //
    // serverPubKey: chiave pubblica del server
    //
    // Returns:
    //   localPrivKey: materiale privato del client (usato in DeriveSessionKeys)
    //   peerKeyShare: materiale da inviare al server (ciphertext + pub client)
    ClientEncapsulate(serverPubKey []byte) (localPrivKey, peerKeyShare []byte, err error)
}
```

**Utilizzo lato STRIPES:**

```go
if kp, ok := provider.(KemProvider); ok {
    localPrivKey, peerKeyShare, err := kp.ClientEncapsulate(serverPubKey)
    // trasmetti peerKeyShare al server via handshake out-of-band
    keys, err := kp.DeriveSessionKeys(quicSecret, localPrivKey, serverX25519Pub, sessionID)
}
```

### 5.3 Layout output `DeriveSessionKeys`

Il layout di `SessionKeys` è **obbligatorio** a prescindere dall'algoritmo KEX:

| Campo | Offset | Dimensione | Note |
|-------|--------|-----------|------|
| `ClientKey` | 0 | 32 B | Chiave AES-256 o equivalente client→server |
| `ServerKey` | 32 | 32 B | Chiave AES-256 o equivalente server→client |
| `ClientIV` | 64 | 12 B | Base IV client→server |
| `ServerIV` | 76 | 12 B | Base IV server→client |
| Totale | — | 88 B | Sliciato fisso da STRIPES |

---

## 6. Interfacce Go — Livello C (Full provider)

### 6.1 Interfaccia `ExternalCryptoAdapter`

Questa interfaccia è il punto di ingresso del plugin. STRIPES carica il `.so` e cerca il simbolo `CryptoProvider` di questo tipo.

```go
// ExternalCryptoAdapter è l'interfaccia che il plugin Go del fornitore deve
// implementare ed esportare come simbolo "CryptoProvider".
//
// Caricamento:
//   p, err := plugin.Open("/path/to/vendor_crypto.so")
//   sym, err := p.Lookup("CryptoProvider")
//   adapter := sym.(ExternalCryptoAdapter)
type ExternalCryptoAdapter interface {
    // Init inizializza il provider con il path al file di configurazione.
    // Chiamato una sola volta prima di qualsiasi altro metodo.
    Init(configPath string) error

    // Name restituisce il nome del provider (stringa libera, max 64 char).
    Name() string

    // Version restituisce la versione del provider (es. "1.2.3").
    Version() string

    // KeyExchangeProvider restituisce l'implementazione KEX.
    // Restituisce nil se il provider gestisce solo AEAD (Livello A).
    KeyExchangeProvider() KeyExchangeProvider

    // AEADProvider restituisce l'implementazione AEAD.
    // Restituisce nil se il provider gestisce solo KEX (Livello B).
    AEADProvider() AEADProvider

    // Close rilascia le risorse e azera (zeroize) le chiavi in memoria.
    Close() error
}
```

### 6.2 Simbolo esportato obbligatorio

```go
// In vendor_crypto/main.go

package main

// CryptoProvider è il simbolo cercato da STRIPES tramite plugin.Lookup.
// Deve essere una variabile esportata del tipo ExternalCryptoAdapter.
var CryptoProvider ExternalCryptoAdapter = &MyVendorProvider{}
```

> **Nota**: il fornitore deve copiare le definizioni delle interfacce nel proprio package (le interfacce STRIPES sono in `internal/`, non importabili). La compatibilità è garantita dalla struttura Go (duck typing: se i metodi coincidono, la type assertion funziona).

---

## 7. Formato AAD esteso (v2)

L'Additional Authenticated Data (AAD) ha una struttura packed di **24 byte** che il fornitore Livello A o C deve includere nell'autenticazione AEAD.

### 7.1 Schema

```
Offset  Size  Field
──────  ────  ─────────────────────────────────────────────────────
 0       1B   version          0x02 (provider esterno riceve sempre v2)
 1       1B   epoch_id         contatore rekey (uint8, 0-255)
 2       2B   path_pipe_id     path_id[7:0] | pipe_id[7:0] (big-endian)
 4       1B   traffic_class    QoS: 0=best-effort, 1=critical, 2=bulk
 5       1B   flags            bit0=FEC, bit1=direction(0=c2s,1=s2c),
                               bit2=rekey_in_progress
 6       2B   fec_group_id     ID gruppo FEC (big-endian uint16; 0 se no FEC)
 8       8B   sequence_number  contatore monotono uint64, big-endian
16       8B   session_id_low   64 bit meno significativi del session ID
──────  ────  ─────────────────────────────────────────────────────
Totale  24B
```

### 7.2 Utilizzo nell'autenticazione

L'intero AAD di 24 byte viene passato come `additionalData` a `cipher.AEAD.Seal` e `cipher.AEAD.Open`. STRIPES verifica il campo `version` prima di chiamare il fornitore; il fornitore non deve filtrare per versione.

---

## 8. Profilo YAML `custom_provider`

Per attivare il plugin del fornitore, la configurazione dell'istanza STRIPES deve contenere:

```yaml
stripe_crypto_enabled: true

crypto:
  enabled: true
  profile: custom_provider

  custom_provider:
    path: /opt/mpquic/plugins/crypto/vendor_crypto.so
    config_file: /etc/mpquic/crypto/vendor_config.yaml

  rekey:
    enabled: false
```

### 8.1 Campi rilevanti

| Campo YAML | Tipo | Descrizione |
|-----------|------|-------------|
| `custom_provider.path` | string | Path assoluto al file `.so` del plugin |
| `custom_provider.config_file` | string | Path passato a `ExternalCryptoAdapter.Init(configPath)` |

Il file `vendor_config.yaml` è opaco per STRIPES — il suo contenuto è definito dal fornitore.

---

## 9. Compilazione e consegna del plugin

### 9.1 Compilazione

```bash
# Stessa versione del toolchain Go del sistema STRIPES (1.26+)

go build \
  -buildmode=plugin \
  -o vendor_crypto.so \
  ./vendor_crypto/

# Per linux/arm64
GOOS=linux GOARCH=arm64 go build \
  -buildmode=plugin \
  -o vendor_crypto_arm64.so \
  ./vendor_crypto/
```

> **Importante**: Il plugin Go e il binario STRIPES devono essere compilati con lo stesso toolchain Go e le stesse dipendenze condivise (stesso build ID). Telespazio fornirà le specifiche esatte del toolchain al momento della consegna.

### 9.2 Struttura minima del package plugin

```
vendor_crypto/
├── main.go          # package main; esporta var CryptoProvider
├── provider.go      # struct MyVendorProvider; implementa ExternalCryptoAdapter
├── aead.go          # implementazione AEADProvider (Livello A o C)
├── kex.go           # implementazione KeyExchangeProvider (Livello B o C)
├── config.go        # lettura vendor_config.yaml
└── go.mod           # modulo Go separato, versione Go >= 1.26
```

### 9.3 Dipendenze consentite

| Tipo | Consentito |
|------|-----------|
| Standard library Go | ✅ Tutti i package |
| Librerie crittografiche certificate (FIPS 140-3) | ✅ |
| Dipendenze proprie del fornitore (in vendor/) | ✅ |
| Package `github.com/telespazio/mpquic/...` o internal STRIPES | ❌ Non importabili |
| Librerie che avviano goroutine background non controllate | ❌ |

---

## 10. Checklist di consegna

| Artefatto | Obbligatorio | Descrizione |
|-----------|-------------|-------------|
| `vendor_crypto.so` (linux/amd64) | ✅ | Plugin compilato per architettura target principale |
| `vendor_crypto_arm64.so` (linux/arm64) | ✅ | Plugin per architettura alternativa |
| Sorgenti Go del plugin | ✅ | Package completo compilabile |
| `go.mod` | ✅ | Versione Go e dipendenze per ricompilazione |
| Test vectors in formato JSON | ✅ | Vettori verificabili per KEX e/o AEAD |
| Schema `vendor_config.yaml` documentato | ✅ | Tutti i campi con tipo e descrizione |
| Certificazione (FIPS 140-3, NATO CC, o equivalente) | Raccomandata | |

### 10.1 Formato test vectors

```json
{
  "provider": "VendorCipher-256-GCM",
  "version": "1.0.0",
  "aead_vectors": [
    {
      "description": "Nominal encrypt/decrypt",
      "key_hex": "...",
      "nonce_hex": "020000000000000000000000",
      "plaintext_hex": "...",
      "aad_hex": "0201000000000000000000000000000000000000000000001",
      "ciphertext_hex": "...",
      "tag_hex": "..."
    }
  ],
  "kex_vectors": [
    {
      "description": "Cross-derivation symmetry",
      "quic_secret_hex": "...",
      "server_pubkey_hex": "...",
      "client_privkey_hex": "...",
      "session_id_hex": "aabbccdd",
      "expected_client_key_hex": "...",
      "expected_server_key_hex": "...",
      "expected_client_iv_hex": "...",
      "expected_server_iv_hex": "..."
    }
  ]
}
```

---

*Fine documento — CIFRANTE STRIPES Partner Integration Specification v1.0 — 2026-06-04*  
*Documento a distribuzione limitata — Riservato ai partner tecnici autorizzati*
