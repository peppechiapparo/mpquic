# CIFRANTE STRIPES — Crypto Abstraction Layer

**Progetto:** MPQUIC/STRIPES — Telespazio  
**Feature:** `feat/crypto-abstraction-layer`  
**Versione baseline:** v4.9 → commit 1459ed8  
**Data:** 2026-06-03  
**Stato:** Fasi A-D completate — Implementazione Fase E in corso

---

## Indice

1. [Scopo e motivazione](#1-scopo-e-motivazione)
2. [Stato attuale della cifratura](#2-stato-attuale-della-cifratura)
3. [Decisioni di design (Transport Expert)](#3-decisioni-di-design)
4. [Architettura del nuovo sistema](#4-architettura-del-nuovo-sistema)
5. [Interfacce Go definitive](#5-interfacce-go-definitive)
6. [Profili crittografici e configurazione YAML](#6-profili-crittografici-e-configurazione-yaml)
7. [Formato AAD esteso](#7-formato-aad-esteso)
8. [Specifica per fornitore esterno](#8-specifica-per-fornitore-esterno)
9. [Roadmap implementativa](#9-roadmap-implementativa)
10. [Piano di migrazione](#10-piano-di-migrazione)
11. [Piano di rollback](#11-piano-di-rollback)
12. [Analisi rischi e invarianti](#12-analisi-rischi-e-invarianti)
13. [Test e benchmark plan](#13-test-e-benchmark-plan)

---

## Stato di implementazione (aggiornato 2026-06-03)

| Fase | Nome | Stato | Commit | Test |
|------|------|--------|--------|------|
| A | Foundation — interfacce e tipi | ✅ Completata | `c08d5c3` | 18/18 |
| B | Provider AES-GCM | ✅ Completata | `4ff1d3a` | 18/18 |
| C | NonceManager + AAD esteso | ✅ Completata | `35e6f13` | 18/18 |
| D | KeyExchangeProvider classico + hybrid | ✅ Completata | `1459ed8` | 36/36 |
| E | External provider skeleton | 🔜 In pianificazione | — | — |
| F | Rekey engine | 🔜 In pianificazione | — | — |
| G | Integrazione completa | 🔜 In pianificazione | — | — |

**Go version**: `1.24` (aggiornato da 1.22 in Fase D — richiesto da `crypto/mlkem`)  
**Branch attivo**: `feat/crypto-abstraction-layer`  
**Prossima release target**: v5.0 (completamento Fase G)

---

## 1. Scopo e motivazione

### 1.1 Obiettivo

Realizzare una **Crypto Abstraction Layer** per STRIPES, implementata in Go, che renda il sottosistema cifrante **indipendente dal data plane**, dal multipath scheduler, dal FEC engine e dalla logica di tunnel.

### 1.2 Motivazioni

Il sistema STRIPES attualmente implementa la cifratura AES-256-GCM **direttamente** in `cmd/mpquic/stripe_crypto.go` con chiamate dirette a `crypto/aes` e `crypto/cipher`. Questa architettura presenta le seguenti limitazioni:

| Limitazione | Impatto |
|-------------|---------|
| Cifrante hardcoded | Impossibile cambiare algoritmo senza modificare il core STRIPES |
| No profili configurabili | Performance vs Security non è una scelta runtime |
| No agilità post-quantum | Impossibile integrare ML-KEM senza refactoring invasivo |
| No integrazione fornitori terzi | Clienti con cifrante certificata non possono integrarla |
| No rekey strutturato | Il rekey non è gestito in modo event-driven esplicito |

### 1.3 Principio architetturale obbligatorio

> **Il codice STRIPES NON deve dipendere direttamente da AES-GCM, ML-KEM, X25519 o da una specifica libreria crittografica. Deve dipendere SOLO da interfacce Go stabili definite in un package dedicato.**

---

## 2. Stato attuale della cifratura

### 2.1 File coinvolti

| File | Righe | Ruolo |
|------|-------|-------|
| `cmd/mpquic/stripe_crypto.go` | 256 | Cifratura AES-256-GCM, key derivation, nonce management |
| `cmd/mpquic/stripe.go` | 247 | Data plane principale, chiama `stripeEncrypt*` |
| `cmd/mpquic/stripe_arq.go` | 269 | Hybrid ARQ NACK (non tocca crypto direttamente) |

### 2.2 Architettura attuale

```
stripe.go
    │
    ├── stripeEncrypt(sc *stripeCipher, pkt)
    ├── stripeEncryptShard(sc *stripeCipher, hdr, shard)
    ├── stripeEncryptShardReuse(sc *stripeCipher, hdr, shard, buf) ← ZERO ALLOC HOT PATH
    └── stripeDecryptPkt(aead cipher.AEAD, pkt)
              │
              └── stripe_crypto.go
                      │
                      ├── import "crypto/aes"     ← dipendenza diretta
                      ├── import "crypto/cipher"  ← dipendenza diretta
                      └── stripeCipher {aead cipher.AEAD, txNonce uint64}
```

### 2.3 Wire format attuale

```
[stripeHdr 16B — cleartext AAD][seq 8B][ciphertext + 16B GCM tag]
                                          ↑
                                  overhead: 24 byte/pacchetto
```

AAD attuale: `stripeHdr(16B) || seq(8B)` = 24 byte totali

### 2.4 Key derivation attuale

```
QUIC TLS Exporter("mpquic-stripe-v1", sessionID_bytes, 64)
    → byte[0:32]  = c2sKey (client→server AES-256)
    → byte[32:64] = s2cKey (server→client AES-256)
```

---

## 3. Decisioni di design

*Definite dal @transport-expert. Queste decisioni sono **vincolanti** per l'implementazione.*

| Area | Decisione | Giustificazione | Riferimenti |
|------|-----------|----------------|-------------|
| **Nonce Management** | `NonceManager` separato, contatore `uint64` atomico, strategia per-worker (ogni worker ha il proprio contatore con step=N_workers) | Separation of concerns, lock-free, previene nonce reuse su path multipli | NIST SP 800-38D, RFC 5116 |
| **PQC/Hybrid KEX** | `crypto/mlkem` Go 1.24+ con build tag `goexperiment.mlkem` | Standard FIPS 203, stdlib maintainability, no supply-chain risk | FIPS 203, Go 1.24 release notes |
| **KEX Combination** | HKDF-SHA-384: IKM = `ss_x25519 ‖ ss_mlkem`; info = `"mpquic-stripe-v2" ‖ session_id ‖ client_pubkeys ‖ server_pubkeys ‖ profile_name` | Conforme draft-ietf-tls-hybrid-design, domain separation garantito | RFC 5869, draft-ietf-tls-hybrid-design |
| **AAD Esteso** | Packed struct 24 byte (schema fisso, vedere §7) | Overhead computazionale minimo, parsing O(1), allineamento ottimale | NIST SP 800-38D |
| **Epoch ID** | `uint8`, incrementato a ogni rekey; mantenere chiavi epoch N e N+1 per 3×RTT_max | 256 rekey/sessione sufficienti; gestisce pacchetti in transito durante rekey | — |
| **Rekey Triggers** | Threshold (byte, pacchetti, tempo) + Event (path recovery) con **anti-flap backoff 10s** | Bilancia sicurezza e stabilità; previene storm su link instabili | — |
| **Interfacce Go** | `KeyExchangeProvider` + `AEADProvider` + `NonceManager` — NO custom handshake | Riusa key exchange QUIC TLS Exporter esistente; evita over-engineering | RFC 5705, RFC 8446 §7.5 |
| **Performance hot path** | Generics Go 1.18+ per devirtualizzare chiamate; zero-alloc goal preservato | Interface dispatch evitato per tipo concreto in produzione | Go spec §Interface types |
| **Package structure** | `internal/mpquic/crypto/` | Impedisce import esterno, isola logica crypto | Go spec §Internal packages |
| **Versione Go** | Upgrade a **Go 1.24** obbligatorio (da 1.22 attuale) | `crypto/mlkem` richiede Go 1.24+ | Go 1.24 release notes |

---

## 4. Architettura del nuovo sistema

### 4.1 Visione d'insieme

```
┌─────────────────────────────────────────────────────────────────┐
│                        stripe.go (data plane)                   │
│                                                                 │
│  stripeEncryptShard(session CryptoSession, hdr, shard) []byte   │
│  stripeDecryptPkt(session CryptoSession, pkt) ([]byte, bool)    │
│                              │                                  │
│                              │ interface calls only             │
└──────────────────────────────┼──────────────────────────────────┘
                               │
┌──────────────────────────────▼──────────────────────────────────┐
│              internal/mpquic/crypto/  (CryptoSession)           │
│                                                                 │
│  ┌──────────────────┐  ┌────────────────┐  ┌─────────────────┐  │
│  │ KeyExchangeProv. │  │  AEADProvider  │  │  NonceManager   │  │
│  │                  │  │                │  │                 │  │
│  │ • ClassicalKEX   │  │ • AESGCMProv.  │  │ • CtxNonceMgr   │  │
│  │ • HybridKEX      │  │ (future: CC20) │  │ (per-worker)    │  │
│  │   (X25519+MLKEM) │  │                │  │                 │  │
│  └──────────────────┘  └────────────────┘  └─────────────────┘  │
│                                                                 │
│  ┌──────────────────┐  ┌────────────────┐                       │
│  │  RekeyManager    │  │ ExtCryptoAdapt.│                       │
│  │ (epoch, triggers)│  │ (plugin loader)│                       │
│  └──────────────────┘  └────────────────┘                       │
└─────────────────────────────────────────────────────────────────┘
              │                    │
    ┌─────────▼────────┐  ┌────────▼───────────┐
    │  crypto/mlkem    │  │ external .so plugin│
    │  crypto/ecdh     │  │ (fornitore terzo)  │
    │  crypto/aes      │  └────────────────────┘
    │  crypto/hkdf     │
    └──────────────────┘
```

### 4.2 Package structure completa

```
internal/mpquic/crypto/
├── doc.go              # Documentazione package
├── errors.go           # Tipi errore specifici (ErrAuthFailed, ErrNonceExhausted, ...)
├── types.go            # CryptoProfile, SessionKeys, EpochID, CryptoMetrics
├── config.go           # CryptoConfig (mapping YAML), validazione
├── crypto.go           # CryptoSession, factory NewCryptoSession()
├── aead.go             # AEADProvider interface + AESGCMProvider
├── aead_test.go        # Unit test + benchmark
├── kex.go              # KeyExchangeProvider interface + ClassicalKEX + HybridKEX
├── kex_hybrid.go       # HybridKEXProvider (build tag goexperiment.mlkem)
├── kex_test.go         # Unit test key derivation
├── nonce.go            # NonceManager interface + ContextualNonceManager
├── nonce_test.go       # Unit test concorrenza nonce
├── rekey.go            # RekeyManager (trigger, epoch, anti-flap)
├── rekey_test.go       # Unit test trigger rekey
├── external.go         # ExternalCryptoAdapter interface + plugin loader
├── metrics.go          # CryptoMetrics, registrazione Prometheus
└── testdata/
    ├── fake_provider/  # Fake provider per test (implementa ExternalCryptoAdapter)
    └── testvectors/    # Test vectors AES-GCM e HKDF noti
```

---

## 5. Interfacce Go definitive

### 5.1 Tipi base

```go
// package: internal/mpquic/crypto

// CryptoProfile identifica un profilo di sicurezza preconfigurato.
type CryptoProfile string

const (
    ProfilePerformance    CryptoProfile = "performance"
    ProfileHybridSecurity CryptoProfile = "hybrid_security"
    ProfileCustomProvider CryptoProfile = "custom_provider"
)

// SessionKeys contiene le chiavi operative per una sessione, generate da KeyExchangeProvider.
type SessionKeys struct {
    ClientKey []byte // client→server: AES-256 key
    ServerKey []byte // server→client: AES-256 key
    ClientIV  []byte // client→server: base IV (opzionale se NonceManager gestisce il nonce)
    ServerIV  []byte // server→client: base IV
    EpochID   uint8  // epoch corrente, incrementa a ogni rekey
}

// CryptoMetrics contiene i contatori Prometheus per il sottosistema crypto.
// Tutti i campi sono uint64 e devono essere aggiornati con atomic.AddUint64.
type CryptoMetrics struct {
    TotalEncryptions        uint64
    TotalDecryptions        uint64
    TotalDecryptionFailures uint64
    TotalAuthFailures       uint64 // pacchetti con tag GCM non valido
    TotalRekeyEvents        uint64
    TotalKEXCompleted       uint64
    TotalKEXFailures        uint64
    ActiveEpoch             uint8  // epoch corrente (snapshot, non atomico)
    ActiveProfile           CryptoProfile
}
```

### 5.2 KeyExchangeProvider

```go
// KeyExchangeProvider astrae la logica di key exchange (classico o ibrido).
// Usato una sola volta per sessione (handshake) e ad ogni rekey.
// Non sostituisce il handshake QUIC — deriva le chiavi STRIPES sopra
// il secret già negoziato da QUIC TLS Exporter.
type KeyExchangeProvider interface {
    // Name restituisce il nome del provider, es. "X25519" o "X25519+MLKEM768".
    Name() string

    // GenerateKeyPair genera una coppia di chiavi pubblica/privata per il KEX.
    GenerateKeyPair() (publicKey, privateKey []byte, err error)

    // DeriveSessionKeys calcola le SessionKeys a partire dal secret QUIC
    // e dalle chiavi pubbliche dei due peer.
    //
    // quicSecret: output di QUIC TLS Exporter (64 byte)
    // localPrivKey: chiave privata locale generata con GenerateKeyPair()
    // remotePubKey: chiave pubblica del peer
    // sessionID: identificatore univoco di sessione
    //
    // Per profilo performance: usa solo X25519 + HKDF-SHA-256
    // Per profilo hybrid: usa X25519 + ML-KEM-768, combina con HKDF-SHA-384
    DeriveSessionKeys(quicSecret, localPrivKey, remotePubKey []byte, sessionID []byte) (*SessionKeys, error)
}
```

### 5.3 AEADProvider

```go
// AEADProvider astrae un cifrante AEAD.
// Usato nel hot path per ogni pacchetto.
// Le implementazioni DEVONO essere thread-safe e allocazione-minima.
type AEADProvider interface {
    // Name restituisce il nome dell'algoritmo, es. "AES-256-GCM".
    Name() string

    // NewAEAD crea un'istanza del cifrante AEAD per la chiave fornita.
    // key deve avere lunghezza uguale a KeySize().
    NewAEAD(key []byte) (cipher.AEAD, error)

    // KeySize restituisce la dimensione della chiave richiesta in byte (es. 32 per AES-256).
    KeySize() int

    // NonceSize restituisce la dimensione del nonce in byte (es. 12 per GCM).
    NonceSize() int
}
```

### 5.4 NonceManager

```go
// NonceManager genera nonce unici per evitare il riuso sotto la stessa chiave.
// L'implementazione usa un contatore atomico per-worker (lock-free).
// Non deve mai restituire lo stesso nonce due volte per la stessa chiave/sessione.
type NonceManager interface {
    // NextNonce restituisce il prossimo nonce per il workerID specificato.
    // nonce è una slice che punta a un buffer interno; non deve essere modificata.
    // workerID identifica il goroutine/path worker corrente (0-based).
    NextNonce(workerID uint) (nonce []byte, err error)

    // Reset resetta il contatore (chiamato esclusivamente a seguito di rekey).
    Reset()

    // NonceSize restituisce la dimensione del nonce in byte.
    NonceSize() int
}
```

### 5.5 ExternalCryptoAdapter

```go
// ExternalCryptoAdapter è l'interfaccia che il plugin Go di un fornitore esterno
// deve implementare ed esportare come simbolo "CryptoProvider".
// Vedi §8 per la specifica completa per il fornitore.
type ExternalCryptoAdapter interface {
    // Init inizializza il provider con il path al proprio file di configurazione.
    Init(configPath string) error

    // Name restituisce il nome del provider esterno.
    Name() string

    // Version restituisce la versione del provider esterno.
    Version() string

    // KeyExchangeProvider restituisce l'implementazione KEX del provider.
    // Può restituire nil se il provider gestisce solo AEAD (Livello A).
    KeyExchangeProvider() KeyExchangeProvider

    // AEADProvider restituisce l'implementazione AEAD del provider.
    AEADProvider() AEADProvider

    // Close rilascia le risorse del provider in modo sicuro (zero chiavi in memoria).
    Close() error
}
```

### 5.6 CryptoConfig (mapping YAML)

```go
// CryptoConfig mappa la sezione `crypto:` del file YAML di configurazione STRIPES.
type CryptoConfig struct {
    Enabled  bool          `yaml:"enabled"`   // default: true in produzione
    Profile  CryptoProfile `yaml:"profile"`   // obbligatorio

    Rekey RekeyConfig `yaml:"rekey"`

    // Custom provider config — obbligatorio se Profile == "custom_provider"
    CustomProvider *CustomProviderConfig `yaml:"custom_provider,omitempty"`
}

type RekeyConfig struct {
    Enabled          bool   `yaml:"enabled"`
    IntervalSeconds  int    `yaml:"interval_seconds"`  // rekey periodico
    MaxPackets       uint64 `yaml:"max_packets"`       // threshold pacchetti
    MaxBytes         uint64 `yaml:"max_bytes"`         // threshold byte
    OnPathRecovery   bool   `yaml:"on_path_recovery"`  // trigger su path recovery
    OnEpochChange    bool   `yaml:"on_epoch_change"`   // trigger su cambio epoch
    AntiFlappingS    int    `yaml:"anti_flapping_seconds"` // default: 10
}

type CustomProviderConfig struct {
    Path       string `yaml:"path"`        // path al .so plugin
    ConfigFile string `yaml:"config_file"` // path alla config del provider
}
```

---

## 6. Profili crittografici e configurazione YAML

### 6.1 Profilo `performance`

Destinazione: ambienti dove throughput e bassa latenza sono prioritari.  
Algoritmi: X25519 (KEX) + AES-256-GCM (AEAD).

```yaml
# stripes_crypto_performance.yaml
crypto:
  enabled: true
  profile: performance

  rekey:
    enabled: true
    interval_seconds: 3600       # rekey ogni ora
    max_packets: 1000000000      # rekey dopo 1 miliardo di pacchetti
    max_bytes: 1073741824        # rekey dopo 1 GB
    on_path_recovery: false      # no rekey su path recovery (bassa latenza)
    on_epoch_change: true
    anti_flapping_seconds: 10
```

### 6.2 Profilo `hybrid_security`

Destinazione: ambienti ad alta sicurezza, resistenza a "store now decrypt later".  
Algoritmi: X25519 + ML-KEM-768 (KEX ibrido) + AES-256-GCM (AEAD).  
**Richiede Go 1.24+ e build tag `goexperiment.mlkem`.**

```yaml
# stripes_crypto_hybrid_security.yaml
crypto:
  enabled: true
  profile: hybrid_security

  rekey:
    enabled: true
    interval_seconds: 1800       # rekey ogni 30 minuti (più frequente)
    max_packets: 500000000       # threshold più bassa
    max_bytes: 536870912         # 512 MB
    on_path_recovery: true       # rekey su path recovery
    on_epoch_change: true
    anti_flapping_seconds: 10
```

### 6.3 Profilo `custom_provider`

Destinazione: integrazione con cifrante certificata di fornitore terzo.  
Il provider esterno gestisce KEX e/o AEAD tramite plugin Go.

```yaml
# stripes_crypto_custom_provider.yaml
crypto:
  enabled: true
  profile: custom_provider

  custom_provider:
    path: /opt/mpquic/plugins/crypto/vendor_crypto.so
    config_file: /etc/mpquic/crypto/vendor_config.yaml

  rekey:
    enabled: false  # gestito dal provider esterno o disabilitato
```

### 6.4 Regole di validazione

| Campo | Obbligatorio | Default | Combinazioni invalide |
|-------|-------------|---------|----------------------|
| `crypto.enabled` | No | `true` | — |
| `crypto.profile` | Sì | — | — |
| `custom_provider.path` | Se `profile=custom_provider` | — | Profile≠custom + path impostato |
| `rekey.anti_flapping_seconds` | No | `10` | < 0 |
| `rekey.max_packets` | No | `1000000000` | = 0 se rekey.enabled=true |

---

## 7. Formato AAD esteso

L'AAD (Additional Authenticated Data) viene esteso dal formato attuale (24 byte) a un **packed struct da 24 byte** con semantica migliorata.

### 7.1 Schema

```
Offset  Size  Field
──────  ────  ─────────────────────────────────────────────
0       1B    version          (0x01 = AAD v1 legacy, 0x02 = AAD v2 extended)
1       1B    epoch_id         (contatore rekey, 0-255)
2       2B    path_pipe_id     (path_id[7:0] in alto, pipe_id[7:0] in basso)
4       1B    traffic_class    (QoS class: 0=best-effort, 1=critical, 2=bulk)
5       1B    flags            (bit field: FEC, direction, rekey_in_progress, ...)
6       2B    fec_group_id     (ID del gruppo FEC, 0 se non FEC)
8       8B    sequence_number  (contatore monotono uint64, big-endian)
16      8B    session_id_low   (64 bit meno significativi del session ID)
──────  ────  ─────────────────────────────────────────────
Total   24B
```

### 7.2 Backward compatibility

- `version = 0x01`: formato legacy (`stripeHdr 16B + seq 8B`). Il ricevitore lo decodifica con il vecchio parser.
- `version = 0x02`: nuovo formato esteso. Il ricevitore usa il parser AAD v2.
- Durante la fase di transizione, entrambi i formati sono supportati in ricezione; la versione del formato da usare in invio è governata dal YAML (`crypto.aad_version: v1|v2`).

---

## 8. Specifica per fornitore esterno

*Documento completo: `docs/STRIPES_External_Crypto_Provider_Spec.md` (da creare nella Fase E).*

### 8.1 Livelli di integrazione

| Livello | Cosa fornisce il fornitore | STRIPES gestisce |
|---------|--------------------------|-----------------|
| **A** — AEAD only | `AEADProvider`: `Seal` e `Open` | KEX, KDF, nonce, sessione, epoch |
| **B** — KEX only | `KeyExchangeProvider`: `GenerateKeyPair` e `DeriveSessionKeys` | AEAD, AAD, nonce, sessione, pacchetti |
| **C** — Full provider | `ExternalCryptoAdapter` completo | Nulla (solo orchestrazione) |

### 8.2 Requisiti obbligatori per tutti i livelli

1. **Thread safety**: tutti i metodi devono essere goroutine-safe
2. **No I/O in hot path**: `Seal`/`Open` non devono fare I/O su disco o rete
3. **No key logging**: vietato scrivere chiavi, nonce o materiale derivato in log o stdout
4. **Gestione errori**: errori restituiti come `error`, mai `panic()`
5. **Test vectors**: il fornitore deve consegnare test vectors verificabili
6. **Cross-compilation**: deve compilare su `linux/amd64` e `linux/arm64`
7. **Licenza**: compatibile con il progetto STRIPES (non GPL)

### 8.3 Formato plugin

```bash
# Compilazione del plugin
go build -buildmode=plugin -o vendor_crypto.so ./vendor_crypto/

# Il plugin deve esportare il simbolo:
var CryptoProvider ExternalCryptoAdapter = &MyVendorProvider{}
```

---

## 9. Roadmap implementativa

### Overview fasi

```
v4.9 (baseline)
    │
    ▼
[Fase A] Foundation — interfacce e tipi
    │ ✦ Nessuna modifica al codice esistente
    │ ✦ Package internal/mpquic/crypto/ creato
    ▼
[Fase B] Provider AES-GCM — migrazione trasparente
    │ ✦ AESGCMProvider implementato
    │ ✦ stripe_crypto.go migrato a usare AEADProvider
    │ ✦ Comportamento identico all'attuale
    ▼
[Fase C] NonceManager + AAD esteso
    │ ✦ ContextualNonceManager (per-worker, lock-free)
    │ ✦ Nuovo formato AAD v2 (backward compatible)
    │ ⚠ Breaking change di protocollo — richiede versioning
    ▼
[Fase D] KeyExchangeProvider classico + hybrid
    │ ✦ ClassicalKEXProvider (X25519)
    │ ✦ HybridKEXProvider (X25519 + ML-KEM-768)
    │ ✦ Upgrade Go 1.22 → 1.24
    ▼
[Fase E] External provider skeleton
    │ ✦ ExternalCryptoAdapter + plugin loader
    │ ✦ Fake provider per test
    ▼
[Fase F] Rekey engine
    │ ✦ RekeyManager (threshold + event triggers)
    │ ✦ Epoch management
    │ ✦ Anti-flap backoff 10s
    ▼
[Fase G] Integrazione completa
    │ ✦ Wire-up di tutti i componenti
    │ ✦ Test end-to-end 3 profili
    │ ✦ Benchmark comparativi
    ▼
v5.0 (release Crypto Abstraction Layer)
```

### Dettaglio fasi

#### Fase A — Foundation
**Obiettivo:** Creare il package `internal/mpquic/crypto/` con tutte le interfacce e i tipi. Nessuna modifica al codice funzionale esistente.

**File da creare:**
- `internal/mpquic/crypto/doc.go`
- `internal/mpquic/crypto/errors.go` — ErrAuthFailed, ErrNonceExhausted, ErrInvalidProfile, ErrProviderNotFound
- `internal/mpquic/crypto/types.go` — CryptoProfile, SessionKeys, CryptoMetrics
- `internal/mpquic/crypto/config.go` — CryptoConfig, RekeyConfig, CustomProviderConfig + Validate()
- `internal/mpquic/crypto/aead.go` — interface AEADProvider
- `internal/mpquic/crypto/kex.go` — interface KeyExchangeProvider
- `internal/mpquic/crypto/nonce.go` — interface NonceManager
- `internal/mpquic/crypto/external.go` — interface ExternalCryptoAdapter
- `internal/mpquic/crypto/crypto.go` — CryptoSession struct, NewCryptoSession() factory
- `internal/mpquic/crypto/metrics.go` — CryptoMetrics, RegisterPrometheus()

**Criteri di done:** Il package compila senza errori. Nessun test di regressione fallisce.
**Rollback:** `git revert <commit_fase_A>` o `rm -rf internal/mpquic/crypto/`

---

#### Fase B — Provider AES-GCM
**Obiettivo:** Implementare `AESGCMProvider` e migrare `stripe_crypto.go` a usarlo. Comportamento identico.

**File da creare/modificare:**
- `internal/mpquic/crypto/aead.go` — aggiungere `AESGCMProvider` struct
- `internal/mpquic/crypto/aead_test.go` — unit test + benchmark baseline
- `cmd/mpquic/stripe_crypto.go` — modificare per usare `AEADProvider` invece di `cipher.AEAD`
- `cmd/mpquic/stripe_crypto_test.go` — test di regressione (output deve essere identico)

**Criteri di done:** Benchmark ≤ +2% overhead rispetto alla baseline. Tutti i test esistenti passano.
**Rollback:** Revert `stripe_crypto.go` a version precedente.

---

#### Fase C — NonceManager + AAD esteso
**Obiettivo:** Implementare nonce management dedicato e il nuovo formato AAD v2.

**File da creare/modificare:**
- `internal/mpquic/crypto/nonce.go` — ContextualNonceManager
- `internal/mpquic/crypto/nonce_test.go` — test concorrenza 1000 goroutine
- `cmd/mpquic/stripe_crypto.go` — usa NonceManager, supporta AAD v1 e v2
- Feature flag YAML: `crypto.aad_version: v1|v2`

**Criteri di done:** Test concorrenza nonce: zero collisioni. Interoperabilità v1↔v2 testata.
**Rollback:** Impostare `crypto.aad_version: v1` in YAML.

---

#### Fase D — KeyExchangeProvider classico + hybrid
**Obiettivo:** Implementare i due provider KEX.

**Prerequisito:** Upgrade `go.mod` da 1.22 a 1.24.

**File da creare/modificare:**
- `internal/mpquic/crypto/kex.go` — ClassicalKEXProvider (X25519)
- `internal/mpquic/crypto/kex_hybrid.go` — HybridKEXProvider (`//go:build goexperiment.mlkem`)
- `internal/mpquic/crypto/kex_test.go` — test derivazione chiavi con test vectors
- `go.mod` — `go 1.24`
- `Makefile` — aggiungere `-tags goexperiment.mlkem` per build hybrid

**Criteri di done:** Classical KEX test vectors OK. Hybrid test vectors OK. Build senza flag produce solo ClassicalKEXProvider.
**Rollback:** Non usare profilo `hybrid_security` nella configurazione.

---

#### Fase E — External provider skeleton
**Obiettivo:** Plugin loader e fake provider per test.

**File da creare:**
- `internal/mpquic/crypto/external.go` — loader via `plugin.Open`
- `internal/mpquic/crypto/testdata/fake_provider/` — fake ExternalCryptoAdapter
- `docs/STRIPES_External_Crypto_Provider_Spec.md` — specifica per fornitore esterno

**Criteri di done:** Il fake provider viene caricato correttamente. Test di integrazione con profilo `custom_provider` passano.
**Rollback:** Non usare profilo `custom_provider`.

---

#### Fase F — Rekey engine
**Obiettivo:** RekeyManager con tutti i trigger, gestione epoch, anti-flap.

**File da creare/modificare:**
- `internal/mpquic/crypto/rekey.go` — RekeyManager
- `internal/mpquic/crypto/rekey_test.go` — test tutti i trigger
- `cmd/mpquic/stripe.go` — integrare hook RekeyManager nei punti di path event

**Criteri di done:** Tutti i trigger di rekey funzionano. Anti-flap 10s verificato. Nessun nonce reuse durante rekey.
**Rollback:** Impostare `rekey.enabled: false` in YAML.

---

#### Fase G — Integrazione completa
**Obiettivo:** Wire-up di tutti i componenti, test end-to-end, benchmark comparativi.

**File da modificare:**
- `cmd/mpquic/main.go` — lettura configurazione crypto, init CryptoSession
- `cmd/mpquic/stripe.go` — usa CryptoSession per tutte le operazioni crypto
- Feature flag: `features.new_crypto_path: true|false`

**Criteri di done:**
- [ ] Profilo `performance` funziona senza ML-KEM
- [ ] Profilo `hybrid_security` funziona con ML-KEM 768 attivo
- [ ] Profilo `custom_provider` carica il fake provider
- [ ] Il data plane non importa direttamente `crypto/aes`, `crypto/cipher`
- [ ] Benchmark: differenza performance vs hybrid documentata
- [ ] Zero nonce reuse in test di concorrenza
- [ ] Il codice non scrive mai chiavi o segreti nei log
- [ ] Compilazione `linux/amd64` + `linux/arm64`

**Rollback:** Impostare `features.new_crypto_path: false` in YAML.

---

#### Release v5.0
**Prerequisiti per release:**
- Tutte le fasi A-G completate e accettate
- Security audit (@security-nis2) completato senza finding critici
- Benchmark documentati nel CHANGELOG
- Documento `STRIPES_External_Crypto_Provider_Spec.md` revisionato
- Test in ambiente pre-produzione per ≥ 48 ore

---

## 10. Piano di migrazione

### 10.1 Strangler Fig Pattern

La migrazione avviene **senza breaking change per il caller** (`stripe.go`):

```
STATO ATTUALE:
stripe.go → stripeEncrypt(sc *stripeCipher, pkt) → crypto/aes diretto

FASE B (coesistenza):
stripe.go → stripeEncrypt(sc AEADProvider, pkt) → AESGCMProvider → crypto/aes
           (comportamento identico, interfaccia astratta)

FASE G (cutover):
stripe.go → session.Encrypt(shard, &aadData) → CryptoSession → AEADProvider
```

### 10.2 Feature flag di cutover

```yaml
# In config YAML principale
features:
  new_crypto_path: false  # default: false fino a validazione completa
```

Quando `new_crypto_path: false`, il codice usa ancora il vecchio `stripeEncrypt*` originale.  
Quando `true`, usa il nuovo `CryptoSession`.

### 10.3 Criterio di eliminazione del vecchio codice

Il vecchio `stripe_crypto.go` (dipendenze dirette da `crypto/aes`) verrà rimosso quando:
1. `features.new_crypto_path: true` è stato in produzione per ≥ 1 settimana senza incidenti
2. Benchmark confermano ≤ 2% degradation
3. Security audit completato

---

## 11. Piano di rollback

| Fase | Feature Flag / Rollback | Tempo di recovery |
|------|------------------------|-------------------|
| A | `git revert` — nessun impatto su codice esistente | < 5 min |
| B | Revert `stripe_crypto.go` | < 5 min |
| C | `crypto.aad_version: v1` in YAML — no redeploy | **Immediato** |
| D | Profilo `performance` in YAML — no redeploy | **Immediato** |
| E | Profilo ≠ `custom_provider` — no redeploy | **Immediato** |
| F | `rekey.enabled: false` in YAML — no redeploy | **Immediato** |
| G | `features.new_crypto_path: false` in YAML — no redeploy | **Immediato** |

---

## 12. Analisi rischi e invarianti

### 12.1 Rischi principali

| Rischio | Probabilità | Impatto | Mitigazione |
|---------|-------------|---------|-------------|
| Regressione performance hot path | Media | Alto | Benchmark-driven dev; zero-alloc invariant; generics |
| Nonce reuse durante rekey | Bassa | Critico | NonceManager.Reset() solo dopo cambio chiave confermato; test concorrenza |
| Breaking change AAD incompatibile | Media | Alto | Versioning AAD v1/v2; backward compatibility in ricezione |
| ML-KEM non disponibile su Go 1.22 | Certa | Medio | Upgrade Go 1.24 — prerequisito Fase D |
| Plugin esterno instabile | Alta | Medio | Sandbox via interface; no panic propagation; timeout |

### 12.2 Invarianti di performance non negoziabili

1. **Zero allocations** nel hot path (`Encrypt`, `Decrypt`) per il profilo `performance`
2. **Latenza < 5µs** per pacchetto per l'overhead dell'astrazione
3. **Throughput ≥ 200 MB/s per core** con AES-256-GCM + AES-NI
4. **Contatore nonce atomico** — nessun mutex nel hot path

---

## 13. Test e benchmark plan

### 13.1 Unit test

| File | Test | Cosa verifica |
|------|------|---------------|
| `aead_test.go` | `TestAESGCMProvider_NewAEAD` | Chiavi corrette/errate |
| `aead_test.go` | `TestAESGCMProvider_EncryptDecrypt` | Test vectors AES-256-GCM NIST |
| `nonce_test.go` | `TestContextualNonceManager_Sequential` | Nonce monotoni |
| `nonce_test.go` | `TestContextualNonceManager_Concurrency` | 1000 goroutine, zero collisioni |
| `kex_test.go` | `TestClassicalKEXProvider_DeriveKeys` | Test vectors X25519 + HKDF |
| `kex_test.go` | `TestHybridKEXProvider_DeriveKeys` | Test vectors X25519 + MLKEM + HKDF |
| `rekey_test.go` | `TestRekeyManager_PacketThreshold` | Trigger a N pacchetti |
| `rekey_test.go` | `TestRekeyManager_ByteThreshold` | Trigger a N byte |
| `rekey_test.go` | `TestRekeyManager_AntiFlap` | No storm su path instabile |

### 13.2 Integration test

| File | Test | Scenario |
|------|------|---------|
| `crypto_test.go` | `TestCryptoSession_Performance` | Sessione completa profilo performance |
| `crypto_test.go` | `TestCryptoSession_Hybrid` | Sessione completa profilo hybrid |
| `crypto_test.go` | `TestCryptoSession_Rekey` | Sessione lunga con rekey |
| `stripe_integration_test.go` | `TestStripe_AAD_Interop` | Peer v1 ↔ peer v2 |
| `stripe_integration_test.go` | `TestStripe_PathRecovery_Rekey` | Path down/up → rekey |

### 13.3 Benchmark

```
BenchmarkAESGCM_Encrypt_1400B       # throughput 1.4KB packet
BenchmarkAESGCM_Decrypt_1400B       # throughput 1.4KB packet
BenchmarkStripeEncryptShard_Before  # baseline pre-refactoring
BenchmarkStripeEncryptShard_After   # post-refactoring (deve essere ≤ baseline + 2%)
BenchmarkHybridKEX_DeriveKeys       # overhead ML-KEM vs classical
BenchmarkRekey_Full                 # overhead rekey completo
```

---

## 14. Fase D — Dettaglio implementazione KeyExchangeProvider

*Data completamento: 2026-06-03 | Commit: `1459ed8` | Branch: `feat/crypto-abstraction-layer`*

### 14.1 File implementati

| File | LOC | Ruolo |
|------|-----|-------|
| `internal/mpquic/crypto/kex.go` | ~50 | Interfacce `KeyExchangeProvider` e `KemProvider` |
| `internal/mpquic/crypto/kex_classical.go` | ~90 | `ClassicalKEXProvider` (X25519 + HKDF-SHA256) |
| `internal/mpquic/crypto/kex_hybrid.go` | ~240 | `HybridKEXProvider` (X25519 + ML-KEM-768 + HKDF-SHA256) |
| `internal/mpquic/crypto/kex_factory.go` | ~30 | Factory `NewKeyExchangeProvider(profile)` |
| `internal/mpquic/crypto/kex_classical_test.go` | ~120 | 5 test ClassicalKEX |
| `internal/mpquic/crypto/kex_hybrid_test.go` | ~180 | 6 test HybridKEX |
| `internal/mpquic/crypto/kex_factory_test.go` | ~60 | 4 test factory (100% coverage) |
| `internal/mpquic/crypto/kex_bench_test.go` | ~80 | 4 benchmark handshake/derive |
| `go.mod` | — | `go 1.22` → `go 1.24` |
| **Eliminato** | — | `errors_kex.go` (consolidato in `errors.go`) |

### 14.2 Interfaccia `KeyExchangeProvider` (invariata dal design Fase A)

```go
type KeyExchangeProvider interface {
    Name() string
    GenerateKeyPair() (publicKey, privateKey []byte, err error)
    DeriveSessionKeys(quicSecret, localPrivKey, remotePubKey, sessionID []byte) (*SessionKeys, error)
}
```

### 14.3 Sub-interfaccia `KemProvider` (nuova in Fase D)

Introdotta per i provider KEM che richiedono un passo di encapsulation client-side prima della derivazione delle chiavi. I provider DH classici (es. `ClassicalKEXProvider`) implementano solo `KeyExchangeProvider`; i provider KEM implementano `KemProvider`.

```go
type KemProvider interface {
    KeyExchangeProvider
    // ClientEncapsulate prepara il materiale per il lato client del KEX KEM.
    // serverPubKey: chiave pubblica ibrida del server (X25519_pub || MLKEM_ek) = 1216 bytes
    // Returns:
    //   localPrivKey  = X25519_priv_client || mlkem_shared (64 bytes)
    //   peerKeyShare  = X25519_pub_client  || MLKEM_ciphertext (1120 bytes)
    ClientEncapsulate(serverPubKey []byte) (localPrivKey, peerKeyShare []byte, err error)
}
```

**Nota per integratori**: per usare `HybridKEXProvider` come client, fare type assertion a `KemProvider`:
```go
if kp, ok := provider.(crypto.KemProvider); ok {
    localPrivKey, peerKeyShare, err := kp.ClientEncapsulate(serverPubKey)
}
```

### 14.4 ClassicalKEXProvider — Specifica tecnica

| Parametro | Valore |
|-----------|--------|
| **Algoritmo KEX** | X25519 Elliptic Curve Diffie-Hellman |
| **KDF** | HKDF-SHA-256 (RFC 5869) |
| **Libreria** | `crypto/ecdh` (Go stdlib) |
| **Chiave pubblica** | 32 byte |
| **Chiave privata** | 32 byte |
| **HKDF salt** | `quicSecret` (64 byte, da QUIC TLS Exporter) |
| **HKDF IKM** | `sharedX` (32 byte, output ECDH X25519) |
| **HKDF info** | `"X25519-HKDF-SHA256" \|\| "\|" \|\| sessionID` |
| **HKDF output** | 88 byte |
| **Output layout** | `ClientKey[0:32] \| ServerKey[32:64] \| ClientIV[64:76] \| ServerIV[76:88]` |
| **Sicurezza classica** | 128-bit |
| **Sicurezza post-quantum** | ❌ Non protetto |

**Flusso handshake Classical**:
```
SERVER:
  pub_s, priv_s := ClassicalKEX.GenerateKeyPair()  // → X25519 keypair server
  # trasmetti pub_s al client

CLIENT:
  pub_c, priv_c := ClassicalKEX.GenerateKeyPair()  // → X25519 keypair client
  # trasmetti pub_c al server

ENTRAMBI:
  SessionKeys = ClassicalKEX.DeriveSessionKeys(quicSecret, priv_own, pub_peer, sessionID)
  # quicSecret = QUIC TLS Exporter("mpquic-stripe-v1", sessionID_bytes, 64)
  # risultato identico su entrambi i lati (simmetria ECDH)
```

### 14.5 HybridKEXProvider — Specifica tecnica

| Parametro | Valore |
|-----------|--------|
| **Algoritmo KEX** | X25519 + ML-KEM-768 (ibrido) |
| **Standard ML-KEM** | NIST FIPS 203 — IND-CCA2, livello 3 |
| **KDF** | HKDF-SHA-256 (RFC 5869) |
| **Libreria X25519** | `crypto/ecdh` (Go stdlib) |
| **Libreria ML-KEM** | `crypto/mlkem` (Go 1.24 stdlib, FIPS 203) |
| **Chiave pubblica server** | 1216 byte = `X25519_pub(32) \|\| ML-KEM-768_ek(1184)` |
| **Chiave privata server** | 96 byte = `X25519_priv(32) \|\| ML-KEM-768_dk_seed(64)` |
| **peerKeyShare (client→server)** | 1120 byte = `X25519_pub_client(32) \|\| ML-KEM-768_ciphertext(1088)` |
| **localPrivKey client** | 64 byte = `X25519_priv_client(32) \|\| mlkem_shared(32)` |
| **HKDF salt** | `quicSecret` (64 byte) |
| **HKDF IKM** | `sharedX(32) \|\| mlkemShared(32)` = 64 byte |
| **HKDF info** | `"X25519+ML-KEM-768-HKDF-SHA256" \|\| "\|" \|\| sessionID` |
| **HKDF output** | 88 byte |
| **Output layout** | `ClientKey[0:32] \| ServerKey[32:64] \| ClientIV[64:76] \| ServerIV[76:88]` |
| **Sicurezza classica** | 128-bit (X25519) |
| **Sicurezza post-quantum** | ✅ NIST level 3 (ML-KEM-768, 178-bit quantum security) |
| **Protezione SNDL** | ✅ "Store Now Decrypt Later" mitigato |

**Flusso handshake Hybrid**:
```
SERVER (una sola volta per sessione):
  pub_s, priv_s := HybridKEX.GenerateKeyPair()
  # pub_s = X25519_pub_s(32) || ML-KEM-768_ek(1184) — trasmetti al client

CLIENT:
  localPrivKey, peerKeyShare := HybridKEX.ClientEncapsulate(pub_s)
  # Internamente:
  #   1. Genera X25519_priv_c, X25519_pub_c
  #   2. Encapsula su ML-KEM-768_ek del server → mlkem_shared(32), mlkem_ct(1088)
  #   3. localPrivKey = X25519_priv_c(32) || mlkem_shared(32)
  #   4. peerKeyShare = X25519_pub_c(32)  || mlkem_ct(1088)
  # trasmetti peerKeyShare al server

  servPubX25519 := pub_s[:32]  # primi 32 byte di pub_s
  clientKeys := HybridKEX.DeriveSessionKeys(quicSecret, localPrivKey, servPubX25519, sessionID)

SERVER:
  serverKeys := HybridKEX.DeriveSessionKeys(quicSecret, priv_s, peerKeyShare, sessionID)
  # Internamente:
  #   1. X25519 ECDH: sharedX = X25519(priv_s[:32], peerKeyShare[:32])
  #   2. ML-KEM Decapsulate: mlkemShared = dk.Decapsulate(peerKeyShare[32:])
  #   3. IKM = sharedX(32) || mlkemShared(32) = 64 byte
  #   4. HKDF(sha256, IKM, quicSecret, "X25519+ML-KEM-768-HKDF-SHA256|<sessionID>", 88)
  #   5. SessionKeys = keyMat[:88]

# clientKeys == serverKeys (proprietà di correttezza del KEM)
```

### 14.6 Sicurezza crittografica — dettaglio

**Domain separation** (applicato in Fase D su indicazione @security-nis2):
L'`info` HKDF include il nome dell'algoritmo come prefix (`"algo_name|sessionID"`). Questo impedisce cross-protocol confusion tra provider diversi anche in caso di futura negoziazione on-wire.

**Zeroization** (in tutti i provider):
- `defer zeroize(sharedX)` — segreto X25519 azzerato immediatamente dopo l'uso
- `defer zeroize(mlkemShared)` — shared secret ML-KEM azzerato
- `defer zeroize(ikm)` — IKM composito azzerato
- `defer zeroize(keyMat)` — output HKDF azzerato dopo la copia in SessionKeys
- `runtime.KeepAlive` applicato per prevenire dead-code elimination del compilatore

**Input validation** (fail strict):
- Tutte le slice sono validate con `!= expected_size` (uguaglianza esatta) prima di ogni slicing
- `ErrEmptySessionID` tornato se `len(sessionID) == 0`
- `ErrInvalidKeySize` tornato per qualsiasi dimensione non attesa

### 14.7 Factory — `NewKeyExchangeProvider`

```go
func NewKeyExchangeProvider(profile CryptoProfile) (KeyExchangeProvider, error) {
    switch profile {
    case ProfilePerformance:    return NewClassicalKEXProvider(), nil
    case ProfileHybridSecurity: return NewHybridKEXProvider(), nil
    case ProfileCustomProvider: return nil, ErrProviderNotFound  // Fase E
    default:                    return nil, ErrInvalidProfile
    }
}
```

### 14.8 Benchmark performance (misurati su Intel Core Ultra 9 185H, Go 1.26.0)

| Operazione | Classical (X25519) | Hybrid (X25519+ML-KEM-768) | Overhead assoluto |
|------------|-------------------|---------------------------|-------------------|
| **Handshake completo** | 203 µs | 387 µs | +184 µs (+90%) |
| **DeriveSessionKeys only** | 68 µs | 153 µs | +85 µs (+125%) |
| Allocazioni/handshake | 84 allocs, 4.9 KB | 96 allocs, 31.8 KB | +12 allocs, +26.9 KB |

**Impatto sul throughput tunnel**: **ZERO**. Il KEX avviene una sola volta al riavvio del tunnel (handshake ~0.4ms). AES-256-GCM rimane il cifrante per-pacchetto in entrambi i profili. Test iperf3 reale: 365 Mbps receiver su tunnel mp1 (WAN5+WAN6, 12 pipe/WAN, 20 secondi) — risultato identico pre e post Fase D.

### 14.9 Test suite (risultati aggiornati)

| Suite | Test | Risultato | Race detector |
|-------|------|-----------|---------------|
| `TestClassicalKEX_GenerateKeyPair` | Dimensioni chiavi | ✅ PASS | ✅ |
| `TestClassicalKEX_DeriveSessionKeys` | Output SessionKeys | ✅ PASS | ✅ |
| `TestClassicalKEX_CrossDerivation` | Simmetria ECDH | ✅ PASS | ✅ |
| `TestClassicalKEX_EmptySessionID` | Errore atteso | ✅ PASS | ✅ |
| `TestClassicalKEX_InvalidKeySize` | Errore atteso | ✅ PASS | ✅ |
| `TestHybridKEX_GenerateKeyPair` | Dimensioni 1216/96 | ✅ PASS | ✅ |
| `TestHybridKEX_ClientEncapsulate` | Dimensioni 64/1120 | ✅ PASS | ✅ |
| `TestHybridKEX_CrossDerivation` | Simmetria KEM | ✅ PASS | ✅ |
| `TestHybridKEX_EmptySessionID` | Errore atteso | ✅ PASS | ✅ |
| `TestHybridKEX_InvalidLocalPrivKeySize` | Errore atteso | ✅ PASS | ✅ |
| `TestHybridKEX_ClientEncapsulate_InvalidKeySize` | Errore atteso | ✅ PASS | ✅ |
| `TestNewKeyExchangeProvider_*` (4) | Factory profiles | ✅ PASS | ✅ |
| **Totale crypto package** | **36/36** | **✅ PASS** | **✅ 0 race** |

### 14.10 Security audit — esito (@security-nis2, 2026-06-03)

**Esito**: APPROVATO CON NOTE — nessuna vulnerabilità critica.

| Finding | Severità | Stato |
|---------|----------|-------|
| SEC-D01: Domain separation HKDF mancante | Media (CVSSv3 4.8) | ✅ Risolto in Fase D |
| SEC-D03: Validazione lunghezza `<` invece di `==` | Bassa | ✅ Risolto in Fase D |
| SEC-D02: Ordine IKM X25519‖ML-KEM vs standard TLS | Bassa | 📝 Debito tecnico (protocollo STRIPES proprietario) |
| SEC-D04: zeroize best-effort (limite Go stdlib) | Informativa | 📝 Documentato, non risolvibile |

### 14.11 Stato di integrazione nel tunnel

⚠️ **I provider Fase D NON sono ancora integrati nel tunnel operativo.**

Il tunnel attivo usa ancora `stripe_keyex.go` (QUIC TLS Exporter), che produce chiavi per AES-256-GCM. Il campo `encrypted=AES-256-GCM` nei log di sistema è corretto e **rimarrà tale** — AES-256-GCM è il cifrante simmetrico per-pacchetto (non il KEX).

L'integrazione completa dei provider Fase D avverrà in **Fase G** (`NewCryptoSession` → `stripe.go`).

---

## Note di release

Questa feature sarà rilasciata nella versione **v5.0** del progetto MPQUIC/STRIPES.

Il branch `feat/crypto-abstraction-layer` deve essere ribasato su `main` prima del merge.

La review finale (Fase 3 del workflow) deve coprire:
- Correttezza crittografica (no ad-hoc crypto, solo primitive standard)
- Invarianti di performance (benchmark allegati alla PR)
- Aderenza al piano di migrazione e ai criteri di rollback

L'audit di sicurezza (@security-nis2) deve verificare in particolare:
- Assenza di key logging nei percorsi di errore
- Gestione sicura del rekey (no window di pacchetti non autenticati)
- Compliance NIS2 per il profilo `hybrid_security`
