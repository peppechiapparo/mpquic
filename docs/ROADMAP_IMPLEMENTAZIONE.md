# Roadmap implementazione MPQUIC

*Allineata al documento "QUIC over Starlink TSPZ" — aggiornata 2026-03-15*

### Nota deploy monitoraggio (2026-03-15)
- Fix `metrics_listen: auto` mancante in mpq4/5/6 (commit `af14a2d`) — i single-link
  tunnel non esponevano endpoint metriche, Prometheus li segnava DOWN
- Dashboard Grafana v8 (commit `c07b825`, `a217474`): fix leggibilità Uptime/TX/RX,
  regex stale limitate a `[456]` per evitare match su vecchie time series,
  duplicato mp1 rimosso dal pannello Uptime con filtro `job="mpquic-client"`
- Prometheus: **17/18 target UP** (mt4 DOWN lato VPS — non correlato)
- Fase 5 metriche + monitoraggio: completamente operativa end-to-end

### Nota manutenzione (2026-03-01)
- Cleanup diagnostico completato in `cmd/mpquic/main.go` (commit `c15b235`):
  - rimosse log temporanee `[DIAG]` in `registerStripe()` e `dispatch()`
  - rimosso `dispatchCounter` usato solo per sampling debug
- Nessun impatto funzionale previsto; modifica di sola pulizia osservabilità

### Nota debug regressione throughput (2026-03-02)
- Obiettivo immediato: identificare la root-cause del degrado prestazionale osservato
  dopo le modifiche sicurezza stripe (MAC/rekey) e migliorare il bilanciamento
  multi-canale lato server/client.
- Vincolo operativo: test one-by-one, una variabile per volta, con raccolta metrica
  client + server sincronizzata.

#### Piano operativo (nuova traccia)
1. Congelare baseline riproducibile (scenario fisso WAN, durata test, parallelismo)
2. Eseguire matrice A/B `stripe_auth_key` / `stripe_rekey_seconds`
3. Profilare decode FEC + scheduler path-aware lato runtime
4. Analizzare contention tra path lato server (dispatch, reorder, recovery)
5. Ottimizzare instradamento shard/path e policy di selezione
6. Valutare FEC alternativi a Reed-Solomon con benchmark comparativo
7. Validare fix su scenario reale e promuovere profilo produzione

#### Fase 1 — Matrice A/B sicurezza Stripe ✅ COMPLETATA (esito: MAC/rekey rimosso)

**Scopo**: verificare in modo misurabile se MAC/rekey introducono regressione
di throughput/stabilità rispetto al baseline storico.

**Test eseguiti**:
- `T1`: `auth=off`, `rekey=off` (baseline riferimento) — **115 Mbps** wan6, **59 Mbps** wan4
- `T2`: `auth=on`, `rekey=off` — **0 Mbps**: bug critico scoperto (server non firmava i pacchetti TX)

**Esito**: il sistema MAC/rekey era **fondamentalmente broken** (il server non applicava
mai la firma HMAC ai pacchetti in uscita). Anche dopo il fix, il throughput crollava da
123 Mbps a ~3 Mbps con auth abilitata. T3/T4 non eseguiti.

**Decisione**: intero sistema MAC/rekey rimosso e sostituito con AES-256-GCM +
TLS 1.3 Exporter key exchange (vedi Step 4.10/4.11). Rimossi ~340 righe di codice
MAC/rekey, eliminati i parametri `stripe_auth_key` e `stripe_rekey_seconds`.

---

## Concetti chiave: Multi-link vs Multi-tunnel vs Multi-path

```
┌──────────────────────────────────────────────────────────────────────┐
│ Multi-link (Step 1 ✅)                                               │
│   1 tunnel QUIC per ogni link WAN fisico                             │
│   mpq4 ↔ WAN4, mpq5 ↔ WAN5, mpq6 ↔ WAN6                          │
│   Ogni tunnel trasporta TUTTO il traffico della LAN associata        │
├──────────────────────────────────────────────────────────────────────┤
│ Multi-tunnel per link (Step 1+2 ← PROSSIMO STEP)                    │
│   N tunnel QUIC sullo STESSO link fisico                             │
│   Ogni tunnel trasporta UNA classe di traffico (applicazione)        │
│   "Many small pipes are faster than a single large tube"             │
│   Client: nftables classifica traffico → TUN dedicata per classe     │
│   Server: porta condivisa, accetta N connessioni, UNA TUN condivisa  │
├──────────────────────────────────────────────────────────────────────┤
│ Multi-path per tunnel (Step 4 — futuro)                              │
│   1 tunnel che usa N link per redundanza                             │
│   Bonding: aggrega bandwidth di più WAN                              │
│   Backup: failover automatico su link alternativo                    │
│   Duplication: pacchetti critici inviati su più link                 │
│   Richiede QUIC Multipath (RFC 9443) o implementazione applicativa   │
└──────────────────────────────────────────────────────────────────────┘
```

## Visione d'insieme (5 Step dal PDF competitor)

| Step | Descrizione | Nostro concetto | Stato |
|------|-------------|-----------------|-------|
| **1** | QUIC tunnels multi-link 1:1 | Multi-link | **✅ DONE** |
| **2** | Traffico distribuito per applicazione, non per pacchetto | Multi-tunnel per link | **🔄 IN PROGRESS** (2.1-2.4 ✅, 2.5 🔄 deploy OK, OpenWrt net+fw ✅, mwan3 pending) |
| **3** | BBR + Reliable Transport | CC per tunnel + transport mode | **✅ DONE** (BBRv1, reliable streams, benchmarkato) |
| **4** | Bonding, Backup, Duplicazione | Multi-path per tunnel | **✅ DONE** (4b ottimizzazioni ✅, 4c stabilizzazione 🔄) |
| **5** | AI/ML-Ready (Quality on Demand) | Decision layer | **⬜ NOT STARTED** |

---

## Fase 1 — Multi-link: Baseline 6 sessioni QUIC 1:1 ✅ COMPLETATA

**Obiettivo**: 6 sessioni indipendenti, una per WAN, tunnel bidirezionali.

### Risultati
- 6 istanze server VPS (`mpquic@1..6`) attive su porte 45001-45006
- 3 tunnel client attivi e bidirezionali: mpq4, mpq5, mpq6
- WAN1-3 senza modem (degradazione controllata, istanze inattive)
- Policy routing source-based per 6 tabelle (`wan1..wan6`)
- LAN transit `172.16.x.1/30` instradato nel tunnel dedicato
- TLS hardened: certificati con SAN, trust CA esplicito, no InsecureSkipVerify
- Watchdog tunnel con peer-liveness check e restart automatico
- Interface hotplug via networkd-dispatcher
- Network: pure systemd-networkd (ifupdown rimosso)

### Bug risolti
- Server goroutine leak su riconnessione client (commit `73474a9`)
- Gateway detection avvelenato da lease dhclient stantii (commit `3ac4036`)
- Routing table incompleta per wan5 (fix combinato dei due sopra)

---

## Fase 2 — Multi-tunnel per link: QoS per applicazione 🔄 IN CORSO (2.1-2.3 ✅)

**Obiettivo (Step 2 PDF)**: più tunnel QUIC sullo STESSO link, ciascuno dedicato a una classe
di traffico. Il packet loss su un link impatta solo le applicazioni di quella classe.

> "Each pipe (ie application) is independent and does not suffer the packet loss of the others"
> "Each tunnel is associated to a homogeneous type of traffic and application"

### Architettura Multi-tunnel

```
CLIENT (VM MPQUIC)                                            SERVER (VPS)
                                                             
 LAN traffic ──▶ nftables classifier                         ┌─────────────────────┐
                  │                                          │ porta 45015         │
                  ├─ VoIP (UDP 5060) ──▶ tun-critical ─┐     │                     │
                  │                     10.200.15.1/24  │     │  conn_1 ◄──────────┤
                  ├─ HTTPS (TCP 443) ──▶ tun-default  ─┼QUIC─┤  conn_2 (same port)│──▶ tun-mt5
                  │                     10.200.15.5/24  │WAN5 │  conn_3            │   10.200.15.0/24
                  └─ Bulk (TCP 5001) ──▶ tun-bulk     ─┘     │                     │
                                        10.200.15.9/24       │  routing table:     │──▶ NAT ──▶ Internet
                                                             │  .1 → conn_1       │
                                                             │  .5 → conn_2       │
                                                             │  .9 → conn_3       │
                                                             └─────────────────────┘
```

**Porta condivisa**: N connessioni QUIC client (diverse porte sorgente) → stessa porta server →
stessa TUN server. Il server mantiene un mapping `peer_TUN_IP → QUIC_connection` per il routing
di ritorno. Ogni connessione è identificata dal suo Connection ID QUIC, non dalla porta.

### Modifiche Go necessarie

1. **Server: rimuovere logica `superseded`** → accettare N connessioni concorrenti sulla stessa porta
2. **Server: peer-IP routing table** → mapping `dst_IP → connection` per pacchetti TUN→QUIC
3. **Server: TUN subnet** → `/24` invece di `/30`, condivisa da tutte le connessioni
4. **Config: `connection_id`** → ogni connessione client dichiara il suo TUN IP peer al server

### Classi di traffico

| Classe | Tipo applicazioni | Policy | Esempio regole nftables |
|--------|-------------------|--------|--------------------------|
| **critical** | VoIP, telemetria, controllo | Low latency | UDP 5060, 10000-20000, DSCP 46 |
| **default** | Web, HTTPS, API, business apps | Balanced | TCP 80, 443, 8443 |
| **bulk** | Backup, sync, download | Best effort | TCP 5001-6000, tutto il resto |

### Step 2.1 — Server multi-connessione (modifica Go) ✅ COMPLETATO (b0bbddf)
1. Refactor `runServer()`: rimuovere active/supersede, accettare N conn concorrenti
2. Implementare `connectionTable`: mappa `netip.Addr → quic.Connection`
3. La goroutine TUN-reader legge pacchetto, estrae dst IP, lookup nella tabella
4. Handshake: il client invia il proprio TUN IP come primo datagram (registration)
5. Cleanup automatico: rimuovere connessione dalla tabella quando il contesto termina
6. TUN usa subnet `/24` (es. `10.200.10.0/24`)
7. Test unitario: 3 connessioni parallele, verifica routing bidirezionale

### Step 2.2 — Client: istanze per-classe con nftables classifier ✅ COMPLETATO (058ddca, 477d08d)
1. Definire 3 classi: `critical`, `default`, `bulk`
2. Per ogni classe: una istanza `mpquic` client (diversa TUN, stesso `bind_ip` WAN)
3. Script `mpquic-nft-classifier.sh`:
   - Clasifica traffico LAN con nftables marks (fwmark)
   - Policy routing: `fwmark X → table class-X → default dev tun-class-X`
   - NAT MASQUERADE per ciascuna TUN in uscita verso il tunnel
4. Config YAML per-classe (TUN IP diverso, stessa WAN, stessa porta server)
5. Systemd units per-classe

### Step 2.3 — Deploy e test su infra reale ✅ COMPLETATO (2026-02-28) — DECOMMISSIONATO
> **Nota**: Step 2.3 usava subnet 10.200.10.0/24 con server mt1 (porta 45010).
> Decommissionato il 15/03/2026 con il rename tunnel=WAN.

1. Deploy server multi-connessione sul VPS (porta 45010, TUN `mt1`, `10.200.10.0/24`)
2. Deploy 3 istanze client su VM (WAN5):
   - `mpquic-critical@5`: TUN `cr5` → 10.200.10.1, bind WAN5, server :45010
   - `mpquic-default@5`: TUN `df5` → 10.200.10.5, bind WAN5, server :45010
   - `mpquic-bulk@5`:    TUN `bk5` → 10.200.10.9, bind WAN5, server :45010
3. Installare nftables classifier
4. Verificare:
   - 3 connessioni concorrenti sulla stessa porta server
   - Ping peer bidirezionale per ogni classe TUN
   - Traffico VoIP → tun-critical, HTTPS → tun-default, bulk → tun-bulk
   - tcpdump: tutto il traffico QUIC esce sulla stessa WAN5

### Step 2.4 — Test isolamento e QoS ✅ COMPLETATO (2026-02-28)

**Metodologia**: netem loss injection su singola TUN (br5), misura su tutte e 3 le TUN
(cr5/br5/df5) dello stesso link WAN5. Binding esplicito per-device (`-B IP%dev`).
iperf3 3.12 → VPS iperf3 server (porta 5201).

#### Risultati RTT (ping, 20 pacchetti per tunnel)

| Tunnel | Baseline RTT | Baseline Loss | 10% netem br5 | 30% netem br5 |
|--------|-------------|---------------|----------------|----------------|
| cr5 | 13.0 ms | 0% | **0% loss** | **0% loss** |
| br5 | 13.2 ms | 0% | 15% loss | 35% loss |
| df5 | 13.1 ms | 0% | **0% loss** | **0% loss** |

#### Risultati Throughput (iperf3, 5s per tunnel, device-bound)

| Tunnel | Baseline (Mbps) | 10% loss br5 (Mbps) | 30% loss br5 (Mbps) |
|--------|----------------|---------------------|---------------------|
| cr5 (critical) | 50.2 | **50.2** (±0%) | **50.2** (±0%) |
| br5 (bulk) | 48.1 | **2.3** (−95%) | **0.4** (−99%) |
| df5 (default) | 50.0 | **50.2** (±0%) | **49.8** (±0%) |

