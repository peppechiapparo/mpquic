# Architettura MPQUIC POC (Debian 12)

## Obiettivo
Realizzare 6 tunnel IP-over-QUIC indipendenti (multi-sessione 1:1), coerenti con il layer L3 esistente sulla VM client MPQUIC, senza modificare policy routing e NAT già validati.

## Componenti principali
- Binario unico: `mpquic` (stesso eseguibile per client/server)
- Trasporto: QUIC su UDP (`quic-go`)
- Incapsulamento: QUIC DATAGRAM extension
- Interfaccia locale per tunnel: TUN Linux dedicata per ogni istanza
- Orchestrazione servizi: `systemd` template `mpquic@.service`
- Configurazione runtime: YAML per istanza + `.env` per `ExecStartPre`

## Topologia logica
Per ogni istanza `i` (1..6):
1. Client legge pacchetti IP da `mpq{i}` (TUN)
2. Client apre sessione QUIC verso VPS su `remote_addr:remote_port`
3. Socket UDP client è bindata su IP WAN fisica corretta (`bind_ip` / `if:<ifname>`)
4. Server riceve datagram QUIC e li scrive sulla propria TUN `mpq{i}`
5. Flusso bidirezionale simmetrico (TUN <-> DATAGRAM)

## Mapping WAN client (1:1)
- Istanza 1 -> `enp7s3` (WAN1)
- Istanza 2 -> `enp7s4` (WAN2)
- Istanza 3 -> `enp7s5` (WAN3)
- Istanza 4 -> `enp7s6` (WAN4)
- Istanza 5 -> `enp7s7` (WAN5)
- Istanza 6 -> `enp7s8` (WAN6)

## Coerenza con L3 esistente
La logica esistente rimane invariata:
- Source-based policy routing già presente (tabelle `wan1..wan6`)
- NAT applicato solo sulle WAN fisiche
- Vincoli transit->WAN rispettati (1:1)

Il POC si inserisce sopra il piano L3: ogni processo `mpquic@i` usa la WAN associata tramite bind sorgente UDP.

## Struttura file rilevante
- `cmd/mpquic/main.go`: dataplane TUN <-> QUIC DATAGRAM / stripe dispatch
- `cmd/mpquic/stripe.go`: trasporto UDP stripe + FEC Reed-Solomon + Hybrid ARQ + batch I/O + socket tuning (~2370 LOC)
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

## Parametri configurazione per istanza
Ogni YAML include:
- `role`: `client` o `server`
- `bind_ip`: IP locale o `if:<ifname>` (con `if:` applica anche SO_BINDTODEVICE)
- `remote_addr`: endpoint remoto (richiesto lato client)
- `remote_port`: porta UDP/QUIC istanza
- `tun_name`: nome TUN
- `tun_cidr`: CIDR locale TUN
- `log_level`: `debug|info|error`
- `tls_ca_file`: path CA certificato (client)
- `tls_cert_file` / `tls_key_file`: certificato e chiave (server)
- `tls_server_name`: CN atteso nel certificato server (client)
- `tls_insecure_skip_verify`: disabilita verifica TLS (solo test)
- `congestion_algorithm`: `cubic` (default) o `bbr`
- `transport_mode`: `datagram` (default) o `reliable` (QUIC streams)
- `multipath_enabled`: `true/false` — abilita multipath
- `multipath_policy`: `priority|failover|balanced`
- `multi_conn_enabled`: `true/false` — server accetta N connessioni sulla stessa porta
- `stripe_enabled`: `true/false` — server abilita listener stripe
- `stripe_port`: porta UDP per protocollo stripe
- `stripe_data_shards`: K shards dati FEC (default 10)
- `stripe_parity_shards`: M shards parità FEC (default 2)
- ~~`stripe_auth_key`~~: **RIMOSSO** — sostituito da AES-256-GCM con chiavi TLS Exporter
- ~~`stripe_rekey_seconds`~~: **RIMOSSO** — PFS automatico per sessione

## Architettura a 3 livelli

### Livello 1: Multi-link (IMPLEMENTATO)
Un tunnel QUIC per WAN link fisico. 1:1 mapping. Ogni tunnel trasporta tutto il traffico della LAN associata.

