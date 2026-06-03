# MPQUIC — Documentazione di Sistema

_Documento consolidato: Architettura, Infrastruttura, Dataplane, Metriche, Installazione, Operazioni_

_Ultima revisione: 14 maggio 2026_

---

## Indice

1. [Architettura del sistema](#1-architettura-del-sistema)
2. [Infrastruttura di deploy](#2-infrastruttura-di-deploy)
3. [Dataplane QoS e orchestratore esterno](#3-dataplane-qos-e-orchestratore-esterno)
4. [Monitoraggio e metriche](#4-monitoraggio-e-metriche)
5. [Metriche Zabbix e TBOX](#5-metriche-zabbix-e-tbox)
6. [Installazione e configurazione](#6-installazione-e-configurazione)
7. [Operazioni e debug tunnel](#7-operazioni-e-debug-tunnel)

---


## 1. Architettura del sistema

### Obiettivo
Realizzare 6 tunnel IP-over-QUIC indipendenti (multi-sessione 1:1), coerenti con il layer L3 esistente sulla VM client MPQUIC, senza modificare policy routing e NAT già validati.

### Componenti principali
- Binario unico: `mpquic` (stesso eseguibile per client/server)
- Trasporto: QUIC su UDP (`quic-go`)
- Incapsulamento: QUIC DATAGRAM extension
- Interfaccia locale per tunnel: TUN Linux dedicata per ogni istanza
- Orchestrazione servizi: `systemd` template `mpquic@.service`
- Configurazione runtime: YAML per istanza + `.env` per `ExecStartPre`

### Topologia logica
Per ogni istanza `i` (1..6):
1. Client legge pacchetti IP da `mpq{i}` (TUN)
2. Client apre sessione QUIC verso VPS su `remote_addr:remote_port`
3. Socket UDP client è bindata su IP WAN fisica corretta (`bind_ip` / `if:<ifname>`)
4. Server riceve datagram QUIC e li scrive sulla propria TUN `mpq{i}`
5. Flusso bidirezionale simmetrico (TUN <-> DATAGRAM)

### Mapping WAN client (1:1)
- Istanza 1 -> `enp7s3` (WAN1)
- Istanza 2 -> `enp7s4` (WAN2)
- Istanza 3 -> `enp7s5` (WAN3)
- Istanza 4 -> `enp7s6` (WAN4)
- Istanza 5 -> `enp7s7` (WAN5)
- Istanza 6 -> `enp7s8` (WAN6)

### Coerenza con L3 esistente
La logica esistente rimane invariata:
- Source-based policy routing già presente (tabelle `wan1..wan6`)
- NAT applicato solo sulle WAN fisiche
- Vincoli transit->WAN rispettati (1:1)

Il POC si inserisce sopra il piano L3: ogni processo `mpquic@i` usa la WAN associata tramite bind sorgente UDP.

### Struttura file rilevante
- `cmd/mpquic/main.go`: dataplane TUN <-> QUIC DATAGRAM / stripe dispatch
- `cmd/mpquic/stripe.go`: trasporto UDP stripe + FEC Reed-Solomon + Hybrid ARQ + batch I/O + GSO + socket tuning (~2800 LOC)
- `cmd/mpquic/stripe_gso_linux.go`: UDP GSO (UDP_SEGMENT) — probe, OOB builder, fallback detection (85 LOC)
- `cmd/mpquic/stripe_gso_other.go`: stub GSO per non-Linux (15 LOC)
- `cmd/mpquic/stripe_crypto.go`: cifratura AES-256-GCM + key exchange TLS Exporter (224 LOC)
- `cmd/mpquic/stripe_arq.go`: Hybrid ARQ con NACK selettivo — TX ring buffer, RX gap tracker, NACK encode/decode (269 LOC)
- `cmd/mpquic/stripe_test.go`: test unitari stripe + crypto + ARQ (14 test)
- `deploy/systemd/mpquic@.service`: template servizio
- `deploy/config/client/{1..6}.yaml`
- `deploy/config/server/{1..6}.yaml`
- `scripts/ensure_tun.sh`: creazione/config TUN persistente e idempotente
- `scripts/render_config.sh`: rendering YAML con sostituzione `VPS_PUBLIC_IP`
- `scripts/mpquic-healthcheck.sh`: check strutturato per ruolo (`client|server`) con auto-recovery opzionale
- `scripts/mpquic-lan-routing-check.sh`: validazione/fix routing LAN->tunnel (`check|fix`, target `1..6|all`)
- `scripts/mpquic-update.sh`: aggiornamento automatico (pull, build, stop/start, self-re-exec)
- `scripts/install_client.sh`: installazione lato client
- `scripts/install_server.sh`: installazione lato server


### Parametri configurazione

Per la lista completa dei parametri YAML per istanza, vedere `docs/INSTALLAZIONE_TEST.md` §11.

### Architettura a 3 livelli

#### Livello 1: Multi-link (IMPLEMENTATO)
Un tunnel QUIC per WAN link fisico. 1:1 mapping. Ogni tunnel trasporta tutto il traffico della LAN associata.

```
WAN4 (enp7s6) ──── mpq4 ──── 10.200.4.1/30 ↔ 10.200.4.2/30 (:45004)
WAN5 (enp7s7) ──── mpq5 ──── 10.200.5.1/30 ↔ 10.200.5.2/30 (:45005)
WAN6 (enp7s8) ──── mpq6 ──── 10.200.6.1/30 ↔ 10.200.6.2/30 (:45006)
```

#### Livello 2: Multi-tunnel per link
N tunnel QUIC sullo STESSO link, ciascuno dedicato a una classe di traffico.
Il classificatore è esterno (nftables + fwmark + policy routing).
Tutti i tunnel convergono sulla STESSA porta server e sulla STESSA TUN server.

```
CLIENT (WAN5)                                         SERVER (:45015)
  tun-cr5 (10.200.15.1) ─┐                            ┌─ conn_1 ──┐
  tun-br5 (10.200.15.5) ─┼─── QUIC (diverse src port)─┼─ conn_2 ──┼─ mt5 (10.200.15.0/24)
  tun-df5 (10.200.15.9) ─┘    same WAN, same dst port ┼─ conn_3 ──┘
                                                        │
                                                  routing table:
                                                  .1 → conn_1
                                                  .5 → conn_2
                                                  .9 → conn_3
```

**Server multi-connessione**: accetta N connessioni sulla stessa porta.
Il server mantiene `connectionTable` che mappa `peer_TUN_IP → QUIC_connection`.
Alla connessione iniziale, il client invia un pacchetto di registrazione con il proprio TUN IP.

**Classificazione esterna (nftables)**:
1. Traffico LAN entra su interfacce enp6s20-23, enp7s1-2
2. nftables ispeziona L3/L4 (protocollo, porte, DSCP) e applica fwmark
3. Policy routing: `fwmark X → table class-X → default dev tun-class-X`
4. Ogni TUN ha la propria istanza `mpquic` client
5. NAT MASQUERADE su ogni TUN per gestire traffico di ritorno

#### Livello 3: Multi-path per tunnel (IMPLEMENTATO)
Un singolo tunnel può usare N link per resilienza:
- Bonding: aggregazione bandwidth su più WAN
- Backup: failover automatico
- Duplicazione: pacchetti critici su più link simultaneamente

Implementato con codice applicativo `multipathConn` + UDP Stripe + FEC.
Testato su infra reale: 303 Mbps su 3 link Starlink (12 pipe UDP).

```
WAN4 (enp7s6) ─── 4 pipe stripe ───┐
WAN5 (enp7s7) ─── 4 pipe stripe ───┼─── mp1 ─── 10.200.17.1/24 ↔ 10.200.17.254/24
WAN6 (enp7s8) ─── 4 pipe stripe ───┘
```

### Architettura multipath applicativa (codice esistente, client)

Quando `multipath_enabled: true`, il client non usa più il singolo blocco `bind_ip/remote_addr/remote_port`, ma crea una sessione logica con N path definiti in `multipath_paths`.

**Nota aggiornata (2026-03-02)**: il runtime multipath è stato validato su
infra reale in modalità UDP Stripe + FEC (303 Mbps su 3 WAN, 12 pipe).
Rimangono aperti i lavori di generalizzazione Step 2.5 (9 tunnel VLAN) e
hardening sicurezza del protocollo stripe.

Per ogni elemento `multipath_paths[i]`:
1. risoluzione bind su `bind_ip` (`if:<ifname>` supportato)
2. apertura socket UDP locale dedicata
3. dial QUIC verso `remote_addr:remote_port`
4. registrazione stato path (`up/down`, cooldown, errori, reconnect)

La sessione multipath parte se almeno un path è up. Se uno o più path sono non disponibili (es. WAN senza IPv4), il runtime entra in modalità degradata controllata e avvia recovery path-level in background.


Per i campi di configurazione `multipath_paths` e `stripe_*`, vedere `docs/INSTALLAZIONE_TEST.md` §11.

#### Policy multipath (`multipath_policy`)
- `priority` (default): bilancia priorità/peso/penalità errori
- `failover`: usa preferenzialmente il path con priorità più alta (valore numerico più basso), con fallback sui successivi
- `balanced`: aumenta l'effetto del `weight` per distribuire di più sui path a peso alto

#### Limiti min/max path
- minimo configurabile: 1 path
- minimo operativo: almeno 1 path inizialmente attivo
- massimo: non hard-coded nel runtime; dipende da porte/listener disponibili e risorse host
### Scheduler path-aware

Lo scheduler seleziona il path TX in base a score composto da:
- `priority`
- penalità per `consecutiveFails`
- bonus leggero per `weight`

In caso di errore TX/RX:
- il path viene marcato down
- aumenta la penalità
- applica cooldown progressivo
- parte reconnect in loop con backoff

Se il reconnect riesce, il path rientra nel pool attivo (`path recovered`).


### Telemetria e metriche

Per metriche Prometheus e telemetria path/classe, vedere `docs/METRICS.md`.

### QoS dataplane

Per la documentazione completa QoS (classificazione, policy, orchestrator API), vedere `docs/DATAPLANE_ORCHESTRATOR.md`.

### Tuning operativo consigliato

- **Failover primario/backup**: `priority` molto diversa (es. 10, 100, 200), `weight=1`
- **Bilanciamento leggero**: stessa `priority`, `weight` differenziati (es. 3,2,1)
- **Path costoso ma resiliente**: `priority` più alta (meno preferito) ma sempre disponibile come backup

### Persistenza al boot
- `install_*` abilita `mpquic@1..6.service`
- Ad ogni start, `ExecStartPre` assicura presenza/configurazione TUN
- `Restart=always` mantiene sessioni attive in caso di fault

### SO_BINDTODEVICE — binding interfaccia a livello kernel

Quando il client ha multiple interfacce WAN con IP sorgente diversi, il solo
`bind(IP)` non è sufficiente: il kernel potrebbe non sapere su quale interfaccia
instradare il pacchetto, producendo `sendto: invalid argument` (EINVAL).

La soluzione è `SO_BINDTODEVICE`, una socket option Linux che forza l'interfaccia
di uscita a livello kernel:

```go
syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, "enp7s6")
```

**Quando serve**: obbligo su ogni socket UDP delle pipe stripe quando ci sono
più interfacce WAN sulla stessa macchina.

**Come funziona nel codice**: il campo `bind_ip: if:enp7s6` viene parsato da
`resolveBindIP()`:
1. Il prefisso `if:` identifica il nome interfaccia
2. L'IP viene risolto dall'interfaccia (`getFirstIPv4()`)
3. Il nome interfaccia viene passato a `bindPipeToDevice()` che applica `SO_BINDTODEVICE`
4. Il socket usa `udp4` (non `udp`) per forzare IPv4


### Ottimizzazioni I/O implementate

| Ottimizzazione | Descrizione |
|----------------|-------------|
| UDP Socket Buffer 7 MB | Copre burst 100ms a 500 Mbps su Starlink jitter |
| TX ActivePipes Cache | Slice pre-calcolata per zero-alloc dispatch |
| UDP GSO (client TX) | `UDP_SEGMENT` — N shards in 1 `sendmsg` per pipe |
| sendmmsg (server TX) | `WriteBatch` per N datagrammi in 1 syscall |
| recvmmsg (batch RX) | Fino a 8 datagrammi per syscall (client + server) |

Dettagli implementativi in `docs/ROADMAP_IMPLEMENTAZIONE.md` (Step 4.17-4.24).

### Architettura UDP Stripe + FEC Transport

#### Motivazione: bypass traffic shaping Starlink
Starlink applica un cap di ~80 Mbps per sessione UDP. Con un singolo tunnel QUIC
il throughput è limitato a ~50 Mbps. Il trasporto stripe apre N socket UDP
("pipe") per path, ciascuno trattato da Starlink come sessione indipendente.

#### Schema complessivo dettagliato

Il diagramma seguente mostra il flusso dati completo del trasporto UDP Stripe
con tutte le ottimizzazioni implementate (FEC adattivo, Hybrid ARQ v2,
cifratura AES-256-GCM, batch I/O, socket buffer tuning, TX cache).

```
╔═════════════════════════════════════════════════════════════════════════════════╗
║                          CLIENT (VM MPQUIC)                                     ║
╠═════════════════════════════════════════════════════════════════════════════════╣
║                                                                                 ║
║  ┌──────────┐     ┌──────────────────────────────────────────────────┐          ║
║  │ TUN mp1  │     │            Stripe Engine (stripe.go)             │          ║
║  │ 10.200.  │     │                                                  │          ║
║  │ 17.1/24  │     │  ┌─────────────────┐   ┌──────────────────────┐  │          ║
║  │          │◄───▶│  │  FEC Encoder     │   │   FEC Decoder       │  │          ║
║  │ TUN read │────▶│  │  (Reed-Solomon)  │   │   (Reed-Solomon)    │──┼──▶TUN    ║
║  │          │     │  │                  │   │                     │  │   write  ║
║  │          │     │  │  Mode: adaptive  │   │  Reconstruct if     │  │          ║
║  │          │     │  │  M=0: passthrough│   │  shards missing     │  │          ║
║  │          │     │  │  M>0: K+M shards │   │  (up to M losses)   │  │          ║
║  │          │     │  └────────┬─────────┘   └──────────▲──────────┘  │          ║
║  └──────────┘     │           │                        │             │          ║
║                   │           ▼                        │             │          ║
║                   │  ┌─────────────────┐   ┌──────────┴────────┐     │          ║
║                   │  │  AES-256-GCM    │   │   AES-256-GCM     │     │          ║
║                   │  │  Encrypt        │   │   Decrypt         │     │          ║
║                   │  │                 │   │                   │     │          ║
║                   │  │  Key: TLS 1.3   │   │  Nonce monotono   │     │          ║
║                   │  │  Exporter (PFS) │   │  (anti-replay)    │     │          ║
║                   │  └────────┬────────┘   └──────────▲────────┘     │          ║
║                   │           │                        │             │          ║
║                   │           ▼                        │             │          ║
║                   │  ┌─────────────────┐   ┌──────────┴────────┐     │          ║
║                   │  │  ARQ TX Buffer  │   │  ARQ RX Tracker   │     │          ║
║                   │  │  (ring 4096)    │   │  (bitmap 8192)    │     │          ║
║                   │  │                 │   │                   │     │          ║
║                   │  │  Stores plain-  │   │  Detects gaps,    │     │          ║
║                   │  │  text for re-   │   │  sends NACK every │     │          ║
║                   │  │  encrypt+resend │   │  5ms (rate limit  │     │          ║
║                   │  │  on NACK recv   │   │  30ms, thresh 96) │     │          ║
║                   │  └────────┬────────┘   └──────────▲────────┘     │          ║
║                   │           │                        │             │          ║
║                   │           ▼              Dedup     │             │          ║
║                   │  ┌────────────────────────────────────────┐      │          ║
║                   │  │        Wire Format (16B header)        │      │          ║
║                   │  │  magic(2)+ver(1)+type(1)+session(4)    │      │          ║
║                   │  │  +groupSeq(4)+shardIdx(1)+dataN(1)     │      │          ║
║                   │  │  +dataLen(2) + [encrypted payload]     │      │          ║
║                   │  └────────────────┬───────────────────────┘      │          ║
║                   └───────────────────┼──────────────────────────────┘          ║
║                                       │ ▲                                       ║
║                          TX round-    │ │  RX batch I/O                         ║
║                          robin        │ │  (recvmmsg, 8 dgram/syscall)          ║
║                                       ▼ │                                       ║
║  ┌─── WAN5 (enp7s7, Starlink) ──────────────────────────────┐                   ║
║  │  SO_BINDTODEVICE + Socket Buffers 7 MB (RX+TX)           │                   ║
║  │                                                          │                   ║
║  │  Pipe 0  (UDP :rand) ──────┐                             │                   ║
║  │  Pipe 1  (UDP :rand) ──────┤                             │                   ║
║  │  Pipe 2  (UDP :rand) ──────┤                             │                   ║
║  │  Pipe 3  (UDP :rand) ──────┤                             │                   ║
║  │  Pipe 4  (UDP :rand) ──────┤  ◀── Starlink vede 12       │                   ║
║  │  Pipe 5  (UDP :rand) ──────┤      sessioni UDP           │                   ║
║  │  Pipe 6  (UDP :rand) ──────┤      indipendenti           │                   ║
║  │  Pipe 7  (UDP :rand) ──────┤      (~80 Mbps cap/each)    │                   ║
║  │  Pipe 8  (UDP :rand) ──────┤                             │                   ║
║  │  Pipe 9  (UDP :rand) ──────┤                             │                   ║
║  │  Pipe 10 (UDP :rand) ──────┤                             │                   ║
║  │  Pipe 11 (UDP :rand) ──────┘                             │                   ║
║  └──────────────────────────────────────────────────────────┘                   ║
║                                                                                 ║
║  ┌─── WAN6 (enp7s8, Starlink) ────────────────────────────┐                     ║
║  │  SO_BINDTODEVICE + Socket Buffers 7 MB (RX+TX)         │                     ║
║  │                                                        │                     ║
║  │  Pipe 0..11 (UDP :rand) ── identica struttura ──       │                     ║
║  └────────────────────────────────────────────────────────┘                     ║
║                                                                                 ║
║  Totale: 24 pipe UDP ── 2 path × 12 pipe                                        ║
╚═════════════════════════════════════════════════════════════════════════════════╝
                              │ │                     ▲ ▲
                              │ │    Internet         │ │
                              ▼ ▼    (Starlink LEO)   │ │
╔══════════════════════════════════════════════════════════════════════════════╗
║                     SERVER VPS (172.238.232.223)                             ║
╠══════════════════════════════════════════════════════════════════════════════╣
║                                                                              ║
║  ┌─── UDP Listener :46017 ────────────────────────────────────┐              ║
║  │  Socket Buffers 7 MB (RX+TX) + Batch I/O (recvmmsg)        │              ║
║  │                                                            │              ║
║  │  Riceve da tutte le 24 pipe client su un unico socket      │              ║
║  │  Demultiplex per session ID (ipToUint32 ^ fnv32a(path))    │              ║
║  └───────────────────────────┬────────────────────────────────┘              ║
║                              │                                               ║
║                              ▼                                               ║
║  ┌───────────────────────────────────────────────────────────┐               ║
║  │               Stripe Session (per path)                   │               ║
║  │                                                           │               ║
║  │  ┌─────────────┐  ┌──────────────┐  ┌────────────────┐    │               ║
║  │  │ AES-256-GCM │  │ FEC Decoder  │  │ ARQ RX Tracker │    │               ║
║  │  │ Decrypt     │  │ Reconstruct  │  │ NACK generator │    │               ║
║  │  └──────┬──────┘  └──────┬───────┘  └───────┬────────┘    │               ║
║  │         │               │                    │            │               ║
║  │         ▼               ▼                    ▼            │               ║
║  │  ┌─────────────────────────────────────────────────┐      │               ║
║  │  │  TUN write ──▶ mp1 (10.200.17.254/24)           │      │               ║
║  │  └─────────────────────────────────────────────────┘      │               ║
║  │                                                           │               ║
║  │  ┌─────────────┐  ┌──────────────┐  ┌────────────────┐    │               ║
║  │  │ TUN read    │  │ FEC Encoder  │  │ AES-256-GCM    │    │               ║
║  │  │ mp1 ────────┼─▶│ (adaptive)  ─┼─▶│ Encrypt       ─┼────┼──▶ TX         ║
║  │  └─────────────┘  └──────────────┘  └────────────────┘    │               ║
║  │                                                           │               ║
║  │  TX dispatch: txActivePipes cache (zero-alloc)            │               ║
║  │  Flow-hash FNV-1a (5-tupla) → sessione per flusso TCP     │               ║
║  └───────────────────────────────────────────────────────────┘               ║
║                                                                              ║
╚══════════════════════════════════════════════════════════════════════════════╝
```

##### Legenda componenti

| Componente | Funzione | Dettaglio |
|------------|----------|-----------|  
| **FEC Encoder/Decoder** | Protezione packet loss proattiva | Reed-Solomon K=10 data, M=2 parità. Adaptive: M=0 se loss=0, sale a M=2 se loss >2% |
| **AES-256-GCM** | Cifratura + autenticazione | Chiavi derivate da TLS 1.3 Exporter (PFS per sessione). Nonce monotono anti-replay |
| **ARQ TX Buffer** | Buffer ritrasmissione | Ring buffer 4096 entry (~200ms a 20K pps). Plaintext pronto per re-encrypt |
| **ARQ RX Tracker** | Rilevamento gap | Bitmap circolare 8192 bit. NACK ogni 5ms, rate limit 30ms, soglia 96 seq |
| **Dedup Receiver** | Eliminazione duplicati | `markReceived()` verificato prima della consegna TUN. Drop silenzioso duplicati ARQ |
| **Batch I/O** | Riduzione overhead syscall | `recvmmsg` legge fino a 8 datagrammi per syscall (server RX + client RX) |
| **UDP GSO (client)** | Riduzione syscall TX | `UDP_SEGMENT`: concatena N shards in 1 buffer → 1 `sendmsg`/pipe. Kernel split. Fallback su EIO |
| **sendmmsg (server)** | Riduzione syscall TX | `WriteBatch`: N datagrammi in 1 `sendmmsg`. Per destinazioni diverse (round-robin pipe client) |
| **Socket Buffers 7 MB** | Prevenzione drop kernel | Copre burst fino a 100ms a 500 Mbps (~4700 pacchetti). Richiede sysctl `rmem_max` |
| **TX ActivePipes Cache** | Zero-alloc dispatch | Slice `[]*net.UDPAddr` pre-calcolata, ricostruita solo su REGISTER/keepalive |
| **SO_BINDTODEVICE** | Binding interfaccia kernel | Forza uscita su interfaccia corretta. Necessario con multiple WAN |
| **Flow-hash FNV-1a** | Anti-reordering TCP | Hash sulla 5-tupla → stesso flusso TCP sempre sullo stesso path |

#### Wire Protocol
```
Pacchetto stripe:
  [stripeHdr 16 bytes][shard payload (variabile)]

Header: magic(2) + ver(1) + type(1) + session(4) + groupSeq(4) +
        shardIdx(1) + groupDataN(1) + dataLen(2) = 16 bytes

Tipi: DATA (0x01), PARITY (0x02), REGISTER (0x03), KEEPALIVE (0x04), NACK (0x05)

Pacchetto NACK (type 0x05):
  [stripeHdr 16B][base_seq 4B][bitmap 8B]
  bitmap: 64 bit, bit i=1 → base_seq+i mancante
```

#### FEC Reed-Solomon
- K=10 shards dati (il pacchetto TUN viene copiato in uno shard)
- M=2 shards parità (calcolati da Reed-Solomon)
- Tolleranza: fino al 16.7% di loss per gruppo FEC senza retransmit
- Dipendenza: `github.com/klauspost/reedsolomon`
- Modalità adattiva (`stripe_fec_mode: adaptive`): M effettivo parte da 0 (nessuna parità),
  sale a M configurato se rilevata perdita significativa via feedback keepalive bidirezionale


#### Hybrid ARQ (NACK selettivo)
Ritrasmissione reattiva complementare a FEC. TX ring buffer 4096 entry, RX bitmap 8192 bit.
NACK ogni 5ms (rate limit 30ms). Attivo solo quando effectiveM=0.
Bidirezionale (client + server). Benchmark: +48% su dual Starlink (239 → 354 Mbps).

#### Flow-hash dispatch (server → client)
Il server usa hash FNV-1a sulla 5-tupla IP (srcIP, dstIP, proto, srcPort, dstPort)
per assegnare ogni flusso TCP/UDP a una sessione stripe specifica. Pacchetti dello
stesso flusso percorrono sempre lo stesso link → nessun reordering TCP.

#### Session management
- Session ID: `ipToUint32(tunIP) ^ fnv32a(pathName)` — unico per path
- Keepalive: ogni 5s client→server, server risponde solo per sessioni note
- Timeout: 30s senza RX → close + reconnect
- GC: server rimuove sessioni idle dopo timeout

#### Validità delle scelte architetturali con Stripe (stato attuale)

Le considerazioni fatte su congestion control, cifratura TLS, classi traffico e
multipath **restano valide**, ma con perimetro diverso tra path QUIC e path stripe.

| Tema | Path `transport: quic` | Path `transport: stripe` |
|------|------------------------|--------------------------|
| Congestion control (`bbr`/`cubic`) | **Sì**: governato da QUIC stack | **No**: stripe non usa CC QUIC per pipe |
| Cifratura TLS | **Sì**: TLS 1.3 intrinseco QUIC | **Sì**: AES-256-GCM con chiavi derivate da TLS 1.3 Exporter |
| Classi di traffico dataplane | **Sì** | **Sì** (decisione resta a livello scheduler/classifier) |
| Multipath applicativo | **Sì** | **Sì** (con FEC + pipe multiple per path) |

**Impatti pratici**:
- Su path stripe, il guadagno prestazionale deriva da parallelismo pipe + FEC,
  non dal CC QUIC.
- Le policy QoS per classe (`preferred_paths`, `excluded_paths`, duplication)
  continuano a funzionare in modo trasversale al tipo trasporto.
- **Sicurezza stripe**: AES-256-GCM cifratura + autenticazione per ogni pacchetto UDP.
  Chiavi direzionali derivate da handshake TLS 1.3 effimero (PFS per sessione).
  Nonce monotono per anti-replay. Zero configurazione manuale.
- Metriche sicurezza disponibili lato server: `decrypt_fail` (tentativi decifrazione falliti).

### Limiti deliberati (fase corrente) → risolti in Fase 5
- ~~Nessun endpoint/API di controllo dinamico runtime~~ → **Fase 5a**: Management REST API (`mpquic-mgmt`)
- ~~Nessuna UI per operatori~~ → **Fase 5b**: LuCI app per OpenWrt (`luci-app-mpquic`)
- ~~Nessun auto-tuning~~ → **Fase 5c**: Rule-based + AI/ML decision layer
- Multipath in singola connessione QUIC disponibile in modalità sperimentale (scheduler path-aware con priorità/peso e fail-cooldown)
- TLS server self-signed runtime (POC)

---

## 2. Infrastruttura di deploy

> **REGOLA CRITICA**: mpquic gira SOLO su VM MPQUIC (10.10.11.100) e VPS (172.238.232.223).  
> **OpenWrt (10.10.11.254) è SOLO un router** — esegue mwan3, nftables, LuCI.  
> Non eseguire MAI comandi mpquic, systemctl mpquic@, o diagnostica tunnel su OpenWrt.

---

### 1. Inventario Host

| Host | IP | Ruolo | OS | SSH |
|------|-----|-------|-----|-----|
| **VM MPQUIC** (client) | 10.10.11.100 | Tunnel client, binari mpquic, servizi systemd | Debian 12 (Proxmox VM 200) | `ssh root@10.10.11.100` |
| **OpenWrt** (router) | 10.10.11.254 | Router, mwan3, nftables, VLAN trunk, LuCI | OpenWrt 24.10 x86_64 | `ssh root@10.10.11.254` |
| **VPS Server** | 172.238.232.223 | Tunnel server, binari mpquic server-side | Ubuntu 24.04 | `ssh vps-it-mpquic` (no one-liner, IPS attiva) |
| **Proxmox Host** | 10.10.11.2 | Hypervisor (VM 200, CT 201, CT 202) | Proxmox VE 8.x | `ssh root@10.10.11.2` |
| **Prometheus** | 10.10.11.201 | Monitoraggio metriche | Debian 12 LXC (CT 201) | — |
| **Grafana** | 10.10.11.202 | Dashboard | Debian 12 LXC (CT 202) | — |

---

### 2. Cosa gira DOVE

#### VM MPQUIC (10.10.11.100) — il cuore del sistema

| Componente | Descrizione |
|-----------|-------------|
| `mpquic@{1..6}` | Servizi systemd, 1 tunnel QUIC single-path per WAN (porte 45001-45006) |
| `mpquic@mp1` | Tunnel multipath stripe bonding (WAN5+WAN6, porte 45017/46017) |
| `mpquic@cr4..6, br4..6, df4..6` | 9 tunnel VLAN multi-tunnel-per-link |
| `mpquic-mgmt` | Daemon gestione API REST (:8080) |
| `mpquic-routing.service` | Policy routing per binding WAN |
| `mpquic-watchdog` | Health check + auto-recovery tunnel |
| `wan-watchdog` | Rileva cambio IP DHCP sulle WAN Starlink |
| `systemd-networkd` | Configurazione rete (WAN DHCP, LAN static) |

**Dispositivi TUN** (creati qui, NON su OpenWrt):
- `mpq1`..`mpq6` — tunnel single-path
- `mp1` — tunnel multipath bonding
- `cr4`..`cr6`, `br4`..`br6`, `df4`..`df6` — tunnel multi-class VLAN

**Path repo**: `/opt/mpquic` (produzione), `/opt/SATCOMVAS/src/mpquic` (dev)

#### OpenWrt (10.10.11.254) — solo routing

| Componente | Descrizione |
|-----------|-------------|
| mwan3 | Multi-WAN policy routing + health tracking |
| nftables/fw4 | Firewall, DSCP marking per traffic class |
| VLAN trunk | 9 VLAN per classi di traffico (cr/br/df × WAN4-6) |
| LuCI + rpcd | Interfaccia web, chiama API mpquic-mgmt su VM |

**NON gira su OpenWrt**: mpquic, systemd, tunnel TUN, mpquic-mgmt.

#### VPS Server (172.238.232.223) — endpoint remoto

| Componente | Descrizione |
|-----------|-------------|
| `mpquic@{1..6}` | Server-side dei tunnel QUIC |
| `mpquic@mp1` | Server-side multipath bonding |
| `mpquic@mt1, mt4, mt5, mt6` | Server-side multi-conn per stack VLAN/classi |
| nftables | Firewall con IPS (attenzione: no SSH one-liner) |

Nota: i tunnel `cr/br/df` sulla VM MPQUIC possono essere lasciati spenti se non
usati nella demo; questo non indica un fault se pianificato.

---

### 3. Interfacce di Rete — VM MPQUIC

| Gruppo | Interfacce | Ruolo | IP |
|--------|-----------|-------|-----|
| MGMT | enp6s18, enp6s19 | Management/SSH | 10.10.11.100, 10.10.10.100 |
| LAN transit | enp6s20-23, enp7s1-2 | Collegamento verso OpenWrt | 172.16.{1-6}.1/30 |
| VLAN | enp6s20.17 | Transit dedicato mp1/BOND1 | 172.16.17.1/30 |
| WAN | enp7s3-8 | Uplink Starlink (DHCP, CGNAT) | Dinamico |

#### Mappatura WAN → Interfaccia
| WAN | Interfaccia | Tipo |
|-----|-----------|------|
| WAN1 | enp7s3 | Starlink |
| WAN2 | enp7s4 | Starlink |
| WAN3 | enp7s5 | Starlink |
| WAN4 | enp7s6 | Starlink |
| WAN5 | enp7s7 | Starlink |
| WAN6 | enp7s8 | Starlink |

---

### 4. Topologia di Rete

```
              Internet
                 |
         ┌────────────────┐
         │  VPS Server    │
         │ 172.238.232.223│
         └───────┬────────┘
                 │ QUIC tunnels (UDP)
     ┌───────────┼───────────┐
     │ WAN5      │ WAN6      │ ... WAN1-4
     │ enp7s7    │ enp7s8    │
┌────┴───────────┴───────────┴────┐
│         VM MPQUIC               │
│         10.10.11.100            │
│  TUN: mpq1-6, mp1, cr/br/df     │
│  Services: mpquic@*, mgmt, etc  │
└────────────┬────────────────────┘
             │ LAN transit 172.16.x.0/30
             │ VLAN trunk (enp6s20.17 per mp1)
┌────────────┴────────────────────┐
│         OpenWrt Router          │
│         10.10.11.254            │
│  mwan3, nftables, VLAN, LuCI    │
│  Interfaces: SL1-6, BOND1       │
└────────────┬────────────────────┘
             │ LAN
         Rete locale
```

---

### 5. mwan3 su OpenWrt — Stato attuale

#### Interfacce mwan3
| Interfaccia | Stato | Tracking | Note |
|------------|-------|----------|------|
| SL1-SL4 | OFFLINE | — | WAN 1-4 non attivi |
| SL5 | ONLINE | ping 8.8.8.8 + 1.1.1.1 | Starlink attivo |
| SL6 | ONLINE | ping 8.8.8.8 + 1.1.1.1 | Starlink attivo |
| BOND1 | ONLINE | ping 8.8.8.8 + 1.1.1.1 (interval=30) | Tunnel bonding mp1, uptime 765h+ |

#### Config BOND1 attuale (UCI)
```
mwan3.BOND1=interface
mwan3.BOND1.initial_state='online'
mwan3.BOND1.track_ip='8.8.8.8' '1.1.1.1'
mwan3.BOND1.interval='30'
mwan3.BOND1.down='5'
mwan3.BOND1.up='5'
```

#### Policy mwan3
- **BALANCED**: usa tutti gli SL + BOND1 con pesi/metriche
- **FAILOVER**: failover tra SL e BOND1

---

### 6. Procedure di Deploy

#### Aggiornare mpquic su VM MPQUIC o VPS
```bash
# Su VM MPQUIC:
ssh root@10.10.11.100
sudo /opt/mpquic/scripts/mpquic-update.sh /opt/mpquic

# Su VPS (sessione interattiva, NO one-liner per IPS):
ssh vps-it-mpquic
sudo /opt/mpquic/scripts/mpquic-update.sh /opt/mpquic
```

#### Deployare script OpenWrt
```bash
scp deploy/openwrt/XX-script.sh root@10.10.11.254:/tmp/
ssh root@10.10.11.254 'sh /tmp/XX-script.sh'
```

#### Ricaricare mwan3
```bash
ssh root@10.10.11.254 '/etc/init.d/mwan3 restart'
```

---

### 7. Regole Operative

1. **Mai** eseguire `mpquic`, `systemctl mpquic@*`, `ip link show mpqX` su OpenWrt
2. **Mai** SSH one-liner su VPS (IPS blocca) — usare sessione interattiva
3. I device TUN (`mpq1-6`, `mp1`, `cr/br/df*`) esistono SOLO su VM MPQUIC
4. OpenWrt vede i tunnel come interfacce di rete via transit VLAN, non come device locali
5. Per diagnostica tunnel: `ssh root@10.10.11.100` e poi `ip link`, `wg show`, `journalctl -u mpquic@*`
6. Per diagnostica routing mwan3: `ssh root@10.10.11.254` e poi `mwan3 interfaces`, `mwan3 policies`

---

## 3. Dataplane QoS e orchestratore esterno

Questo documento definisce come configurare il dataplane multipath per QoS applicativa e come interfacciarlo con orchestrator esterno via file policy e Control API locale.

### Obiettivo

Separare:
- **config applicativa MPQUIC** (path WAN, endpoint, TLS, TUN)
- **config dataplane QoS** (classi traffico, classifier, policy scheduler)

Per ambienti orchestrati è raccomandato il file dedicato `dataplane_config_file`.

### Modelli di configurazione supportati

#### 1) File dataplane dedicato (raccomandato)

Nel file applicativo client multipath:

```yaml
role: client
multipath_enabled: true
dataplane_config_file: /etc/mpquic/instances/dataplane.yaml
...
```

Poi in `dataplane.yaml`:

```yaml
default_class: default
classes:
  default:
    scheduler_policy: balanced
    preferred_paths: [wan4, wan5, wan6]
  critical:
    scheduler_policy: failover
    preferred_paths: [wan4, wan5]
    duplicate: true
    duplicate_copies: 2
  bulk:
    scheduler_policy: balanced
    excluded_paths: [wan4]
classifiers:
  - name: voip-sip
    class: critical
    protocol: udp
    dst_ports: ["5060", "10000-20000"]
    dscp: [46]
  - name: telemetry-control
    class: critical
    protocol: tcp
    dst_ports: ["443", "8443"]
  - name: backup-stream
    class: bulk
    protocol: tcp
    dst_ports: ["5001-6000"]
```

#### 2) Dataplane inline nello YAML applicativo

Alternativa valida per POC piccoli:

```yaml
dataplane:
  default_class: default
  classes:
    ...
  classifiers:
    ...
```

#### Precedenza

Se sono presenti sia `dataplane` inline sia `dataplane_config_file`, il runtime usa il file dedicato (`dataplane_config_file`).

### Schema dataplane

#### `default_class`
- classe di fallback quando nessuna regola classifier matcha.

#### `classes.<name>`
- `scheduler_policy`: `priority | failover | balanced`
- `preferred_paths`: lista nomi path da favorire (es. `wan4`)
- `excluded_paths`: path da escludere per la classe
- `duplicate`: abilita duplicazione datagrammi per classe
- `duplicate_copies`: copie inviate su path distinti (2..3)

#### `classifiers[]`
- `name`: etichetta regola
- `class`: classe target
- `protocol`: `udp | tcp | icmp | icmpv6` (opzionale)
- `src_cidrs`, `dst_cidrs`: CIDR IPv4/IPv6 (opzionali)
- `src_ports`, `dst_ports`: porta singola (`"443"`) o range (`"10000-20000"`)
- `dscp`: lista valori DSCP (0..63)

Le regole sono valutate in ordine; il primo match vince.

### Vincoli di validazione

- `default_class` deve esistere in `classes`
- ogni `classifiers[].class` deve esistere in `classes`
- `preferred_paths` / `excluded_paths` devono riferire path presenti in `multipath_paths`
- `scheduler_policy` valido per ogni classe
- `duplicate_copies` clamp a 2..3 quando `duplicate: true`
- CIDR, range porte e DSCP validati a startup

### Pattern QoS consigliati

#### Mission-critical
- classe `critical`
- `scheduler_policy: failover`
- `preferred_paths`: solo WAN più affidabili
- `duplicate: true`, `duplicate_copies: 2`

#### Default business traffic
- classe `default`
- `scheduler_policy: balanced`
- uso di tutti i path disponibili

#### Bulk
- classe `bulk`
- `scheduler_policy: balanced`
- esclusione path costosi/sensibili con `excluded_paths`

### Pattern per orchestrator esterno

#### Stato desiderato (source of truth)
- orchestrator mantiene versioni di `dataplane.yaml` per tenant/sito.

#### Flusso consigliato
1. orchestrator genera nuovo `dataplane.yaml`
2. valida schema e riferimenti path lato control-plane
3. distribuisce file sul nodo MPQUIC in `/etc/mpquic/instances/dataplane.yaml`
4. applica policy via Control API (`/dataplane/reload`) oppure restart controllato istanza
5. verifica log runtime `class telemetry` e `path telemetry`

### Control API locale (implementata)

La Control API è disponibile nel client multipath quando è configurato:

```yaml
control_api_listen: 127.0.0.1:19090
control_api_auth_token: "change-me"
```

Campi consigliati:
- `control_api_listen`: bind locale (`127.0.0.1:<port>`)
- `control_api_auth_token`: token Bearer opzionale ma fortemente consigliato

Endpoint:
- `GET /healthz`: stato processo/API
- `GET /dataplane`: snapshot policy dataplane attiva
- `POST /dataplane/validate`: valida payload dataplane (JSON o YAML) senza applicare
- `POST /dataplane/apply`: valida e applica payload dataplane in runtime
- `POST /dataplane/reload`: ricarica e applica `dataplane_config_file` da disco

Esempio validate:

```bash
curl -sS -X POST \
  -H 'Authorization: Bearer change-me' \
  -H 'Content-Type: application/yaml' \
  --data-binary @/etc/mpquic/instances/dataplane.yaml \
  http://127.0.0.1:19090/dataplane/validate
```

Esempio apply:

```bash
curl -sS -X POST \
  -H 'Authorization: Bearer change-me' \
  -H 'Content-Type: application/yaml' \
  --data-binary @/etc/mpquic/instances/dataplane.yaml \
  http://127.0.0.1:19090/dataplane/apply
```

Esempio reload da file:

```bash
curl -sS -X POST \
  -H 'Authorization: Bearer change-me' \
  http://127.0.0.1:19090/dataplane/reload
```

Sicurezza operativa:
- non esporre la Control API su IP pubblici
- usare sempre token Bearer quando possibile
- limitare accesso con firewall locale/host policy

### Come generare e usare il token (pratico)

#### 1) Genera token strong

Opzione A (openssl):

```bash
TOKEN="$(openssl rand -hex 32)"
echo "$TOKEN"
```

Opzione B (fallback senza openssl):

```bash
TOKEN="$(head -c 32 /dev/urandom | xxd -p -c 256)"
echo "$TOKEN"
```

#### 2) Configura token e bind API

Nel file client multipath dell'istanza (es. `/etc/mpquic/instances/4.yaml.tpl`):

```yaml
control_api_listen: 127.0.0.1:19090
control_api_auth_token: REPLACE_WITH_TOKEN
```

Poi restart istanza:

```bash
sudo systemctl restart mpquic@4.service
```

#### 3) Chiamate API base

```bash
TOKEN="<token_generato>"
BASE="http://127.0.0.1:19090"

curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/healthz"
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/dataplane"
```

#### 4) Valida prima di applicare

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/yaml' \
  --data-binary @/etc/mpquic/instances/dataplane.yaml \
  "$BASE/dataplane/validate"
```

#### 5) Applica policy runtime

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/yaml' \
  --data-binary @/etc/mpquic/instances/dataplane.yaml \
  "$BASE/dataplane/apply"
```

#### 6) Reload da file dedicato

Usa quando hai già aggiornato `dataplane_config_file` su disco:

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  "$BASE/dataplane/reload"
```

#### 7) Verifica post-apply

```bash
journalctl -u mpquic@4.service -n 200 --no-pager | egrep 'class telemetry|path telemetry|dataplane policy applied'
```

#### Errori tipici
- `401 unauthorized`: token errato/mancante.
- `400 ...`: policy non valida (classe/path/DSCP/porte/CIDR).
- timeout curl: API non attiva o bind diverso da `127.0.0.1:19090`.

#### Convenzioni operative
- tenere i nomi path stabili (`wan4`, `wan5`, `wan6`)
- usare classi canoniche (`critical`, `default`, `bulk`)
- evitare regole classifier sovrapposte non necessarie
- versionare i file policy (`dataplane.vNN.yaml`) e mantenere rollback rapido

### Esempio operativo su nodo client

```bash
sudo install -m 0644 /opt/SATCOMVAS/src/mpquic/deploy/config/client/dataplane.yaml /etc/mpquic/instances/dataplane.yaml
sudo cp /opt/SATCOMVAS/src/mpquic/deploy/config/client/multipath-dataplane-file.yaml /etc/mpquic/instances/4.yaml.tpl
sudo systemctl restart mpquic@4.service
journalctl -u mpquic@4.service -n 200 --no-pager | egrep 'path telemetry|class telemetry'
```

### Telemetria e osservabilità

- `path telemetry ...`: stato e contatori per path
- `class telemetry ...`: contatori per classe (`tx_pkts`, `tx_err`, `tx_dups`)

Questo permette a un orchestrator di verificare che le policy QoS siano realmente applicate dopo rollout.

---

## 4. Monitoraggio e metriche

> Versione: 4.2 — Fase 5 Layer 1 + Layer 2  
> Data: 2025-07-15

---

### Indice

1. [Panoramica architettura](#panoramica-architettura)
2. [Configurazione](#configurazione)
3. [Endpoint HTTP](#endpoint-http)
   - [JSON API (`/api/v1/stats`)](#json-api-apiv1stats)
   - [Prometheus (`/metrics`)](#prometheus-metrics)
4. [Struttura JSON — Server](#struttura-json--server)
5. [Struttura JSON — Client](#struttura-json--client)
6. [Catalogo metriche Prometheus](#catalogo-metriche-prometheus)
   - [Metriche globali](#metriche-globali)
   - [Metriche per-session (server)](#metriche-per-session-server)
   - [Metriche per-path (client)](#metriche-per-path-client)
7. [Esempi di scraping Prometheus](#esempi-di-scraping-prometheus)
8. [Query PromQL utili](#query-promql-utili)
9. [Dashboard Grafana — Pannelli suggeriti](#dashboard-grafana--pannelli-suggeriti)

---

### Panoramica architettura

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 3: Consumer (Fase 5.2)                               │
│  ┌───────────┐  ┌────────────┐  ┌─────────────────────────┐ │
│  │  Grafana  │  │ Alerting   │  │  AI/ML Engine (Fase 6)  │ │
│  │ Dashboard │  │ (rules)    │  │  Quality on Demand      │ │
│  └─────┬─────┘  └─────┬──────┘  └──────────┬──────────────┘ │
│        │              │                    │                │
│        └──────────────┼────────────────────┘                │
│                       │                                     │
│              ┌────────▼────────┐                            │
│              │   Prometheus    │                            │
│              │  (scrape ogni   │                            │
│              │   15s–30s)      │                            │
│              └────────┬────────┘                            │
├───────────────────────┼─────────────────────────────────────┤
│  Layer 2: Export      │                                     │
│  ┌────────────────────▼─────────────────────────────────┐   │
│  │  HTTP Server (tunnel_ip:9090)                        │   │
│  │                                                      │   │
│  │  GET /metrics       → Prometheus text exposition     │   │
│  │  GET /api/v1/stats  → JSON strutturato               │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│  Layer 1: Collection (nel dataplane hot path)               │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  sync/atomic counters — zero-alloc, zero-lock        │   │
│  │                                                      │   │
│  │  Server (per-session):                               │   │
│  │    tx/rx bytes, tx/rx pkts, FEC encode/recover,      │   │
│  │    ARQ nack/retx/dup, loss rate, decrypt failures    │   │
│  │                                                      │   │
│  │  Client (per-path):                                  │   │
│  │    tx/rx pkts, stripe tx/rx bytes/pkts,              │   │
│  │    stripe FEC recovered, path alive status           │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

**Principi di design:**
- **Zero-alloc nel hot path**: tutti i contatori usano `sync/atomic` — nessuna allocazione heap durante TX/RX
- **Zero-lock in lettura**: gli snapshot iterano le sessioni sotto `RLock` solo al momento della richiesta HTTP
- **Isolamento di rete**: l'HTTP server è bound all'IP del tunnel (es. `10.200.17.254:9090`), non esposto su interfacce pubbliche
- **Nessun impatto sul throughput**: operazioni atomiche `Add` costano ~1 ns ciascuna

---

### Configurazione

Nel file YAML dell'istanza (`/etc/mpquic/<instance>.yaml`):

```yaml
# Abilita il server metriche sull'IP del tunnel, porta 9090
metrics_listen: auto
```

| Valore | Comportamento |
|--------|---------------|
| `auto` | Bind su `<tunnel_ip>:9090` automaticamente (raccomandato) |
| `10.200.17.254:9091` | Bind su indirizzo:porta specifici |
| *(omesso)* | Metriche disabilitate |

L'opzione `auto` usa l'IP del tunnel configurato come `tun_local` (client) o il primo IP della subnet `tun_cidr` lato server per calcolare l'indirizzo `.254:9090`.

---

### Endpoint HTTP

#### JSON API (`/api/v1/stats`)

| Proprietà | Valore |
|-----------|--------|
| **URL** | `http://<tunnel_ip>:9090/api/v1/stats` |
| **Method** | `GET` |
| **Content-Type** | `application/json` |
| **Uso** | Portali cliente, script di monitoraggio, debug manuale |

```bash
# Esempio
curl -s http://10.200.17.254:9090/api/v1/stats | jq .
```

#### Prometheus (`/metrics`)

| Proprietà | Valore |
|-----------|--------|
| **URL** | `http://<tunnel_ip>:9090/metrics` |
| **Method** | `GET` |
| **Content-Type** | `text/plain; version=0.0.4; charset=utf-8` |
| **Uso** | Scraping Prometheus, integrazione Grafana |

```bash
# Esempio
curl -s http://10.200.17.254:9090/metrics
```

---

### Struttura JSON — Server

Il server espone l'array `sessions[]`, uno per ciascun peer (client) connesso.

```json
{
  "role": "server",
  "version": "4.2",
  "uptime_sec": 14523.45,
  "sessions": [
    {
      "session_id": "a1b2c3d4",
      "peer_ip": "10.150.19.95",
      "pipes": 100,
      "tx_bytes": 892345678,
      "tx_pkts": 612345,
      "rx_bytes": 1234567890,
      "rx_pkts": 845678,
      "fec_mode": "adaptive",
      "adaptive_m": 0,
      "fec_encoded": 12345,
      "fec_recovered": 234,
      "arq_nack_sent": 567,
      "arq_retx_recv": 523,
      "arq_dup_filtered": 89,
      "loss_rate_pct": 0,
      "uptime_sec": 14500.12,
      "decrypt_fail": 0
    },
    {
      "session_id": "e5f6a7b8",
      "peer_ip": "100.64.86.226",
      "pipes": 100,
      "tx_bytes": 456789012,
      "tx_pkts": 312456,
      "rx_bytes": 678901234,
      "rx_pkts": 467890,
      "fec_mode": "adaptive",
      "adaptive_m": 0,
      "fec_encoded": 6789,
      "fec_recovered": 123,
      "arq_nack_sent": 234,
      "arq_retx_recv": 210,
      "arq_dup_filtered": 45,
      "loss_rate_pct": 0,
      "uptime_sec": 14500.12,
      "decrypt_fail": 0
    }
  ],
  "total_tx_bytes": 1349134690,
  "total_rx_bytes": 1913469124,
  "total_tx_pkts": 924801,
  "total_rx_pkts": 1313568
}
```

#### Campi per-session (server)

| Campo | Tipo | Descrizione |
|-------|------|-------------|
| `session_id` | string | ID sessione hex (8 char), identifica il peer client |
| `peer_ip` | string | IP sorgente del peer (WAN IP del client) |
| `pipes` | int | Numero di pipe UDP attive nella sessione |
| `tx_bytes` | uint64 | Byte trasmessi verso il client (counter) |
| `tx_pkts` | uint64 | Pacchetti trasmessi verso il client (counter) |
| `rx_bytes` | uint64 | Byte ricevuti dal client (counter) |
| `rx_pkts` | uint64 | Pacchetti ricevuti dal client (counter) |
| `fec_mode` | string | Modalità FEC: `"none"`, `"static"`, `"adaptive"` |
| `adaptive_m` | int | Parità FEC corrente (M). `0` = FEC inattivo |
| `fec_encoded` | uint64 | Gruppi FEC codificati in TX (counter) |
| `fec_recovered` | uint64 | Gruppi FEC recuperati in RX (counter) |
| `arq_nack_sent` | uint64 | NACK ARQ inviati (counter) — pacchetti mancanti segnalati |
| `arq_retx_recv` | uint64 | Ritrasmissioni ARQ ricevute (counter) |
| `arq_dup_filtered` | uint64 | Pacchetti duplicati filtrati (counter) |
| `loss_rate_pct` | uint32 | Tasso di perdita riportato dal peer (0–100%) |
| `uptime_sec` | float64 | Durata della sessione in secondi |
| `decrypt_fail` | uint64 | Fallimenti di decifratura (counter) — potenziale security issue |

---

### Struttura JSON — Client

Il client espone l'array `paths[]`, uno per ciascun percorso WAN configurato.

```json
{
  "role": "client",
  "version": "4.2",
  "uptime_sec": 14523.45,
  "paths": [
    {
      "name": "wan5",
      "bind_ip": "10.150.19.95",
      "alive": true,
      "tx_bytes": 0,
      "tx_pkts": 612345,
      "rx_bytes": 0,
      "rx_pkts": 845678,
      "stripe_tx_bytes": 892345678,
      "stripe_tx_pkts": 612345,
      "stripe_rx_bytes": 1234567890,
      "stripe_rx_pkts": 845678,
      "stripe_fec_recovered": 234
    },
    {
      "name": "wan6",
      "bind_ip": "100.64.86.226",
      "alive": true,
      "tx_bytes": 0,
      "tx_pkts": 312456,
      "rx_bytes": 0,
      "rx_pkts": 467890,
      "stripe_tx_bytes": 456789012,
      "stripe_tx_pkts": 312456,
      "stripe_rx_bytes": 678901234,
      "stripe_rx_pkts": 467890,
      "stripe_fec_recovered": 123
    }
  ],
  "total_tx_bytes": 1349134690,
  "total_rx_bytes": 1913469124,
  "total_tx_pkts": 924801,
  "total_rx_pkts": 1313568
}
```

#### Campi per-path (client)

| Campo | Tipo | Descrizione |
|-------|------|-------------|
| `name` | string | Nome del path (da YAML, es. `"wan5"`, `"wan6"`) |
| `bind_ip` | string | IP sorgente di bind per questo path |
| `alive` | bool | `true` se il path è attivo e raggiungibile |
| `tx_bytes` | uint64 | Byte trasmessi (livello QUIC tunnel) |
| `tx_pkts` | uint64 | Pacchetti trasmessi (livello QUIC tunnel) |
| `rx_bytes` | uint64 | Byte ricevuti (livello QUIC tunnel) |
| `rx_pkts` | uint64 | Pacchetti ricevuti (livello QUIC tunnel) |
| `stripe_tx_bytes` | uint64 | Byte trasmessi dal motore stripe (omesso se 0) |
| `stripe_tx_pkts` | uint64 | Pacchetti trasmessi dal motore stripe (omesso se 0) |
| `stripe_rx_bytes` | uint64 | Byte ricevuti dal motore stripe (omesso se 0) |
| `stripe_rx_pkts` | uint64 | Pacchetti ricevuti dal motore stripe (omesso se 0) |
| `stripe_fec_recovered` | uint64 | Gruppi FEC recuperati sullo stripe (omesso se 0) |

#### Campi globali (comuni client e server)

| Campo | Tipo | Descrizione |
|-------|------|-------------|
| `role` | string | `"server"` o `"client"` |
| `version` | string | Versione del software (es. `"4.2"`) |
| `uptime_sec` | float64 | Uptime del processo in secondi |
| `total_tx_bytes` | uint64 | Somma byte TX di tutte le session/path |
| `total_rx_bytes` | uint64 | Somma byte RX di tutte le session/path |
| `total_tx_pkts` | uint64 | Somma pacchetti TX di tutte le session/path |
| `total_rx_pkts` | uint64 | Somma pacchetti RX di tutte le session/path |

---

### Catalogo metriche Prometheus

Tutte le metriche hanno il prefisso `mpquic_`.

#### Metriche globali

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `mpquic_uptime_seconds` | gauge | Uptime del processo in secondi |
| `mpquic_tx_bytes_total` | counter | Byte totali trasmessi (tutte le sessioni/path) |
| `mpquic_rx_bytes_total` | counter | Byte totali ricevuti (tutte le sessioni/path) |
| `mpquic_tx_packets_total` | counter | Pacchetti totali trasmessi |
| `mpquic_rx_packets_total` | counter | Pacchetti totali ricevuti |

#### Metriche per-session (server)

Labels: `session` (hex ID), `peer` (IP sorgente)

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `mpquic_session_tx_bytes` | counter | Byte trasmessi verso il peer |
| `mpquic_session_rx_bytes` | counter | Byte ricevuti dal peer |
| `mpquic_session_tx_packets` | counter | Pacchetti trasmessi verso il peer |
| `mpquic_session_rx_packets` | counter | Pacchetti ricevuti dal peer |
| `mpquic_session_pipes` | gauge | Pipe UDP attive nella sessione |
| `mpquic_session_adaptive_m` | gauge | Parità FEC corrente (M). 0 = FEC disattivato |
| `mpquic_session_fec_encoded` | counter | Gruppi FEC codificati (TX) |
| `mpquic_session_fec_recovered` | counter | Gruppi FEC recuperati (RX) |
| `mpquic_session_arq_nack_sent` | counter | NACK ARQ inviati (pacchetti mancanti) |
| `mpquic_session_arq_retx_recv` | counter | Ritrasmissioni ARQ ricevute |
| `mpquic_session_arq_dup_filtered` | counter | Pacchetti duplicati scartati |
| `mpquic_session_loss_rate_pct` | gauge | Tasso di perdita riportato dal peer (0–100) |
| `mpquic_session_uptime_seconds` | gauge | Durata della sessione in secondi |
| `mpquic_session_decrypt_fail` | counter | Fallimenti di decifratura |

#### Metriche per-path (client)

Labels: `path` (nome WAN), `bind` (IP sorgente)

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `mpquic_path_alive` | gauge | Path attivo (1) o inattivo (0) |
| `mpquic_path_tx_packets` | counter | Pacchetti trasmessi su questo path |
| `mpquic_path_rx_packets` | counter | Pacchetti ricevuti su questo path |
| `mpquic_path_stripe_tx_bytes` | counter | Byte stripe trasmessi su questo path |
| `mpquic_path_stripe_rx_bytes` | counter | Byte stripe ricevuti su questo path |
| `mpquic_path_stripe_fec_recovered` | counter | Gruppi FEC stripe recuperati |

---

### Esempi di scraping Prometheus

#### Target statici nel `prometheus.yml`

In un deploy dove Prometheus ha visibilità sulle reti tunnel (10.200.x.x), i target vengono configurati staticamente:

```yaml
# /etc/prometheus/prometheus.yml

global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  # ── Server VPS (172.238.232.223) ───────────────────────
  - job_name: "mpquic-server"
    static_configs:
      - targets:
          - "10.200.17.254:9090"   # mp1 server
        labels:
          instance_name: "mp1"
          site: "vps"

  # ── Client (Proxmox VM 200) ────────────────────────────
  - job_name: "mpquic-client"
    static_configs:
      - targets:
          - "10.200.17.1:9090"     # mp1 client
          - "10.200.14.1:9090"     # cr4 client (WAN4)
          - "10.200.15.1:9090"     # cr5 client (WAN5)
          - "10.200.16.1:9090"     # cr6 client (WAN6)
        labels:
          site: "client"
```

#### Verifica manuale dal container Prometheus

```bash
# Testa la raggiungibilità dei target
curl -s http://10.200.17.254:9090/metrics | head -5
curl -s http://10.200.17.1:9090/metrics | head -5
```

---

### Query PromQL utili

#### Throughput istantaneo (byte/s)

```promql
# TX rate per session (server)
rate(mpquic_session_tx_bytes[1m])

# RX rate totale (client)
rate(mpquic_rx_bytes_total[1m])
```

#### Tasso di perdita per sessione

```promql
# Loss rate corrente
mpquic_session_loss_rate_pct

# Sessioni con loss > 5%
mpquic_session_loss_rate_pct > 5
```

#### Efficacia FEC

```promql
# Ratio recovered/encoded (FEC efficiency)
rate(mpquic_session_fec_recovered[5m])
  / rate(mpquic_session_fec_encoded[5m])

# FEC recovery rate per session
rate(mpquic_session_fec_recovered[5m])
```

#### ARQ health

```promql
# NACK rate → indica packet loss prima del recovery
rate(mpquic_session_arq_nack_sent[5m])

# Retransmission success ratio
rate(mpquic_session_arq_retx_recv[5m])
  / rate(mpquic_session_arq_nack_sent[5m])
```

#### Path health (client)

```promql
# Path attivi per istanza
sum by (instance) (mpquic_path_alive)

# Throughput per path
rate(mpquic_path_stripe_tx_bytes[1m])
```

#### Anomalie e security

```promql
# Decrypt failures (security alarm)
increase(mpquic_session_decrypt_fail[5m]) > 0

# Duplicati anomali (possibile replay attack)
rate(mpquic_session_arq_dup_filtered[5m]) > 100
```

---

### Dashboard Grafana — Pannelli suggeriti

#### Row 1: Overview

| Pannello | Tipo | Query |
|----------|------|-------|
| Uptime | Stat | `mpquic_uptime_seconds` |
| Sessioni attive | Stat | `count(mpquic_session_pipes)` |
| Path attivi | Stat | `sum(mpquic_path_alive)` |
| TX totale | Stat (bytes) | `mpquic_tx_bytes_total` |
| RX totale | Stat (bytes) | `mpquic_rx_bytes_total` |

#### Row 2: Throughput

| Pannello | Tipo | Query |
|----------|------|-------|
| TX rate per session | Time series | `rate(mpquic_session_tx_bytes[1m])` |
| RX rate per session | Time series | `rate(mpquic_session_rx_bytes[1m])` |
| TX rate per path | Time series | `rate(mpquic_path_stripe_tx_bytes[1m])` |
| RX rate per path | Time series | `rate(mpquic_path_stripe_rx_bytes[1m])` |

#### Row 3: Quality (FEC + ARQ + Loss)

| Pannello | Tipo | Query |
|----------|------|-------|
| Loss rate | Time series | `mpquic_session_loss_rate_pct` |
| FEC recovery rate | Time series | `rate(mpquic_session_fec_recovered[5m])` |
| ARQ NACK rate | Time series | `rate(mpquic_session_arq_nack_sent[5m])` |
| Adaptive M | Time series | `mpquic_session_adaptive_m` |

#### Row 4: Infrastructure

| Pannello | Tipo | Query |
|----------|------|-------|
| Pipe per session | Gauge | `mpquic_session_pipes` |
| Path alive map | Table | `mpquic_path_alive` |
| Session uptime | Table | `mpquic_session_uptime_seconds` |
| Decrypt failures | Alert table | `increase(mpquic_session_decrypt_fail[5m])` |

---

### Mappa target attivi

| Istanza | Ruolo | Tunnel IP | Endpoint metriche |
|---------|-------|-----------|-------------------|
| mp1 | server | 10.200.17.254 | `http://10.200.17.254:9090` |
| mp1 | client | 10.200.17.1 | `http://10.200.17.1:9090` |
| cr4 | client | 10.200.14.1 | `http://10.200.14.1:9090` |
| cr5 | client | 10.200.15.1 | `http://10.200.15.1:9090` |
| cr6 | client | 10.200.16.1 | `http://10.200.16.1:9090` |

> **Nota**: I target server per le istanze multi-conn (mt4/mt5/mt6, mpq4-6)
> espongono metriche sugli IP `.254` del rispettivo tunnel ma con `role=server`
> e `sessions[]` invece di `paths[]`.

---

## 5. Metriche Zabbix e TBOX

**Data**: 13 aprile 2026  
**Versione**: 1.0  
**Autori**: Team Engineering SATCOMVAS  
**Classificazione**: Interna / Cliente

---

### Indice

1. [Scopo del Documento](#1-scopo-del-documento)
2. [Architettura di Integrazione](#2-architettura-di-integrazione)
3. [Endpoint Prometheus sulla TBOX](#3-endpoint-prometheus-sulla-tbox)
4. [Catalogo Metriche per Zabbix](#4-catalogo-metriche-per-zabbix)
   - [Metriche Globali (Aggregate)](#41-metriche-globali-aggregate)
   - [Metriche per-Path (per link WAN)](#42-metriche-per-path-per-link-wan)
   - [Metriche per-Session (lato server)](#43-metriche-per-session-lato-server)
   - [Metriche Derivate (calcolate da Zabbix)](#44-metriche-derivate-calcolate-da-zabbix)
5. [Mapping Zabbix — Modalità di Acquisizione](#5-mapping-zabbix--modalità-di-acquisizione)
6. [Trigger e Alert](#6-trigger-e-alert)
7. [Template Zabbix](#7-template-zabbix)
8. [Pacchetto Minimo vs Esteso](#8-pacchetto-minimo-vs-esteso)
9. [Best Practice](#9-best-practice)

---

### 1. Scopo del Documento

Questo documento descrive le metriche di qualità e throughput esposte da ciascuna TBOX tramite endpoint Prometheus, e definisce la strategia di integrazione con un sistema di monitoraggio Zabbix centralizzato.

L'obiettivo è consentire al NOC/SOC di monitorare in tempo reale:

- **Throughput** (bitrate TX/RX) per ciascun link WAN (VSAT, Starlink)
- **Qualità** (loss, FEC recovery, ARQ retransmission)
- **Disponibilità** (stato link attivo/inattivo, failover)
- **Sicurezza** (decrypt failures, duplicati anomali)
- **SLA** (conformità basata su soglie configurabili)

---

### 2. Architettura di Integrazione

```
┌──────────────────────────────────────────────────────────────────┐
│  TBOX (Nave / Sito remoto)                                       │
│                                                                  │
│  ┌─────────────────────────────────────────────┐                 │
│  │  mpquic client (dataplane)                  │                 │
│  │                                             │                 │
│  │  wan5 (VSAT)      wan6 (Starlink)           │                 │
│  │    ▲                  ▲                     │                 │
│  │    │  UDP stripe      │  UDP stripe         │                 │
│  │    └──────┬───────────┘                     │                 │
│  │           │                                 │                 │
│  │  ┌────────▼────────────┐                    │                 │
│  │  │  Metrics HTTP       │                    │                 │
│  │  │  <tun_ip>:9090      │                    │                 │
│  │  │                     │                    │                 │
│  │  │  GET /metrics  ←────┼── Prometheus text  │                 │
│  │  │  GET /api/v1/stats ─┼── JSON             │                 │
│  │  └─────────────────────┘                    │                 │
│  └─────────────────────────────────────────────┘                 │
└──────────────────────┬───────────────────────────────────────────┘
                       │  Tunnel IP / ZeroTier / VPN
                       │
┌──────────────────────▼───────────────────────────────────────────┐
│  NOC / Data Center                                               │
│                                                                  │
│  ┌──────────────┐     ┌──────────────┐     ┌──────────────────┐ │
│  │   Zabbix     │────▶│  HTTP Agent  │────▶│ /metrics         │ │
│  │   Server     │     │  (scrape)    │     │ endpoint TBOX    │ │
│  │              │     └──────────────┘     └──────────────────┘ │
│  │              │                                               │
│  │  ┌────────┐  │     ┌──────────────┐                          │
│  │  │ Items  │◀─┼─────│ Preprocessing│                          │
│  │  │Triggers│  │     │ Prometheus   │                          │
│  │  │ Graphs │  │     │ pattern      │                          │
│  │  └────────┘  │     └──────────────┘                          │
│  └──────────────┘                                               │
└─────────────────────────────────────────────────────────────────┘
```

#### Flusso dati

1. **TBOX** espone metriche Prometheus su `<tunnel_ip>:9090/metrics` (bind su interfaccia tunnel, non pubblico)
2. **Zabbix HTTP Agent** esegue scrape periodico dell'endpoint (intervallo consigliato: 30s)
3. Un **item master** acquisisce il payload grezzo in formato Prometheus text exposition
4. **Dependent items** con preprocessing Prometheus pattern estraggono le singole metriche
5. **Trigger** valutano soglie e generano alert

---

### 3. Endpoint Prometheus sulla TBOX

| Proprietà | Valore |
|-----------|--------|
| **URL** | `http://<tunnel_ip>:9090/metrics` |
| **Metodo** | `GET` |
| **Content-Type** | `text/plain; version=0.0.4; charset=utf-8` |
| **Autenticazione** | Nessuna (protetto da isolamento rete tunnel) |
| **Prefisso metriche** | `mpquic_` |
| **Frequenza scrape consigliata** | 30s |

**Esempio di output:**

```
# HELP mpquic_uptime_seconds Process uptime in seconds
# TYPE mpquic_uptime_seconds gauge
mpquic_uptime_seconds 14523.45
# HELP mpquic_tx_bytes_total Total bytes transmitted
# TYPE mpquic_tx_bytes_total counter
mpquic_tx_bytes_total 1349134690
# HELP mpquic_path_alive Path is alive (1) or down (0)
# TYPE mpquic_path_alive gauge
mpquic_path_alive{path="wan5",bind="10.150.19.95"} 1
mpquic_path_alive{path="wan6",bind="100.64.86.226"} 1
# HELP mpquic_path_stripe_tx_bytes Stripe bytes transmitted per path
# TYPE mpquic_path_stripe_tx_bytes counter
mpquic_path_stripe_tx_bytes{path="wan5",bind="10.150.19.95"} 892345678
mpquic_path_stripe_tx_bytes{path="wan6",bind="100.64.86.226"} 456789012
```

---

### 4. Catalogo Metriche per Zabbix

#### 4.1 Metriche Globali (Aggregate)

Queste metriche rappresentano il servizio TBOX nel suo complesso, indipendentemente dal link in uso.

| Metrica Prometheus | Tipo | Unità | Zabbix Item Key | Descrizione |
|---|---|---|---|---|
| `mpquic_uptime_seconds` | gauge | secondi | `mpquic.uptime` | Uptime processo mpquic |
| `mpquic_tx_bytes_total` | counter | byte | `mpquic.tx.bytes.total` | Byte totali trasmessi (tutte path/sessioni) |
| `mpquic_rx_bytes_total` | counter | byte | `mpquic.rx.bytes.total` | Byte totali ricevuti |
| `mpquic_tx_packets_total` | counter | pacchetti | `mpquic.tx.pkts.total` | Pacchetti totali trasmessi |
| `mpquic_rx_packets_total` | counter | pacchetti | `mpquic.rx.pkts.total` | Pacchetti totali ricevuti |

#### 4.2 Metriche per-Path (per link WAN)

Queste metriche sono disponibili **lato client** con label `path` che identifica il link WAN.

Nella configurazione TBOX standard:
- `path="wan5"` → **VSAT** (o LTE/5G nel progetto TRINA)
- `path="wan6"` → **Starlink**

| Metrica Prometheus | Tipo | Unità | Zabbix Item Key (wan5/VSAT) | Zabbix Item Key (wan6/Starlink) | Descrizione |
|---|---|---|---|---|---|
| `mpquic_path_alive` | gauge | 0/1 | `mpquic.path.wan5.alive` | `mpquic.path.wan6.alive` | Link attivo/inattivo |
| `mpquic_path_tx_packets` | counter | pkt | `mpquic.path.wan5.tx.pkts` | `mpquic.path.wan6.tx.pkts` | Pacchetti TX per path |
| `mpquic_path_rx_packets` | counter | pkt | `mpquic.path.wan5.rx.pkts` | `mpquic.path.wan6.rx.pkts` | Pacchetti RX per path |
| `mpquic_path_stripe_tx_bytes` | counter | byte | `mpquic.path.wan5.stripe.tx.bytes` | `mpquic.path.wan6.stripe.tx.bytes` | Byte stripe TX per path |
| `mpquic_path_stripe_rx_bytes` | counter | byte | `mpquic.path.wan5.stripe.rx.bytes` | `mpquic.path.wan6.stripe.rx.bytes` | Byte stripe RX per path |
| `mpquic_path_stripe_tx_packets` | counter | pkt | `mpquic.path.wan5.stripe.tx.pkts` | `mpquic.path.wan6.stripe.tx.pkts` | Pacchetti stripe TX per path |
| `mpquic_path_stripe_rx_packets` | counter | pkt | `mpquic.path.wan5.stripe.rx.pkts` | `mpquic.path.wan6.stripe.rx.pkts` | Pacchetti stripe RX per path |
| `mpquic_path_stripe_fec_recovered` | counter | gruppi | `mpquic.path.wan5.fec.recovered` | `mpquic.path.wan6.fec.recovered` | Gruppi FEC recuperati per path |

#### 4.3 Metriche per-Session (lato server)

Queste metriche sono disponibili **lato server** con label `session` e `peer`.

| Metrica Prometheus | Tipo | Unità | Zabbix Item Key | Descrizione |
|---|---|---|---|---|
| `mpquic_session_tx_bytes` | counter | byte | `mpquic.session.tx.bytes[{#SESSION}]` | Byte TX verso il peer |
| `mpquic_session_rx_bytes` | counter | byte | `mpquic.session.rx.bytes[{#SESSION}]` | Byte RX dal peer |
| `mpquic_session_tx_packets` | counter | pkt | `mpquic.session.tx.pkts[{#SESSION}]` | Pacchetti TX |
| `mpquic_session_rx_packets` | counter | pkt | `mpquic.session.rx.pkts[{#SESSION}]` | Pacchetti RX |
| `mpquic_session_pipes` | gauge | count | `mpquic.session.pipes[{#SESSION}]` | Pipe UDP attive |
| `mpquic_session_fec_encoded` | counter | gruppi | `mpquic.session.fec.encoded[{#SESSION}]` | Gruppi FEC codificati |
| `mpquic_session_fec_recovered` | counter | gruppi | `mpquic.session.fec.recovered[{#SESSION}]` | Gruppi FEC recuperati |
| `mpquic_session_arq_nack_sent` | counter | NACK | `mpquic.session.arq.nack[{#SESSION}]` | NACK ARQ inviati |
| `mpquic_session_arq_retx_recv` | counter | pkt | `mpquic.session.arq.retx[{#SESSION}]` | Ritrasmissioni ARQ ricevute |
| `mpquic_session_arq_dup_filtered` | counter | pkt | `mpquic.session.arq.dup[{#SESSION}]` | Duplicati filtrati |
| `mpquic_session_loss_rate_pct` | gauge | % | `mpquic.session.loss.pct[{#SESSION}]` | Tasso di perdita (0–100) |
| `mpquic_session_uptime_seconds` | gauge | sec | `mpquic.session.uptime[{#SESSION}]` | Durata sessione |
| `mpquic_session_decrypt_fail` | counter | errori | `mpquic.session.decrypt.fail[{#SESSION}]` | Fallimenti decifratura |
| `mpquic_session_adaptive_m` | gauge | M | `mpquic.session.fec.adaptive_m[{#SESSION}]` | Parità FEC corrente |

#### 4.4 Metriche Derivate (calcolate da Zabbix)

Queste metriche sono calcolate da Zabbix usando **preprocessing** sui counter raw.

| Zabbix Item Key | Formula | Unità | Descrizione |
|---|---|---|---|
| `mpquic.path.wan5.stripe.tx.rate` | `change(mpquic.path.wan5.stripe.tx.bytes) / interval * 8` | bps | Bitrate TX istantaneo VSAT |
| `mpquic.path.wan5.stripe.rx.rate` | `change(mpquic.path.wan5.stripe.rx.bytes) / interval * 8` | bps | Bitrate RX istantaneo VSAT |
| `mpquic.path.wan6.stripe.tx.rate` | `change(mpquic.path.wan6.stripe.tx.bytes) / interval * 8` | bps | Bitrate TX istantaneo Starlink |
| `mpquic.path.wan6.stripe.rx.rate` | `change(mpquic.path.wan6.stripe.rx.bytes) / interval * 8` | bps | Bitrate RX istantaneo Starlink |
| `mpquic.agg.tx.rate` | `change(mpquic.tx.bytes.total) / interval * 8` | bps | Bitrate TX aggregato |
| `mpquic.agg.rx.rate` | `change(mpquic.rx.bytes.total) / interval * 8` | bps | Bitrate RX aggregato |
| `mpquic.session.fec.efficiency[{#S}]` | `fec_recovered / fec_encoded` | ratio | Efficienza FEC |
| `mpquic.session.arq.retx.ratio[{#S}]` | `arq_retx_recv / tx_packets` | ratio | Ratio ritrasmissioni |

---

### 5. Mapping Zabbix — Modalità di Acquisizione

#### Opzione A: HTTP Agent + Prometheus Preprocessing (Consigliata)

Questa è la modalità più semplice e nativa in Zabbix 6.0+.

**Configurazione:**

1. **Item master** (tipo: HTTP Agent):
   - URL: `http://<tunnel_ip>:9090/metrics`
   - Tipo informazione: Text
   - Intervallo: 30s
   - Key: `mpquic.prometheus.raw`

2. **Dependent items** (uno per ogni metrica):
   - Tipo: Dependent item
   - Master item: `mpquic.prometheus.raw`
   - Preprocessing: Prometheus pattern
   - Pattern esempio: `mpquic_path_alive{path="wan5"}`

**Esempio preprocessing Prometheus pattern:**

| Item | Prometheus Pattern | Tipo risultato |
|------|-------------------|----------------|
| Path VSAT alive | `mpquic_path_alive{path="wan5"}` | Numeric (unsigned) |
| Path Starlink alive | `mpquic_path_alive{path="wan6"}` | Numeric (unsigned) |
| Stripe TX bytes VSAT | `mpquic_path_stripe_tx_bytes{path="wan5"}` | Numeric (unsigned) |
| Stripe TX bytes Starlink | `mpquic_path_stripe_tx_bytes{path="wan6"}` | Numeric (unsigned) |
| Loss rate sessione | `mpquic_session_loss_rate_pct{session="<id>"}` | Numeric (float) |

#### Opzione B: Prometheus → Zabbix Bridge (ambienti grandi)

Se esiste già un Prometheus centrale:
1. Prometheus scrape le TBOX
2. Un adapter/exporter push verso Zabbix trapper items
3. Utile per flotte > 50 TBOX con Prometheus già operativo

#### Opzione C: JSON API (alternativa)

Zabbix HTTP Agent su `http://<tunnel_ip>:9090/api/v1/stats` con JSONPath preprocessing. Meno elegante ma utile se il formato Prometheus non è supportato dalla versione Zabbix in uso.

---

### 6. Trigger e Alert

#### Trigger per Link (per-path)

| Nome Trigger | Espressione | Severità | Note |
|---|---|---|---|
| VSAT link down | `last(/host/mpquic.path.wan5.alive)=0` | High | Link VSAT non raggiungibile |
| Starlink link down | `last(/host/mpquic.path.wan6.alive)=0` | High | Link Starlink non raggiungibile |
| Entrambi i link down | `last(/host/mpquic.path.wan5.alive)=0 and last(/host/mpquic.path.wan6.alive)=0` | Disaster | Nessuna connettività WAN |
| VSAT throughput zero | `last(/host/mpquic.path.wan5.stripe.tx.rate)=0 and last(/host/mpquic.path.wan5.alive)=1` | Warning | Link attivo ma nessun traffico |
| Starlink throughput zero | `last(/host/mpquic.path.wan6.stripe.tx.rate)=0 and last(/host/mpquic.path.wan6.alive)=1` | Warning | Link attivo ma nessun traffico |

#### Trigger per Sessione (server-side)

| Nome Trigger | Espressione | Severità | Note |
|---|---|---|---|
| Loss elevata | `last(/host/mpquic.session.loss.pct[{#SESSION}])>5` | High | Perdita > 5% per sessione |
| Loss warning | `last(/host/mpquic.session.loss.pct[{#SESSION}])>2` | Warning | Perdita > 2% |
| Decrypt failure | `change(/host/mpquic.session.decrypt.fail[{#SESSION}])>0` | High | Fallimento decifratura — possibile anomalia sicurezza |
| ARQ retransmission elevata | `change(/host/mpquic.session.arq.retx[{#SESSION}]) / change(/host/mpquic.session.tx.pkts[{#SESSION}]) > 0.03` | Warning | Ritrasmissioni > 3% TX |
| FEC overload | `last(/host/mpquic.session.fec.adaptive_m[{#SESSION}])>5` | Information | FEC parità alta — canale degradato |
| Duplicati anomali | `change(/host/mpquic.session.arq.dup[{#SESSION}])>100` | Warning | Possibile replay o loop |

#### Trigger Globali

| Nome Trigger | Espressione | Severità | Note |
|---|---|---|---|
| Processo MPQUIC down | `nodata(/host/mpquic.uptime,120s)=1` | Disaster | Nessun dato per 2 minuti |
| Uptime reset (riavvio) | `change(/host/mpquic.uptime)<0` | Information | Il processo è stato riavviato |

#### Soglie Indicative per Ambiente Satellitare

| Parametro | VSAT (tipico) | Starlink (tipico) | Note |
|---|---|---|---|
| RTT atteso | 550–700 ms | 25–60 ms | Geostationary vs LEO |
| Packet loss fisiologico | 0.1–1% | 0.01–0.5% | Dipendente da condizioni meteo |
| Throughput tipico down | 2–20 Mbps | 50–250 Mbps | Variabile per piano e congestione |
| Throughput tipico up | 1–5 Mbps | 10–40 Mbps | VSAT spesso asimmetrico |
| Jitter atteso | 20–80 ms | 5–30 ms | |

---

### 7. Template Zabbix

Il template XML pronto per l'import è disponibile in:

```
zabbix/zbx_template_mpquic_tbox.xml
```

Il template include:
- 1 item master HTTP Agent per scrape `/metrics`
- Items dependent per tutte le metriche globali, per-path (VSAT/Starlink), e per-session
- Items calcolati per bitrate derivati
- Trigger per link down, loss, decrypt failure, processo down
- Macros configurabili per IP endpoint e soglie

#### Macros del Template

| Macro | Default | Descrizione |
|---|---|---|
| `{$MPQUIC.ENDPOINT}` | `http://{HOST.CONN}:9090/metrics` | URL endpoint metriche |
| `{$MPQUIC.SCRAPE.INTERVAL}` | `30s` | Intervallo di scrape |
| `{$MPQUIC.LOSS.WARN}` | `2` | Soglia loss warning (%) |
| `{$MPQUIC.LOSS.HIGH}` | `5` | Soglia loss high (%) |
| `{$MPQUIC.NODATA.TIMEOUT}` | `120s` | Timeout no-data per processo down |
| `{$MPQUIC.WAN.VSAT}` | `wan5` | Nome path VSAT |
| `{$MPQUIC.WAN.STARLINK}` | `wan6` | Nome path Starlink |

---

### 8. Pacchetto Minimo vs Esteso

#### Pacchetto Minimo (10 KPI — monitoraggio operativo base)

| # | Metrica | Ambito | Motivo |
|---|---------|--------|--------|
| 1 | `mpquic_path_alive` (wan5) | per-link | Disponibilità VSAT |
| 2 | `mpquic_path_alive` (wan6) | per-link | Disponibilità Starlink |
| 3 | `mpquic_path_stripe_tx_bytes` (wan5) | per-link | Volume TX VSAT |
| 4 | `mpquic_path_stripe_tx_bytes` (wan6) | per-link | Volume TX Starlink |
| 5 | `mpquic_path_stripe_rx_bytes` (wan5) | per-link | Volume RX VSAT |
| 6 | `mpquic_path_stripe_rx_bytes` (wan6) | per-link | Volume RX Starlink |
| 7 | `mpquic_tx_bytes_total` | globale | Throughput aggregato TX |
| 8 | `mpquic_rx_bytes_total` | globale | Throughput aggregato RX |
| 9 | `mpquic_session_loss_rate_pct` | sessione | Qualità servizio |
| 10 | `mpquic_uptime_seconds` | globale | Salute processo |

#### Pacchetto Esteso (aggiuntivi per troubleshooting e SLA)

| # | Metrica | Motivo |
|---|---------|--------|
| 11 | `mpquic_session_fec_recovered` | Efficacia FEC — quanti pacchetti recuperati |
| 12 | `mpquic_session_fec_encoded` | Volume codifica FEC |
| 13 | `mpquic_session_arq_nack_sent` | Segnale di perdita pre-recovery |
| 14 | `mpquic_session_arq_retx_recv` | Volume ritrasmissioni |
| 15 | `mpquic_session_arq_dup_filtered` | Duplicati — indicatore anomalia |
| 16 | `mpquic_session_decrypt_fail` | Security — fallimenti crypto |
| 17 | `mpquic_session_pipes` | Numero pipe attive |
| 18 | `mpquic_session_adaptive_m` | Stato FEC adattivo |
| 19 | `mpquic_path_stripe_fec_recovered` | FEC recovery per-path |
| 20 | Bitrate derivati (calculated) | Throughput in bps per dashboard |

---

### 9. Best Practice

#### Label e Cardinalità

- Le label `path` e `session` hanno cardinalità bassa e controllata (2 path, poche sessioni)
- **Non** inserire in Zabbix item per ogni combinazione IP sorgente — usare solo le label strutturali
- Per flotte grandi (>50 TBOX), valutare Zabbix Proxy dedicato per evitare saturazione del server centrale

#### Retention

| Tipo dato | Retention suggerita |
|-----------|-------------------|
| Raw values | 7 giorni |
| Trend (hourly) | 365 giorni |
| Events/Alert | 180 giorni |

#### Sicurezza

- L'endpoint metriche è esposto **solo** sull'IP tunnel (rete privata)
- Zabbix accede via rete tunnel/ZeroTier — **non** esporre su interfacce pubbliche
- In ambienti con requisiti NIS2, valutare TLS mutual auth tra Zabbix e endpoint

#### Naming Convention Host Zabbix

```
Template:  TBOX-MPQUIC-<site>-<tbox_id>
Esempio:   TBOX-MPQUIC-NAVE01-001
Gruppi:    TBOX / Satellite / MPQUIC
```

#### Intervallo di Scrape

- **30 secondi**: bilanciamento ottimale tra reattività e carico
- **15 secondi**: per ambienti critici con SLA stringenti
- **60 secondi**: per flotte molto grandi (>100 TBOX) per ridurre carico server Zabbix

---

## 6. Installazione e configurazione

### 1) Prerequisiti (entrambi i nodi)
```bash
sudo apt-get update
sudo apt-get install -y iproute2 systemd ca-certificates golang-go
```
Verifica:
```bash
go version
systemctl --version
ip -V
```

### 2) Build binario
```bash
cd /opt/SATCOMVAS/src/mpquic
make build
ls -l bin/mpquic
```

### 3) Installazione lato SERVER (VPS)
```bash
cd /opt/SATCOMVAS/src/mpquic
sudo ./scripts/install_server.sh
```
Verifica file:
```bash
ls -l /etc/systemd/system/mpquic@.service
ls -l /etc/mpquic/instances/{1..6}.yaml.tpl
cat /etc/mpquic/global.env
```

### 4) Installazione lato CLIENT (VM MPQUIC)
```bash
cd /opt/SATCOMVAS/src/mpquic
sudo ./scripts/install_client.sh
```
Verifica file:
```bash
ls -l /etc/mpquic/instances/{1..6}.yaml.tpl
```

### 4.1) Configurazione WAN con systemd-networkd (per-interfaccia)

#### Problema

Le WAN usano DHCP per ottenere l'IP dal modem collegato (Starlink, terrestre, LTE).
In ambienti virtualizzati (Proxmox/VirtIO), quando si scollega e ricollega un cavo
su una diversa porta fisica (es. da modem terrestre a modem Starlink), la NIC
virtuale **non perde il carrier** — il DHCP client non sa che deve fare un nuovo
DISCOVER e mantiene il lease vecchio (rete sbagliata). L'unico rimedio senza
watchdog sarebbe un reboot.

#### Soluzione: configurazione per-WAN + wan-watchdog

Due componenti:

1. **File `.network` individuali per WAN** — sostituiscono il singolo file condiviso.
   Ogni WAN ha la propria configurazione con `RouteMetric` dedicata, `KeepConfiguration=no`
   per rilascio IP immediato su reconfigure, e `ClientIdentifier=mac` per DHCP robusto.

2. **`wan-watchdog.service`** — daemon che ogni 15s pinga il gateway DHCP di ogni WAN.
   Se il gateway diventa irraggiungibile per 4 check consecutivi (60s), forza
   `networkctl reconfigure` sull'interfaccia per triggerare un nuovo DHCP DISCOVER.

#### 4.1.1) Installazione configurazione di rete per-WAN

I file di configurazione sono in `deploy/networkd/wan/`:

| File | Interfaccia | RouteMetric | Note |
|------|-------------|-------------|------|
| `10-wan1.network` | enp7s3 | 101 | WAN1 |
| `11-wan2.network` | enp7s4 | 102 | WAN2 |
| `12-wan3.network` | enp7s5 | 103 | WAN3 |
| `13-wan4.network` | enp7s6 | 104 | WAN4 (fibra/terrestre) |
| `14-wan5.network` | enp7s7 | 105 | WAN5 (Starlink #1) |
| `15-wan6.network` | enp7s8 | 106 | WAN6 (Starlink #2) |

Esempio contenuto (`14-wan5.network`):
```ini
# WAN5 — enp7s7 (Starlink #1)
[Match]
Name=enp7s7

[Network]
DHCP=yes
IPv6AcceptRA=no
LinkLocalAddressing=no
KeepConfiguration=no

[DHCP]
RouteMetric=105
UseDNS=no
UseRoutes=yes
SendRelease=yes
ClientIdentifier=mac

[Link]
RequiredForOnline=no
```

Opzioni chiave:
- **`KeepConfiguration=no`** — rimuove l'IP vecchio immediatamente su reconfigure
- **`SendRelease=yes`** — invia DHCP RELEASE prima del nuovo DISCOVER
- **`ClientIdentifier=mac`** — identifica il client per MAC (più robusto del DUID condiviso)
- **`RequiredForOnline=no`** — le WAN senza modem non bloccano il boot
- **`RouteMetric` diverso** — evita conflitti nella routing table tra WAN

Deploy:
```bash
# Rimuovi il vecchio file condiviso (se presente)
sudo rm -f /etc/systemd/network/10-wan.network

# Installa i file per-WAN
sudo cp deploy/networkd/wan/*.network /etc/systemd/network/

# Ricarica configurazione
sudo networkctl reload
```

Verifica:
```bash
# Ogni WAN deve mostrare il proprio Network File
networkctl status enp7s7  # → Network File: /etc/systemd/network/14-wan5.network
networkctl status enp7s8  # → Network File: /etc/systemd/network/15-wan6.network
```

#### 4.1.2) Installazione wan-watchdog (auto-recovery DHCP)

Il watchdog rileva automaticamente quando il gateway DHCP di una WAN diventa
irraggiungibile e forza un DHCP re-discover, senza necessità di reboot.

**Flusso operativo:**
```
Cable swap → gateway vecchio irraggiungibile → 4 ping falliti (60s)
  → networkctl reconfigure → DHCP DISCOVER → nuovo IP dal modem collegato
  → WAN operativa (~70s totale)
```

Installazione:
```bash
# Script
sudo cp scripts/wan-watchdog.sh /usr/local/bin/
sudo chmod +x /usr/local/bin/wan-watchdog.sh

# Service systemd
sudo cp deploy/systemd/wan-watchdog.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now wan-watchdog.service
```

Verifica:
```bash
sudo systemctl status wan-watchdog.service
# Deve mostrare: Active: active (running)

# Log del watchdog
journalctl -u wan-watchdog.service -f
```

Esempio di log durante un cable swap:
```
wan-watchdog: enp7s7: gateway 10.150.19.1 UNREACHABLE (1/4)
wan-watchdog: enp7s7: gateway 10.150.19.1 UNREACHABLE (2/4)
wan-watchdog: enp7s7: gateway 10.150.19.1 UNREACHABLE (3/4)
wan-watchdog: enp7s7: gateway 10.150.19.1 UNREACHABLE (4/4)
wan-watchdog: enp7s7: *** RECONFIGURE *** reason: gateway 10.150.19.1 unreachable for 60s
wan-watchdog: enp7s7: old gateway=10.150.19.1, old addr=10.150.19.116
wan-watchdog: enp7s7: new gateway=100.64.0.1, new addr=100.110.241.142
```

#### 4.1.3) Riconfigurazione manuale WAN (wan-reconfigure.sh)

Per forzare un DHCP re-discover immediato (senza attendere il watchdog):

```bash
# Singola interfaccia
sudo /opt/mpquic/scripts/wan-reconfigure.sh enp7s7

# Tutte le WAN
sudo /opt/mpquic/scripts/wan-reconfigure.sh
```

#### 4.1.4) Configurazione watchdog (opzionale)

I parametri del watchdog sono configurabili via variabili d'ambiente nel service file.
Decommentare le righe desiderate in `/etc/systemd/system/wan-watchdog.service`:

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `WAN_INTERFACES` | `enp7s3 ... enp7s8` | Interfacce da monitorare |
| `CHECK_INTERVAL` | `15` | Secondi tra un check e l'altro |
| `FAIL_THRESHOLD` | `4` | Check falliti prima di reconfigure (4 × 15s = 60s) |
| `COOLDOWN` | `120` | Secondi minimo tra due reconfigure sulla stessa iface |
| `PING_TIMEOUT` | `3` | Timeout singolo ping in secondi |

Per applicare le modifiche: `sudo systemctl daemon-reload && sudo systemctl restart wan-watchdog`

### 4.2) Persistenza route tunnel TUN (systemd-networkd)

#### Problema

Ogni volta che `mpquic@N.service` si avvia o riconnette, `ensure_tun.sh` (in `ExecStartPre`)
ricrea il device TUN con un nuovo ifindex kernel. Il kernel elimina automaticamente tutte le
route legate al vecchio ifindex. La route `default dev mpqN table wanN` sparisce e il traffico
dei tunnel singolo-link esce dalla routing table main invece di usare la WAN corretta, causando
intermittente perdita di connettività su tutti i link gestiti dai tunnel `mpq1–mpq6`.

#### Soluzione

Creare file `.network` in `/etc/systemd/network/` per ogni device TUN `mpqN`.
`systemd-networkd` monitora lo stato dei device e ripristina automaticamente la route
dichiarata ogni volta che il device appare o acquisisce carrier — senza modificare alcun
service file esistente.

Questo replica il pattern già usato da `mp1` / `BOND1` tramite `27-bd1.network`.

#### Mapping TUN → tabella di routing

| File | Device | Tabella (`/etc/iproute2/rt_tables`) |
|------|--------|--------------------------------------|
| `50-mpq1.network` | mpq1 | 100 (wan1) |
| `51-mpq2.network` | mpq2 | 101 (wan2) |
| `52-mpq3.network` | mpq3 | 102 (wan3) |
| `53-mpq4.network` | mpq4 | 103 (wan4) |
| `54-mpq5.network` | mpq5 | 104 (wan5) |
| `55-mpq6.network` | mpq6 | 105 (wan6) |

#### 4.2.1) Installazione

I file sono in `deploy/networkd/tun/`:

```bash
# Installa i file .network per i device TUN
sudo cp deploy/networkd/tun/5*.network /etc/systemd/network/

# Ricarica networkd — NON fare restart, preserva lo stato delle altre interfacce
sudo networkctl reload
```

#### 4.2.2) Struttura dei file

Esempio (`53-mpq4.network`):

```ini
[Match]
Name=mpq4

[Network]
LinkLocalAddressing=no
IPv6AcceptRA=no

[Route]
Destination=0.0.0.0/0
Table=103
Scope=link
```

Opzioni chiave:
- **`LinkLocalAddressing=no`** — nessun indirizzo link-local sui TUN (non necessario)
- **`IPv6AcceptRA=no`** — nessun IPv6 RA sui TUN
- **`Scope=link`** — route di livello link, next-hop implicito è il device stesso

#### 4.2.3) Verifica post-installazione

```bash
# Ogni TUN attivo deve mostrare il proprio Network File
for n in 1 2 3 4 5 6; do
  echo -n "mpq$n: "
  networkctl status mpq$n 2>/dev/null | grep "Network File" || echo "non gestito (servizio inattivo)"
done

# Verifica route nelle tabelle WAN (solo per i servizi attivi)
for t in wan1 wan2 wan3 wan4 wan5 wan6; do
  echo -n "$t: "
  ip route show table $t | grep "^default" || echo "MANCANTE (servizio inattivo)"
done
```

Output atteso (esempio con mpq4/5/6 attivi):
```
mpq4: Network File: /etc/systemd/network/53-mpq4.network
mpq5: Network File: /etc/systemd/network/54-mpq5.network
mpq6: Network File: /etc/systemd/network/55-mpq6.network
wan4: default dev mpq4 scope link
wan5: default dev mpq5 scope link
wan6: default dev mpq6 scope link
```

#### 4.2.4) Test di resilienza (obbligatorio post-install)

Simula la ricreazione del TUN come avviene su ogni reconnect QUIC:

```bash
# Verifica stato iniziale
ip route show table wan6 | grep "^default" && echo "OK: route presente" || echo "MANCANTE"

# Riavvia il servizio (ensure_tun.sh ricrea il TUN con nuovo ifindex)
sudo systemctl restart mpquic@6.service

# Attendi ~10s (reconnect QUIC + carrier detection networkd)
sleep 10

# La route deve essere stata ripristinata automaticamente da networkd
ip route show table wan6 | grep "^default" && echo "OK: route ripristinata automaticamente" || echo "ERRORE: fix non attivo"
```

Tempo di ripristino atteso: **~5 secondi** dal momento in cui mpquic stabilisce la connessione
QUIC (evento `Gained carrier` nel log di networkd).

#### 4.2.5) Nota su route duplicate

Nelle tabelle WAN possono comparire **due voci** `default dev mpqN`:
- una aggiunta da `mpquic-policy-routing.sh` (senza attributo `proto`)
- una da systemd-networkd (con `proto static`)

È un comportamento cosmético, non funzionale: il kernel le tratta come equivalenti.
Cleanup futuro possibile: rimuovere il loop `mpqN` da `mpquic-policy-routing.sh` dopo
aver confermato la stabilità dei file `.network` su più cicli di reboot.
**Non effettuare questo cleanup prima di un reboot test completo.**

### 5) Parametrizzazione endpoint
#### Client
Imposta IP pubblico VPS una sola volta (vale per tutte le istanze):
```bash
sudo sed -i 's/^VPS_PUBLIC_IP=.*/VPS_PUBLIC_IP=172.238.232.223/' /etc/mpquic/global.env
cat /etc/mpquic/global.env
```
Verifica:
```bash
grep -R "remote_addr" /etc/mpquic/instances/*.yaml.tpl
```

#### Server
Opzionale: bind dedicato (`bind_ip`) al posto di `0.0.0.0`.

### 5.1) Configurazione dataplane multipath (completa)

Per policy avanzate (`critical/default/bulk`, classifier L3/L4, duplication) sono supportati due modelli:

#### Modello consigliato: file dataplane dedicato
Nel file applicativo client (es. `/etc/mpquic/instances/multipath.yaml.tpl`) aggiungi:
```yaml
dataplane_config_file: /etc/mpquic/instances/dataplane.yaml
```

E crea/copia `dataplane.yaml` in path assoluto:
```bash
sudo install -m 0644 /opt/mpquic/deploy/config/client/dataplane.yaml /etc/mpquic/instances/dataplane.yaml
```

Contenuto esempio:
```yaml
default_class: default
classes:
	default:
		scheduler_policy: balanced
	critical:
		scheduler_policy: failover
		preferred_paths: [wan4, wan5]
		duplicate: true
		duplicate_copies: 2
	bulk:
		scheduler_policy: balanced
		excluded_paths: [wan4]
classifiers:
	- name: voip
		class: critical
		protocol: udp
		dst_ports: ["5060", "10000-20000"]
		dscp: [46]
	- name: backup
		class: bulk
		protocol: tcp
		dst_ports: ["5001-6000"]
```

#### Modello alternativo: dataplane inline nello YAML applicativo
Nel medesimo file YAML client, usa sezione `dataplane:` con la stessa struttura di cui sopra.

#### Precedenza di configurazione
Se presenti entrambe:
- `dataplane` inline
- `dataplane_config_file`

il file dedicato (`dataplane_config_file`) ha precedenza.

#### Control API orchestrator (opzionale)
Nel file client multipath puoi abilitare API locale per validare/applicare policy dataplane:
```yaml
control_api_listen: 127.0.0.1:19090
control_api_auth_token: change-me
```

Generazione token (consigliata):
```bash
TOKEN="$(openssl rand -hex 32)"
echo "$TOKEN"
```

Sostituisci `change-me` con il token nel file YAML e riavvia istanza:
```bash
sudo systemctl restart mpquic@4.service
```

Esempio verifica:
```bash
TOKEN="<token_generato>"
curl -sS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:19090/healthz
curl -sS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:19090/dataplane
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/yaml' --data-binary @/etc/mpquic/instances/dataplane.yaml http://127.0.0.1:19090/dataplane/validate
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/yaml' --data-binary @/etc/mpquic/instances/dataplane.yaml http://127.0.0.1:19090/dataplane/apply
```

#### Verifica operativa
Dopo riavvio istanza multipath, controlla:
```bash
journalctl -u mpquic@4.service -n 200 --no-pager | egrep 'path telemetry|class telemetry'
```

Per schema completo, pattern QoS e flusso orchestrator esterno: `docs/DATAPLANE_ORCHESTRATOR.md`.

#### Test automatico Control API + Load-balancing + Failover (mpq3/mpq4/mpq5)

Script pronto:
```bash
sudo /usr/local/sbin/mpquic-controlapi-lb-failover-test.sh 4 vps-it-mpquic
```

Con trigger traffico da OpenWRT (`mwan3 use SL1/SL2/SL3`):
```bash
sudo /usr/local/sbin/mpquic-controlapi-lb-failover-test.sh 4 vps-it-mpquic openwrt-host
```

Cosa fa:
1. backup config `4.yaml.tpl`
2. applica config test multipath su path `wan3/wan4/wan5` con Control API locale
3. verifica API (`/healthz`, `/dataplane`)
4. misura distribuzione traffico su UDP `45003/45004/45005` (load-balancing)
5. simula failover fermando `mpquic@4` lato VPS e rimisura il traffico
6. riporta automaticamente la configurazione originale al termine

Output:
- `/tmp/mpquic-lb-capture-4.txt`
- `/tmp/mpquic-failover-capture-4.txt`
- riepilogo finale su stdout

### 6) Test incrementale: prima 1 tunnel
#### Server
```bash
sudo systemctl enable --now mpquic@1.service
sudo systemctl --no-pager --full status mpquic@1.service
sudo ss -lunp | grep 45001
```

#### Client
```bash
sudo systemctl enable --now mpquic@1.service
sudo systemctl --no-pager --full status mpquic@1.service
ip -br a show dev mpq1
sudo ss -unap | grep mpquic
```

Ping di test (client -> server tunnel peer):
```bash
ping -I mpq1 -c 3 10.200.1.2
```

### 7) Estensione ai 6 tunnel
#### Server
```bash
for i in 1 2 3 4 5 6; do sudo systemctl enable --now mpquic@$i.service; done
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
sudo ss -lunp | egrep '4500[1-6]'
```

#### Client
```bash
for i in 1 2 3 4 5 6; do sudo systemctl enable --now mpquic@$i.service; done
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
ip -br a | grep '^mpq'
sudo ss -unap | grep mpquic
```

### 8) Troubleshooting

Per troubleshooting completo (debug per sintomo, TLS, raccolta evidenze), vedere `docs/TUNNEL_OPERATIONS_DEBUG.md`.

### 9) Persistenza al reboot
```bash
sudo reboot
```
Dopo reboot:
```bash
for i in 1 2 3 4 5 6; do systemctl is-enabled mpquic@$i.service; systemctl is-active mpquic@$i.service; done
ip -br a | grep '^mpq'
```

### 11) Riferimento completo attributi YAML

Questa sezione documenta **ogni attributo** supportato nei file YAML di configurazione
delle istanze MPQUIC, organizzati per categoria.

#### 11.1 Attributi globali (presenti in ogni YAML)

| Attributo | Valori | Obbligatorio | Descrizione |
|-----------|--------|:------------:|-------------|
| `role` | `client` / `server` | ✅ | Ruolo dell'istanza |
| `tun_name` | stringa (es. `mpq4`, `mp1`, `cr5`) | ✅ | Nome interfaccia TUN Linux |
| `tun_cidr` | CIDR (es. `10.200.4.1/30`) | ✅ | Indirizzo IP e subnet della TUN |
| `log_level` | `debug` / `info` / `error` | ✅ | Livello di logging |
| `metrics_listen` | `auto` / `<ip>:<porta>` / (vuoto) | No | Indirizzo di ascolto server metriche. `auto` = deriva IP da `tun_cidr` + porta 9090. Espone `/metrics` (Prometheus) e `/api/v1/stats` (JSON) |

#### 11.2 Attributi di rete e connessione

| Attributo | Valori | Obbligatorio | Descrizione |
|-----------|--------|:------------:|-------------|
| `bind_ip` | IP o `if:<ifname>` | Client: ✅ | IP sorgente per il socket UDP. Con `if:` risolve l'IP dall'interfaccia e applica `SO_BINDTODEVICE` |
| `remote_addr` | IP o hostname | Client: ✅ | Indirizzo del server (può usare `VPS_PUBLIC_IP` come placeholder) |
| `remote_port` | intero (es. `45004`) | ✅ | Porta UDP del listener QUIC server |

**Nota su `bind_ip`**:
- `192.168.1.100`: bind solo all'IP (senza SO_BINDTODEVICE)
- `if:enp7s6`: risolve il primo IPv4 di `enp7s6`, applica SO_BINDTODEVICE (raccomandato per multi-WAN)
- `0.0.0.0`: bind su tutte le interfacce (solo server)

#### 11.3 Attributi TLS

| Attributo | Valori | Obbligatorio | Descrizione |
|-----------|--------|:------------:|-------------|
| `tls_ca_file` | path (es. `/etc/mpquic/tls/ca.crt`) | Client: ✅ | Certificato CA per verifica server |
| `tls_cert_file` | path (es. `/etc/mpquic/tls/server.crt`) | Server: ✅ | Certificato TLS server |
| `tls_key_file` | path (es. `/etc/mpquic/tls/server.key`) | Server: ✅ | Chiave privata TLS server |
| `tls_server_name` | stringa (es. `mpquic-server`) | Client: ✅ | CN (Common Name) o SAN atteso nel certificato server |
| `tls_insecure_skip_verify` | `true` / `false` | No | Disabilita verifica certificato (solo per test, **mai in produzione**) |

#### 11.4 Attributi trasporto e congestion control

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `congestion_algorithm` | `cubic` / `bbr` | `cubic` | Algoritmo di congestion control QUIC |
| `transport_mode` | `datagram` / `reliable` | `datagram` | Modalità trasporto: `datagram` = QUIC DATAGRAM frames (unreliable); `reliable` = QUIC streams (ritrasmissione) |

**Raccomandazione**: usare **sempre** `transport_mode: reliable` su link satellitari.
`datagram` è utile solo per applicazioni UDP real-time che gestiscono la loss internamente.

#### 11.5 Attributi multi-connessione server

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `multi_conn_enabled` | `true` / `false` | `false` | Se `true`, il server accetta N connessioni QUIC sulla stessa porta (necessario per multi-tunnel per link e multipath) |

#### 11.6 Attributi multipath (client)

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `multipath_enabled` | `true` / `false` | `false` | Abilita la modalità multipath (N path verso lo stesso server) |
| `multipath_policy` | `priority` / `failover` / `balanced` | `priority` | Policy di selezione path TX |

**Policy multipath**:
- `priority`: seleziona il path con score migliore (priority + penalty + weight)
- `failover`: usa il path con priority più bassa (valore numerico), fallback sui successivi
- `balanced`: distribuisce il traffico su tutti i path attivi con round-robin flow-hash

#### 11.7 Attributi `multipath_paths[]` (client)

Array di path, ciascuno con:

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `name` | stringa (es. `wan4`) | ✅ obbligatorio | Etichetta operativa del path (usata in log e telemetria) |
| `bind_ip` | IP o `if:<ifname>` | ✅ obbligatorio | IP sorgente / interfaccia WAN per questo path |
| `remote_addr` | IP o hostname | ✅ obbligatorio | Indirizzo IP del server |
| `remote_port` | intero | ✅ obbligatorio | Porta UDP del listener server |
| `priority` | intero ≥ 1 | `1` | Priorità (valore più basso = più preferito). Per failover: primary=1, backup=2 |
| `weight` | intero ≥ 1 | `1` | Peso di preferenza. Per `balanced`, pesi uguali = distribuzione uniforme |
| `pipes` | intero ≥ 1 | `1` | Numero di socket UDP paralleli per il path. Con `transport: stripe`, ogni pipe è una sessione Starlink indipendente |
| `transport` | `quic` / `stripe` | `quic` | Tipo di trasporto per il path. `stripe` usa UDP raw + FEC, `quic` usa connessione QUIC standard |

#### 11.8 Attributi stripe (trasporto UDP + FEC + ARQ)

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `stripe_port` | intero (es. `46017`) | `remote_port + 1000` | Porta UDP del listener stripe sul server |
| `stripe_data_shards` | intero (es. `10`) | `10` | K — numero shards dati per gruppo FEC. Anche con FEC disabilitato (M=0), K è usato come soglia nel protocollo RX per distinguere pacchetti diretti (GroupDataN < K) da gruppi FEC completi. **Deve essere coerente tra client e server.** |
| `stripe_parity_shards` | intero (es. `2`) | `2` | M — numero shards parità Reed-Solomon. Con K=10, M=2: tolleranza 16.7% loss. In modalità `adaptive`, l'encoder RS viene pre-creato con questo valore anche se M effettivo parte da 0 |
| `stripe_fec_mode` | `always` / `adaptive` / `off` | `always` | Modalità FEC: `always` = M fisso, ogni gruppo ha K+M shards; `adaptive` = parte da M=0 (nessuna parità, invio diretto), sale a M configurato se rilevata perdita; `off` = M=0 permanente, nessun encoder RS creato |
| `stripe_arq` | `true` / `false` | `false` | Abilita Hybrid ARQ con NACK selettivo. Il receiver rileva gap di sequenza e invia NACK bitmap al sender, che ritrasmette solo i pacchetti mancanti. Attivo solo quando effectiveM=0. Overhead ~0% in assenza di loss |
| `stripe_pacing_rate` | intero (Mbps) | `0` (disabilitato) | Rate di pacing per sessione. Con valore >0, abilita **kernel pacing** via `SO_TXTIME` + `sch_fq` (granularità nanosecondo). Richiede: kernel ≥4.19 e qdisc `sch_fq` attivo (`scripts/setup-fq-qdisc.sh`). Se il kernel non supporta SO_TXTIME, fallback automatico a software pacer. Raccomandato: `800` per dual Starlink |
| `stripe_disable_gso` | `true` / `false` | `false` | Disabilita UDP GSO (`UDP_SEGMENT`) sul client TX. GSO è rilevato automaticamente all'avvio (kernel ≥5.0). Usare `true` solo per A/B test diagnostici |
| `stripe_fec_type` | `rs` / `xor` | `rs` | Tipo FEC: `rs` = Reed-Solomon (blocco K+M), `xor` = Sliding Window XOR (RFC 8681). Quando `xor`: RS disabilitato (parityM forzato a 0), i dati vanno tramite fast path M=0, repair XOR generato a fianco — zero impatto latenza. **Deve essere identico su client e server** |
| `stripe_fec_window` | intero (es. `10`) | `10` | W — dimensione finestra XOR. Ogni W pacchetti sorgente consecutivi generano 1 pacchetto di riparazione XOR. Recupera esattamente 1 perdita per finestra. Solo usato quando `stripe_fec_type: xor`. Valori consigliati: 5-20 |
| `stripe_enabled` | `true` / `false` | `false` | Solo server: abilita il listener UDP stripe |

**Formula FEC**: può recuperare fino a M shards persi su K+M totali.
Con K=10, M=2: gruppo di 12 shards, tolleranza 2 shards persi (16.7%).
Aumentando M si migliora la resilienza al costo di più overhead di rete.

**Configurazione raccomandata per Starlink**: `stripe_fec_type: xor` + `stripe_fec_window: 10` + `stripe_arq: true`.
XOR FEC genera 1 pacchetto di riparazione ogni W sorgenti (10% overhead con W=10),
recupera esattamente 1 perdita per finestra senza latenza aggiuntiva sul data path.
ARQ ritrasmette selettivamente le perdite multiple (rare).
Alternativa RS: `stripe_fec_type: rs` + `stripe_fec_mode: adaptive` per quando servono
recovery multi-loss più aggressive (M=2: tolleranza 16.7% loss per gruppo).
Benchmark dual Starlink 24 pipe: **354 Mbps** media, picco 390 Mbps (+48% vs baseline 239 Mbps).
Con GSO (v4.4): picco **548 Mbps**, best 30s **400 Mbps**.
Con kernel pacing SO_TXTIME (v4.5): media 333 Mbps (stabile), mediana 352 Mbps.
Variabilità Starlink (23% CoV) domina rispetto alle ottimizzazioni software.

**Nota critica**: `stripe_fec_type` e `stripe_fec_mode` **devono essere identici su client e server**.
Se il client usa `off` ma il server ha `adaptive`, il server può inviare gruppi
FEC con parità che il client non sa decodificare. Dopo qualsiasi modifica,
riavviare **entrambi** i nodi.

#### 11.9 Attributi dataplane e QoS (avanzati)

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `dataplane_config_file` | path assoluto | — | File YAML esterno con configurazione dataplane (ha precedenza su inline) |
| `control_api_listen` | `host:port` (es. `127.0.0.1:19090`) | — | Endpoint API REST locale per controllo runtime |
| `control_api_auth_token` | stringa | — | Token Bearer per autenticazione API |

Per schema completo dataplane: `docs/DATAPLANE_ORCHESTRATOR.md`.

---

#### 11.10 Esempio completo: client single-link (mpq4)

Configurazione minima per un tunnel QUIC singolo su WAN4:

```yaml
# /etc/mpquic/instances/4.yaml.tpl
role: client
bind_ip: if:enp7s6
remote_addr: VPS_PUBLIC_IP
remote_port: 45004
tun_name: mpq4
tun_cidr: 10.200.4.1/30
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
```

Corrispondente server:

```yaml
# /etc/mpquic/instances/4.yaml.tpl (VPS)
role: server
bind_ip: 0.0.0.0
remote_port: 45004
tun_name: mpq4
tun_cidr: 10.200.4.2/30
log_level: info
tls_cert_file: /etc/mpquic/tls/server.crt
tls_key_file: /etc/mpquic/tls/server.key
```

#### 11.12 Esempio completo: client multipath stripe dual Starlink (mp1)

Configurazione produzione — bonding 2 link Starlink con stripe + FEC adattivo + ARQ:

```yaml
# /etc/mpquic/instances/mp1.yaml (client)
role: client
multipath_enabled: true
multipath_policy: balanced
tun_name: mp1
tun_cidr: 10.200.17.1/24
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
congestion_algorithm: bbr
transport_mode: reliable
stripe_port: 46017
stripe_data_shards: 10
stripe_parity_shards: 2
stripe_fec_mode: adaptive
stripe_fec_type: xor
stripe_fec_window: 10
stripe_arq: true
multipath_paths:
- name: wan5
  bind_ip: if:enp7s7
  remote_addr: 172.238.232.223
  remote_port: 45017
  priority: 1
  weight: 1
  pipes: 12
  transport: stripe
- name: wan6
  bind_ip: if:enp7s8
  remote_addr: 172.238.232.223
  remote_port: 45017
  priority: 1
  weight: 1
  pipes: 12
  transport: stripe
```

Corrispondente server:

```yaml
# /etc/mpquic/instances/mp1.yaml (VPS)
role: server
bind_ip: 0.0.0.0
remote_port: 45017
multi_conn_enabled: true
stripe_enabled: true
stripe_port: 46017
stripe_data_shards: 10
stripe_parity_shards: 2
stripe_fec_mode: adaptive
stripe_fec_type: xor
stripe_fec_window: 10
stripe_arq: true
tun_name: mp1
tun_cidr: 10.200.17.254/24
log_level: info
tls_cert_file: /etc/mpquic/tls/server.crt
tls_key_file: /etc/mpquic/tls/server.key
```

#### 11.13 Esempio: failover primario/backup

```yaml
# Failover: wan5 primario, wan6 backup
multipath_policy: failover
multipath_paths:
- name: wan5
  bind_ip: if:enp7s7
  remote_addr: 172.238.232.223
  remote_port: 45017
  priority: 1       # primario
  weight: 1
- name: wan6
  bind_ip: if:enp7s8
  remote_addr: 172.238.232.223
  remote_port: 45017
  priority: 2       # backup
  weight: 1
```

---

### 12) File .env per istanza

Ogni istanza systemd richiede un file `.env` accanto al `.yaml.tpl`:

```bash
# /etc/mpquic/instances/mp1.env
TUN_NAME=mp1
TUN_CIDR=10.200.17.1/24
TUN_MTU=1300
```

Il file `.env` è letto da `EnvironmentFile=` nel service systemd e usato da
`ensure_tun.sh` per creare la TUN prima dell'avvio del processo.

```bash
# /etc/mpquic/instances/4.env (esempio single-link)
TUN_NAME=mpq4
TUN_CIDR=10.200.4.1/30
TUN_MTU=1300
```

#### File globale `/etc/mpquic/global.env`

Variabili condivise da tutte le istanze:

```bash
VPS_PUBLIC_IP=172.238.232.223
```

La variabile `VPS_PUBLIC_IP` viene sostituita nei template `.yaml.tpl` dallo
script `render_config.sh` durante l'`ExecStartPre` del servizio.

---

### 13) Configurazione di rete client — interfacce e routing

#### 13.1 Layout interfacce (Debian 12, systemd-networkd)

La VM client ha 16 interfacce di rete suddivise in 4 ruoli:

| Gruppo | Interfacce | Ruolo | IP |
|--------|------------|-------|-----|
| MGMT | enp6s18, enp6s19 | Management SSH | 10.10.11.100, 10.10.10.100 |
| LAN | enp6s20-23, enp7s1-2 | Transit verso OpenWrt | 172.16.{1-6}.1/30 |
| VLAN | enp6s20.17 | Transit dedicato mp1 (bd1) | 172.16.17.1/30 |
| WAN | enp7s3-8 | Uplink Starlink (DHCP) | Dinamici (CGNAT/privato) |
| TUN | mpq1-6, mp1, cr5, etc. | Tunnel MPQUIC | 10.200.x.x |

#### 13.2 Configurazione interfacce con systemd-networkd

La VM client usa `systemd-networkd` come backend di rete. I file di configurazione
sono in `/etc/systemd/network/` e vengono forniti dal progetto in `deploy/networkd/`.

##### MGMT e LAN (statiche)

Le interfacce di management e LAN usano IP statici configurati nei file
`01-mgmt1.network`, `02-mgmt2.network`, `20-lan1.network` ... `25-lan6.network`.

##### WAN (DHCP — file per-interfaccia)

Ogni WAN ha il proprio file `.network` individuale (vedi §4.1 per dettagli):

```bash
/etc/systemd/network/10-wan1.network   # enp7s3 — WAN1 (metric 101)
/etc/systemd/network/11-wan2.network   # enp7s4 — WAN2 (metric 102)
/etc/systemd/network/12-wan3.network   # enp7s5 — WAN3 (metric 103)
/etc/systemd/network/13-wan4.network   # enp7s6 — WAN4 (metric 104)
/etc/systemd/network/14-wan5.network   # enp7s7 — WAN5 (metric 105)
/etc/systemd/network/15-wan6.network   # enp7s8 — WAN6 (metric 106)
```

I file vengono installati con:
```bash
sudo cp deploy/networkd/wan/*.network /etc/systemd/network/
sudo networkctl reload
```

**Importante**: non usare un singolo file condiviso per tutte le WAN. La configurazione
per-interfaccia è necessaria per il corretto funzionamento del `wan-watchdog` e per
avere `RouteMetric` diversificate.

#### 13.3 Rendere permanente la configurazione di rete

La configurazione in `/etc/systemd/network/` è già persistente. Dopo un reboot:

```bash
# Verifica tutte le interfacce sono up con IP
ip -br a | egrep 'enp6s|enp7s'

# Verifica specifiche WAN (DHCP)
ip -4 -br a show dev enp7s6   # WAN4
ip -4 -br a show dev enp7s7   # WAN5
ip -4 -br a show dev enp7s8   # WAN6

# Verifica che ogni WAN usi il proprio file .network
networkctl status enp7s7   # → Network File: /etc/systemd/network/14-wan5.network
```

Il servizio `wan-watchdog.service` (vedi §4.1.2) garantisce la recovery automatica
del lease DHCP anche in caso di cable swap senza carrier loss.

---

### 14) Configurazione nftables (NAT) — Client

#### 14.1 File di configurazione

```bash
# /etc/nftables.conf (client)
table ip nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;

        # === NAT sulle WAN fisiche (Starlink) ===
        oifname "enp7s3" masquerade
        oifname "enp7s4" masquerade
        oifname "enp7s5" masquerade
        oifname "enp7s6" masquerade
        oifname "enp7s7" masquerade
        oifname "enp7s8" masquerade

        # === NAT sui tunnel single-link ===
        oifname "mpq1" masquerade
        oifname "mpq2" masquerade
        oifname "mpq3" masquerade
        oifname "mpq4" masquerade
        oifname "mpq5" masquerade
        oifname "mpq6" masquerade

        # === NAT sui tunnel avanzati ===
        oifname "cr*" masquerade
        oifname "br*" masquerade
        oifname "df*" masquerade
        oifname "mp1" masquerade
    }
}
```

**Perché MASQUERADE su ogni interfaccia?**

- **WAN**: il traffico che esce dalle WAN fisiche verso Internet ha IP sorgente
  delle LAN transit (172.16.x.x) → va NATtato con l'IP WAN
- **Tunnel**: il traffico che entra nei tunnel ha IP sorgente LAN → va NATtato
  con l'IP TUN del tunnel per il ritorno corretto dal VPS
- **mp1**: il tunnel multipath aggrega traffico da LAN instradato via
  `ip route replace default dev mp1 table wan1` → va NATtato

#### 14.2 Installazione e persistenza

```bash
# Installare nftables
sudo apt-get install -y nftables

# Copiare la configurazione
sudo cp /etc/nftables.conf /etc/nftables.conf.bak  # backup
sudo nano /etc/nftables.conf                         # editare

# Applicare
sudo nft -f /etc/nftables.conf

# Abilitare al boot
sudo systemctl enable --now nftables

# Verifica
sudo nft list ruleset
```

#### 14.3 nftables VPS (server)

```bash
# /etc/nftables.conf (VPS) — estratto rilevante
table inet filter {
    chain input {
        type filter hook input priority filter; policy drop;

        # conntrack
        ct state established,related accept
        iif "lo" accept

        # SSH
        tcp dport 22 accept

        # QUIC tunnel ports
        udp dport 45001-45006 accept
        udp dport 45010 accept
        udp dport 45017 accept

        # Stripe UDP port
        udp dport 46017 accept

        # Tunnel interfaces (everything from TUN is trusted)
        iifname "mpq*" accept
        iifname "mt*" accept
        iifname "mp*" accept

        # iperf3 (test)
        tcp dport 5201 accept
    }

    chain forward {
        type filter hook forward priority filter; policy accept;
    }
}

table ip nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "eth0" masquerade
    }
}
```

**Rendere permanente sul VPS:**

```bash
# Dopo ogni modifica
sudo nft list ruleset | sudo tee /etc/nftables.conf
sudo systemctl enable nftables
```

---

### 15) Tabelle di routing — Client

#### 15.1 Policy routing source-based (1:1)

Ogni LAN transit è instradato nel tunnel corrispondente tramite policy routing:

```
Sorgente 172.16.1.0/30 → table wan1 → default dev mpq1
Sorgente 172.16.2.0/30 → table wan2 → default dev mpq2
...
Sorgente 172.16.6.0/30 → table wan6 → default dev mpq6
```

Le tabelle sono gestite dallo script `mpquic-policy-routing.sh` attivato dal
service `mpquic-routing.service`.

#### 15.2 Definizione tabelle in `/etc/iproute2/rt_tables`

```bash
# /etc/iproute2/rt_tables — aggiungere:
100 wan1
101 wan2
102 wan3
103 wan4
104 wan5
105 wan6
200 bd1
```

#### 15.3 Regole e route per tunnel single-link

Per ogni WAN attiva (es. WAN4, tabella 103):

```bash
# Rule: traffico da LAN4 → table wan4
ip rule add from 172.16.4.0/30 table wan4 priority 1004

# Route: default via tunnel mpq4
ip route replace default dev mpq4 table wan4

# Route: raggiungere il VPS via WAN4 (necessario per il tunnel stesso)
GATEWAY=$(ip route show dev enp7s6 | awk '/default/ {print $3}')
ip route replace 172.238.232.223/32 via "$GATEWAY" dev enp7s6 table wan4
```

#### 15.4 Route per tunnel multipath mp1 — tabella bd1

Il tunnel multipath mp1 utilizza una tabella di routing dedicata `bd1` (ID 200)
con VLAN 17 su enp6s20, completamente indipendente dalle tabelle wan1–wan6 e
dallo script watchdog `mpquic-lan-routing-check.sh`.

**Infrastruttura VLAN 17 (systemd-networkd):**

```ini
# /etc/systemd/network/26-vlan17.netdev
[NetDev]
Name=enp6s20.17
Kind=vlan

[VLAN]
Id=17
```

```ini
# /etc/systemd/network/27-bd1.network
[Match]
Name=enp6s20.17

[Network]
Address=172.16.17.1/30
LinkLocalAddressing=no
IPv6AcceptRA=no

[RoutingPolicyRule]
From=172.16.17.0/30
Table=200
Priority=1017
```

```ini
# /etc/systemd/network/20-lan1.network — aggiungere sotto [Network]:
VLAN=enp6s20.17
```

**Route bd1 (persistenti tramite systemd drop-in):**

```ini
# /etc/systemd/system/mpquic@mp1.service.d/bd1-route.conf
[Service]
ExecStartPost=/bin/sh -c "sleep 1 && ip route replace default dev mp1 table bd1 && ip route replace 172.16.17.0/30 dev enp6s20.17 table bd1"
```

**Schema routing:**

```
Sorgente 172.16.17.0/30 → rule 1017 → table bd1 → default dev mp1
                                                   → 172.16.17.0/30 dev enp6s20.17
```

**VPS — route di ritorno:**

```bash
ip route replace 172.16.17.0/30 dev mp1
```

**Verifica:**

```bash
# Client
ip route show table bd1
ip rule show | grep 1017

# VPS
ip route show | grep 172.16.17
```

#### 15.5 Route VPS (server)

```bash
# Route di ritorno per ogni LAN transit
ip route add 172.16.1.0/30 dev mpq1
ip route add 172.16.2.0/30 dev mpq2
ip route add 172.16.3.0/30 dev mpq3
ip route add 172.16.4.0/30 dev mpq4
ip route add 172.16.5.0/30 dev mpq5
ip route add 172.16.6.0/30 dev mpq6

# Route per subnet mp1 (multipath, tabella bd1)
ip route add 172.16.17.0/30 dev mp1
```

#### 15.6 Rendere le route permanenti

**Metodo 1: service systemd (raccomandato)**

Le route sono gestite dai service dedicati:

```bash
# Client: policy routing
sudo systemctl enable --now mpquic-routing.service

# VPS: route di ritorno
sudo systemctl enable --now mpquic-vps-routes.service
```

Questi service vengono eseguiti dopo l'avvio dei tunnel e ri-applicano tutte le
route necessarie.

**Metodo 2: post-up in /etc/network/interfaces**

Per route aggiuntive non gestite dai service:

```bash
auto enp7s6
iface enp7s6 inet dhcp
    post-up ip route replace default dev mpq4 table wan4 2>/dev/null || true
```

#### 15.7 Verifica stato route dopo reboot

```bash
# Client — rule policies
ip rule show | egrep '100[1-6]'

# Client — route tables
for t in wan1 wan2 wan3 wan4 wan5 wan6; do
    echo "=== $t ==="
    ip route show table "$t"
done

# VPS — route di ritorno
ip route show | egrep '172\.16\.[1-6]\.0/30|10\.200\.'
```

---

### 16) Forwarding IP (server VPS)

Il forwarding IPv4 è necessario perché il VPS fa da gateway Internet per il
traffico tunnellizzato.

```bash
# Abilita forwarding permanente
echo 'net.ipv4.ip_forward = 1' | sudo tee /etc/sysctl.d/99-mpquic-forward.conf
sudo sysctl --system

# Verifica
sysctl net.ipv4.ip_forward
# Atteso: net.ipv4.ip_forward = 1
```

---

### 16.1) Tuning UDP Socket Buffers (entrambi i nodi)

MPQUIC configura automaticamente buffer da 7 MB per ogni socket UDP stripe
tramite `SetReadBuffer()` / `SetWriteBuffer()`. Tuttavia il kernel Linux
limita il buffer massimo ai valori di `net.core.rmem_max` e `net.core.wmem_max`
(default ~208 KB). Senza questo tuning, la chiamata viene silenziosamente
troncata e le burst Starlink possono causare drop a livello kernel.

```bash
# Imposta buffer max 7 MB (= 7340032 bytes, valore usato anche da quic-go)
cat <<'EOF' | sudo tee /etc/sysctl.d/99-mpquic-buffers.conf
# MPQUIC stripe: socket buffer 7 MB per prevenire drop durante burst Starlink
net.core.rmem_max = 7340032
net.core.wmem_max = 7340032
# Opzionale: aumenta anche il default (per tutti i socket, non solo mpquic)
net.core.rmem_default = 1048576
net.core.wmem_default = 1048576
EOF

sudo sysctl --system
```

**Verifica**:
```bash
sysctl net.core.rmem_max net.core.wmem_max
# Atteso:
# net.core.rmem_max = 7340032
# net.core.wmem_max = 7340032
```

**Nota**: questa configurazione è necessaria sia sul client che sul server VPS.
Senza di essa, i buffer effettivi restano a ~208 KB nonostante il codice
richieda 7 MB. Il tuning è persistente (sopravvive al reboot).

---

### 17) Certificati TLS

#### 17.1 Generazione CA e certificati (una tantum)

```bash
# Genera CA
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout ca.key -out ca.crt -days 3650 -nodes \
  -subj "/CN=MPQUIC CA"

# Genera chiave + CSR server
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout server.key -out server.csr -nodes \
  -subj "/CN=mpquic-server"

# Firma con SAN
cat > san.ext <<EOF
subjectAltName = DNS:mpquic-server
EOF
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key \
  -CAcreateserial -out server.crt -days 3650 -extfile san.ext

# Cleanup
rm -f server.csr san.ext ca.srl
```

#### 17.2 Distribuzione

```bash
# Server VPS
sudo install -d /etc/mpquic/tls
sudo install -m 0600 server.key /etc/mpquic/tls/server.key
sudo install -m 0644 server.crt /etc/mpquic/tls/server.crt
sudo install -m 0644 ca.crt /etc/mpquic/tls/ca.crt

# Client
sudo install -d /etc/mpquic/tls
sudo install -m 0644 ca.crt /etc/mpquic/tls/ca.crt
```

#### 17.3 Verifica

```bash
# Verificare che il CN corrisponda a tls_server_name
openssl x509 -in /etc/mpquic/tls/server.crt -noout -subject -ext subjectAltName
# Atteso: CN = mpquic-server, SAN: DNS:mpquic-server
```

---

### 18) Servizio systemd — template e funzionamento

#### 18.1 Template `mpquic@.service`

```ini
[Unit]
Description=MPQUIC tunnel instance %i
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-/etc/mpquic/global.env
EnvironmentFile=/etc/mpquic/instances/%i.env
ExecStartPre=/bin/sh -c '/usr/local/lib/mpquic/ensure_tun.sh "$TUN_NAME" "$TUN_CIDR" "${TUN_MTU:-1300}"'
ExecStartPre=/bin/sh -c '/usr/local/lib/mpquic/render_config.sh "%i"'
ExecStart=/usr/local/bin/mpquic --config /run/mpquic/%i.yaml
ExecStopPost=-/bin/sh -c 'ip link set dev "$TUN_NAME" down 2>/dev/null || true'
Restart=always
RestartSec=2
TimeoutStopSec=15
KillMode=mixed
KillSignal=SIGTERM
User=root
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
NoNewPrivileges=true
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

#### 18.2 Flusso di avvio di un'istanza

1. `systemd` legge `global.env` (VPS_PUBLIC_IP) e `instances/%i.env` (TUN_NAME, TUN_CIDR, TUN_MTU)
2. `ensure_tun.sh` crea la TUN se non esiste, assegna IP e MTU, porta up
3. `render_config.sh` sostituisce `VPS_PUBLIC_IP` nel template `.yaml.tpl` e genera `/run/mpquic/%i.yaml`
4. `mpquic --config /run/mpquic/%i.yaml` avvia il processo con la configurazione renderizzata
5. Al termine, `ExecStopPost` porta down la TUN

#### 18.3 Comandi operativi

```bash
# Avviare un'istanza
sudo systemctl start mpquic@mp1.service

# Avviare e abilitare al boot
sudo systemctl enable --now mpquic@mp1.service

# Fermare
sudo systemctl stop mpquic@mp1.service

# Restart
sudo systemctl restart mpquic@mp1.service

# Log
journalctl -u mpquic@mp1.service -n 100 --no-pager -f

# Stato tutte le istanze
for i in 1 2 3 4 5 6 mp1 cr4 br4 df4 cr5 br5 df5 cr6 br6 df6; do
    printf "%-6s " "$i"
    systemctl is-active mpquic@$i.service 2>/dev/null || echo inactive
done
```

---

### 19) Aggiornamento software — `mpquic-update.sh`

Lo script di aggiornamento è il modo raccomandato per aggiornare il software:

```bash
sudo /usr/local/sbin/mpquic-update.sh
```

**Flusso completo:**

1. `git pull` dal repository
2. Se lo script stesso è cambiato → `exec` della nuova versione con `MPQUIC_UPDATE_SKIP_PULL=1`
3. `go build` del binario
4. Discovery delle istanze attive
5. Stop parallelo di tutte le istanze
6. `pkill -9` di eventuali processi residui
7. `rm -f` + `cp` del nuovo binario (evita ETXTBSY)
8. Install del template systemd aggiornato
9. Start parallelo di tutte le istanze
10. Health check post-deploy

---

### 20) Checklist post-installazione completa

#### Client
```bash
# 1. Certificati
ls -l /etc/mpquic/tls/ca.crt

# 2. Configurazioni istanze
ls -l /etc/mpquic/instances/*.yaml.tpl /etc/mpquic/instances/*.env

# 3. Global env
cat /etc/mpquic/global.env

# 4. Binario
/usr/local/bin/mpquic --version 2>&1 || /usr/local/bin/mpquic --help 2>&1 | head -1

# 5. Servizi attivi
for i in 1 2 3 4 5 6 mp1; do
    printf "%-6s " "$i"
    systemctl is-active mpquic@$i.service 2>/dev/null || echo inactive
done

# 6. TUN interfaces
ip -br a | egrep 'mpq[1-6]|mp1|cr[456]|br[456]|df[456]'

# 7. nftables
sudo nft list ruleset | head -30

# 8. Routing tables
ip rule show | egrep '100[1-6]'

# 9. Connettività tunnel
ping -I mp1 -c 3 10.200.17.254

# 10. Throughput
iperf3 -c 10.200.17.254 -p 5201 -t 5 -P 4 -R --bind-dev mp1
```

#### Server VPS
```bash
# 1. Certificati
ls -l /etc/mpquic/tls/server.crt /etc/mpquic/tls/server.key /etc/mpquic/tls/ca.crt

# 2. Forwarding
sysctl net.ipv4.ip_forward

# 3. nftables
sudo nft list chain inet filter input | egrep '4500|4601|mpq|mt|mp'

# 4. Servizi attivi
for i in 1 2 3 4 5 6 mp1; do
    printf "%-6s " "$i"
    systemctl is-active mpquic@$i.service 2>/dev/null || echo inactive
done

# 5. Porte in ascolto
ss -lunp | egrep '4500|4601'

# 6. Route di ritorno
ip route show | egrep '172\.16\.[1-6]\.0/30|10\.200\.'
```

### 21) Metriche e osservabilità (Fase 5)

#### 21.1 Architettura

Ogni istanza mpquic può esporre metriche su un server HTTP dedicato, **vincolato
all'IP tunnel** (non esposto su Internet). Gli endpoint disponibili sono:

| Endpoint | Formato | Uso |
|----------|---------|-----|
| `/metrics` | Prometheus text exposition | Scraping da Prometheus/Grafana |
| `/api/v1/stats` | JSON strutturato | Portali, script, AI/ML feedback |

Il binding sull'IP tunnel garantisce che le metriche siano raggiungibili **solo
attraverso il tunnel crittografato MPQUIC**, senza alcuna porta esposta su
Internet.

#### 21.2 Configurazione

Aggiungere `metrics_listen: auto` al file YAML dell'istanza:

```yaml
# Server (es. /etc/mpquic/instances/mt4.yaml.tpl)
role: server
bind_ip: 0.0.0.0
remote_port: 45014
multi_conn_enabled: true
tun_name: mt4
tun_cidr: 10.200.14.254/24
metrics_listen: auto          # ← deriva 10.200.14.254:9090 da tun_cidr
log_level: info
tls_cert_file: /etc/mpquic/tls/server.crt
tls_key_file: /etc/mpquic/tls/server.key
```

```yaml
# Client (es. /etc/mpquic/instances/cr4.yaml.tpl)
role: client
bind_ip: if:enp7s6
remote_addr: VPS_PUBLIC_IP
remote_port: 45014
tun_name: cr4
tun_cidr: 10.200.14.1/24
metrics_listen: auto          # ← deriva 10.200.14.1:9090 da tun_cidr
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
```

**Valori possibili per `metrics_listen`:**

| Valore | Risultato |
|--------|-----------|
| `auto` | Estrae l'IP da `tun_cidr` e usa porta 9090 (raccomandato) |
| `10.200.14.254:9090` | Bind esplicito a IP e porta |
| (vuoto/assente) | Metriche disabilitate per questa istanza |

#### 21.3 Installazione config sulle macchine

Dopo aver modificato i template nel repository:

```bash
# 1. Push delle modifiche
cd /opt/mpquic
git add deploy/config/
git commit -m "config: add metrics_listen to instances"
git push origin main
```

**Sul server VPS:**
```bash
cd /opt/mpquic && git pull

# Copia i config aggiornati
for i in mt4 mt5 mt6; do
  cp deploy/config/server/$i.yaml /etc/mpquic/instances/$i.yaml
done

# Rebuild + restart
bash scripts/mpquic-update.sh /opt/mpquic
```

**Sul client:**
```bash
cd /opt/mpquic && git pull

# Copia i config aggiornati
for i in cr4 br4 df4 cr5 br5 df5 cr6 br6 df6; do
  cp deploy/config/client/$i.yaml /etc/mpquic/instances/$i.yaml.tpl
done

# Rebuild + restart
sudo bash scripts/mpquic-update.sh /opt/mpquic
```

#### 21.4 Verifica

```bash
# Dal server → metriche server mt4 (Prometheus format)
curl http://10.200.14.254:9090/metrics

# Dal server → metriche server mt4 (JSON)
curl http://10.200.14.254:9090/api/v1/stats

# Verifica che la porta NON sia raggiungibile da Internet
# (questo deve fallire — nessuna porta esposta)
curl --connect-timeout 3 http://<VPS_PUBLIC_IP>:9090/metrics
# curl: (28) Connection timed out  ← OK, corretto

# Dal client → metriche server (attraverso il tunnel)
curl http://10.200.14.254:9090/api/v1/stats

# Dal server → metriche client cr4 (attraverso il tunnel)
curl http://10.200.14.1:9090/api/v1/stats
```

**Output atteso (JSON):**
```json
{
  "role": "server",
  "version": "4.2",
  "uptime_sec": 35.18,
  "sessions": [...],
  "total_tx_bytes": 123456,
  "total_rx_bytes": 789012,
  "total_tx_pkts": 100,
  "total_rx_pkts": 200
}
```

#### 21.5 Metriche Prometheus esposte

Per la lista completa delle metriche Prometheus (globali, per sessione, per path), vedere `docs/METRICS.md`.

### 22) Stack di monitoraggio: Prometheus + Grafana (LXC Proxmox)

#### 22.1 Architettura

```
┌─────────────────────────────────────────────────────────────┐
│  Proxmox Host (10.10.11.2)                                  │
│                                                             │
│  ┌───────────────────┐    ┌─────────────────────────────┐   │
│  │ CT 201 Prometheus │    │  CT 202 Grafana             │   │
│  │ 10.10.11.201      │    │  10.10.11.202               │   │
│  │ :9090 (web+scrape)│◄───│  :3000 (dashboard)          │   │
│  └────────┬─────────┘     └─────────────────────────────┘   │
│           │                                                 │
│           │ scrape ogni 5s                                  │
│           ▼                                                 │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  VM 200 (10.10.11.100) — Client MPQUIC               │   │
│  │  Gateway per subnet tunnel 10.200.x.0/24             │   │
│  │                                                      │   │
│  │  10.200.17.1:9090  (mp1 client)                      │   │
│  │  10.200.14.1:9090  (cr4)   10.200.15.1:9090 (cr5)    │   │
│  │  10.200.16.1:9090  (cr5)   10.200.10.1:9090 (cr6)    │   │
│  └──────────────────────────────────────────────────────┘   │
│           │                                                 │
│           │ tunnel QUIC/stripe                              │
│           ▼                                                 │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  VPS Server (172.238.232.223)                        │   │
│  │  10.200.17.254:9090 (mp1 server)                     │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

**Decisione chiave**: il gateway dei container è VM 200 (`10.10.11.100`), non il Proxmox host.
Questo permette ai container di raggiungere le subnet tunnel `10.200.x.0/24` senza route statiche.

#### 22.2 Prerequisiti

- Proxmox VE 8.x con bridge `vmbr1` su rete `10.10.11.0/24`
- VM 200 (client MPQUIC) operativa su `10.10.11.100` con IP forwarding abilitato
- Tunnel MPQUIC attivi con `metrics_listen: auto` nei YAML

#### 22.3 Creazione container LXC

I file di deployment sono in `deploy/monitoring/`. Lo script automatizza tutto:

```bash
# Da Proxmox come root (oppure via SSH)
ssh root@10.10.11.2

# Scaricare il template Debian 12 (se non presente)
pveam download local debian-12-standard_12.12-1_amd64.tar.zst

# Creare CT 201 (Prometheus) — 1 vCPU, 512 MB RAM, 8 GB disk (ZFS)
pct create 201 local:vztmpl/debian-12-standard_12.12-1_amd64.tar.zst \
  --hostname prometheus \
  --cores 1 --memory 512 --swap 256 \
  --rootfs local-zfs:8 \
  --net0 name=eth0,bridge=vmbr1,ip=10.10.11.201/24,gw=10.10.11.100 \
  --nameserver 10.10.11.2 \
  --password 'mpquic2025!' \
  --unprivileged 1 --features nesting=1 --onboot 1

# Creare CT 202 (Grafana) — 1 vCPU, 512 MB RAM, 4 GB disk (ZFS)
pct create 202 local:vztmpl/debian-12-standard_12.12-1_amd64.tar.zst \
  --hostname grafana \
  --cores 1 --memory 512 --swap 256 \
  --rootfs local-zfs:4 \
  --net0 name=eth0,bridge=vmbr1,ip=10.10.11.202/24,gw=10.10.11.100 \
  --nameserver 10.10.11.2 \
  --password 'mpquic2025!' \
  --unprivileged 1 --features nesting=1 --onboot 1

# Avviare
pct start 201 && pct start 202
```

#### 22.4 Installazione Prometheus (CT 201)

```bash
# Dall'host Proxmox
pct exec 201 -- bash << 'SETUP'
apt-get update -qq && apt-get install -y -qq curl tar ca-certificates >/dev/null

# Download Prometheus 2.53.4
cd /tmp
curl -sSLO https://github.com/prometheus/prometheus/releases/download/v2.53.4/prometheus-2.53.4.linux-amd64.tar.gz
tar xzf prometheus-2.53.4.linux-amd64.tar.gz

# Installazione
useradd --system --no-create-home --shell /usr/sbin/nologin prometheus 2>/dev/null || true
mkdir -p /opt/prometheus /var/lib/prometheus /etc/prometheus
cp prometheus-2.53.4.linux-amd64/{prometheus,promtool} /opt/prometheus/
chmod +x /opt/prometheus/{prometheus,promtool}
chown -R prometheus:prometheus /var/lib/prometheus
SETUP
```

**Configurazione** (`/etc/prometheus/prometheus.yml`):
```yaml
global:
  scrape_interval: 5s
  evaluation_interval: 5s
  scrape_timeout: 4s

scrape_configs:
  - job_name: "prometheus"
    static_configs:
      - targets: ["localhost:9090"]

  - job_name: "mpquic-server"
    static_configs:
      - targets: ["10.200.17.254:9090"]
        labels:
          instance_name: "mp1"
          role: "server"
          site: "vps"

  - job_name: "mpquic-client"
    static_configs:
      - targets: ["10.200.17.1:9090"]
        labels:
          instance_name: "mp1"
          role: "client"
          site: "client"
          transport: "stripe"
      - targets: ["10.200.14.1:9090"]
        labels:
          instance_name: "cr4"
          role: "client"
          site: "client"
          transport: "quic"
      - targets: ["10.200.16.1:9090"]
        labels:
          instance_name: "cr5"
          role: "client"
          site: "client"
          transport: "quic"
      - targets: ["10.200.10.1:9090"]
        labels:
          instance_name: "cr6"
          role: "client"
          site: "client"
          transport: "quic"
      - targets: ["10.200.15.1:9090"]
        labels:
          instance_name: "cr5"
          role: "client"
          site: "client"
          transport: "quic"
```

**Systemd unit** (`/etc/systemd/system/prometheus.service`):
```ini
[Unit]
Description=Prometheus Monitoring
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=prometheus
Group=prometheus
ExecStart=/opt/prometheus/prometheus \
    --config.file=/etc/prometheus/prometheus.yml \
    --storage.tsdb.path=/var/lib/prometheus \
    --storage.tsdb.retention.time=30d \
    --web.listen-address=0.0.0.0:9090 \
    --web.enable-lifecycle \
    --log.level=info
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
pct exec 201 -- bash -c "systemctl daemon-reload && systemctl enable prometheus && systemctl start prometheus"
```

**Verifica**:
```bash
# Da Proxmox
pct exec 201 -- systemctl is-active prometheus    # → active
pct exec 201 -- curl -s http://localhost:9090/api/v1/targets | grep -o '"health":"[^"]*"' | sort | uniq -c
# Output atteso: N "health":"down"  (tunnel inattivi) + M "health":"up" (tunnel attivi)
```

#### 22.5 Installazione Grafana (CT 202)

```bash
pct exec 202 -- bash << 'SETUP'
apt-get update -qq && apt-get install -y -qq curl ca-certificates gnupg >/dev/null

# Repository Grafana APT
curl -fsSL https://apt.grafana.com/gpg.key | gpg --dearmor -o /usr/share/keyrings/grafana-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/grafana-archive-keyring.gpg] https://apt.grafana.com stable main" \
    > /etc/apt/sources.list.d/grafana.list
apt-get update -qq && apt-get install -y -qq grafana >/dev/null

# Datasource Prometheus (auto-provisioning)
mkdir -p /etc/grafana/provisioning/datasources
cat > /etc/grafana/provisioning/datasources/prometheus.yml << 'DS'
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    uid: prometheus
    access: proxy
    url: http://10.10.11.201:9090
    isDefault: true
    editable: true
    jsonData:
      timeInterval: "5s"
      httpMethod: POST
DS

# Dashboard provider
mkdir -p /etc/grafana/provisioning/dashboards /var/lib/grafana/dashboards
cat > /etc/grafana/provisioning/dashboards/mpquic.yml << 'DPROV'
apiVersion: 1
providers:
  - name: "MPQUIC"
    orgId: 1
    type: file
    disableDeletion: false
    updateIntervalSeconds: 30
    allowUiUpdates: true
    options:
      path: /var/lib/grafana/dashboards
      foldersFromFilesStructure: false
DPROV

systemctl daemon-reload && systemctl enable grafana-server && systemctl start grafana-server
SETUP
```

**Dashboard MPQUIC**: copiare il file JSON nel container:
```bash
# Da Proxmox — il file è in deploy/monitoring/grafana/mpquic-dashboard.json
pct push 202 mpquic-dashboard.json /var/lib/grafana/dashboards/mpquic-dashboard.json
# Oppure:
pct exec 202 -- systemctl restart grafana-server
```

**Credenziali Grafana**: `admin` / `mpquic2025!` (da cambiare al primo accesso)

**Verifica**:
```bash
# Datasource
pct exec 202 -- curl -s -u admin:mpquic2025! http://localhost:3000/api/datasources | grep -o '"name":"[^"]*"'
# → "name":"Prometheus"

# Dashboard
pct exec 202 -- curl -s -u admin:mpquic2025! http://localhost:3000/api/search?type=dash-db | grep -o '"title":"[^"]*"'
# → "title":"MPQUIC Overview"
```

#### 22.6 Accesso alle interfacce web

| Servizio | URL | Credenziali |
|----------|-----|-------------|
| **Prometheus** | http://10.10.11.201:9090 | — (nessuna auth) |
| **Prometheus Targets** | http://10.10.11.201:9090/targets | — |
| **Grafana** | http://10.10.11.202:3000 | `admin` / `mpquic2025!` |
| **Dashboard MPQUIC** | http://10.10.11.202:3000/d/mpquic-overview | — |

#### 22.7 Manutenzione

```bash
# Aggiungere un nuovo target (es. mpq4)
# Editare /etc/prometheus/prometheus.yml nel CT 201, poi:
pct exec 201 -- curl -s -X POST http://localhost:9090/-/reload

# Reload Prometheus senza restart (hot-reload via lifecycle API)
pct exec 201 -- curl -X POST http://localhost:9090/-/reload

# Verificare scrape status
pct exec 201 -- curl -s http://localhost:9090/api/v1/targets | python3 -m json.tool

# Backup dati Prometheus (snapshot)
pct exec 201 -- curl -s -X POST http://localhost:9090/api/v1/admin/tsdb/snapshot
# Lo snapshot viene salvato in /var/lib/prometheus/snapshots/

# Restart servizi
pct exec 201 -- systemctl restart prometheus
pct exec 202 -- systemctl restart grafana-server
```

#### 22.8 Parametri di scraping

| Parametro | Valore | Note |
|-----------|--------|------|
| `scrape_interval` | **5s** | Bilancio tra reattività e carico |
| `scrape_timeout` | 4s | Deve essere < scrape_interval |
| `evaluation_interval` | 5s | Valutazione regole/alert |
| `retention` | 30d | Storico dati su disco |
| Grafana `timeInterval` | 5s | Allineato allo scrape_interval |
| Dashboard `refresh` | 5s | Auto-refresh pannelli |

#### 22.9 Riepilogo infrastruttura monitoring

| CTID | Hostname | IP | Servizio | Storage | RAM |
|------|----------|------|----------|---------|-----|
| 201 | prometheus | 10.10.11.201 | Prometheus 2.53.4 | local-zfs:8GB | 512 MB |
| 202 | grafana | 10.10.11.202 | Grafana OSS | local-zfs:4GB | 512 MB |

Gateway per entrambi: `10.10.11.100` (VM 200 — client MPQUIC)

### 23) Multi-tunnel per link con VLAN (Step 2.5)

#### 23.1 Panoramica

Ogni WAN attiva ottiene 3 tunnel QUIC, uno per classe di traffico (critical/bulk/default).
La classificazione avviene tramite VLAN tagging lato OpenWrt: il traffico arriva su
sub-interface VLAN dedicati nel client VM, e il classifier instrada nel tunnel corretto
in base all'interfaccia di ingresso.

```
OpenWrt → VLAN 21 (critical LAN2) → enp7s1.21 → ip rule iif → cr5 TUN → WAN5 → VPS:45015
OpenWrt → VLAN 22 (bulk LAN2)     → enp7s1.22 → ip rule iif → br5 TUN → WAN5 → VPS:45015
OpenWrt → VLAN 23 (default LAN2)  → enp7s1.23 → ip rule iif → df5 TUN → WAN5 → VPS:45015
```

#### 23.2 Schema VLAN → Tunnel → Server

| LAN trunk | VLAN | Classe | Tunnel | Server TUN | Server porta |
|-----------|------|--------|--------|------------|-------------|
| enp6s23 (LAN4) | 11 | critical | cr4 | mt4 | 45014 |
| enp6s23 (LAN4) | 12 | bulk | br4 | mt4 | 45014 |
| enp6s23 (LAN4) | 13 | default | df4 | mt4 | 45014 |
| enp7s1 (LAN5) | 21 | critical | cr5 | mt5 | 45015 |
| enp7s1 (LAN5) | 22 | bulk | br5 | mt5 | 45015 |
| enp7s1 (LAN5) | 23 | default | df5 | mt5 | 45015 |
| enp7s2 (LAN6) | 31 | critical | cr6 | mt6 | 45016 |
| enp7s2 (LAN6) | 32 | bulk | br6 | mt6 | 45016 |
| enp7s2 (LAN6) | 33 | default | df6 | mt6 | 45016 |

#### 23.3 Installazione automatica (consigliata)

L'installazione è integrata in `install_mpquic.sh`. Su una macchina nuova:

**Client:**
```bash
cd /opt/mpquic
sudo ./scripts/install_client.sh
# Installa automaticamente:
#   - VLAN .netdev/.network in /etc/systemd/network/
#   - Config cr4/br4/df4, cr5/br5/df5, cr6/br6/df6 in /etc/mpquic/instances/
#   - VLAN classifier in /usr/local/sbin/
#   - Abilita tutti i servizi (1-6, cr/br/df, mp1)
#   - Applica il VLAN classifier
```

**Server:**
```bash
cd /opt/mpquic
sudo ./scripts/install_server.sh
# Installa automaticamente:
#   - Config mt4/mt5/mt6 in /etc/mpquic/instances/
#   - NFT: porte 45014-45016, forward mt*, NAT subnet
#   - VPS routes per VLAN transit subnets
```

#### 23.4 Installazione manuale (passo-passo)

##### 23.4.1 Client: VLAN sub-interfaces

```bash
# Copia VLAN netdev e network files
sudo cp deploy/networkd/vlan/*.netdev deploy/networkd/vlan/*.network /etc/systemd/network/

# Ricarica networkd
sudo networkctl reload

# Verifica VLAN interfaces
ip -br link show type vlan
# Atteso: enp6s23.11, enp6s23.12, enp6s23.13, enp7s1.21, ... enp7s2.33
```

##### 23.4.2 Client: config multi-tunnel

```bash
# Copia configs (il .yaml diventa .yaml.tpl; render_config.sh sostituisce VPS_PUBLIC_IP)
for inst in cr4 br4 df4 cr5 br5 df5 cr6 br6 df6; do
  sudo cp deploy/config/client/${inst}.yaml /etc/mpquic/instances/${inst}.yaml.tpl
  sudo cp deploy/config/client/${inst}.env  /etc/mpquic/instances/${inst}.env
done

# Abilita e avvia i servizi
for inst in cr4 br4 df4 cr5 br5 df5 cr6 br6 df6; do
  sudo systemctl enable --now mpquic@${inst}.service
done
```

##### 23.4.3 Client: VLAN classifier

```bash
# Applica tutte le regole di routing per-VLAN
sudo /usr/local/sbin/mpquic-vlan-classifier.sh apply all

# Verifica
sudo /usr/local/sbin/mpquic-vlan-classifier.sh status
```

##### 23.4.4 Server: config multi-tunnel

```bash
# Copia server configs
for inst in mt4 mt5 mt6; do
  sudo cp deploy/config/server/${inst}.yaml /etc/mpquic/instances/${inst}.yaml.tpl
  sudo cp deploy/config/server/${inst}.env  /etc/mpquic/instances/${inst}.env
done

# Abilita e avvia
for inst in mt4 mt5 mt6; do
  sudo systemctl enable --now mpquic@${inst}.service
done
```

##### 23.4.5 Server: nftables e routing

```bash
# Apri porte multi-tunnel
sudo nft add rule inet filter input udp dport 45014 accept
sudo nft add rule inet filter input udp dport 45015 accept
sudo nft add rule inet filter input udp dport 45016 accept

# Forward per mt* tunnel
for tun in mt4 mt5 mt6; do
  sudo nft add rule inet filter forward iifname "$tun" oifname "eth0" accept
  sudo nft add rule inet filter forward iifname "eth0" oifname "$tun" ct state established,related accept
done

# NAT per subnet multi-tunnel
for subnet in 10.200.14.0/24 10.200.15.0/24 10.200.16.0/24; do
  sudo nft add rule ip nat postrouting oifname "eth0" ip saddr "$subnet" masquerade
done

# Salva
sudo nft list ruleset > /etc/nftables.conf

# Route di ritorno per VLAN transit
sudo bash scripts/mpquic-vps-routes.sh
```

#### 23.5 Verifica end-to-end

```bash
# Client: verifica tutti i 9 tunnel UP
for t in cr4 br4 df4 cr5 br5 df5 cr6 br6 df6; do
  printf "%-4s: " "$t"
  ip -4 addr show dev "$t" 2>/dev/null | awk '/inet/{print $2}' || echo "DOWN"
done

# Client: ping peer per ogni classe
for t in cr4 br4 df4; do ping -c1 -W2 -I "$t" 10.200.14.254 && echo "$t OK"; done
for t in cr5 br5 df5; do ping -c1 -W2 -I "$t" 10.200.15.254 && echo "$t OK"; done
for t in cr6 br6 df6; do ping -c1 -W2 -I "$t" 10.200.16.254 && echo "$t OK"; done

# Client: verifica VLAN classifier
sudo /usr/local/sbin/mpquic-vlan-classifier.sh status

# Client: verifica ip rules
ip rule show | grep -E "prio(rity)? 80[0-8]"
```

#### 23.6 Configurazione OpenWrt (lato router)

Il router OpenWrt classifica il traffico LAN tramite VLAN tagging. Ogni trunk
fisico (SL4/SL5/SL6) porta 3 VLAN (critical/bulk/default) verso il client TBOX.

##### Mapping interfacce OpenWrt ↔ TBOX

| OpenWrt IF | Device | TBOX LAN | TBOX Device | Subnet transit |
|------------|--------|----------|-------------|----------------|
| SL4        | eth11  | LAN4     | enp6s23     | 172.16.4.0/30  |
| SL5        | eth12  | LAN5     | enp7s1      | 172.16.5.0/30  |
| SL6        | eth13  | LAN6     | enp7s2      | 172.16.6.0/30  |

##### VLAN → Classe → Tunnel

| VLAN | Classe   | OpenWrt IF | IP OpenWrt  | IP TBOX (gw)  | Tunnel | Metric |
|------|----------|------------|-------------|---------------|--------|--------|
| 11   | critical | eth11.11   | 172.16.11.2 | 172.16.11.1   | cr4    | 10     |
| 12   | bulk     | eth11.12   | 172.16.12.2 | 172.16.12.1   | br4    | 30     |
| 13   | default  | eth11.13   | 172.16.13.2 | 172.16.13.1   | df4    | 20     |
| 21   | critical | eth12.21   | 172.16.21.2 | 172.16.21.1   | cr5    | 10     |
| 22   | bulk     | eth12.22   | 172.16.22.2 | 172.16.22.1   | br5    | 30     |
| 23   | default  | eth12.23   | 172.16.23.2 | 172.16.23.1   | df5    | 20     |
| 31   | critical | eth13.31   | 172.16.31.2 | 172.16.31.1   | cr6    | 10     |
| 32   | bulk     | eth13.32   | 172.16.32.2 | 172.16.32.1   | br6    | 30     |
| 33   | default  | eth13.33   | 172.16.33.2 | 172.16.33.1   | df6    | 20     |

##### Script UCI automatici

Gli script sono in `deploy/openwrt/` e vanno eseguiti in ordine:

```bash
# Copiare su OpenWrt
scp deploy/openwrt/*.sh root@openwrt:/tmp/

# 1. VLAN devices + interfacce statiche (obbligatorio)
ssh root@openwrt 'sh /tmp/01-network-vlan.sh'

# 2. Firewall zones + forwarding da LAN (obbligatorio)
ssh root@openwrt 'sh /tmp/02-firewall-zones.sh'

# 3. mwan3 per classificazione traffico (quando richiesto)
ssh root@openwrt 'sh /tmp/03-mwan3-policy.sh'

# 4. DSCP marking nftables (opzionale, se si usa DSCP)
ssh root@openwrt 'sh /tmp/04-nft-dscp-mark.sh'

# Cleanup completo (rimuove tutto)
ssh root@openwrt 'sh /tmp/99-remove-vlan.sh'
```

I trunk interface sono configurabili in `01-network-vlan.sh` (variabili
`TRUNK_SL4`, `TRUNK_SL5`, `TRUNK_SL6`). Il resto è identico per ogni TBOX.

**Stato attuale (2026-03-15)**: step 1 (network VLAN) e step 2 (firewall zones)
applicati. mwan3 posticipato a fase test/produzione.

##### Verifica da OpenWrt

```bash
# VLAN devices creati
uci show network | grep vlan

# Interfacce attive
ifstatus cr4    # deve mostrare up: true, ipv4-address: 172.16.11.2

# Ping verso TBOX gateway (verifica connettività VLAN)
ping -c3 172.16.11.1   # cr4 via eth11.11
ping -c3 172.16.21.1   # cr5 via eth12.21
ping -c3 172.16.31.1   # cr6 via eth13.31

# Firewall zones
uci show firewall | grep wan_cr
```

##### Classificazione traffico (mwan3)

Quando mwan3 verrà attivato:

| Classe       | Policy        | Protocolli                                       |
|--------------|---------------|--------------------------------------------------|
| **critical** | pol_critical  | SIP (UDP 5060), RTP (10000-20000), DNS, SSH      |
| **default**  | pol_default   | HTTP (80), HTTPS (443), HTTPS-alt (8443)         |
| **bulk**     | pol_bulk      | Tutto il resto (catch-all)                       |

Ogni policy bilancia su 3 tunnel (uno per WAN) con failover automatico.

### 24) Management API — `mpquic-mgmt` (Fase 5a)

#### 24.1 Panoramica

`mpquic-mgmt` è un daemon REST API che gira sulla TBOX e fornisce controllo
centralizzato su tutte le istanze tunnel mpquic. Viene consumato da:
- **LuCI UI** su OpenWrt (via rpcd proxy)
- **AI/ML Decision Layer** per auto-tuning
- **Operatori** via curl/script dalla rete di management

Il daemon è un binario Go separato (`cmd/mpquic-mgmt/`) che non interferisce
con i tunnel in esecuzione.

#### 24.2 Architettura

```
┌────────────────────┐     ┌─────────────────────────────────────────────┐
│ OpenWrt (LuCI)     │     │ TBOX (10.10.11.100)                         │
│ 10.10.11.254       │────▶│                                             │
│  rpcd → ubus       │     │  mpquic-mgmt :8080 ◀── Bearer token auth    │
└────────────────────┘     │       │                                     │
                           │       ├─▶ systemctl start/stop/restart      │
┌────────────────────┐     │       ├─▶ /etc/mpquic/instances/*.yaml      │
│ Operatore (curl)   │────▶│       ├─▶ mpquic@{name} journalctl          │
└────────────────────┘     │       └─▶ {tunnel}:9090/api/v1/stats proxy  │
                           └─────────────────────────────────────────────┘
```

#### 24.3 Build

```bash
cd /opt/mpquic
make build-mgmt
# Produce: bin/mpquic-mgmt (versione iniettata via ldflags)

# Per build completa (tunnel + mgmt):
make build-all
```

#### 24.4 Installazione

##### 24.4.1 Copia binario

```bash
sudo cp bin/mpquic-mgmt /usr/local/bin/mpquic-mgmt
sudo chmod 0755 /usr/local/bin/mpquic-mgmt
```

##### 24.4.2 Genera token autenticazione

Il token DEVE essere:
- Minimo 16 caratteri (il daemon rifiuta token più corti)
- Generato con fonte crittografica (non valori predicibili)
- Salvato in file con permessi 600

```bash
# Genera token casuale (44 chars base64)
TOKEN=$(openssl rand -base64 32)

# Scrivi environment file
sudo bash -c "echo 'MGMT_AUTH_TOKEN=$TOKEN' > /etc/mpquic/mgmt.env"
sudo chmod 600 /etc/mpquic/mgmt.env

# Mostra (annotare per configurazione LuCI/client)
echo "Token: $TOKEN"
```

**IMPORTANTE**: il token viene letto SOLO dalla variabile d'ambiente `MGMT_AUTH_TOKEN`
(tramite `EnvironmentFile` di systemd), mai dalla riga di comando. Questo impedisce
che sia visibile in `ps aux` o `/proc/PID/cmdline`.

##### 24.4.3 Installa systemd unit

```bash
sudo cp deploy/systemd/mpquic-mgmt.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now mpquic-mgmt
```

##### 24.4.4 Verifica servizio

```bash
sudo systemctl status mpquic-mgmt
# Active: active (running)
# Main PID: ... (mpquic-mgmt)
# → listening on 127.0.0.1:8080

# Test health endpoint
TOKEN=$(sudo grep MGMT_AUTH_TOKEN /etc/mpquic/mgmt.env | cut -d= -f2-)
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/health
```

**Output atteso:**
```json
{
  "ok": true,
  "version": "8095e01",
  "hostname": "mpquic",
  "tunnels_total": 16,
  "tunnels_running": 4,
  "tunnels_stopped": 12,
  "tunnels_failed": 0,
  "timestamp": "2026-03-25T11:40:56Z"
}
```

#### 24.5 Configurazione

##### 24.5.1 Flag CLI

| Flag | Default | Descrizione |
|------|---------|-------------|
| `--listen` | `127.0.0.1:8080` | Indirizzo di ascolto HTTP |
| `--instance-dir` | `/etc/mpquic/instances` | Directory YAML tunnel |
| `--auth-token` | (vuoto) | Token auth (preferire MGMT_AUTH_TOKEN env) |
| `--tls-cert` | (vuoto) | Certificato TLS per HTTPS |
| `--tls-key` | (vuoto) | Chiave privata TLS |
| `--cors-origins` | (vuoto) | Origini CORS permesse, comma-separated |

##### 24.5.2 Accesso dalla LAN

Per default il daemon ascolta solo su localhost. Per permettere accesso dalla
LAN (es. da OpenWrt o dalla rete di management), modificare il servizio:

**Opzione A — Bind su interfaccia LAN (senza TLS):**
Editare `/etc/systemd/system/mpquic-mgmt.service`:
```ini
ExecStart=/usr/local/bin/mpquic-mgmt \
    --listen 10.10.11.100:8080 \
    --instance-dir /etc/mpquic/instances
```

**Opzione B — HTTPS con certificato self-signed (raccomandato per produzione):**
```bash
# Genera certificato self-signed
sudo openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout /etc/mpquic/tls/mgmt.key \
  -out /etc/mpquic/tls/mgmt.crt \
  -days 3650 -subj "/CN=mpquic-mgmt"
```
```ini
ExecStart=/usr/local/bin/mpquic-mgmt \
    --listen 10.10.11.100:8443 \
    --instance-dir /etc/mpquic/instances \
    --tls-cert /etc/mpquic/tls/mgmt.crt \
    --tls-key /etc/mpquic/tls/mgmt.key
```

Dopo ogni modifica:
```bash
sudo systemctl daemon-reload
sudo systemctl restart mpquic-mgmt
```

##### 24.5.3 CORS per LuCI

Se LuCI (OpenWrt su `http://10.10.11.254`) deve accedere direttamente all'API:
```ini
ExecStart=/usr/local/bin/mpquic-mgmt \
    --listen 10.10.11.100:8080 \
    --cors-origins http://10.10.11.254
```

#### 24.6 API Reference

##### Endpoints

| Metodo | Endpoint | Descrizione | Auth |
|--------|----------|-------------|------|
| GET | `/api/v1/health` | Overview sistema | Sì |
| GET | `/api/v1/tunnels` | Lista tunnel con stato | Sì |
| GET | `/api/v1/tunnels/{name}` | Dettaglio + config completa | Sì |
| POST | `/api/v1/tunnels/{name}/start` | Avvia istanza | Sì |
| POST | `/api/v1/tunnels/{name}/stop` | Ferma istanza | Sì |
| POST | `/api/v1/tunnels/{name}/restart` | Riavvia istanza | Sì |
| GET | `/api/v1/tunnels/{name}/config` | Config JSON + categorie | Sì |
| PATCH | `/api/v1/tunnels/{name}/config` | Modifica parziale config | Sì |
| POST | `/api/v1/tunnels/{name}/config/validate` | Dry-run validazione | Sì |
| GET | `/api/v1/tunnels/{name}/metrics` | Proxy metriche tunnel | Sì |
| GET | `/api/v1/tunnels/{name}/logs?lines=N&level=error` | Journal logs | Sì |
| GET | `/api/v1/metrics` | Metriche aggregate | Sì |
| GET | `/api/v1/system/info` | Versione, uptime, OS | Sì |
| GET | `/api/v1/system/logs/{name}?lines=N` | Logs via system route | Sì |

##### Autenticazione

Ogni richiesta deve includere header:
```
Authorization: Bearer <TOKEN>
```

Risposte errore:
- **401 Unauthorized**: token mancante o errato
- **429 Too Many Requests**: rate limit superato (10 tentativi falliti / 5min per IP)

##### Classificazione parametri config

I parametri YAML sono classificati in 3 categorie:

| Categoria | Comportamento | Parametri |
|-----------|---------------|-----------|
| **A — Hot-reload** | Modifica applicata senza restart | `log_level`, `stripe_pacing_rate`, `stripe_fec_mode`, `multipath_policy` |
| **B — Restart** | Richiede restart tunnel | `tun_mtu`, `congestion_algorithm`, `transport_mode`, `stripe_arq`, `stripe_fec_type`, `stripe_fec_window`, `stripe_fec_interleave`, `stripe_disable_gso`, `detect_starlink`, `starlink_default_pipes`, `starlink_transport`, `stripe_enabled` |
| **C — Bloccato** | Non modificabile (server-coupled) | `role`, `bind_ip`, `remote_addr`, `remote_port`, `tun_name`, `tun_cidr`, `stripe_port`, `stripe_data_shards`, `stripe_parity_shards`, `tls_*`, `metrics_listen`, `control_api_*` |

Esempio modifica Cat. A (nessun restart):
```bash
curl -X PATCH -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"log_level":"debug"}' \
  http://127.0.0.1:8080/api/v1/tunnels/mp1/config
# → {"ok": true, "needs_restart": false}
```

Esempio modifica Cat. B (richiede restart, auto_restart opzionale):
```bash
curl -X PATCH -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"congestion_algorithm":"cubic"}' \
  'http://127.0.0.1:8080/api/v1/tunnels/mp1/config?auto_restart=true'
# → {"ok": true, "needs_restart": true, "restart_applied": true}
```

Esempio modifica Cat. C (bloccata):
```bash
curl -X PATCH -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"remote_addr":"1.2.3.4"}' \
  http://127.0.0.1:8080/api/v1/tunnels/mp1/config
# → 400 {"error": "server-coupled parameters cannot be modified: [remote_addr]"}
```

#### 24.7 Sicurezza

Il daemon implementa le seguenti misure di sicurezza:

| Misura | Dettaglio |
|--------|-----------|
| **Timing-safe compare** | `crypto/subtle.ConstantTimeCompare` per confronto token |
| **Rate limiting** | 10 fallimenti auth / 5min per IP, poi 429 |
| **Token da env var** | Non esposto in `/proc/PID/cmdline` |
| **Token minimo 16 char** | Fatal error all'avvio se troppo corto |
| **Input sanitization** | Nomi tunnel: regex `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$` |
| **TLS opzionale** | `--tls-cert/--tls-key` per HTTPS |
| **CORS locked** | Disabilitato default, whitelist esplicita |
| **Localhost default** | `127.0.0.1:8080` (non esposto su LAN) |
| **Security headers** | X-Frame-Options DENY, HSTS, no-sniff, no-cache |
| **Systemd hardening** | NoNewPrivileges, ProtectSystem=strict, PrivateTmp |
| **Audit logging** | Auth failure e injection attempt → journald con IP |

Verifica audit log:
```bash
sudo journalctl -u mpquic-mgmt --grep='SECURITY' --no-pager -n 20
```

#### 24.8 Aggiornamento

`mpquic-mgmt` è automaticamente aggiornato da `mpquic-update.sh`:

```bash
sudo bash scripts/mpquic-update.sh /opt/mpquic
# Step 2: make build-all  (include mgmt)
# Step 5: installa bin/mpquic-mgmt in /usr/local/bin/
# Step 7: systemctl restart mpquic-mgmt (se enabled)
```

#### 24.9 Troubleshooting

```bash
# Servizio non parte
sudo journalctl -u mpquic-mgmt --no-pager -n 50
# Errori comuni:
#   "FATAL: auth token required"     → manca MGMT_AUTH_TOKEN in mgmt.env
#   "FATAL: auth token too short"    → token < 16 caratteri

# Test da altra macchina (verifica bind)
curl -v http://10.10.11.100:8080/api/v1/health
# Se "Connection refused" → il daemon ascolta su 127.0.0.1 (default),
# serve --listen 10.10.11.100:8080

# Verifica che il token non sia in cmdline
ps aux | grep mpquic-mgmt | grep -v grep
# Il token NON deve apparire nella riga di comando

# Verifica permessi env file
ls -la /etc/mpquic/mgmt.env
# -rw------- 1 root root ... → OK (solo root può leggere)
```

### 25) LuCI App per OpenWrt — `luci-app-mpquic` (Fase 5b)

#### 25.1 Panoramica

`luci-app-mpquic` fornisce interfaccia web LuCI integrata nel router OpenWrt
(10.10.11.254) per gestione e monitoraggio di tutti i tunnel MPQUIC sulla TBOX.

**Architettura:**
```
Browser → LuCI JS → ubus/rpcd → rpcd/mpquic (shell, wget) → TBOX mpquic-mgmt :8080
```

Il token di autenticazione è custodito in UCI (`/etc/config/mpquic`) sul router,
mai esposto al browser. Il rpcd plugin inietta il token server-side nelle chiamate.

#### 25.2 Prerequisiti

- OpenWrt 24.10+ (verificato su 24.10.0, x86_64)
- `wget` BusyBox integrato (disponibile di default)
- `jsonfilter` integrato (disponibile di default)
- `mpquic-mgmt` sulla TBOX in ascolto su IP LAN (es. `10.10.11.100:8080`)
- Token auth TBOX noto (da `/etc/mpquic/mgmt.env` sulla TBOX)

**NON serve**: curl, Lua extra, node.js, compilatore. Zero dipendenze aggiuntive.

#### 25.3 Configurazione TBOX — Listen address

Prima di installare LuCI, assicurarsi che `mpquic-mgmt` ascolti su IP LAN:

```bash
# Sulla TBOX:
# Verificare /etc/systemd/system/mpquic-mgmt.service contiene:
#   --listen 10.10.11.100:8080
# Se serve cambiare da 127.0.0.1 a IP LAN:
sudo sed -i 's/127.0.0.1:8080/10.10.11.100:8080/' /etc/systemd/system/mpquic-mgmt.service
sudo systemctl daemon-reload
sudo systemctl restart mpquic-mgmt

# Verifica
sudo systemctl status mpquic-mgmt | grep listen
# → listening on 10.10.11.100:8080
```

#### 25.4 Installazione automatica

```bash
# Dal dev machine, con ssh-agent verso OpenWrt e TBOX:
cd /opt/mpquic
TOKEN=$(ssh mpquic 'sudo grep MGMT_AUTH_TOKEN /etc/mpquic/mgmt.env | cut -d= -f2-')
bash deploy/luci-app-mpquic/install.sh 10.10.11.254 10.10.11.100 "$TOKEN"
```

Lo script installa automaticamente:
1. rpcd plugin → `/usr/libexec/rpcd/mpquic`
2. ACL → `/usr/share/rpcd/acl.d/luci-app-mpquic.json`
3. Menu LuCI → `/usr/share/luci/menu.d/luci-app-mpquic.json`
4. Views JS → `/www/luci-static/resources/view/mpquic/`
5. UCI config → `/etc/config/mpquic`
6. Restart rpcd + uhttpd

#### 25.5 Installazione manuale (step-by-step)

Se l'installazione automatica non funziona, eseguire manualmente:

```bash
# 1. Copiare rpcd plugin
scp deploy/luci-app-mpquic/root/usr/libexec/rpcd/mpquic root@10.10.11.254:/usr/libexec/rpcd/mpquic
ssh root@10.10.11.254 'chmod 0755 /usr/libexec/rpcd/mpquic'

# 2. Copiare ACL
scp deploy/luci-app-mpquic/root/usr/share/rpcd/acl.d/luci-app-mpquic.json \
    root@10.10.11.254:/usr/share/rpcd/acl.d/luci-app-mpquic.json

# 3. Copiare menu LuCI
scp deploy/luci-app-mpquic/root/usr/share/luci/menu.d/luci-app-mpquic.json \
    root@10.10.11.254:/usr/share/luci/menu.d/luci-app-mpquic.json

# 4. Copiare views JavaScript
ssh root@10.10.11.254 'mkdir -p /www/luci-static/resources/view/mpquic'
scp deploy/luci-app-mpquic/htdocs/luci-static/resources/view/mpquic/*.js \
    root@10.10.11.254:/www/luci-static/resources/view/mpquic/

# 5. Creare config UCI
TOKEN="..."  # Token dalla TBOX
ssh root@10.10.11.254 "cat > /etc/config/mpquic <<EOF
config api 'api'
	option host '10.10.11.100'
	option port '8080'
	option proto 'http'
	option token '${TOKEN}'
	option timeout '10'
EOF"

# 6. Restart servizi
ssh root@10.10.11.254 '/etc/init.d/rpcd restart && /etc/init.d/uhttpd restart'
```

#### 25.6 Verifica rpcd

```bash
# Da locale verso OpenWrt:
ssh root@10.10.11.254 'ubus call mpquic health'
# Output atteso:
# {
#     "ok": true,
#     "tunnels_total": 16,
#     "tunnels_running": 4,
#     ...
# }

# Test tutti i metodi:
ssh root@10.10.11.254 'ubus call mpquic tunnels' | head -10
ssh root@10.10.11.254 'ubus call mpquic tunnel_detail "{\"name\":\"mp1\"}"'
ssh root@10.10.11.254 'ubus call mpquic system_info'
ssh root@10.10.11.254 'ubus call mpquic tunnel_logs "{\"name\":\"mp1\",\"lines\":5}"'

# Verifica sicurezza (injection):
ssh root@10.10.11.254 'ubus call mpquic tunnel_detail "{\"name\":\"../etc/passwd\"}"'
# → {"error": "invalid tunnel name"}
```

#### 25.7 Accesso LuCI

Dopo l'installazione, la UI è disponibile su:

```
http://10.10.11.254/cgi-bin/luci/admin/services/mpquic/dashboard
```

Menu LuCI: **Services → MPQUIC Tunnels → Dashboard / Configuration**

**Dashboard**: mostra health cards (Total/Running/Stopped/Failed/Version),
tabella tunnel con stato, WAN, uptime, TX/RX, loss, RTT, FEC, azioni start/stop/restart.
Auto-refresh ogni 10 secondi.

**Configuration**: selettore tunnel, form editor con parametri divisi per categoria:
- **Cat. A** (verde): modificabili senza restart — `log_level`, `stripe_pacing_rate`, ecc.
- **Cat. B** (giallo): modificabili con restart — `tun_mtu`, `congestion_algorithm`, ecc.
- **Cat. C** (rosso): read-only — `remote_addr`, `tun_name`, ecc.
Bottoni: Validate (dry-run), Apply, Apply+Restart.

#### 25.8 Note tecniche

**Perché wget e non curl:**
OpenWrt 24.10.0 ha una versione di curl con libcurl mismatch (8.12.1 vs 8.10.1)
che causa `Error relocating: curl_easy_ssls_import: symbol not found`.
BusyBox wget è sempre presente e funzionante.

**PATCH via X-HTTP-Method-Override:**
BusyBox wget non supporta il metodo HTTP PATCH. Il rpcd plugin usa POST con
header `X-HTTP-Method-Override: PATCH` che il server mpquic-mgmt riconosce.

**Config UCI protetto:**
Il token è in `/etc/config/mpquic`, leggibile solo da root. L'ACL rpcd
limita l'accesso ubus al solo utente `admin` di LuCI.

#### 25.9 Aggiornamento

Per aggiornare le views LuCI dopo un commit:
```bash
cd /opt/mpquic
git pull origin main
# Reinstallare files modificati:
scp deploy/luci-app-mpquic/htdocs/luci-static/resources/view/mpquic/*.js \
    root@10.10.11.254:/www/luci-static/resources/view/mpquic/
scp deploy/luci-app-mpquic/root/usr/libexec/rpcd/mpquic \
    root@10.10.11.254:/usr/libexec/rpcd/mpquic
ssh root@10.10.11.254 'chmod 0755 /usr/libexec/rpcd/mpquic && /etc/init.d/rpcd restart'
```

#### 25.10 Troubleshooting

```bash
# rpcd non elenca mpquic
ubus list | grep mpquic
# Se assente: verificare che /usr/libexec/rpcd/mpquic è eseguibile (chmod 0755)
# e che /etc/init.d/rpcd restart è stato eseguito

# "no auth token configured"
cat /etc/config/mpquic
# → Verificare option token non sia vuoto

# "connection to TBOX failed"
# 1. Verificare connettività: ping 10.10.11.100
# 2. Verificare mgmt ascolta su LAN: ssh satcom@10.10.11.100 'ss -tlnp | grep 8080'
# 3. Test manuale: wget -q -O - --header="Authorization: Bearer TOKEN" http://10.10.11.100:8080/api/v1/health

# LuCI mostra pagina vuota
# Svuotare cache browser, poi verificare file JS:
ls -la /www/luci-static/resources/view/mpquic/
# Se mancanti, reinstallare views (vedi 25.5 punto 4)

# Menu "MPQUIC Tunnels" non appare
ls -la /usr/share/luci/menu.d/luci-app-mpquic.json
# Se presente ma non visibile: /etc/init.d/uhttpd restart
```

---

## 7. Operazioni e debug tunnel

Questa guida è la runbook pratica per esercire e debuggare i tunnel `mpq1..mpq6` su TBOX/MPQUIC.

### 0) Regola SSH operativa (IPS)

Host VPS canonicale: `vps-it-mpquic`.

Per evitare disconnessioni IPS, su VPS usare sempre sequenza interattiva:

```bash
ssh vps-it-mpquic
# esegui i comandi dentro la sessione
exit
```

Da evitare su VPS:

```bash
ssh vps-it-mpquic 'comando'
```

### 1) Uso operativo quotidiano

### 1.0 Aggiornamento repository (regola operativa)

Per aggiornare il software usare solo:

```bash
sudo /usr/local/sbin/mpquic-update.sh
```

Non usare `scp` per aggiornamenti standard: per evitare disallineamenti, la fonte di verità resta il repository Git.

### 1.1 Stato rapido client
```bash
for i in 1 2 3 4 5 6; do
  printf "@%s=" "$i"
  systemctl is-active mpquic@$i.service || true
done

ip -br a | egrep '^enp7s[3-8]|^mpq[1-6]'
ss -unap | grep mpquic || true
```

### 1.2 Stato rapido VPS
```bash
for i in 1 2 3 4 5 6; do
  printf "@%s=" "$i"
  systemctl is-active mpquic@$i.service || true
done

ip -br a | egrep '^mpq[1-6]'
ss -lunp | egrep '4500[1-6]' || true
```

### 1.3 Restart completo dopo restart rete

Client:
```bash
for i in 1 2 3 4 5 6; do systemctl restart mpquic@$i.service; done
systemctl restart mpquic-routing.service
systemctl restart mpquic-watchdog.service
```

VPS:
```bash
for i in 1 2 3 4 5 6; do systemctl restart mpquic@$i.service; done
systemctl restart mpquic-vps-routes.service
systemctl restart nftables
```

### 1.3-bis Problema ricorrente VM/OpenWRT (procedura iniziale)

Sintomo osservato più volte: tunnel formalmente attivi ma traffico non instradato correttamente tra VM MPQUIC e router OpenWRT finché non viene ripristinata la rete della VM.

Ordine operativo obbligatorio:
1. restart network lato VM MPQUIC (prima azione)
2. restart servizi MPQUIC/routing
3. verifiche healthcheck + route table
4. reboot VM solo se i passi 1..3 non risolvono

Client (first response):
```bash
systemctl restart networking || true
ifreload -a || true
for i in 1 2 3 4 5 6; do systemctl restart mpquic@$i.service; done
systemctl restart mpquic-routing.service
systemctl restart mpquic-watchdog.timer
/usr/local/sbin/mpquic-healthcheck.sh client fix
/usr/local/sbin/mpquic-lan-routing-check.sh fix all
```

Se ancora KO:
```bash
reboot
```

### 1.4 Check rapido strutturato (con auto-fix)

Client:
```bash
sudo /usr/local/sbin/mpquic-healthcheck.sh client check
sudo /usr/local/sbin/mpquic-healthcheck.sh client fix
```

Server:
```bash
sudo /usr/local/sbin/mpquic-healthcheck.sh server check
sudo /usr/local/sbin/mpquic-healthcheck.sh server fix
```

Regola firewall obbligatoria su VPS (nftables input policy drop):
```bash
nft list chain inet filter input
# deve esistere una riga equivalente a:
# udp dport 45001-45006 accept
```

### 1.5 Smoke test multipath (Fase 4 Step 1, sperimentale)

Config template di riferimento:
- `deploy/config/client/multipath.yaml`

Client (esegue test non distruttivo con TUN dedicata `mpqm`):
```bash
export VPS_PUBLIC_IP=<IP_VPS>
sudo /usr/local/sbin/mpquic-multipath-smoke.sh
```

Atteso:
- output `smoke test PASS`
- nel log compaiono `connected multipath paths=` e almeno un `path up name=`

Verifica telemetria path-level (client):
```bash
journalctl -u mpquic@4.service -n 200 --no-pager | grep 'path telemetry' || true
```

Chiavi da leggere nel log telemetrico:
- `state=up|down`
- `tx_pkts`, `rx_pkts`
- `tx_err`, `rx_err`
- `fails`, `cooldown_until`, `last_up`, `last_down`

Tuning QoS path-aware (attuale):
- regola `priority` e `weight` in `multipath_paths`
- riavvia il processo che usa quella config
- verifica nei log che i path desiderati risultino preferiti/stabili

Controllo VPS (sequenza SSH obbligatoria):
```bash
ssh vps-it-mpquic
ss -lunp | egrep '4500[4-6]'
journalctl -u mpquic@4.service -n 30 --no-pager
journalctl -u mpquic@5.service -n 30 --no-pager
journalctl -u mpquic@6.service -n 30 --no-pager
exit
```

### 2) Mapping e comportamento atteso

- `LAN1 (172.16.1.0/30)` -> `mpq1` -> QUIC `udp/45001` su `enp7s3`
- `LAN2 (172.16.2.0/30)` -> `mpq2` -> QUIC `udp/45002` su `enp7s4`
- `LAN3 (172.16.3.0/30)` -> `mpq3` -> QUIC `udp/45003` su `enp7s5`
- `LAN4 (172.16.4.0/30)` -> `mpq4` -> QUIC `udp/45004` su `enp7s6`
- `LAN5 (172.16.5.0/30)` -> `mpq5` -> QUIC `udp/45005` su `enp7s7`
- `LAN6 (172.16.6.0/30)` -> `mpq6` -> QUIC `udp/45006` su `enp7s8`

Nessun failover cross-tunnel: se WANx non è disponibile, il tunnel x deve fermarsi (o restare non connesso).

### 3) Debug per sintomo

### 3.0 Multipath: rumore "superseded" durante test

Se esegui smoke multipath su porte già usate da istanze `mpquic@X` attive, il server può chiudere sessioni precedenti con evento `superseded`.

Per test pulito:
1. stop temporaneo delle istanze client in conflitto con le porte usate dal multipath
2. esecuzione smoke test
3. riavvio istanze baseline

### 3.1 Tunnel `active` ma non passa traffico

Client:
```bash
journalctl -u mpquic@4.service -n 80 --no-pager
ip route show table 103
ip route show table 104
ip route show table 105
```

Controlla:
- presenza `default dev mpqX` nella tabella corretta
- presenza route `/32` verso VPS sulla WAN associata

VPS:
```bash
ip route get 172.16.4.2
ip route get 172.16.5.2
ip route get 172.16.6.2
nft list ruleset | sed -n '1,220p'
```

Controlla:
- ritorno verso LAN su `mpqX` (non su `eth0`)
- NAT attivo su `eth0` per subnet `172.16.x.0/30`
- apertura UDP `45001-45006` nella chain `inet filter input` (altrimenti client in timeout continuo)

### 3.2 Messaggio `no ipv4 found on enp7sX`

Significa che la WAN associata non ha IPv4 DHCP valida.

Check:
```bash
ip -4 -br a show dev enp7s3
ip -4 -br a show dev enp7s4
ip -4 -br a show dev enp7s5
ip -4 -br a show dev enp7s6
ip -4 -br a show dev enp7s7
ip -4 -br a show dev enp7s8
```

Se mancante: il tunnel relativo non potrà connettersi finché WAN non torna up.

### 3.3 Verifica 1:1 reale (no cross-tunnel)

Esempio LAN4:

OpenWRT:
```bash
mwan3 use SL4 ping 8.8.8.8
```

Client (deve vedere traffico su `mpq4`):
```bash
tcpdump -ni mpq4
tcpdump -ni mpq5
tcpdump -ni mpq6
```

Atteso:
- pacchetti nel solo `mpq4`
- assenza di pacchetti equivalenti su `mpq5/mpq6`

Verifica incapsulamento QUIC su WAN4:
```bash
tcpdump -ni enp7s6 udp port 45004
```

### 3.4 Auto-heal non interviene dopo flap WAN

Check:
```bash
systemctl is-active mpquic-watchdog.timer
systemctl status mpquic-watchdog.timer --no-pager
journalctl -u mpquic-watchdog.service -n 50 --no-pager
ls -l /etc/network/if-up.d/mpquic-auto /etc/network/if-post-down.d/mpquic-auto
```

Recovery:
```bash
systemctl restart mpquic-watchdog.timer
systemctl restart mpquic-watchdog.service
```

Copertura attuale watchdog client:
- single-path: `1..6`
- multipath: `mp1`
- multi-class VLAN: `cr4..6`, `br4..6`, `df4..6`

### 3.5 Su VPS i tunnel restano down

Check:
```bash
systemctl is-active mpquic-server-watchdog.timer
systemctl status mpquic-server-watchdog.timer --no-pager
journalctl -u mpquic-server-watchdog.service -n 50 --no-pager
for i in 1 2 3 4 5 6 mp1 mt1 mt4 mt5 mt6; do
  printf "@%s=" "$i"
  systemctl is-active mpquic@$i.service || true
done
```

Nota: il watchdog verifica sia `is-active` sia salute TUN (`TUN_NAME/TUN_CIDR` presenti e interfaccia `UP`).
Se il processo è attivo ma il tunnel è rotto (es. `write tun: input/output error`), forza restart dell'istanza.

Recovery:
```bash
for i in 1 2 3 4 5 6 mp1 mt1 mt4 mt5 mt6; do systemctl restart mpquic@$i.service; done
systemctl restart mpquic-vps-routes.service
systemctl restart mpquic-server-watchdog.timer
systemctl restart mpquic-server-watchdog.service
```

### 3.6 Tunnel cr/br/df in stato down

Check rapido su VM MPQUIC:
```bash
for s in cr4 cr5 cr6 br4 br5 br6 df4 df5 df6; do
  printf "%s: enabled=%s active=%s\n" \
    "$s" \
    "$(systemctl is-enabled mpquic@$s.service 2>/dev/null || echo no)" \
    "$(systemctl is-active mpquic@$s.service 2>/dev/null || echo down)"
done
```

Restart mirato:
```bash
for s in cr4 cr5 cr6 br4 br5 br6 df4 df5 df6; do
  systemctl restart mpquic@$s.service
done
```

Nota: per la demo, i tunnel cr/br/df possono essere lasciati intenzionalmente spenti.
In quel caso un alert Grafana su queste istanze va marcato come expected/maintenance.

### 4) TLS debug

### 4.1 File certificati

Server:
```bash
ls -l /etc/mpquic/tls/server.crt /etc/mpquic/tls/server.key /etc/mpquic/tls/ca.crt
```

Client:
```bash
ls -l /etc/mpquic/tls/ca.crt
grep -R "tls_" /etc/mpquic/instances/*.yaml.tpl /etc/mpquic/instances/*.yaml
```

### 4.2 Errori tipici

- `x509: certificate signed by unknown authority`
  - `ca.crt` assente/non allineato sul client
- `tls: failed to find any PEM data`
  - file certificato/chiave corrotti o path errato
- mismatch `tls_server_name`
  - CN certificato diverso da valore in YAML client

### 5) Raccolta evidenze per troubleshooting

Client:
```bash
date
hostname
for i in 1 2 3 4 5 6 mp1 cr4 cr5 cr6 br4 br5 br6 df4 df5 df6; do systemctl is-active mpquic@$i.service || true; done
ip -br a | egrep '^enp7s[3-8]|^mpq[1-6]'
ip rule show | egrep '100[1-6]'
ip route show table 100
ip route show table 101
ip route show table 102
ip route show table 103
ip route show table 104
ip route show table 105
ss -unap | grep mpquic || true
```

VPS:
```bash
date
hostname
for i in 1 2 3 4 5 6 mp1 mt1 mt4 mt5 mt6; do systemctl is-active mpquic@$i.service || true; done
ip -br a | egrep '^mpq[1-6]|^eth0'
ip route show | egrep '172\.16\.[1-6]\.0/30|10\.200\.'
nft list ruleset | sed -n '1,220p'
```

---

### Appendice A – Installazione watchdog

#### A.1 Client — tunnel watchdog + hook eventi interfaccia

```bash
sudo install -m 0755 scripts/mpquic-tunnel-watchdog.sh /usr/local/lib/mpquic/mpquic-tunnel-watchdog.sh
sudo install -m 0755 scripts/mpquic-if-event.sh /usr/local/lib/mpquic/mpquic-if-event.sh
sudo install -m 0644 deploy/systemd/mpquic-watchdog.service /etc/systemd/system/mpquic-watchdog.service
sudo install -m 0644 deploy/systemd/mpquic-watchdog.timer /etc/systemd/system/mpquic-watchdog.timer
sudo install -m 0755 deploy/hooks/mpquic-ifupdown-hook /etc/network/if-up.d/mpquic-auto
sudo install -m 0755 deploy/hooks/mpquic-ifupdown-hook /etc/network/if-post-down.d/mpquic-auto
sudo systemctl daemon-reload
sudo systemctl enable --now mpquic-watchdog.timer
```

Verifica:
```bash
systemctl is-active mpquic-watchdog.timer
journalctl -u mpquic-watchdog.service -n 20 --no-pager
```

#### A.2 VPS — server watchdog

```bash
sudo install -m 0755 scripts/mpquic-server-watchdog.sh /usr/local/lib/mpquic/mpquic-server-watchdog.sh
sudo install -m 0644 deploy/systemd/mpquic-server-watchdog.service /etc/systemd/system/mpquic-server-watchdog.service
sudo install -m 0644 deploy/systemd/mpquic-server-watchdog.timer /etc/systemd/system/mpquic-server-watchdog.timer
sudo systemctl daemon-reload
sudo systemctl enable --now mpquic-server-watchdog.timer
```

Verifica:
```bash
systemctl is-active mpquic-server-watchdog.timer
journalctl -u mpquic-server-watchdog.service -n 50 --no-pager
```

---

### Test chaos mp1 fast failover A+E

> Combo A+E (commit `4e36d0e` + fixup `e18dd08`): keepalive 1s, healthCheckLoop
> 500 ms, soglia degraded 3 s, recovery 1 s. Obiettivo: blackhole ≤ 3.5 s su
> failover stripe mp1 (Starlink wan5+wan6) anche se carrier resta UP ma il
> backhaul è morto.

Le ricette seguenti vanno eseguite **in produzione dal tech-lead** durante una
run iperf3 (o ping ad alta cadenza) attraverso `mp1`. Tutti i comandi
distruttivi includono il cleanup; eseguire sempre il cleanup anche in caso di
errore nel test.

#### Recipe 1 — `nft` drop UDP su una WAN (carrier rimane UP)

Esegui sull'OpenWrt CPE, mentre un iperf3 su `mp1` è in corso lato server:

```bash
# Setup chaos: blocca il traffico UDP della pipe 0 su wan6 (sport 6201).
# Adatta sport al pipe configurato (vedi cmd/mpquic/stripe.go: stripeBaseSport).
sudo nft add table inet chaos
sudo nft 'add chain inet chaos out { type filter hook output priority 0; }'
sudo nft 'add rule inet chaos out oifname "wan6" udp sport 6201 drop'

# ... lascia girare 60 s e raccogli metriche (vedi Recipe 4) ...

# Cleanup OBBLIGATORIO
sudo nft delete table inet chaos
```

#### Recipe 2 — `tc netem` con loss 100 % su una pipe

Equivalente più aggressivo: disabilita interamente il device per il flusso UDP.

```bash
sudo tc qdisc add dev wan6 root netem loss 100%
# ... 60 s di test ...
sudo tc qdisc del dev wan6 root
```

> Nota: `tc netem` impatta anche eventuali flow non-stripe sul device. Preferire
> `nft` (Recipe 1) quando la WAN è condivisa.

#### Recipe 3 — Acceptance criteria

| Metrica | Soglia | Sorgente |
|---|---|---|
| Detection blackhole `mp1` | ≤ 3.5 s | `mpquic_path_degraded_since_seconds` ≥ 3 (poi ricovero) |
| Loss totale finestra 60 s, policy `balanced`, 1 path su 2 down | ≤ 5 % | `iperf3 -u` o `ping` |
| Tempo di fail-back dopo cleanup | ≤ 2 s | `mpquic_path_failback_total` += 1 |
| Restart del servizio durante chaos | nessuno | `systemctl show mpquic@mp1 -p NRestarts` invariato |
| Deadlock / panic | nessuno | `journalctl -u mpquic@mp1 --since "5 min ago" \| grep -E "panic\|fatal"` |
| Throughput iperf3 (recovery dopo chaos) | ≥ 80 % nominal entro 5 s | `iperf3 -i 1` |

#### Recipe 4 — Lettura metriche live durante il test

```bash
watch -n 1 'curl -s http://10.10.11.100:9090/metrics \
  | grep -E "mpquic_path_(alive|degraded|last_rx|blackhole|failover|failback)" \
  | head -40'
```

Snapshot sincronizzato con il chaos (eseguire in parallelo a Recipe 1/2):

```bash
for i in $(seq 1 60); do
  ts=$(date -u +%H:%M:%S)
  echo "=== T+${i}s ${ts} ==="
  curl -s http://10.10.11.100:9090/metrics \
    | grep -E "^mpquic_path_(alive|degraded|degraded_since_seconds|blackhole_seconds_total|failover_total|failback_total)\{path=\"mp1\"" 
  sleep 1
done | tee /tmp/chaos_mp1_metrics.log
```

Esempio di output atteso al momento del flap:

```
mpquic_path_alive{path="mp1",bind="if:enp7s8"} 1
mpquic_path_degraded{path="mp1",bind="if:enp7s8"} 1
mpquic_path_degraded_since_seconds{path="mp1",bind="if:enp7s8"} 3.215
mpquic_path_failover_total{path="mp1",bind="if:enp7s8"} 1
mpquic_path_failback_total{path="mp1",bind="if:enp7s8"} 0
```

Dopo il cleanup (entro ≤ 2 s):

```
mpquic_path_degraded{path="mp1",bind="if:enp7s8"} 0
mpquic_path_degraded_since_seconds{path="mp1",bind="if:enp7s8"} 0
mpquic_path_failback_total{path="mp1",bind="if:enp7s8"} 1
mpquic_path_blackhole_seconds_total{path="mp1",bind="if:enp7s8"} 3.215
```

#### Note operative

- Il `mpquic-tunnel-watchdog` ha la sua soglia di restart (vedi
  `/etc/default/mpquic-watchdog`); verificare che non scatti un restart durante
  i 60 s di chaos. Se scatta, il test è invalidato (rumore esterno).
- Lo stesso test va ripetuto contro `wan5` (mirror simmetrico) per validare
  entrambi i path.
- I valori `mpquic_path_blackhole_seconds_total` sono cumulativi: confrontare il
  delta pre/post chaos, non il valore assoluto.