**Conclusione**: isolamento perfetto — packet loss su un tunnel ha ZERO impatto
su latenza e throughput degli altri tunnel, anche sotto loss del 30%.
I tunnel cr5 e df5 mantengono throughput pieno (~50 Mbps) e 0% loss
mentre br5 crolla a 0.4 Mbps. Questo dimostra il valore architetturale
della separazione per classe di traffico.

**Nota tecnica**: i 3 tunnel condividono la stessa subnet /24. Il kernel Linux
usa la prima route (cr5). Per test isolati è necessario il binding esplicito
`iperf3 -B IP%dev`. In produzione il VLAN classifier instrada correttamente.

### Step 2.5 — Generalizzazione: 3 WAN × 3 classi = 9 tunnel con VLAN 🔄 IN CORSO

**Architettura**: ogni WAN attiva (SL4/SL5/SL6) ottiene 3 tunnel di classe
(critical/bulk/default). La classificazione avviene lato OpenWrt tramite VLAN tagging.
Ogni VLAN arriva su un sub-interface dedicato del client VM; il classifier
instrada nel tunnel corretto in base alla VLAN di origine.

**Schema VLAN → Tunnel**:

| TBOX LAN (trunk) | OpenWrt IF | VLAN | Classe | Tunnel | WAN uscita |
|------------------|------------|------|--------|--------|------------|
| LAN4 (enp6s23) | eth11 | 11 | critical | cr4 | WAN4 (SL4) |
| LAN4 (enp6s23) | eth11 | 12 | bulk | br4 | WAN4 (SL4) |
| LAN4 (enp6s23) | eth11 | 13 | default | df4 | WAN4 (SL4) |
| LAN5 (enp7s1) | eth12 | 21 | critical | cr5 | WAN5 (SL5) |
| LAN5 (enp7s1) | eth12 | 22 | bulk | br5 | WAN5 (SL5) |
| LAN5 (enp7s1) | eth12 | 23 | default | df5 | WAN5 (SL5) |
| LAN6 (enp7s2) | eth13 | 31 | critical | cr6 | WAN6 (SL6) |
| LAN6 (enp7s2) | eth13 | 32 | bulk | br6 | WAN6 (SL6) |
| LAN6 (enp7s2) | eth13 | 33 | default | df6 | WAN6 (SL6) |

**Flusso traffico**:
```
OpenWrt (eth12.21) → VLAN 21 (critical) → TBOX enp7s1.21 → ip rule → cr5 TUN → WAN5 → VPS:45011
OpenWrt (eth12.22) → VLAN 22 (bulk)     → TBOX enp7s1.22 → ip rule → br5 TUN → WAN5 → VPS:45011
OpenWrt (eth12.23) → VLAN 23 (default)  → TBOX enp7s1.23 → ip rule → df5 TUN → WAN5 → VPS:45011
```

**Server layout**: 3 porte, ciascuna multi-conn (3 classi):
- 45010: WAN4 → cr4 + br4 + df4 (TUN mt4, subnet 10.200.10.0/24)
- 45011: WAN5 → cr5 + br5 + df5 (TUN mt5, subnet 10.200.11.0/24)
- 45012: WAN6 → cr6 + br6 + df6 (TUN mt6, subnet 10.200.12.0/24)

**Client VM (TBOX)**: VLAN sub-interfaces su ogni LAN trunk + classifier per-VLAN:
- `enp6s23.11` → routing table mt_cr4 → default via cr4
- `enp6s23.12` → routing table mt_br4 → default via br4
- `enp6s23.13` → routing table mt_df4 → default via df4
- `enp7s1.21` → routing table mt_cr5 → default via cr5
- `enp7s1.22` → routing table mt_br5 → default via br5
- `enp7s1.23` → routing table mt_df5 → default via df5
- `enp7s2.31` → routing table mt_cr6 → default via cr6
- `enp7s2.32` → routing table mt_br6 → default via br6
- `enp7s2.33` → routing table mt_df6 → default via df6

**Lato OpenWrt**: piena libertà di routing — basta taggare il traffico sulla VLAN
giusta (mwan3, firewall zone, DSCP→VLAN map, ecc.)

**Passi implementativi**:
1. ✅ Creare VLAN sub-interfaces su client VM (systemd-networkd .netdev + .network)
2. ✅ Creare 9 configurazioni client YAML (cr4/br4/df4, cr5/br5/df5, cr6/br6/df6)
3. ✅ Creare 3 configurazioni server YAML (mt4, mt5, mt6 multi-conn)
4. ✅ Creare classifier per-VLAN (evoluzione di mpquic-mt-classifier.sh)
5. ✅ Deploy server: 3 istanze multi-conn su porte 45014-45016 + nftables forward/NAT
6. ✅ Deploy client: 9 istanze + VLAN interfaces + classifier + ip rules 800-808
7. ✅ Integrare in install_mpquic.sh (ripetibile su nuove TBOX)
8. 🔄 Configurare OpenWrt: VLAN trunking + firewall ✅ (2026-03-15), mwan3 posticipato a fase test
9. ✅ Test end-to-end: 9 tunnel UP + ping peer VPS bidirezionale verificato (2026-03-14)
10. ✅ Script UCI in `deploy/openwrt/`: 01-network, 02-firewall, 03-mwan3, 04-dscp, 99-remove

### Done criteria Fase 2
- [x] Server accetta N connessioni concorrenti sulla stessa porta
- [x] 3 classi traffico (critical/default/bulk) su TUN separate
- [x] nftables classifier funzionante con routing per-classe
- [x] Traffico applicativo smistato correttamente (verificato con tcpdump)
- [x] 9 tunnel VLAN: config + VLAN networkd + classifier nel repo
- [x] install_mpquic.sh copre l'intero flusso (client + server)
- [x] OpenWrt VLAN network + firewall zones configurati (2026-03-15)
- [ ] OpenWrt mwan3 classificazione traffico (posticipato a fase test)
- [x] Test end-to-end: 9 tunnel UP, ping peer OK su tutti (WAN4 ~110ms, WAN5 ~13ms, WAN6 ~19ms)
- [x] Isolamento dimostrato: loss su un tunnel non impatta gli altri (netem + iperf3)
- [x] Risultati documentati con metriche (RTT + throughput tables)

---

## Fase 3 — BBR Congestion Control + Reliable Transport ✅ COMPLETATA

**Obiettivo (Step 3 PDF)**: sostituire Cubic (default quic-go) con BBR per ottimizzare
throughput su canali LEO con alta variabilità RTT e loss non da congestione.

> "Cubic: slow and decreasing. BBRv3 is relatively fast."
> "Congestion control of Wave was created for high packet loss environments"