```
WAN4 (enp7s6) ──── mpq4 ──── 10.200.4.1/30 ↔ 10.200.4.2/30 (:45004)
WAN5 (enp7s7) ──── mpq5 ──── 10.200.5.1/30 ↔ 10.200.5.2/30 (:45005)
WAN6 (enp7s8) ──── mpq6 ──── 10.200.6.1/30 ↔ 10.200.6.2/30 (:45006)
```

### Livello 2: Multi-tunnel per link (IN SVILUPPO)
N tunnel QUIC sullo STESSO link, ciascuno dedicato a una classe di traffico.
Il classificatore è esterno (nftables + fwmark + policy routing).
Tutti i tunnel convergono sulla STESSA porta server e sulla STESSA TUN server.

```
CLIENT (WAN5)                                         SERVER (:45010)
  tun-cr5 (10.200.10.1) ─┐                            ┌─ conn_1 ──┐
  tun-df5 (10.200.10.5) ─┼─── QUIC (diverse src port)─┼─ conn_2 ──┼─ mt1 (10.200.10.0/24)
  tun-bk5 (10.200.10.9) ─┘    same WAN, same dst port ┼─ conn_3 ──┘
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

### Livello 3: Multi-path per tunnel (IMPLEMENTATO)
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

## Architettura multipath applicativa (codice esistente, client)

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

### Campi `multipath_paths`
- `name`: etichetta operativa del path
- `bind_ip`: IP o `if:<ifname>` della WAN locale
- `remote_addr`: endpoint server
- `remote_port`: porta UDP del listener server
- `priority`: priorità relativa (valore più basso = path più preferito)
- `weight`: peso di preferenza (valore più alto = lieve favore in selezione)
- `pipes`: numero di socket UDP paralleli per path (default: 1; usato con `transport: stripe`)
- `transport`: tipo di trasporto per il path (`quic` default, `stripe` per UDP stripe + FEC)

### Campi stripe (globali, livello root YAML)
- `stripe_port`: porta UDP del server per il protocollo stripe (default: `remote_port` + 1000)
- `stripe_data_shards`: K — numero shards dati per gruppo FEC (default: 10). Anche con M=0, K è usato come soglia nel protocollo RX (`GroupDataN < K` → consegna diretta). **Deve essere coerente tra client e server.**
- `stripe_parity_shards`: M — numero shards parità per gruppo FEC (default: 2). In modalità `adaptive`, l'encoder RS viene pre-creato con questo valore anche se M effettivo parte da 0.
- `stripe_fec_mode`: modalità FEC — `always` (default, M fisso), `adaptive` (M=0 iniziale, sale a M su loss), `off` (M=0 permanente, nessun encoder RS)
- `stripe_arq`: `true/false` (default: `false`) — abilita Hybrid ARQ con NACK selettivo. Il receiver rileva gap di sequenza e invia NACK bitmap, il sender ritrasmette solo i pacchetti mancanti. Attivo solo quando effectiveM=0.
- `stripe_pacing_rate`: rate limiter in Mbps per sessione (default: `0` = disabilitato). **Sconsigliato** per granularità timer Linux.
- `stripe_enabled`: (solo server) abilita il listener stripe
- ~~`stripe_auth_key`~~: **RIMOSSO** — cifratura AES-256-GCM automatica via TLS Exporter
- ~~`stripe_rekey_seconds`~~: **RIMOSSO** — PFS per sessione (nuove chiavi ad ogni connessione)

### Policy multipath (`multipath_policy`)
- `priority` (default): bilancia priorità/peso/penalità errori
- `failover`: usa preferenzialmente il path con priorità più alta (valore numerico più basso), con fallback sui successivi
- `balanced`: aumenta l'effetto del `weight` per distribuire di più sui path a peso alto

### Limiti min/max path
- minimo configurabile: 1 path
- minimo operativo: almeno 1 path inizialmente attivo
- massimo: non hard-coded nel runtime; dipende da porte/listener disponibili e risorse host

## Scheduler path-aware

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

## Telemetria path-level (base)

Il runtime multipath emette periodicamente log telemetrici per ciascun path:
- stato (`up/down`)
- contatori `tx_pkts/rx_pkts`
- errori `tx_err/rx_err`
- `consecutiveFails`
- timestamp `last_up/last_down`

Formato log: `path telemetry name=... state=... tx_pkts=...`.

Il runtime emette anche telemetria per classe dataplane:
- `class telemetry class=... tx_pkts=... tx_err=... tx_dups=...`

## QoS: stato reale e direzione roadmap

QoS applicativa dataplane disponibile in runtime multipath:
- classificazione L3/L4 per protocollo, CIDR src/dst, porte src/dst e DSCP
- classi traffico con policy scheduler dedicate (`priority|failover|balanced`)
- selezione path per classe con `preferred_paths` / `excluded_paths`
- duplication per classi critiche (`duplicate` + `duplicate_copies`)

Modalità di configurazione supportate:
- `dataplane` inline nello YAML applicativo
- `dataplane_config_file` separato (raccomandato per integrazione orchestrator)

Per QoS L3/L2 avanzata si possono applicare policy Linux esterne (`tc`, queueing) sulle WAN fisiche.

## Tuning operativo consigliato

- **Failover primario/backup**: `priority` molto diversa (es. 10, 100, 200), `weight=1`
- **Bilanciamento leggero**: stessa `priority`, `weight` differenziati (es. 3,2,1)
- **Path costoso ma resiliente**: `priority` più alta (meno preferito) ma sempre disponibile come backup

## Persistenza al boot
- `install_*` abilita `mpquic@1..6.service`
- Ad ogni start, `ExecStartPre` assicura presenza/configurazione TUN
- `Restart=always` mantiene sessioni attive in caso di fault

## SO_BINDTODEVICE — binding interfaccia a livello kernel

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

## UDP Socket Buffer Tuning (7 MB)

Ogni socket UDP stripe (client pipe e server listener) viene configurato con
buffer di lettura e scrittura da 7 MB tramite `setStripeSocketBuffers()`.
Questo valore corrisponde a quello usato da quic-go (`DesiredReceiveBufferSize`).

**Perché**: su Starlink, i jitter spike (20-50 ms) provocano burst di centinaia
di pacchetti in arrivo nello stesso momento. Con il buffer default Linux (~208 KB),
~140 pacchetti da 1500B saturano il buffer causando drop kernel invisibili
all'applicazione. Con 7 MB si possono bufferizzare ~4700 pacchetti,
coprendo burst fino a 100ms a 500 Mbps.

**Requisito sysctl**: il kernel limita il buffer massimo a `net.core.rmem_max` /
`net.core.wmem_max`. Per ottenere il beneficio completo, entrambi i nodi devono
avere:

```
net.core.rmem_max = 7340032
net.core.wmem_max = 7340032
```

Vedere sezione *Installazione* per i comandi di configurazione.

## TX ActivePipes Cache (Zero-Alloc Dispatch)

Il server stripe mantiene una slice `txActivePipes []*net.UDPAddr` pre-calcolata
per ogni sessione (`stripeSession`). Questa cache viene ricostruita solo quando:
- Un nuovo client pipe si registra (REGISTER)
- Un indirizzo pipe cambia per CGNAT rebind (keepalive)

La ricostruzione avviene sotto `txMu` per thread-safety. Tutti i path TX
(`SendDatagram` M=0, `sendFECGroupLocked` M>0, `handleNack` retransmit)
leggono la slice cached senza allocazioni, eliminando ~30K `make+append`/s
a 341 Mbps.

## Architettura UDP Stripe + FEC Transport

### Motivazione: bypass traffic shaping Starlink
Starlink applica un cap di ~80 Mbps per sessione UDP. Con un singolo tunnel QUIC
il throughput è limitato a ~50 Mbps. Il trasporto stripe apre N socket UDP
("pipe") per path, ciascuno trattato da Starlink come sessione indipendente.

### Schema complessivo dettagliato

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

#### Legenda componenti

| Componente | Funzione | Dettaglio |
|------------|----------|-----------|  
| **FEC Encoder/Decoder** | Protezione packet loss proattiva | Reed-Solomon K=10 data, M=2 parità. Adaptive: M=0 se loss=0, sale a M=2 se loss >2% |
| **AES-256-GCM** | Cifratura + autenticazione | Chiavi derivate da TLS 1.3 Exporter (PFS per sessione). Nonce monotono anti-replay |
| **ARQ TX Buffer** | Buffer ritrasmissione | Ring buffer 4096 entry (~200ms a 20K pps). Plaintext pronto per re-encrypt |
| **ARQ RX Tracker** | Rilevamento gap | Bitmap circolare 8192 bit. NACK ogni 5ms, rate limit 30ms, soglia 96 seq |
| **Dedup Receiver** | Eliminazione duplicati | `markReceived()` verificato prima della consegna TUN. Drop silenzioso duplicati ARQ |
| **Batch I/O** | Riduzione overhead syscall | `recvmmsg` legge fino a 8 datagrammi per syscall (server RX + client RX) |
| **Socket Buffers 7 MB** | Prevenzione drop kernel | Copre burst fino a 100ms a 500 Mbps (~4700 pacchetti). Richiede sysctl `rmem_max` |
| **TX ActivePipes Cache** | Zero-alloc dispatch | Slice `[]*net.UDPAddr` pre-calcolata, ricostruita solo su REGISTER/keepalive |
| **SO_BINDTODEVICE** | Binding interfaccia kernel | Forza uscita su interfaccia corretta. Necessario con multiple WAN |
| **Flow-hash FNV-1a** | Anti-reordering TCP | Hash sulla 5-tupla → stesso flusso TCP sempre sullo stesso path |

### Wire Protocol
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

### FEC Reed-Solomon
- K=10 shards dati (il pacchetto TUN viene copiato in uno shard)
- M=2 shards parità (calcolati da Reed-Solomon)
- Tolleranza: fino al 16.7% di loss per gruppo FEC senza retransmit
- Dipendenza: `github.com/klauspost/reedsolomon`
- Modalità adattiva (`stripe_fec_mode: adaptive`): M effettivo parte da 0 (nessuna parità),
  sale a M configurato se rilevata perdita significativa via feedback keepalive bidirezionale

### Hybrid ARQ (NACK selettivo)
Meccanismo reattivo di ritrasmissione complementare a FEC adattivo:

1. **TX retransmit buffer** (`arqTxBuf`): ring buffer 4096 entry (~200ms a 20K pps).
   Ogni entry conserva il plaintext pronto per re-encrypt + retransmit.
2. **RX gap tracker** (`arqRxTracker`): bitmap circolare 8192 bit.
   `markReceived(seq)` setta il bit; `getMissing()` scansiona gap > 96 seqs dietro highest.
3. **NACK generation loop**: goroutine dedicata, tick ogni 5ms, rate limit 1 NACK / 30ms.
   Invia NACK bitmap (fino a 64 gap per pacchetto) al peer.
4. **NACK handler**: riceve NACK, lookup in TX buffer, re-encrypt con nonce fresco,
   retransmit round-robin sulle pipe (via `txActivePipes` cache, zero-alloc).
5. **Dedup receiver**: `markReceived()` verificato prima della consegna al TUN.
   Duplicati da retransmit ARQ scartati silenziosamente.
6. **Bidirezionale**: sia client che server hanno TX buf + RX tracker.
7. **Solo M=0**: ARQ attivo solo quando effectiveM = 0 (non compete con FEC grouping).

**Benchmark ARQ v2 + Socket Tuning**: +48% throughput su dual Starlink (239 → 354 Mbps medio,
picco 390 Mbps). Overhead ~0% in condizioni normali (solo pacchetti NACK quando ci sono gap).

### Batch I/O (recvmmsg)
Sostituzione di `ReadFromUDP()` (1 syscall per pacchetto) con
`ipv4.PacketConn.ReadBatch()` che legge fino a 8 datagrammi per syscall
(recvmmsg su Linux). Applicato sia al server RX che al client RX per-pipe.
Risultato benchmark: neutro (+1%), ma il codice resta perché è pulito e
riduce il carico syscall.

### Flow-hash dispatch (server → client)
Il server usa hash FNV-1a sulla 5-tupla IP (srcIP, dstIP, proto, srcPort, dstPort)
per assegnare ogni flusso TCP/UDP a una sessione stripe specifica. Pacchetti dello
stesso flusso percorrono sempre lo stesso link → nessun reordering TCP.

### Session management
- Session ID: `ipToUint32(tunIP) ^ fnv32a(pathName)` — unico per path
- Keepalive: ogni 5s client→server, server risponde solo per sessioni note
- Timeout: 30s senza RX → close + reconnect
- GC: server rimuove sessioni idle dopo timeout

### Validità delle scelte architetturali con Stripe (stato attuale)

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

## Limiti deliberati (fase corrente)
- Multipath in singola connessione QUIC disponibile in modalità sperimentale (scheduler path-aware con priorità/peso e fail-cooldown)
- Nessun endpoint/API di controllo dinamico runtime (oggi policy caricata da YAML a startup)
- TLS server self-signed runtime (POC)