### Approccio adottato: Fork quic-go + Pluggable CC
Poiché quic-go v0.48.2 non espone API CC pubblica (issue #4565), è stato creato un fork
locale (`local-quic-go/`) con:
1. **BBRv1 sender** (`bbr_sender.go`, ~555 LOC) — 4 stati: STARTUP→DRAIN→PROBE_BW→PROBE_RTT
2. **Pluggable CC**: campo `CongestionAlgorithm string` in `quic.Config`, factory in `congestion/`
3. **Death spiral fix**: startup non riduce mai cwnd, BDP floor a initialCwnd, pacing floor
4. **Config YAML**: `congestion_algorithm: bbr|cubic` (default: cubic)

### Scoperta critica: Transport Mode Reliable
Durante i test su Starlink, BBR non mostrava benefici. Root cause:
- **QUIC DATAGRAM frames (RFC 9221) sono unreliable** — pacchetti persi non vengono mai ritrasmessi
- Il TCP dentro il tunnel vedeva direttamente la loss del link → collasso a ~0.5 Mbps
- Il CC algorithm (BBR o Cubic) era irrilevante perché non governa i DATAGRAM

**Soluzione**: nuovo `transport_mode: reliable` che usa **QUIC bidirectional streams**
con framing length-prefixed (2 byte BigEndian + payload). I pacchetti persi vengono
ritrasmessi dal QUIC stack, e il CC algorithm guida pacing e recovery.

### Risultati benchmark su Starlink (WAN6, RTT 25-40ms)

**Datagram mode** (prima del fix):
| Tunnel | CC | Baseline | 10% loss |
|--------|-----|----------|----------|
| cr6 | Cubic | 15.1 Mbps | 0.5 Mbps (−97%) |
| br6 | BBR | 14.5 Mbps | 0.5 Mbps (−97%) |
| df6 | Cubic | 14.9 Mbps | 0.9 Mbps (−94%) |

**Reliable mode** (dopo il fix):
| Tunnel | CC | Baseline | 10% loss | 30% loss |
|--------|-----|----------|----------|----------|
| cr6 | Cubic | 45.2 Mbps | 41.9 Mbps (−7%) | 15.5 Mbps (−66%) |
| br6 | **BBR** | 47.4 Mbps | 28.5 Mbps (−40%) | **26.1 Mbps (−45%)** |
| df6 | Cubic | 55.8 Mbps | 39.7 Mbps (−29%) | 13.6 Mbps (−76%) |

**Conclusioni chiave**:
- Reliable transport: **3× throughput** base (15→50 Mbps)
- BBR con 30% loss: **+79%** vs Cubic (26.1 vs 14.6 Mbps)
- Strategia: `reliable` su tutti i tunnel, `bbr` su tunnel bulk, `cubic` su critici

### Commits
- `05f391e` — BBRv1 implementation + pluggable CC
- `57c7ccd` — BBR death spiral fix
- `2d903ab` — Reliable transport mode (QUIC streams)

### Prossimi sviluppi CC
- **BBRv2/v3**: reazione proporzionale alla loss (atteso miglioramento al 10% loss)
- **Adaptive CC**: selezione automatica BBR vs Cubic basata su condizioni link

---

## Fase 4 — Multi-path per tunnel: Bonding, Backup, Duplicazione ✅ COMPLETATA

**Obiettivo (Step 4 PDF)**: un singolo tunnel "applicativo" può usare N link fisici per
resilienza e aggregazione bandwidth.

> "New services: Bonding, Backup, Duplications"

### Prerequisiti
- [x] Fase 2 funzionante (multi-tunnel, server multi-connessione, connectionTable)
- [x] Fase 3 completata (BBR + reliable transport)
- [x] Codice `multipathConn` esistente con scheduler, duplicazione, reconnect

### Servizi target

| Servizio          | Meccanismo                                                        | Caso d'uso |
|----------         |-----------                                                        |------------|
| **Bonding**       | Un tunnel usa 2+ WAN, pacchetti distribuiti round-robin/weighted  | Max throughput |
| **Active Backup** | Tunnel su WAN primaria, failover su WAN secondaria                | Low latency + resilienza |
| **Duplicazione**  | Pacchetti critici inviati su 2+ WAN simultaneamente               | Zero-loss per VoIP/controllo |

### Approccio: Applicativo (N connessioni QUIC → `multipathConn`)
Il fork quic-go v0.48.2 non supporta QUIC Multipath (RFC 9443).
Usiamo l'implementazione applicativa già presente nel codice (`multipathConn`):
- N connessioni QUIC indipendenti (una per WAN) → singola TUN client
- Scheduler path-aware: priority/failover/balanced
- Duplicazione per classi critiche (configurable copies)
- Reconnect con backoff esponenziale e recovery automatica
- Telemetria per-path e per-class (10s interval)
- Supporto reliable transport mode + BBR/Cubic per-path

### Codice `multipathConn` esistente (~700 LOC)

Implementato e compilante, mai testato su infra reale:

| Componente | Funzione | LOC | Stato |
|-----------|----------|-----|-------|
| `multipathPathState` | Stato per-path (conn, alive, counters) | ~25 | ✅ |
| `newMultipathConn` | Init N path, dial QUIC, reliable/datagram | ~115 | ✅ |
| `SendDatagram` | Invio con retry + dataplane class routing | ~30 | ✅ |
| `sendDuplicate` | Invio su N path per classi duplicate | ~40 | ✅ |
| `selectBestPath` | Scheduler: priority/failover/balanced | ~70 | ✅ |
| `recvLoop` | Ricezione per-path → channel unificato | ~25 | ✅ |
| `reconnectLoop` | Redial con backoff su path down | ~80 | ✅ |
| `telemetryLoop` | Log periodico metriche path+class | ~50 | ✅ |
| `closeAll` | Shutdown graceful | ~15 | ✅ |
| Dataplane classifier | Routing pacchetti per classe/protocol/DSCP | ~200 | ✅ |

### Gap da risolvere

1. **Server-side routing**: il server multi-conn (`connectionTable`) mappa un peer IP
   a **una** connessione. Con multipath, lo stesso client arriva da N WAN diverse →
   N connessioni con lo stesso peer TUN IP. Il server deve:
   - Accettare N conn dallo stesso peer IP (registration multipla)
   - Inviare pacchetti di ritorno su **tutte** le connessioni attive per quel peer
     (o solo la "migliore" / round-robin)
   - Gestire de-duplicazione per pacchetti ricevuti identici (se client usa duplication)

2. **Peer registration**: il client multipath invia la registration (`TUN_IP`) su
   ogni path → il server deve aggregarle in un gruppo, non sovrascrivere

3. **Config YAML**: definire formato per `multipath_paths` con N WAN,
   `multipath_policy`, e `dataplane` config per classe

4. **Test su infra reale**: deploy client multipath su WAN5+WAN6 → stesso server,
   verificare bonding/failover/duplication

### Steps implementativi

#### Step 4.1 — Server multi-path aware
Adattare `connectionTable` per supportare N connessioni dallo stesso peer:
- `connEntry` diventa un gruppo: `[]quic.Connection` per peer IP
- TUN→QUIC: round-robin o best-path tra le connessioni del gruppo
- De-duplicazione opzionale in ricezione (sequence number o hash)

#### Step 4.2 — Test `multipathConn` su infra reale
Creare config multipath client: una TUN, 2 path (WAN5 + WAN6) → stesso server.
Verificare:
- Connessione su entrambi i path
- Failover: spegnere WAN5, traffico migra su WAN6
- Recovery: riaccendere WAN5, path recuperato
- Telemetria: counters per-path corretti

#### Step 4.3 — Bonding test
Verificare aggregazione bandwidth:
- iperf3 via TUN multipath, throughput > singola WAN
- Policy `balanced` con weight proporzionali alla banda WAN

#### Step 4.4 — Duplication test
Verificare zero-loss per classi duplicate:
- Config: classe `critical` con `duplicate: true, duplicate_copies: 2`
- netem loss 30% su WAN5 → pacchetti duplicati su WAN6 → 0% loss end-to-end

#### Step 4.5 — Benchmark comparativo
Confronto metriche:
- Single-path vs Bonding: throughput aggregato
- Single-path vs Failover: tempo di recovery
- Single-path vs Duplication: loss rate sotto netem

### Done criteria Fase 4
- [x] Server supporta N connessioni dallo stesso peer (multi-path group)
- [x] Client multipath connesso su 2+ WAN verso stesso server
- [x] Failover funzionante (2 pkt persi su 74, recovery in ~8s)
- [x] Bonding throughput > singola WAN (74.3 Mbps aggregati, picco 102 Mbps)
- [ ] Duplication zero-loss sotto 30% netem loss
- [x] Benchmark documentato con metriche (NOTA_TECNICA_MPQUIC.md v3.3)

---

## Fase 4b — Starlink Session Striping (UDP Stripe + FEC) ✅ COMPLETATA

**Obiettivo**: bypassare il traffic shaping per-sessione di Starlink aprendo N
socket UDP paralleli ("pipe") sullo stesso link fisico. Ogni pipe è un flusso UDP
con porta sorgente distinta → Starlink lo tratta come sessione indipendente con
il proprio budget di throughput (~80 Mbps ciascuna).

### Problema

```
Senza multi-pipe (attuale):
  Speedtest Ookla SENZA tunnel: ~300 Mbps download, ~20 Mbps upload
  Speedtest Ookla CON tunnel:   ~50 Mbps download (single session capped!)
  Bonding 2 link CON tunnel:    ~74 Mbps (= 2 × ~40 Mbps, entrambi capped)

Con stripe transport (obiettivo):
  4 pipe × 80 Mbps = ~320 Mbps → saturazione della capacità Starlink
  Bonding 2 link × 4 pipe = 8 pipe totali → throughput massimo raggiungibile
```

### Approccio 1 FALLITO: Multi-Pipe QUIC (N connessioni BBR indipendenti)

Il primo tentativo utilizzava N connessioni QUIC parallele con BBR indipendente.
**Risultato: ❌ FALLITO** — le N istanze BBR competono per la stessa banda:

| Config | Throughput | Retransmit | Verdetto |
|--------|-----------|------------|----------|
| pipes=1 (baseline) | 74.3 Mbps | 185 | ✅ |
| pipes=4 (8 pipe totali) | 30-33 Mbps | 2.836-5.724 | ❌ -56% |
| pipes=1 ripristinato | 63.3 Mbps | 6.798 | ✅ |

**Causa**: N istanze BBR × bandwidth stimata = N× overshoot → congestione →
loss massiccio → throughput collapse. Problema noto come "intra-flow competition".

### Approccio 2 ATTUALE: UDP Stripe + FEC (nessun CC per pipe)

Nuovo layer di trasporto che sostituisce QUIC sulle connettività Starlink:

```
┌─────────────────────────────────────────────────────────────────────┐
│ Stripe Transport (per link Starlink)                                │
│                                                                     │
│  TUN reader ──▶ FEC encoder (K data + M parity shards)              │
│              ──▶ round-robin across N UDP sockets                   │
│              ──▶ each socket = independent Starlink session         │
│                                                                     │
│  N UDP sockets ──▶ FEC decoder (reconstruct if K of K+M received)   │
│                ──▶ TUN writer                                       │
│                                                                     │
│  NO congestion control per pipe (rate limited by TCP inside tunnel) │
│  Loss recovery via Reed-Solomon FEC                                 │
└─────────────────────────────────────────────────────────────────────┘
```

**Vantaggi rispetto a Multi-Pipe QUIC:**

| Aspetto | Multi-Pipe QUIC | UDP Stripe + FEC |
|---------|----------------|------------------|
| CC per pipe | BBR indipendente (compete) | **Nessuno** (TCP in-tunnel) |
| Loss recovery | QUIC retransmit (lento) | **FEC proattivo** (< 5ms) |
| Overhead | TLS + QUIC headers per pipe | **16 byte** header stripe |
| Complessità | N connessioni QUIC separate | **1 stripe conn** con N socket |

### Architettura Wire Protocol

```
Pacchetto stripe:
  [stripeHdr 16 bytes][shard payload (variable)]

Header: magic(2) + ver(1) + type(1) + session(4) + groupSeq(4) +
        shardIdx(1) + groupDataN(1) + dataLen(2) = 16 bytes

Tipi: DATA (0x01), PARITY (0x02), REGISTER (0x03), KEEPALIVE (0x04)

FEC default: K=10 data shards, M=2 parity shards (20% redundancy)
```

### Coesistenza con QUIC standard

Il stripe transport è **solo per connettività Starlink**. Le altre connettività
(fibra, LTE, VSAT) continuano a usare il trasporto QUIC standard (BBR, reliable):

```
┌────────────────────────────────────────────────────────────────┐
│ mp1 — multipath tunnel                                         │
│                                                                │
│  wan5 (Starlink enp7s7) ─── transport: stripe ─── 4 UDP pipes  │
│  wan6 (Starlink enp7s8) ─── transport: stripe ─── 4 UDP pipes  │
│  wan4 (Fibra enp7s6)    ─── transport: quic   ─── 1 QUIC conn  │
│                                                                │
│  multipath_policy: balanced (round-robin across all paths)     │
└────────────────────────────────────────────────────────────────┘
```

### Config YAML

```yaml
# Globale: auto-detect Starlink e usa stripe transport
detect_starlink: true
starlink_transport: stripe        # "stripe" o "quic" (default)
starlink_default_pipes: 4
stripe_port: 46017                # porta UDP server per stripe (default: remote_port + 1000)
stripe_data_shards: 10            # K (default 10)
stripe_parity_shards: 2           # M (default 2)
stripe_enabled: true              # server: abilita listener stripe

multipath_paths:
  - name: wan5
    bind_ip: "iface:enp7s7"
    remote_addr: "172.238.232.223"
    remote_port: 45017
    priority: 1
    weight: 1
    pipes: 4
    transport: stripe             # oppure: "auto" per auto-detect

  - name: wan6
    bind_ip: "iface:enp7s8"
    remote_addr: "172.238.232.223"
    remote_port: 45017
    priority: 1
    weight: 1
    pipes: 4
    transport: stripe

  - name: wan4                    # path non-Starlink → QUIC standard
    bind_ip: "iface:enp7s6"
    remote_addr: "172.238.232.223"
    remote_port: 45017
    priority: 2
    weight: 1
    # transport: quic (default)
```

| Implementazione (stripe.go + stripe_crypto.go — ~1700 righe)

| Componente | File | Righe | Descrizione |
|-----------|------|-------|-------------|
| Wire protocol | stripe.go | ~100 | Header 16 byte, encode/decode, costanti |
| FEC group | stripe.go | ~50 | Accumulatore shard con stato present/received |
| Client conn | stripe.go | ~350 | N UDP socket, FEC encode TX, decode RX, keepalive |
| Server listener | stripe.go | ~400 | UDP listener, session management, GC |
| Server DC | stripe.go | ~150 | `datagramConn` per return-path server→client |
| Helpers | stripe.go | ~30 | parseTUNIP, ipToUint32 |
| **AES-GCM crypto** | **stripe_crypto.go** | **186** | **Cipher, key material, pending keys, encrypt/decrypt** |
| Unit tests | stripe_test.go | 331 | Header, FEC group, helpers, crypto — 13 test |
| Main integration | main.go | ~280 | Config, registerStripe, path init, server startup, **key exchange** |

### Steps implementativi

#### Step 4.6 — Multi-Pipe QUIC ✅ COMPLETATO (poi INVALIDATO)
- Campo `pipes`, espansione path, async dispatch — implementati e testati
- **Risultato**: throughput degradato del 56% per competizione CC → **SCARTATO**

#### Step 4.7 — Starlink Auto-Detection ✅ COMPLETATO
- `detectStarlink()` via rDNS (`*.starlinkisp.net`) + CGNAT fallback
- `getWANIPViaDNS()` via OpenDNS resolver bound a interfaccia specifica
- Config: `detect_starlink`, `starlink_default_pipes`, `starlink_transport`

#### Step 4.8 — UDP Stripe + FEC Transport ✅ IMPLEMENTATO
1. `stripe.go`: wire protocol, FEC (Reed-Solomon), client/server transport
2. Integrazione main.go: `registerStripe`, `resolvePathTransport`, server listener
3. `stripe_test.go`: 9 unit test (header encode/decode, FEC group, helpers)
4. Dipendenza: `github.com/klauspost/reedsolomon` aggiunta a go.mod

#### Step 4.9 — Test Stripe su Starlink ✅ COMPLETATO
1. Deploy su client e VPS con stripe_enabled
2. Configurare mp1 con `transport: stripe` + `pipes: 4` su wan4/wan5/wan6
3. iperf3 throughput test: 303 Mbps su 3 link, 313 Mbps su 2 link
4. Fix critici: SO_BINDTODEVICE (commit `560e499`), session timeout (commit `21d6845`),
   graceful shutdown (commit `f401eab`), register fail-fast
5. Risultati documentati in NOTA_TECNICA_MPQUIC.md v3.3

### Done criteria Fase 4b
- [x] Multi-pipe QUIC implementato e testato (❌ fallito per CC competition)
- [x] `detectStarlink()` identifica link Starlink via rDNS
- [x] UDP Stripe + FEC transport implementato (stripe.go, 1400 righe)
- [x] Wire protocol con header 16 byte e FEC Reed-Solomon
- [x] Integrazione main.go: client stripe path + server stripe listener
- [x] 13 unit test passing
- [x] Deploy e test su infrastruttura reale (client + VPS)
- [x] Throughput con stripe su Starlink > 200 Mbps (303 Mbps su 3 WAN)
- [x] Benchmark documentato: QUIC vs stripe vs stripe+bonding
- [x] SO_BINDTODEVICE per multi-interfaccia (commit `560e499`)
- [x] Session timeout + graceful shutdown (commit `21d6845`, `f401eab`)
- [x] Bilanciamento TX verificato: ±0.2% su 3 path

### Step 4.10 — Stripe Security: AES-256-GCM + TLS Exporter ✅ COMPLETATO

**Perché ora**: con `transport: stripe` il canale non eredita automaticamente la
sicurezza TLS di QUIC; il focus diventa **cifratura + autenticazione + PFS**
del protocollo UDP stripe.

**Decisione architetturale**: il precedente sistema MAC/rekey (HMAC-SHA256 + anti-replay
+ rekey per-epoch) è stato **completamente rimosso** perché:
- Bug critico: il server non firmava mai i pacchetti TX
- Anche dopo fix, impatto prestazionale inaccettabile (123 → 3 Mbps)
- Solo autenticazione, nessuna confidenzialità (payload in chiaro)

**Nuovo sistema implementato**: AES-256-GCM con key exchange via QUIC TLS 1.3 Exporter.

**Architettura key exchange**:
1. Client apre connessione QUIC temporanea verso server (ALPN `mpquic-stripe-kx`)
2. Server e client condividono il session ID come contesto TLS
3. Entrambi chiamano `ExportKeyingMaterial("mpquic-stripe-v1", sessionID_bytes, 64)`
4. Primi 32 byte = chiave AES-256 client→server, successivi 32 = server→client
5. Chiavi direzionali: nessun rischio di riuso nonce cross-direction

**Wire format cifrato**:
```
[stripeHdr 16B — cleartext AAD][8B sequence counter][ciphertext + 16B GCM tag]
```

**Proprietà di sicurezza**:
- **Confidenzialità**: AES-256-GCM cifratura payload
- **Autenticazione**: GCM tag 16 byte su header (AAD) + payload
- **Anti-replay**: nonce monotono 8 byte (unique per key + direction)
- **PFS**: chiavi derivate da handshake TLS 1.3 effimero (nuove ad ogni sessione)
- **Zero config**: nessun segreto condiviso da gestire (le chiavi vengono dal TLS)

**File modificati**:
- `stripe_crypto.go` (NUOVO, 186 righe): primitivi AES-GCM, key material, pending keys
- `stripe.go` (−340 righe): rimosso MAC/rekey, integrato encrypt/decrypt
- `main.go` (+183 righe): key exchange QUIC, ALPN routing, pending keys
- `stripe_test.go`: 4 test MAC rimossi, 4 test crypto aggiunti (13 totali pass)

**Diff stat**: +392 insertioni, −528 rimozioni (net −136 righe, codice più semplice)

**Done criteria Step 4.10**:
- [x] Payload stripe cifrato end-to-end (AES-256-GCM)
- [x] Nonce univoco per shard e protezione riuso nonce (monotonic atomic counter)
- [x] Key exchange via TLS 1.3 Exporter con PFS per sessione
- [x] Zero configurazione manuale (rimossi `stripe_auth_key` e `stripe_rekey_seconds`)
- [x] ALPN routing: `mpquic-ip` per tunnel, `mpquic-stripe-kx` per key exchange
- [x] Build OK, `go vet` clean, 13 test pass
- [ ] Benchmark throughput degradazione ≤10% rispetto stripe senza cifratura

### Step 4.11 — FEC Adattivo (M=0 bypass) ✅ COMPLETATO (a54b717, 3867aae)

**Obiettivo**: eliminare l'overhead FEC (20%) quando il canale è pulito (loss=0%),
riattivare automaticamente la parità RS quando il canale degrada.

**Implementazione**:
1. Nuovo parametro `stripe_fec_mode`: `always` | `adaptive` | `off`
2. Fast path M=0: ogni pacchetto IP → shard indipendente (GroupDataN=1), no padding, no parità
3. Feedback bidirezionale: keepalive esteso a [pipe_idx][rx_loss_pct], server risponde [rx_loss_pct]
4. Soglia: peer loss >2% → M=parityM; 0% per 15s → M=0
5. Loss detection: solo FEC-group-based (sequence-based rimosso — falsi positivi al 65%)

**Bug risolti**:
- `a54b717`: implementazione completa adaptive FEC
- `3867aae`: fix falsa rilevazione loss (txSeq condiviso tra M=0 e M>0 creava gap artificiali)

**Benchmark (dual Starlink, 20 pipe, adaptive M=0)**:
- Media: 239 Mbps (range 190–294), ~1.000 retransmit
- Upload: 49.9 Mbps
- Overhead M=0: 2.8% (solo crypto) vs 20% con M=2
- Nessuna transizione falsa dopo fix 3867aae

**Done criteria Step 4.11**:
- [x] Tre modalità FEC (always/adaptive/off) configurabili via YAML
- [x] Fast path M=0 senza accumulo, padding, parità
- [x] Feedback loss bidirezionale via keepalive
- [x] Falsa rilevazione loss corretta (sequence-based rimosso)
- [x] Deploy e benchmark su infra reale (dual Starlink, 20 pipe)
- [x] Risultati documentati in NOTA_TECNICA_MPQUIC.md v3.5

### Step 4.12 — Pacing per Pipe (Token Bucket) ❌ NEGATIVO (commit 2c2e78b→050870d)

**Obiettivo**: eliminare i retransmit auto-inflitti causati da burst UDP nella
tight send loop. I 2.628 retransmit osservati nel run 3 (con M=0 e 0% FEC loss)
dimostrano che la congestione è self-induced.

**Implementazione**: token bucket per sessione (`stripePacer`), rate configurabile
`stripe_pacing_rate: <Mbps>`, burst = max(2ms × rate, 32KB). `pace()` chiamato
prima di ogni `WriteToUDP()` sotto txMu. Commit `2c2e78b` + fix deficit `050870d`.

**Risultati benchmark** (6 run × 30s, iperf3 -R -P8, dual Starlink):

| Config            | Throughput medio | Retransmit medi | Note                          |
|-------------------|------------------|-----------------|-------------------------------|
| Baseline (no pacing) | **239 Mbps**  | ~1.000          | Riferimento M=0               |
| Pacing 250 Mbps (bug) | 213 Mbps    | ~1.609          | Pacer raramente attivo        |
| Pacing 150 Mbps (bug) | ~75 Mbps    | ~615            | Bug cascading deficit         |
| Pacing 200 Mbps (fix) | **145 Mbps**| ~1.152          | Pacer corretto, time.Sleep overhead |

**Root cause del fallimento**: `time.Sleep()` su Linux ha granularità ~1–4 ms
(scheduler tick). Con burst di 2 ms, i micro-sleep richiesti (2 ms) si allungano
a 3–4 ms, riducendo il throughput effettivo al 60–70% del rate configurato.
Anche a 200 Mbps/sessione (totale 400 Mbps > baseline 239), la natura bursty
del TUN reader genera picchi istantanei che attivano il pacer e subiscono
l'overshoot del timer.

**Bug corretto** (commit `050870d`): dopo lo sleep compensativo, i token
restavano negativi causando re-sleep a catena su ogni pacchetto successivo.

**Conclusione**: il pacing via `time.Sleep` non è efficace a queste velocità
su Linux. Alternative possibili (non implementate): busy-wait spin loop,
SO_TXTIME/eBPF kernel-level pacing, batch send con sendmmsg. Il codice
resta nel binary come opzione disabilitata (`stripe_pacing_rate: 0`).

**Decisione**: procedere direttamente a Step 4.13 (Hybrid ARQ) che
risolve il problema con approccio diverso (ritrasmissione selettiva).

### Step 4.13 — Hybrid ARQ con NACK Selettivo ✅ COMPLETATO (commit d158b0a)

**Obiettivo**: meccanismo reattivo di ritrasmissione: il receiver rileva gap di
sequenza, genera NACK bitmap, il sender ritrasmette solo i pacchetti mancanti.
Overhead ~0% in condizioni normali (solo NACK packets quando ci sono gap).

**Implementazione** (file `stripe_arq.go` + integrazioni in `stripe.go`):

1. **Nuovo tipo pacchetto**: `stripeNACK = 0x05`
   - Payload: `[base_seq 4B][bitmap 8B]` → fino a 64 gap per NACK
2. **TX retransmit buffer** (`arqTxBuf`): ring buffer 4096 entry (~200ms a 20K pps)
   - Ogni entry: `{seq, shardData, dataLen}` — plaintext pronto per re-encrypt
   - Thread-safe via RWMutex separato dal txMu
3. **RX gap tracker** (`arqRxTracker`): circular bitmap 8192 bit con indirizzamento modulare
   - `markReceived(seq)`: setta bit, avanza base, rileva duplicati
   - `getMissing()`: scansiona gap > `arqNackThresh` (48 seqs) dietro highest
4. **NACK generation loop**: goroutine dedicata, tick ogni 5ms
   - Client: invia NACK al server via primo pipe
   - Server: invia NACK al client via primo pipe address noto
5. **NACK handler**: riceve NACK, lookup in TX buffer, re-encrypt con nonce fresco, retransmit round-robin
6. **Bidirezionale**: sia client che server hanno TX buf + RX tracker
7. **Solo M=0**: ARQ attivo solo quando effectiveM = 0 (non compete con FEC)
8. **Config**: `stripe_arq: true/false` (default: false)

**Risultati benchmark** (6 run × 30s, iperf3 -R -P8, dual Starlink):

| Config              | Throughput medio | Retransmit medi | Note                                   |
|---------------------|------------------|-----------------|----------------------------------------|
| Baseline (no ARQ)   | **239 Mbps**     | ~1.000          | Riferimento M=0                        |
| **Hybrid ARQ on**   | **274 Mbps**     | ~3.199          | +14.6% throughput, picco 315 Mbps      |

**Analisi**: il throughput medio migliora del 14.6% (239 → 274) con picco a 315 Mbps.
I retransmit TCP aumentano perché le ritrasmissioni ARQ producono pacchetti IP
duplicati al TUN output, interpretati dal QUIC CC come segnali di congestione.
Nonostante ciò, il recupero rapido dei gap (~30ms: 5ms NACK + 25ms RTT) previene
i timeout QUIC e consente al sender di mantenere una finestra di invio più ampia.

### Step 4.13b — ARQ Optimizations: Dedup + NACK Rate Limit + nackThresh ✅ COMPLETATO (commit 9478e56)

**Obiettivo**: ridurre i retransmit TCP (~3199) e l'overhead NACK introdotti dal
Hybrid ARQ iniziale, mantenendo il guadagno di throughput.

**Implementazione** (3 ottimizzazioni in un commit):

1. **ARQ dedup receiver**: il valore di ritorno di `markReceived()` ora viene
   controllato prima della consegna al TUN. Pacchetti con GroupSeq già ricevuto
   vengono scartati silenziosamente (sia client che server RX path). Elimina i
   pacchetti IP duplicati da ritrasmissioni ARQ che entravano nel TUN causando
   retransmit TCP spuri.

2. **NACK rate limiting**: max 1 NACK per `arqNackCooldown` (30ms ≈ 1 RTT Starlink).
   `canSendNack()`/`recordNackSent()` tracciano il timestamp atomicamente.
   Il ticker a 5ms avrebbe altrimenti inviato ~5 NACK per lo stesso gap prima
   che la ritrasmissione arrivi, sprecando banda e causando retransmit multipli.

3. **Adaptive nackThresh**: aumentato da 48 a 96 sequenze. Starlink presenta
   reordering naturale di 20-50 pacchetti; la soglia precedente causava false
   NACK che triggheravano ritrasmissioni non necessarie.

**Nuovo contatore**: `dupFiltered` — pacchetti scartati dal dedup prima del TUN write.
`stats()` ora restituisce `(nacksSent, retxReceived, dupFiltered)`.

**Risultati benchmark** (6 run × 30s, iperf3 -R -P20, dual Starlink, 12 pipe/path = 24 totali):

| Config                            | Throughput medio | Retransmit medi | Note                                                 |
|-----------------------------------|------------------|-----------------|------------------------------------------------------|
| Baseline (no ARQ, P8, 10 pipe)    | **239 Mbps**     | ~1.000          | Riferimento M=0                                      |
| ARQ v1 (P8, 10 pipe)              | **274 Mbps**     | ~3.199          | Step 4.13, +14.6% vs baseline                        |
| **ARQ v2 + dedup (P20, 12 pipe)** | **330 Mbps**     | ~4.110          | **+38% vs baseline, +20% vs ARQ v1, picco 384 Mbps** |
| ARQ v2 + batch I/O (P20, 12 pipe) | **333 Mbps**     | n/d             | Step 4.16, **neutro** (+1%, entro rumore), picco 390 |

Dettaglio run ARQ v2:

| Run | Throughput (Mbps) | Retransmit |
|-----|------------------|------------|
| 1   | 318              | 2.807      |
| 2   | 305              | 4.282      |
| 3   | 351              | 3.407      |
| 4   | 318              | 4.311      |
| 5   | **384**          | 4.399      |
| 6   | 302              | 5.457      |

**Analisi**: il throughput medio migliora del 38% rispetto al baseline originale
(239 → 330 Mbps) con picco a 384 Mbps. I retransmit TCP restano elevati (~4110)
nonostante il dedup: il dedup elimina i duplicati ARQ a livello UDP/stripe, ma i
retransmit TCP sono causati dalla variabilità di latenza Starlink (jitter) e dai
burst loss che provocano timeout QUIC indipendentemente dall'ARQ. Il miglioramento
rispetto ad ARQ v1 (+20%) è dovuto a tre fattori combinati:
- Dedup elimina duplicati che confondevano il QUIC CC
- NACK rate limit (30ms) riduce retransmit inutili
- nackThresh 96 previene false NACK su reorder naturale
- 12 pipe/path (vs 10) e P20 (vs P8) sfruttano meglio il parallelismo Starlink

### Step 4.16 — Batch I/O (recvmmsg) ✅ COMPLETATO — RISULTATO NEUTRO

**Motivazione**: benchmark con P20 (422 Mbps) vs P8 (274 Mbps) mostra che
"più flussi paralleli = più throughput". L'ipotesi iniziale identificava come
bottleneck l'overhead per-packet delle syscall RX.

**Soluzione**: recvmmsg (Linux) via `ipv4.PacketConn.ReadBatch()` — legge fino
a 8 datagrammi per syscall, riducendo l'overhead RX di ~8×.

**Modifiche implementate**:
1. **Server RX** (`Run()`): refactored con `ipv4.NewPacketConn(ss.conn)` +
   `ReadBatch(msgs, 0)`. Logica decrypt/dispatch estratta in
   `processIncomingPacket()`. Eliminata la copia per-packet `make+copy`
   (i buffer batch sono indipendenti).
2. **Client RX** (`recvPipeLoop()`): stesso pattern per-pipe con
   `ipv4.NewPacketConn(conn)` + `ReadBatch()`.
3. Costante `stripeBatchSize = 8` (stessa di quic-go).
4. Import `golang.org/x/net/ipv4` (già dependency indiretta via quic-go).

**Sicurezza buffer**: `stripeDecryptPkt()` alloca un output proprio e non
ritiene il buffer input; tutti gli handler copiano i dati necessari prima
del return. I buffer batch possono essere riusati tra chiamate ReadBatch.

**Risultati benchmark** (8 run × 30s, iperf3 -R -P20 --bind-dev mp1, commit 1e9a8b3):

| Run | Throughput (Mbps) |
|-----|------------------|
| 1   | 298              |
| 2   | 345              |
| 3   | 293              |
| 4   | 351              |
| 5   | **390**          |
| 6   | 363              |
| 7   | 339              |
| 8   | 286              |
| **Media** | **333**    |

**Verdetto: NEUTRO** (+1% vs pre-batch 330 Mbps, entro margine di errore).
Il batching RX non muove l'ago perché il kernel già bufferizza i pacchetti
in arrivo; `recvmmsg` risparmia context switch ma il collo di bottiglia reale
non è nella syscall RX.

**Analisi post-mortem — perché più flussi = più throughput**:
Il pattern P20 >> P8 >> P1 è un fenomeno di **congestion control aggregato**,
non di overhead syscall:
- Ogni flusso TCP ha il proprio CC (Cubic/BBR) indipendente
- Con N flussi, una loss/timeout su 1 flusso rallenta solo 1/N del throughput
- Starlink ha jitter 15-50ms e burst-loss: più flussi assorbono meglio queste
  perturbazioni → throughput aggregato più stabile e alto
- Il singolo flusso TCP non riesce a saturare il link per limiti di window/RTT

Il codice batch resta nel codebase (è pulito e non ha downsides) ma la
prossima ottimizzazione deve targettare il layer TCP/CC o il TX path.

### Step 4.17 — UDP Socket Buffers 7MB + TX ActivePipes Cache ✅ COMPLETATO (commit bef0894)

**Obiettivo**: eliminare due sorgenti di inefficienza nel hot path:
1. Socket buffer kernel insufficienti (default ~208 KB) che causano drop
   durante burst Starlink (jitter 50+ ms → centinaia di pacchetti in coda)
2. Allocazione per-pacchetto `make([]*net.UDPAddr, ...)` nel server TX path
   per filtrare pipe attive ad ogni `SendDatagram()`

**Implementazione**:

1. **`setStripeSocketBuffers()`**: applica `SetReadBuffer(7MB)` + `SetWriteBuffer(7MB)`
   su ogni socket UDP. 7 MB corrisponde al valore usato da quic-go
   (`protocol.DesiredReceiveBufferSize`). Applicato a:
   - Client: ogni pipe UDP in `newStripeClientConn()`
   - Server: socket listener in `newStripeServer()`

2. **`txActivePipes` cache**: campo `[]*net.UDPAddr` su `stripeSession`,
   ricostruito solo su REGISTER e keepalive address update (sotto `txMu`).
   Sostituisce la creazione + filter `make+append` ripetuta su ogni pacchetto
   nei 3 hot path TX:
   - `SendDatagram()` M=0 fast path (~30K pkt/s a 341 Mbps)
   - `sendFECGroupLocked()` M>0 path
   - `handleNack()` ARQ retransmission

**Nota sysctl**: il kernel Linux limita il buffer massimo a `net.core.rmem_max`
e `net.core.wmem_max`. Se questi sono inferiori a 7 MB, il `SetReadBuffer`/
`SetWriteBuffer` viene silenziosamente troncato. Per ottenere il beneficio
completo, configurare:

```bash
net.core.rmem_max = 7340032
net.core.wmem_max = 7340032
```

**Effort**: 50 righe di codice, 0 nuove dipendenze.

**Benchmark** (6 marzo 2026, dual Starlink WAN5+WAN6, 24 pipe, P20 30s × 6 run):

| Run | Receiver (Mbps) | Retransmit         |
|-----|-----------------|------------        |
| 1   | 223             | 3.585 (cold start) |
| 2   | 344             | 3.569              |
| 3   | 345             | 4.695              |
| 4   | **390**         | 2.282              |
| 5   | 338             | 3.030              |
| 6   | 352             | 3.786              |
| **Media (run 2-6)** | **354 Mbps** | **3.472** |

**Risultato**: +3.8% vs Step 4.16 (341 → 354 Mbps), +48% vs baseline (239 Mbps).
Picco 390 Mbps confermato. Efficienza canale 84.3%.

### Step 4.14 — FEC per Dimensione Pacchetto ❌ NEGATIVO (commit ca4f179→revert)

**Obiettivo**: quando M>0 è attivo, pacchetti piccoli (<300B: ACK TCP, DNS, keepalive)
vengono padded a ~1402B sprecando >90% dello shard. Skip FEC per questi pacchetti.

**Implementazione** (poi revertita):
1. Soglia `stripeFECMinSize = 300` (configurabile via `stripe_fec_min_size`)
2. In `SendDatagram()` (client e server): se `len(pkt) < fecMinSize && effectiveM > 0`
   → invio diretto M=0-style (GroupDataN=1, nessun accumulo/padding/parità)
3. Contatore `txFECSkip` (atomic) per telemetria

**Benchmark** (11 marzo 2026, dual Starlink WAN5+WAN6, 24 pipe, P20 30s × 6 run):

| Run | Throughput (Mbps) | Retransmit |
|-----|------------------|------------|
| 1   | 343              | 10.775     |
| 2   | 359              | 10.703     |
| 3   | 352              | 13.469     |
| 4   | 281              | 6.194      |
| 5   | 289              | 12.242     |
| 6   | 361              | 6.172      |
| **Media** | **331 Mbps** | **9.926** |

**Confronto con baseline (Step 4.17, v4.1)**:

| Metrica | Baseline (v4.1) | Step 4.14 | Delta |
|---------|----------------|-----------|-------|
| Media | 354 Mbps | 331 Mbps | -6.5% |
| Picco | 390 Mbps | 361 Mbps | -7.4% |
| Retransmit | ~3.472 | ~9.926 | +186% |

**Root cause del fallimento**: il codice FEC skip è **dead code** in modalità
adaptive M=0 (il 99% del tempo operativo). Il fast path M=0 in `SendDatagram()`
ritorna prima di raggiungere il blocco FEC skip. La feature diventa attiva solo
con effectiveM > 0 (loss >2%), scenario raro e transitorio dove il risparmio
sarebbe comunque marginale.

**Conclusione**: complessità aggiunta (costanti, campi struct, logica condizionale,
contatori atomici) senza beneficio misurabile. Non è prerequisito per nessun
altro step. **Codice revertito**, codebase torna a v4.1 (senza Step 4.14).

### Step 4.15 — Sliding Window FEC ⬜ FUTURO

**Obiettivo**: evoluzione dei gruppi FEC fissi (K blocchi → K+M shard) verso una
finestra scorrevole dove la parità protegge gli ultimi N shard "in volo".

**Vantaggi rispetto a gruppi fissi**:
- Nessuna attesa per completare un gruppo → latenza minore
- Parità calcolata su finestra scorrevole → protezione continua
- Migliore granularità nel recovery (non serve ricevere K shard dello stesso gruppo)

**Effort stimato**: giorni — complessità significativa nel tracking della finestra.

### Step 4.18 — RX Reorder Buffer ❌ NEGATIVO (commit 6e6293c→revert 1b010a9)

**Obiettivo**: buffer di riordinamento RX basato su sequenza per eliminare il
packet reordering causato dal round-robin su N pipe con latenze diverse.

**Implementazione**: `stripeReorderBuf` con window slot, consegna in-order,
timeout per gap. Integrato in client `deliverDataDirect()` e server RX path.

**Tuning testati** (6 run × 30s, iperf3 -R -P20, dual Starlink):

| Tuning | Window | Timeout | Throughput | Retransmit | vs Baseline |
|--------|--------|---------|------------|------------|-------------|
| Default | 128 | 3ms | 307 Mbps | 4.344 | -13.3% |
| #1 | 24 | 1ms | 303 Mbps | 4.245 | -14.4% |
| #2 | 16 | 200µs | 298 Mbps | 11.574 | -15.8% |

**Root cause del fallimento**: il reorder buffer aggiunge **jitter artificiale**
ai pacchetti (in-order: 0 delay, fuori-ordine: fino a timeout). Questo jitter
variabile confonde la stima RTT di TCP più del reordering naturale sui pipe.
TCP gestisce il reordering con DupACK (meccanismo collaudato), ma il jitter
variabile corrompe lo smoothed-RTT e causa backoff del congestion control.

**Conclusione**: approccio fondamentalmente incompatibile con TCP inner flow.
**Codice completamente rimosso**.

### Step 4.19 — pprof Profiling + Analisi Bottleneck ✅ COMPLETATO

**Obiettivo**: aggiungere profiling CPU/memoria runtime per identificare i
bottleneck reali prima di ottimizzare.

**Implementazione**: flag `--pprof :6060` avvia HTTP server con `net/http/pprof`.
Profilo CPU 30s catturabile con: `go tool pprof http://host:6060/debug/pprof/profile?seconds=30`

**Effort**: 5 righe, zero impatto su performance quando non attivato.

**Risultati profiling** (server VPS, 60s sotto carico iperf3 -R -P20, 86.56s CPU totali = 143.9%):

| Funzione | Tempo cum | % CPU | Analisi |
|---|---|---|---|
| `SendDatagram → WriteToUDP → sendto` | **39s** | **45.0%** | TX path: 1 syscall `sendto` per ogni pacchetto |
| `tunWriter → os.File.Write` | **20s** | **22.8%** | TUN write: 1 syscall `write` per ogni pacchetto IP |
| `runtime scheduling` | **12.5s** | **14.4%** | Overhead goroutine scheduling |
| `stripeEncryptShard (AES-GCM)` | **4s** | **4.6%** | Crypto — molto efficiente con AES-NI hardware |
| `stripeServer.Run → RX path` | **4.5s** | **5.2%** | recvmmsg + decrypt + dispatch |
| `runtime.mallocgc` | **5.1s** | **5.9%** | Garbage collection / allocazioni |

**Conclusioni chiave**:
1. **Il 67% del tempo CPU totale è in `syscall.Syscall6`** — il server è completamente I/O-bound
2. **TX path (45%)** è il bottleneck dominante: `SendDatagram()` fa 1 `WriteToUDP` per pacchetto
3. **TUN write (23%)** è il secondo bottleneck: 1 `write()` per pacchetto IP decrittato
4. **AES-GCM è trascurabile (4.6%)** — AES-NI hardware fa il suo lavoro, non da ottimizzare
5. **RX path (5.2%)** — conferma che `recvmmsg` (Step 4.16) ha già risolto il lato RX
6. **UDP GRO avrebbe impatto marginale** — solo 5% del tempo è nel RX path

**Roadmap aggiornata in base al profiling**:
- Priorità #1: **batch TX via sendmmsg** (attacca il 45%)
- Priorità #2: **batch TUN write via writev** (attacca il 23%)
- Deprioritizzato: UDP GRO (solo 5%), crypto optimization (solo 4.6%)

### Step 4.20 — Batch TX via sendmmsg ✅ COMPLETATO (commit ae36b1e)

**Obiettivo**: ridurre le syscall TX da N a N/batch usando `sendmmsg` (batch sendto).
Profiler mostra che `SendDatagram → WriteToUDP → sendto` consuma il **45% del tempo
CPU server** — ogni pacchetto richiede una traversata completa del kernel stack.

**Perché sendmmsg invece di UDP GSO**:
- GSO richiede che tutti i segmenti vadano alla stessa destinazione — incompatibile
  con il round-robin server che ruota tra N pipe address diverse
- `sendmmsg` supporta destinazioni diverse per ogni messaggio nel batch
- Già usato lato RX (`recvmmsg` / `ReadBatch`) con successo
- `ipv4.PacketConn.WriteBatch()` disponibile in `golang.org/x/net/ipv4`

**Architettura**:
1. `SendDatagram()` accumula pacchetti criptati in un ring buffer per-sessione
2. Flush via `WriteBatch()` quando: (a) batch pieno (8 pacchetti) oppure
   (b) nessun altro pacchetto in coda (channel drain)
3. Client: batch per-pipe (stessa destinazione), server: batch multi-destinazione
4. Fallback: se `WriteBatch` non disponibile, singolo `WriteToUDP` come oggi

**Implementazione** (commit ae36b1e, 214 righe):
- `txBatchAddLocked()`: accumula wirePkt+addr in `txBatchMsgs[]`, auto-flush a 8
- `txBatchFlushLocked()`: `WriteBatch()` = sendmmsg — 1 syscall per batch
- `FlushTxBatch()`: thread-safe, chiamato da `drainSendCh` dopo batch-drain
- `drainSendCh` modificato: blocking recv → non-blocking drain → FlushTxBatch
- `txBatcher` interface con type assertion (non modifica `datagramConn`)
- Timer FEC flush include batch flush per pacchetti pendenti

**Risultati profiling (validazione CPU, dual Starlink, pioggia 12/03)**:

| Area | Prima (v4.1) | Dopo (sendmmsg) | Delta |
|---|---|---|---|
| TX path (`drainSendCh`) | **45.0%** | **42.2%** | -2.8 pp |
| TUN write (`tunWriter`) | 22.8% | 26.3% | +3.5 pp (relativo) |
| Scheduling | 14.4% | 11.2% | **-3.2 pp** |
| RX path | 5.2% | 10.0% | +4.8 pp (pioggia) |
| **CPU totale** | **143.9%** | **127.0%** | **-17%** |

**Benchmark throughput (pioggia forte, dual Starlink, P30)**:
- Media: **332 Mbps** (60s, P30, iperf3 -R)
- **Picco: 434 Mbps** — record assoluto del progetto
- Retransmit: 10.375 (influenzati da attenuazione pioggia)
- P30 > P20: confermato che più flussi TCP paralleli migliorano l'aggregazione

**Nota**: FlushTxBatch (flush esplicito) non appare nei top 40 — i batch si
riempiono a 8 e si auto-flushano da txBatchAddLocked. Il batching funziona
come progettato.

**Benchmark definitivo con tempo buono**: pendente (confronto diretto con baseline 354 Mbps).

**Effort**: ~150 righe codice, 0 nuove dipendenze, 0 configurazione.

### Step 4.21 — tunWriter batch-drain + reduce per-packet mutex ✅ IMPLEMENTATO

**Obiettivo**: ridurre overhead di scheduling e mutex contention nel hot loop `tunWriter`.

**Analisi profiling** (dual Starlink, 12 marzo, pioggia):
- `tunWriter → os.File.Write` = **26.3%** del tempo CPU server
- `runtime.findRunnable` (scheduling) = **9.76%** — include park/unpark di tunWriter
- `touchPath` e `learnRoute` chiamati **per ogni pacchetto** con RLock + operazioni

**Limitazione TUN**: il device `/dev/net/tun` in modalità `IFF_TUN` (layer 3) accetta
esattamente **1 pacchetto IP per write()**. Non c'è modo di iniettare più pacchetti
in una singola syscall (`writev` concatena gli iovec in un unico pacchetto).
L'approccio originale `writev` è quindi inapplicabile.

**Approccio implementato — batch-drain rxCh**:
Stesso pattern di `drainSendCh` (Step 4.20):
1. **Blocking receive** di 1 pacchetto da `sess.rxCh` (buffer 512)
2. **Non-blocking drain** di tutti i pacchetti aggiuntivi in coda
3. **Tight write loop**: `tun.Write()` per ogni pacchetto senza re-scheduling
4. **touchPath una volta per batch** (non per pacchetto) — elimina N-1 RLock per batch
5. **learnRoute solo se srcIP ≠ peerIP** — skip per traffico dalla stessa sorgente

**Impatto atteso**:
- Goroutine tunWriter resta running per l'intero batch → meno park/unpark cycles
- Riduzione mutex contention: touchPath da N a 1 lock per batch
- TUN write resta 1 syscall per pacchetto (limitazione strutturale)
- Stima: scheduling da 11.2% → ~8-9%, touchPath overhead quasi azzerato

**Effort**: ~45 righe (rewrite funzione `tunWriter` in `stripe.go`).

**Risultati profiling (validazione post-deploy, dual Starlink, pioggia 12/03)**:

| Area | Step 4.20 | Step 4.20+4.21 | Delta |
|---|---|---|---|
| TX path (`drainSendCh`) | 42.2% | **41.0%** | -1.2 pp |
| TUN write (`tunWriter`) | 26.3% | **26.9%** | +0.6 pp (relativo) |
| Scheduling (`findRunnable`) | 11.2% | **10.1%** | **-1.1 pp** |
| **CPU totale** | **127%** | **108%** | **-19 pp (-15%)** |

**touchPath/learnRoute non appaiono nel top 40** — overhead mutex per-packet
eliminato dal batch-drain. Overhead tunWriter: 26.89% cum - 26.38% (os.File.Write)
= **0.51%** di overhead puro (prima era significativo).

**Benchmark throughput (pioggia, dual Starlink, P30)**:
- **Picco: 458 Mbps** — nuovo record assoluto
- Media: 296 Mbps (variabilità meteo 148-458 Mbps nel run)
- Finestra 67-82s (link buono): 370-458 Mbps stabile

**Commit**: 688e952.

### Step 4.23 — TUN Multiqueue (IFF_MULTI_QUEUE) ✅ COMPLETATO

**Obiettivo**: eliminare la contention sul singolo file descriptor TUN condiviso
tra TUN reader (goroutine dispatch) e N tunWriter (goroutine per-session).

**Profiling post Step 4.21**: TUN write al **26.9%** CPU, TUN read al **7.5%** CPU.
Totale TUN I/O = ~34.4% della CPU. Con singolo fd, tutte le write/read sono
serializzate dal kernel su un'unica coda interna del device.

**Linux TUN multiqueue** (kernel 3.8+): flag `IFF_MULTI_QUEUE` sull'ioctl
`TUNSETIFF` permette di aprire **N file descriptor indipendenti** sullo stesso
TUN device. Il kernel mantiene una coda per fd:
- **RX** (kernel → userspace): i pacchetti sono distribuiti tra le code (hash-based)
- **TX** (userspace → kernel): ogni fd scrive sulla propria coda senza lock globale

**Implementazione**:
1. `openTUN()` helper: crea TUN con `MultiQueue: true`, fallback single-queue
2. `ensure_tun.sh`: creazione con `multi_queue` flag, gestione EBUSY su restart
3. `stripeSession`: nuovo campo `tunFd *water.Interface` — fd dedicato per-sessione
4. Session creation (`handleRegister`): `water.New()` con `MultiQueue: true` → nuovo fd
5. `tunWriter`: usa `sess.tunFd.Write()` invece di `ss.tun.Write()` (fd condiviso)
6. `tunFdReader()`: goroutine di lettura per-session fd (vedi bug critico sotto)
7. Session cleanup (GC + Close): chiusura del per-session fd
8. Fallback: se multiqueue fd fails, usa fd condiviso (backward compatible)

**Bug critico — RX distribution con IFF_MULTI_QUEUE**:
Con `IFF_MULTI_QUEUE`, il kernel distribuisce i pacchetti RX (kernel → userspace)
su **tutti** i fd aperti via hash-based queue selection. I per-session fd creati in
`handleRegister` avevano solo `tunWriter` (goroutine write), **nessun reader**.
Risultato: ~2/3 dei pacchetti di ritorno restavano bloccati in code non lette →
**100% packet loss** (ping e iperf3 a zero).

**Fix**: aggiunta goroutine `tunFdReader()` su ogni per-session fd. Legge pacchetti IP,
estrae dst IP dall'header IPv4/IPv6, dispatcha via `connectionTable.dispatch()`.
Esce quando `sess.tunFd.Close()` viene chiamato durante GC.

**Catena bug-fix deploy** (5 bug sequenziali risolti):
1. `ensure_tun.sh` senza `multi_queue` flag → commit `e9eb1b4`
2. `openTUN()` helper mancante su 2 callsite → commit `128fc0b`
3. EBUSY su ricreazione TUN durante restart → commit `b6c30fd`
4. `mpquic-update.sh` parsava `●` come nome istanza → commit `261342b`, `353d966`
5. Per-session fd senza reader → 100% packet loss → commit `5eeb2d4`

**Risultati profiling (post-deploy, dual Starlink, 2026-03-13)**:

| Area | Step 4.20+4.21 (pre-MQ) | Step 4.23 (multiqueue) | Delta |
|---|---|---|---|
| TX path (`drainSendCh`) | 41.0% | **42.3%** | +1.3 pp |
| TUN write (`tunWriter`) | 26.9% | **27.2%** | +0.3 pp |
| TUN per-session reader (`tunFdReader`) | — | **10.6%** | **+10.6 pp** (nuovo) |
| Scheduling (`findRunnable`) | 10.1% | **9.6%** | -0.5 pp |
| **CPU totale** | **108%** | **116%** | **+8 pp** |

**Nota CPU**: l'aumento di +8 pp è dovuto ai `tunFdReader()` goroutine aggiuntivi
(10.6% CPU). Senza di loro i pacchetti RX restano bloccati, quindi è overhead
necessario e non eliminabile con IFF_MULTI_QUEUE. Il beneficio è la parallelizzazione
delle write TUN su code kernel separate (no lock globale).

**Benchmark throughput (dual Starlink, P20, -R)**:

| Metrica | Step 4.17 (v4.1 baseline) | Step 4.20+4.21 (pioggia) | Step 4.23 (30s) | Step 4.23 (150s) |
|---|---|---|---|---|
| **Media** | **354 Mbps** | **296 Mbps** | **374 Mbps** | **333 Mbps** |
| **Picco** | 390 Mbps | 458 Mbps | 451 Mbps | **499 Mbps** |
| Retransmit | 3.189 | n/a | 2.952 | 19.818 |
| CPU server | 144% | 108% | — | **116%** |
| Meteo | buono | **pioggia** | buono | variabile |

**Nuovo record assoluto: 499 Mbps** (t=76s del run 150s).

Run 150s — pattern Starlink: forte variabilità (170-499 Mbps range) con 3 fasi
distinte: warmup degradato (t=0-17, ~220 Mbps), fase ottimale (t=18-91, ~430 Mbps
sostenuto), fase stabile (t=92-150, ~335 Mbps).

**Confronto 30s fair-weather**: 374 Mbps (+5.6% vs baseline 354 Mbps).

**Commits**: `804262d` (codice Go multiqueue), `e9eb1b4`-`b6c30fd` (deploy fixes),
`261342b`-`353d966` (mpquic-update.sh), `5eeb2d4` (tunFdReader fix critico).

### Step 4.22 — UDP GRO per RX ⬜ DEPRIORITIZZATO

**Obiettivo originale**: Generic Receive Offload per coalescere datagrammi UDP.

**Stato dopo profiling**: il RX path consuma solo il **5.2% del tempo CPU** server.
L'impatto atteso di UDP GRO è marginale (~2-3% miglioramento complessivo).
Resta come opzione futura se altri bottleneck vengono risolti e il RX path
diventa il fattore limitante.

### Ottimizzazioni valutate e scartate

| Ottimizzazione | Motivo esclusione |
|---|---|
| `SO_ZEROCOPY` / `MSG_ZEROCOPY` | Incompatibile con Go GC (runtime gestisce memoria), beneficio solo se memory-bound |
| `SO_BUSY_POLL` | Il jitter è Starlink (15-50ms), non scheduling CPU (µs) — nessun impatto atteso |
| `SO_REUSEPORT` + CPU pinning | Già 24 socket separati con 1 goroutine/pipe, zero contention socket |
| AF_XDP kernel bypass | Richiede CGo o data plane C separato, effort sproporzionato vs beneficio |
| `io_uring` per UDP | Benchmark reali mostrano underperformance vs mmsg per UDP |

### Piano operativo ottimizzazioni (aggiornato post-profiling)

1. ~~**Step 4.19 — pprof profiling**~~ ✅ Completato — bottleneck identificati: TX syscall 45%, TUN write 23%
2. ~~**Step 4.20 — Batch TX (sendmmsg)**~~ ✅ Completato — CPU -17%, picco 434 Mbps
3. ~~**Step 4.21 — tunWriter batch-drain + reduce mutex**~~ ✅ Completato — CPU -19pp (108%), picco 458 Mbps
4. ~~**Step 4.23 — TUN Multiqueue (IFF_MULTI_QUEUE)**~~ ✅ Completato — per-session TUN fd, picco 499 Mbps
5. **Step 4.22 — UDP GRO** → deprioritizzato (RX path solo 5.2%)
6. **Step 4.15 — Sliding Window FEC** → evoluto in Step 4.26 (XOR parity)

### Done criteria Fase 4b (aggiornati)
- [x] UDP Stripe + FEC transport implementato e deployato
- [x] AES-256-GCM + TLS Exporter per sicurezza stripe
- [x] FEC adattivo (M=0 bypass) con feedback bidirezionale
- [x] Pacing per pipe (Step 4.12) — **risultato negativo**, time.Sleep inadeguato
- [x] Hybrid ARQ con NACK (Step 4.13) — **+14.6% throughput** (239→274 Mbps)
- [x] ARQ optimizations (Step 4.13b) — dedup receiver, NACK rate limit 30ms, nackThresh 96
- [x] Batch I/O recvmmsg (Step 4.16) — **risultato neutro** (+1%), ipotesi syscall falsificata
- [x] Socket buffers 7MB + TX cache (Step 4.17) — **+3.8%** (341→354 Mbps), picco 390 Mbps, efficienza 84.3%
- [x] FEC per dimensione pacchetto (Step 4.14) — **risultato negativo**, dead code in M=0 adaptive, revertito
- [x] RX Reorder Buffer (Step 4.18) — **risultato negativo**, jitter artificiale peggiorativo, revertito
- [x] pprof profiling support (Step 4.19) — flag `--pprof` per CPU/memory profiling
- [x] Batch TX sendmmsg (Step 4.20) — **CPU -17%**, picco 434 Mbps (record), sendmmsg confermato in profile
- [x] tunWriter batch-drain + reduce mutex (Step 4.21) — **CPU -19pp** (108%), picco 458 Mbps
- [x] TUN Multiqueue IFF_MULTI_QUEUE (Step 4.23) — **picco 499 Mbps** (record), 374 Mbps media 30s, CPU +8pp (116%)
- [ ] UDP GRO per RX (Step 4.22) — deprioritizzato
- [ ] Throughput ≥ 400 Mbps su dual Starlink — **picco 499 raggiunto**, media 374 (30s buono)

---

## Fase 4c — Stabilizzazione Data Plane: da picco a media ≥400 Mbps 🔄 IN CORSO

**Versione base**: v4.3 (tag `v4.3`, commit `1a87429`)

**Obiettivo**: trasformare il picco dimostrato (499 Mbps) in media stabile ≥400 Mbps
su finestre 30-60s. Il bottleneck non è più capacità del codice né del link, ma
**burstiness e variabilità del sender/receiver** che deprimono la media.

### Valutazione strategica (2026-03-15)

Con v4.3 il throughput ha raggiunto 499 Mbps di picco e 374 Mbps di media (30s buono).
Il software può performare ben oltre il target, ma la media è 25% sotto il picco.
L'analisi mostra che le cause principali sono:

1. **Micro-burstiness TX**: `sendmmsg` riduce syscall ma emette batch in burst
   → code switch congestionate → NACK indotti dal software
2. **Assenza di pacing**: `time.Sleep` fallito (Step 4.12), nessun sostituto kernel-level
3. **FEC overhead nei transitori**: RS block-based con padding 26%+ attivo solo >2% loss
   → nei momenti di beam handoff Starlink il recovery è lento

| Step | Intervento | Target | Effort | Impatto atteso |
|------|-----------|--------|--------|---------------|
| **4.24** | UDP GSO (`UDP_SEGMENT`) | Ridurre overhead/pkt, stabilizzare TX | 1 giorno | Media +5-10% |
| **4.25** | Kernel pacing `SO_TXTIME` + `sch_fq` | Eliminare burstiness software | 1.5 giorni | Media +5-15% |
| **4.26** | Sliding Window FEC (XOR parity, N=8) | Recovery burst loss senza overhead RS | 2 giorni | Ridurre retransmit in transitorio |
| **Target** | **Media ≥ 400 Mbps** su 30-60s dual Starlink | | | |

### Ottimizzazione valutata e scartata (Fase 4c)

| Ottimizzazione | Motivo esclusione |
|---|---|
| `SO_REUSEPORT` + per-core sharding | Architettura ha già 24 socket indipendenti + TUN multiqueue per-session. Profiling non mostra lock contention nel top 40. Go scheduler non supporta affinity statica. Guadagno atteso <2%. |

### Step 4.24 — UDP GSO (`UDP_SEGMENT`) nel TX path ✅ IMPLEMENTATO

**Obiettivo**: sostituire le `WriteToUDP` individuali sul client (N syscall per batch)
con `UDP_SEGMENT` (1 syscall per pipe per batch, kernel segmenta). Riduce overhead
per pacchetto e stabilizza il ritmo di emissione delegando segmentazione al kernel.

**Perché GSO dopo sendmmsg**:
- `sendmmsg` riduce syscall da N a N/8, ma ogni datagramma è ancora costruito e
  passato singolarmente al kernel
- `UDP_SEGMENT` passa un buffer grande e lascia al kernel la segmentazione in
  datagrammi di `segment_size` byte — 1 sola traversata dello stack di rete
- Cloudflare e quic-go usano `UDP_SEGMENT` per trasmissione UDP ad alto rate
- Il codice GSO esiste già in `local-quic-go/sys_conn_helper_linux.go`
  (`appendUDPSegmentSizeMsg`, `isGSOEnabled`, `isGSOError`)

**Scoperta durante code study**: il client NON aveva alcun batching TX!
Ogni `SendDatagram` → `WriteToUDP` individuale. Il server aveva già `sendmmsg`,
ma il client (che è il sender primario in `-R` reverse) faceva 1 syscall per
pacchetto. GSO è quindi il miglioramento più significativo sul client.

**Vincolo architetturale**: GSO richiede stessa destinazione per tutti i segmenti
in un singolo `sendmsg`. Impatto:
- **Client TX**: GSO puro — ogni pipe ha destinazione fissa (`serverAddr`)
- **Server TX**: mantiene `sendmmsg` — round-robin su N indirizzi client rende
  il grouping GSO impratico (<2 pkt consecutivi stesso addr)

**Implementazione effettiva** (v4.4):
1. `stripe_gso_linux.go`: helper Linux-only con build tag
   - `stripeGSOProbe()`: rileva kernel ≥5 + `UDP_SEGMENT` socket option
   - `stripeGSOBuildOOB()`: costruisce ancillary cmsg con `UDP_SEGMENT`
   - `stripeGSOIsError()`: rileva EIO (NIC senza TX checksum offload)
2. `stripe_gso_other.go`: stub per non-Linux (`gsoEnabled=false`)
3. `stripeClientConn`: nuovi campi `gsoEnabled`, `gsoDisabled` (atomic), `gsoBufs []gsoTxPipeBuf`
4. Per-pipe accumulation: `gsoAccumLocked()` concatena shard criptati in buffer per pipe
5. Flush: `gsoFlushPipeLocked()` → `WriteMsgUDP(bigBuf, oob, serverAddr)` per pipe
6. `FlushTxBatch()` implementato su client → `drainSendCh` lo chiama automaticamente
7. Entrambi i path (M=0 fast path e M>0 FEC `sendFECGroupLocked`) usano GSO
8. Fallback: se `isGSOError()` → `gsoDisabled=1` atomic, resend individuale
9. Config: `stripe_disable_gso: true` per A/B test
10. `flushTxGroup` (FEC timer) chiama `gsoFlushAllLocked()` dopo encode

**Benchmark (2026-03-16, dual Starlink, P30, -R, 7 run: 1×60s + 6×30s)**:

| Run | Durata | Receiver | Retransmit | Retransmit/s | Picco 1s |
|-----|--------|----------|------------|-------------|----------|
| 0 | 60s | **364 Mbps** | 9533 | 159 | **548 Mbps** |
| 1 | 30s | **400 Mbps** | 4273 | 142 | 455 Mbps |
| 2 | 30s | **365 Mbps** | 5705 | 190 | 443 Mbps |
| 3 | 30s | **238 Mbps** | 4095 | 137 | 302 Mbps |
| 4 | 30s | **311 Mbps** | 4690 | 156 | 429 Mbps |
| 5 | 30s | **344 Mbps** | 6847 | 228 | 460 Mbps |
| 6 | 30s | **356 Mbps** | 6020 | 201 | 440 Mbps |

**Statistiche 6×30s**: media **336 Mbps**, mediana 350, media senza outlier **355 Mbps**,
best **400 Mbps**, worst 238 Mbps (degradazione Starlink), StdDev 51 Mbps.

**Confronto con v4.3 baseline**:

| Metrica | v4.3 | v4.4 GSO | Delta |
|---------|------|----------|-------|
| Picco per-second | 499 Mbps | **548 Mbps** | **+9.8%** |
| Miglior media 30s | 374 Mbps | **400 Mbps** | **+6.9%** |
| Media 6×30s | — | 336 Mbps | comparabile a v4.3 150s (333) |

**Nuovo record assoluto: 548 Mbps** (Run 0, t=20s). Miglior run 30s (Run 1):
63% dei secondi sopra 400 Mbps (19/30s). Impatto GSO coerente con previsione +5-10%.

**Retransmit aumentati (+80% vs baseline)**: GSO emette burst a wire-speed →
overflow buffer NIC/switch → retransmit TCP self-inflicted.
Questo **conferma la necessità di Step 4.25** (kernel pacing `SO_TXTIME` + `sch_fq`)
per distanziare i pacchetti nel burst GSO e ridurre i retransmit.

**Metriche Prometheus (snapshot post-test)**:
- Client RX totale: 15.39 GB, TX: 553 MB (ACK TCP — coerente con 8 Mbps TX su Grafana)
- Asimmetria wan5/wan6: 36/64% dei dati (hashing 30 TCP flow su TUN multiqueue)
- FEC adattivo M=0, loss rate 0%, decrypt failures 0
- Server NACK sent: ~45K/sessione, retransmit received: 0

**Commit**: `f9c2607`.

### Step 4.25 — Kernel Pacing con `SO_TXTIME` + `sch_fq` ⬜ DA IMPLEMENTARE

**Obiettivo**: eliminare la burstiness software delegando il pacing al kernel.
Invece di emettere batch in burst e sperare che la NIC li distanzi, si assegna
a ogni pacchetto un **timestamp di partenza** (EDT — Earliest Departure Time)
e il qdisc `sch_fq` lo trattiene fino a quel momento.

**Perché kernel pacing dopo Step 4.12 (fallito)**:
- Step 4.12 usava `time.Sleep` in Go → granularità ~1ms, a 400 Mbps serve ~28µs
  inter-packet gap per 1402B shard. Impossibile con timer userspace.
- `SO_TXTIME` + `sch_fq` ha granularità nanosecondo nel kernel, è lo stesso
  approccio usato da implementazioni QUIC moderne (Google, Cloudflare)
- Il pacing riduce burstiness software → meno congestione → meno NACK
  self-inflicted → throughput netto più stabile
- **Validato da Step 4.24**: GSO ha aumentato i retransmit TCP del +80% (176/s vs
  98/s baseline) perché emette burst a wire-speed → overflow buffer → drop.
  Il kernel pacing è il complemento necessario per distanziare i pacchetti nel burst.

**Aspettativa calibrata**: la variabilità dominante è Starlink (beam handoff,
scheduling LEO), non software. Il pacing attacca solo la componente software.
Stima: media +5-15% (da ~374 a ~400-430 Mbps), non miracolo.

**Implementazione pianificata**:
1. Setup `sch_fq` su interfacce WAN: `tc qdisc replace dev enp7sX root fq`
2. Socket option: `setsockopt(fd, SOL_SOCKET, SO_TXTIME, ...)` con
   `clockid=CLOCK_MONOTONIC`, `flags=0` (no error reporting)
3. Per ogni `sendmsg`: ancillary `SCM_TXTIME` con timestamp calcolato:
   `txtime = max(now, last_txtime + inter_packet_gap)`
4. `inter_packet_gap` calcolato da target rate: `pkt_size * 8 / target_bps * 1e9` ns
5. Target rate iniziale = banda stimata per pipe (da telemetria Fase 5)
6. Flag `--pacing` per enable/disable runtime (default: enabled)
7. Fallback: se `SO_TXTIME` non supportato, emissione senza pacing

**Prerequisiti deploy**:
- `sch_fq` qdisc attivo sulle WAN (client + server VPS)
- Kernel ≥4.19 (supporto `SO_TXTIME`) — OK su Debian 12 (6.1) e Ubuntu 24.04 (6.8)

**Effort stimato**: ~1.5 giorni (syscall Cmsghdr manuali in Go + tc setup).

### Step 4.26 — Sliding Window FEC (XOR parity) ⬜ DA IMPLEMENTARE

**Obiettivo**: sostituire il FEC Reed-Solomon block-based (Step 4.8, heavyweight) con
un FEC minimal basato su XOR parity su finestra scorrevole. Copre il caso comune
(1 loss per finestra) con overhead minimo, lasciando NACK per loss multipli.

**Perché XOR parity invece di RS evoluto (Step 4.15)**:
- RS block-based richiede accumulo di K shard + padding → latenza di gruppo
- XOR su finestra scorrevole: parity = XOR degli ultimi N shard, emessa ogni N
- Overhead: 1/N (es. N=8 → 12.5%) vs RS K=6 M=2 (33%)
- Recovery: copre esattamente 1 loss su N. Per >1, NACK copre già
- Nessun padding: ogni shard mantiene la sua dimensione originale
- Compatibile con FEC adattivo M=0: parity calcolata ma emessa solo se M>0

**Perché è coerente con l'architettura**:
- NACK (Step 4.13) gestisce i casi rari (burst loss >1) con retransmit
- XOR copre il caso frequente (single loss, beam handoff) senza latenza
- Hybrid ARQ: FEC lightweight per le perdite comuni, ARQ per il resto
- Transizione trasparente: il receiver distingue shard data da shard parity
  tramite flag nel header

**Implementazione pianificata**:
1. Sender: ring di N shard, XOR running calcolato incrementalmente
2. Ogni N shard emessi, emetti 1 shard parity (XOR dei precedenti N)
3. Receiver: se manca 1 shard su N+1, XOR degli N presenti = shard mancante
4. Se mancano >1: NACK come oggi
5. Parametro `stripe_fec_window: 8` (default), configurabile 4-16
6. Contatori telemetria: `xorRecovered`, `xorEmitted`

**Effort stimato**: ~2 giorni (encoder XOR banale, complessità nel window tracking).

### Done criteria Fase 4c
- [x] UDP GSO implementato e testato (Step 4.24)
- [ ] Kernel pacing SO_TXTIME operativo (Step 4.25)
- [ ] Sliding window XOR FEC implementato (Step 4.26)
- [ ] **Media ≥ 400 Mbps su 30-60s, dual Starlink, tempo buono** — best run 400 Mbps raggiunto, media 6 run 336 (Starlink variabilità)
- [ ] Profiling CPU confronto con v4.3 baseline
- [x] Benchmark GSO 7 run: picco **548 Mbps** (+9.8%), best 30s **400 Mbps** (+6.9%), retransmit +80% → valida Step 4.25

---

## Fase 5 — Metriche strutturate e Osservabilità ✅ COMPLETATA

**Obiettivo (§20 PDF)**: metriche machine-readable per O&M e portale cliente.

> "O&M with more visibility and control: More KPIs"

### Valutazione strategica (2026-03-13)

Con v4.2 il throughput è al tetto Starlink (499 Mbps picco, 374 media) —
il software non è più il bottleneck. Le ottimizzazioni residue di protocollo
(Sliding Window FEC, UDP GRO) darebbero guadagni marginali.

| Opzione residua | Impatto throughput | Impatto piattaforma | Effort | Verdetto |
|---|---|---|---|---|
| **Sliding Window FEC** (4.15) | Medio: recovery burst loss, ma M=0 adattivo raramente attiva FEC | Basso | 2-3 giorni | **Evoluto in Step 4.26 (XOR parity)** |
| **UDP GRO** (4.22) | Marginale: RX = 5.2% CPU | Nessuno | 1 giorno | **Scartare** |
| **Metriche + Osservabilità** (Fase 5) | Nessuno diretto | **Altissimo**: prerequisito AI/ML, debug produzione, SLA cliente | 3-5 giorni (MVP) | **✅ COMPLETATA** |
| **Stabilizzazione data plane** (Fase 4c) | Alto: picco→media | **Alto**: prerequisito per SLA reale | 4-5 giorni | **FARE ORA** |
| **AI/ML feedback loop** (Fase 6) | Potenzialmente alto (tuning dinamico) | Alto: differenziante competitivo | 2+ settimane | **Dopo Fase 4c** |

**Decisione (aggiornata 2026-03-15)**: Fase 4c Stabilizzazione data plane come prossimo step.
Il picco 499 Mbps dimostra che capacità e codice ci sono. Il gap picco→media (374 Mbps)
è causato da burstiness software e overhead FEC, non dal link. Tre interventi mirati
(GSO, kernel pacing, XOR FEC) per portare la media stabile sopra 400 Mbps.

### Architettura a 3 Layer

```
┌─────────────────────────────────────────────────────────┐
│  Layer 3: Consumer (futuro)                             │
│  ┌───────────┐  ┌───────────┐  ┌─────────────────────┐  │
│  │  Grafana  │  │ Alerting  │  │  AI/ML Engine       │  │
│  │ Dashboard │  │ (rules)   │  │  (Quality on Demand)│  │
│  └─────┬─────┘  └─────┬─────┘  └──────────┬──────────┘  │
│        │              │                   │             │
├────────┼──────────────┼───────────────────┼─────────────┤
│  Layer 2: Export                                        │
│  ┌─────────────────────────────────────────────────┐    │
│  │  GET /metrics  (Prometheus text format)         │    │
│  │  GET /api/v1/stats  (JSON, per portale cliente) │    │
│  └─────────────────────────────────────────────────┘    │
│                                                         │
├─────────────────────────────────────────────────────────┤
│  Layer 1: Collection (nello stripe engine)              │
│  ┌──────────────────────────────────────────────────┐   │
│  │  atomic counters + ring buffer per-session       │   │
│  │  • tx_bytes, rx_bytes, tx_pkts, rx_pkts          │   │
│  │  • rtt_min, rtt_avg, rtt_max (EMA)               │   │
│  │  • loss_rate (sliding window 10s)                │   │
│  │  • fec_encoded, fec_recovered, arq_nack_sent     │   │
│  │  • retransmit_count, dup_count                   │   │
│  │  • session_uptime, reconnections                 │   │
│  │  • per-pipe: throughput, rtt, loss               │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### Piano implementativo

**Layer 1 — Collection** ✅ COMPLETATO (v4.2, commit `9cfebdd`):
- Contatori atomici (`sync/atomic`) nel hot path — zero-alloc, zero-lock
- Per-session (server): tx/rx bytes/pkts, FEC encode/recover, ARQ nack/retransmit, dup, loss rate, decrypt failures
- Per-path (client): tx/rx pkts, stripe tx/rx bytes/pkts, stripe FEC recovered, alive status
- Globale: uptime, totali tx/rx aggregati
- Nessun impatto performance misurabile (solo `atomic.Add`)

**Layer 2 — Export** ✅ COMPLETATO (v4.2, commit `9cfebdd`):
- Endpoint HTTP `/metrics` (Prometheus text exposition format) su `<tunnel_ip>:9090`
- Endpoint HTTP `/api/v1/stats` (JSON strutturato, per portale cliente)
- Server metriche dedicato bound all'IP del tunnel (non esposto a Internet)
- Scrape-ready per Prometheus/Grafana — operativo su tutte le istanze attive
- Configurazione YAML: `metrics_listen: auto`
- Fix: `startMetricsServer()` spostato in `main()` per coprire tutti i 4 code path

> **Documentazione completa**: vedi `docs/METRICS.md` per catalogo metriche,
> struttura JSON, label Prometheus, query PromQL e pannelli Grafana suggeriti.

### Fase 5.2 — Stack di monitoraggio Prometheus + Grafana ✅ COMPLETATA (2025-07-17)

**Obiettivo**: deployment di Prometheus e Grafana su container LXC Proxmox
per raccolta, storicizzazione e visualizzazione delle metriche MPQUIC.

**Architettura target**:
```
┌─────────────────────────────────────────────────────────────┐
│  Proxmox Host                                               │
│                                                             │
│  ┌──────────────────┐    ┌──────────────────────────────┐   │
│  │ LXC Prometheus   │    │  LXC Grafana                 │   │
│  │ (CT 201)         │    │  (CT 202)                    │   │
│  │                  │    │                              │   │
│  │ prometheus:9091  │◄───│  grafana:3000                │   │
│  │                  │    │  datasource: prometheus:9091 │   │
│  │ scrape targets:  │    │  dashboard: MPQUIC Overview  │   │
│  │  10.200.x.y:9090 │    │                              │   │
│  └──────────────────┘    └──────────────────────────────┘   │
│         │                                                   │
│         │  scrape ogni 15s via route tunnel                  │
│         ▼                                                   │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  VM 200 (Client MPQUIC)                              │   │
│  │  10.200.17.1:9090  (mp1 client)                      │   │
│  │  10.200.14.1:9090  (cr4 client)                      │   │
│  │  10.200.15.1:9090  (cr5 client)                      │   │
│  │  10.200.16.1:9090  (cr6 client)                      │   │
│  │  10.200.4.1:9090   (mpq4 client)                     │   │
│  │  10.200.5.1:9090   (mpq5 client)                     │   │
│  │  10.200.6.1:9090   (mpq6 client)                     │   │
│  └──────────────────────────────────────────────────────┘   │
│         ▲                                                   │
│         │  scrape via tunnel route                           │
│         │                                                   │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  VPS Server (172.238.232.223)                        │   │
│  │  10.200.17.254:9090  (mp1 server)                    │   │
│  │  ... altri server endpoint                           │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

**Piano deployment** — tutti completati ✅:
1. ✅ Creare LXC Prometheus (CT 201) su Proxmox — Debian 12, 1 vCPU, 512 MB RAM, 8 GB disk
2. ✅ Creare LXC Grafana (CT 202) su Proxmox — Debian 12, 1 vCPU, 512 MB RAM, 8 GB disk
3. ✅ Configurare routing: containers raggiungono subnet 10.200.x.0/24 via VM 200
4. ✅ Installare e configurare Prometheus con scrape targets MPQUIC (tutti: mp1, cr4-cr5, mpq4-mpq6)
5. ✅ Installare Grafana 12.4.1, collegare datasource Prometheus
6. ✅ Creare dashboard "MPQUIC Overview" (uid: `adsnpmk`) con pannelli:
   - Overview: Uptime, Sessioni attive, Path attivi, TX totale, RX totale
   - Throughput: bps per sessione e per path
   - Qualità: FEC recovery rate, ARQ NACK/retransmit, loss rate, decrypt failures
   - Infrastruttura: Pipe per sessione (gauge corretto, non cumulativo)
7. ⬜ Configurare alerting rules (loss > soglia, tunnel down, decrypt failures) — rimandato a Fase 6

**Bug risolti durante deployment**:
- Fix `mpquic_session_pipes`: era contatore cumulativo (`sess.registered`), corretto in gauge che conta entry non-nil in `sess.pipes`
- Fix pannelli stat vuoti: aggiunto `"instant": true` + aggregazione `max by (instance_name, job)`
- Abilitato `metrics_listen: auto` nei template `.yaml.tpl` per mpq4, mpq5, mpq6
- Dashboard gestito via API (provisioning file disabilitato per evitare conflitti UI/file)
- Fix `metrics_listen: auto` mancante nei config repo `{4,5,6}.yaml` client e server (commit `af14a2d`) — i single-link tunnel non esponevano `:9090`
- Dashboard v8: regex `cr.*` → `cr[456]`, `br.*|bk.*` → `br[456]`, `df.*` → `df[456]`, `mpq.*` → `mpq[456]` per evitare match su time series stale (commit `c07b825`)
- Dashboard v8: Uptime filtrato a `job="mpquic-client"` per eliminare duplicato mp1 client/server (commit `a217474`)
- Dashboard v8: TX/RX totale come `sum()` aggregato singolo (era 18 valori individuali)

**Configurazione Prometheus** (`deploy/monitoring/prometheus/prometheus.yml`):
- Scrape interval: 5s (ridotto da 15s per reattività dashboard)
- Job `mpquic-server`: mp1 + mt4, mt5, mt6 (4 target)
- Job `mpquic-client`: mp1, cr4-cr6, br4-br6, df4-df6, mpq4-mpq6 (13 target)
- Totale: **18 target** (incl. prometheus self-monitoring), **17/18 UP** (mt4 DOWN lato VPS)

**Layer 3 — Consumer** (futuro, Fase 6):
- Grafana Dashboard: ✅ operativo (MPQUIC Overview, uid `adsnpmk`)
- Alerting rules: ⬜ da implementare con Fase 6
- AI/ML Engine: legge telemetria → produce policy → applica via Control API
- "Quality on Demand" come contratto API formalizzato

---

## Fase 6 — AI/ML-Ready (Quality on Demand) ⬜ NON INIZIATA

**Obiettivo (Step 5 PDF)**: layer AI/ML che adatta le policy QoS in base a telemetria real-time.

> "The characteristics of the tunnel can be adapted based on decisions coming from an AI/ML layer"

**Prerequisiti**: Fase 4c completata (data plane stabile ≥400 Mbps media),
Fase 5 completata (metriche operative per feedback loop).

### Steps
1. API bidirezionale: AI legge telemetria → produce policy → applica via Control API
2. "Quality on Demand" come contratto API formalizzato
3. Feature store: storico metriche per training modelli
4. PoC: rule-based decision engine (soglie RTT/loss → switch policy)
5. Evoluzione: modello ML per predizione qualità canale LEO

---

## Infrastruttura (2026-03-01)

### Client VM (VMID 200, Debian 12)
| Interfaccia | Ruolo  | IP               | Stato |
|-------------|------- |----------------  |-------|
| enp6s18     | MGMT1  | 10.10.11.100/24  | ✅ |
| enp6s19     | MGMT2  | 10.10.10.100/24  | ✅ |
| enp6s20-23  | LAN1-4 | 172.16.{1-4}.1/30| ✅ |
| enp7s1-2    | LAN5-6 | 172.16.{5-6}.1/30| ✅ |
| enp7s3-5    | WAN1-3 | — | No modem     |
| enp7s6      | WAN4   | 192.168.1.100    | ✅ mpq4 ~108ms |
| enp7s7      | WAN5   | 10.150.19.95     | ✅ mpq5 ~13ms |
| enp7s8      | WAN6   | 100.64.86.226    | ✅ mpq6 ~34ms |

### Server VPS (Ubuntu 24.04, 172.238.232.223)
- 6 listener QUIC (45001-45006)
- 1 listener multipath QUIC+stripe (45017/46017) per mp1
- NFT: UDP 45001-45006 + 45017 + 46017 accept
- Route di ritorno su mpq1-mpq6 e mp1
- Stripe sessions: 2 (wan5, wan6) × 12 pipe = 24 pipe totali

### Istanze operative (2026-03-15)
| Istanza | Tipo | WAN | Trasporto | Throughput |
|---------|------|-----|-----------|------------|
| mpq1-3 | single-link | WAN1-3 | QUIC | — (no modem) |
| mpq4-6 | single-link | WAN4-6 | QUIC reliable | ~50 Mbps |
| cr4/br4/df4 | multi-tunnel | WAN4 | QUIC reliable | ~50 Mbps (isolato) |
| cr5/br5/df5 | multi-tunnel | WAN5 | QUIC reliable | ~50 Mbps (isolato) |
| cr6/br6/df6 | multi-tunnel | WAN6 | QUIC reliable | ~50 Mbps (isolato) |
| **mp1** | **multipath 2 WAN** | **WAN5+6** | **UDP stripe + FEC adaptive + ARQ** | **374 Mbps (picco 499)** |

---

## Comandi verifica rapida

```bash
# Server VPS
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
ss -lunp | egrep '4500[1-6]'

# Client
for i in 4 5 6; do
  printf "mpq%d: " "$i"
  ping -I "10.200.${i}.1" -c 2 -W 2 "10.200.${i}.2" 2>&1 | tail -1
done
```
