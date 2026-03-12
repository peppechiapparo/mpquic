# Roadmap implementazione MPQUIC

*Allineata al documento "QUIC over Starlink TSPZ" — aggiornata 2026-03-03*

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
| **2** | Traffico distribuito per applicazione, non per pacchetto | Multi-tunnel per link | **🔄 IN PROGRESS** (2.1-2.4 ✅, 2.5 in pausa) |
| **3** | BBR + Reliable Transport | CC per tunnel + transport mode | **✅ DONE** (BBRv1, reliable streams, benchmarkato) |
| **4** | Bonding, Backup, Duplicazione | Multi-path per tunnel | **✅ DONE** |
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
                  │                                          │ porta 45010         │
                  ├─ VoIP (UDP 5060) ──▶ tun-critical ─┐     │                     │
                  │                     10.200.10.1/24  │     │  conn_1 ◄──────────┤
                  ├─ HTTPS (TCP 443) ──▶ tun-default  ─┼QUIC─┤  conn_2 (same port)│──▶ tun-mt1
                  │                     10.200.10.5/24  │WAN5 │  conn_3            │   10.200.10.0/24
                  └─ Bulk (TCP 5001) ──▶ tun-bulk     ─┘     │                     │
                                        10.200.10.9/24       │  routing table:     │──▶ NAT ──▶ Internet
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

### Step 2.3 — Deploy e test su infra reale ✅ COMPLETATO (2026-02-28)
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

**Metodologia**: netem loss injection su singola TUN (br2), misura su tutte e 3 le TUN
(cr2/br2/df2) dello stesso link WAN5. Binding esplicito per-device (`-B IP%dev`).
iperf3 3.12 → VPS iperf3 server (porta 5201).

#### Risultati RTT (ping, 20 pacchetti per tunnel)

| Tunnel | Baseline RTT | Baseline Loss | 10% netem br2 | 30% netem br2 |
|--------|-------------|---------------|----------------|----------------|
| cr2 | 13.0 ms | 0% | **0% loss** | **0% loss** |
| br2 | 13.2 ms | 0% | 15% loss | 35% loss |
| df2 | 13.1 ms | 0% | **0% loss** | **0% loss** |

#### Risultati Throughput (iperf3, 5s per tunnel, device-bound)

| Tunnel | Baseline (Mbps) | 10% loss br2 (Mbps) | 30% loss br2 (Mbps) |
|--------|----------------|---------------------|---------------------|
| cr2 (critical) | 50.2 | **50.2** (±0%) | **50.2** (±0%) |
| br2 (bulk) | 48.1 | **2.3** (−95%) | **0.4** (−99%) |
| df2 (default) | 50.0 | **50.2** (±0%) | **49.8** (±0%) |

**Conclusione**: isolamento perfetto — packet loss su un tunnel ha ZERO impatto
su latenza e throughput degli altri tunnel, anche sotto loss del 30%.
I tunnel cr2 e df2 mantengono throughput pieno (~50 Mbps) e 0% loss
mentre br2 crolla a 0.4 Mbps. Questo dimostra il valore architetturale
della separazione per classe di traffico.

**Nota tecnica**: i 3 tunnel condividono la stessa subnet /24. Il kernel Linux
usa la prima route (cr2). Per test isolati è necessario il binding esplicito
`iperf3 -B IP%dev`. In produzione il VLAN classifier instrada correttamente.

### Step 2.5 — Generalizzazione: 3 WAN × 3 classi = 9 tunnel con VLAN ⬜

**Architettura**: ogni WAN attiva (SL4/SL5/SL6) ottiene 3 tunnel di classe
(critical/bulk/default). La classificazione avviene lato OpenWrt tramite VLAN tagging.
Ogni VLAN arriva su un sub-interface dedicato del client VM; il classifier
instrada nel tunnel corretto in base alla VLAN di origine.

**Schema VLAN → Tunnel**:

| LAN (transit) | VLAN | Classe | Tunnel | WAN uscita |
|---------------|------|--------|--------|------------|
| LAN1 (enp6s20) | 11 | critical | cr1 | WAN4 (SL4) |
| LAN1 (enp6s20) | 12 | bulk | br1 | WAN4 (SL4) |
| LAN1 (enp6s20) | 13 | default | df1 | WAN4 (SL4) |
| LAN2 (enp6s21) | 21 | critical | cr2 | WAN5 (SL5) |
| LAN2 (enp6s21) | 22 | bulk | br2 | WAN5 (SL5) |
| LAN2 (enp6s21) | 23 | default | df2 | WAN5 (SL5) |
| LAN3 (enp6s22) | 31 | critical | cr3 | WAN6 (SL6) |
| LAN3 (enp6s22) | 32 | bulk | br3 | WAN6 (SL6) |
| LAN3 (enp6s22) | 33 | default | df3 | WAN6 (SL6) |

**Flusso traffico**:
```
OpenWrt → VLAN 21 (critical LAN2) → enp6s21.21 → ip rule → cr2 TUN → WAN5 → VPS:45011
OpenWrt → VLAN 22 (bulk LAN2)     → enp6s21.22 → ip rule → br2 TUN → WAN5 → VPS:45011
OpenWrt → VLAN 23 (default LAN2)  → enp6s21.23 → ip rule → df2 TUN → WAN5 → VPS:45011
```

**Server layout**: 3 porte, ciascuna multi-conn (3 classi):
- 45010: WAN4 → cr1 + br1 + df1 (TUN mt4, subnet 10.200.10.0/24)
- 45011: WAN5 → cr2 + br2 + df2 (TUN mt5, subnet 10.200.11.0/24)
- 45012: WAN6 → cr3 + br3 + df3 (TUN mt6, subnet 10.200.12.0/24)

**Client VM**: VLAN sub-interfaces su ogni LAN trunk + classifier per-VLAN:
- `enp6s20.11` → routing table → default via cr1
- `enp6s20.12` → routing table → default via br1
- `enp6s20.13` → routing table → default via df1
- (idem per LAN2 → .21/.22/.23, LAN3 → .31/.32/.33)

**Lato OpenWrt**: piena libertà di routing — basta taggare il traffico sulla VLAN
giusta (mwan3, firewall zone, DSCP→VLAN map, ecc.)

**Passi implementativi**:
1. Creare VLAN sub-interfaces su client VM (systemd-networkd .netdev + .network)
2. Creare 9 configurazioni client YAML (cr1/br1/df1, cr2/br2/df2, cr3/br3/df3)
3. Creare 3 configurazioni server YAML (mt4, mt5, mt6 multi-conn)
4. Creare classifier per-VLAN (evoluzione di mpquic-mt-classifier.sh)
5. Deploy server: 3 istanze multi-conn su porte 45010-45012
6. Deploy client: 9 istanze + VLAN interfaces + classifier
7. Configurare OpenWrt: VLAN trunking + mwan3 policy
8. Test end-to-end: 9 flussi indipendenti verso internet

### Done criteria Fase 2
- [x] Server accetta N connessioni concorrenti sulla stessa porta
- [x] 3 classi traffico (critical/default/bulk) su TUN separate
- [x] nftables classifier funzionante con routing per-classe
- [x] Traffico applicativo smistato correttamente (verificato con tcpdump)
- [ ] Generalizzazione 3 WAN × 3 classi = 9 tunnel con VLAN
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
| cr3 | Cubic | 15.1 Mbps | 0.5 Mbps (−97%) |
| br3 | BBR | 14.5 Mbps | 0.5 Mbps (−97%) |
| df3 | Cubic | 14.9 Mbps | 0.9 Mbps (−94%) |

**Reliable mode** (dopo il fix):
| Tunnel | CC | Baseline | 10% loss | 30% loss |
|--------|-----|----------|----------|----------|
| cr3 | Cubic | 45.2 Mbps | 41.9 Mbps (−7%) | 15.5 Mbps (−66%) |
| br3 | **BBR** | 47.4 Mbps | 28.5 Mbps (−40%) | **26.1 Mbps (−45%)** |
| df3 | Cubic | 55.8 Mbps | 39.7 Mbps (−29%) | 13.6 Mbps (−76%) |

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
│  TUN reader ──▶ FEC encoder (K data + M parity shards)             │
│              ──▶ round-robin across N UDP sockets                   │
│              ──▶ each socket = independent Starlink session          │
│                                                                     │
│  N UDP sockets ──▶ FEC decoder (reconstruct if K of K+M received)  │
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
│  wan5 (Starlink enp7s7) ─── transport: stripe ─── 4 UDP pipes │
│  wan6 (Starlink enp7s8) ─── transport: stripe ─── 4 UDP pipes │
│  wan4 (Fibra enp7s6)    ─── transport: quic   ─── 1 QUIC conn │
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

| Config              | Throughput medio | Retransmit medi | Note                                    |
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

| Config                        | Throughput medio | Retransmit medi | Note                                         |
|-------------------------------|------------------|-----------------|----------------------------------------------|
| Baseline (no ARQ, P8, 10 pipe)| **239 Mbps**     | ~1.000          | Riferimento M=0                              |
| ARQ v1 (P8, 10 pipe)         | **274 Mbps**     | ~3.199          | Step 4.13, +14.6% vs baseline               |
| **ARQ v2 + dedup (P20, 12 pipe)** | **330 Mbps** | ~4.110          | **+38% vs baseline, +20% vs ARQ v1, picco 384 Mbps** |
| ARQ v2 + batch I/O (P20, 12 pipe) | **333 Mbps** | n/d             | Step 4.16, **neutro** (+1%, entro rumore), picco 390 |

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

**Commit**: da verificare con profiling post-deploy.

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
2. **Step 4.20 — Batch TX (sendmmsg)** → attacca il 45% del tempo CPU (priorità massima)
3. ~~**Step 4.21 — tunWriter batch-drain + reduce mutex**~~ ✅ Completato — batch-drain rxCh, touchPath 1/batch
4. **Step 4.22 — UDP GRO** → deprioritizzato (RX path solo 5.2%)
5. **Step 4.15 — Sliding Window FEC** → se loss recovery diventa il fattore limitante

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
- [x] tunWriter batch-drain + reduce mutex (Step 4.21) — batch-drain rxCh, touchPath 1/batch
- [ ] UDP GRO per RX (Step 4.22) — deprioritizzato
- [ ] Sliding window FEC (Step 4.15)
- [ ] Throughput ≥ 400 Mbps su dual Starlink

---

## Fase 5 — Metriche strutturate e Osservabilità ⬜ NON INIZIATA

**Obiettivo (§20 PDF)**: metriche machine-readable per O&M e portale cliente.

> "O&M with more visibility and control: More KPIs"

### Steps
1. Endpoint `/metrics` Prometheus-compatible:
   - Per tunnel: RTT, loss rate, throughput, stato, errori, uptime
   - Per classe: packets/bytes tx/rx, duplicazioni
   - Globale: uptime, connessioni, riconnessioni
2. Persistenza locale metriche (ring buffer o TSV rotato)
3. Dashboard Grafana template
4. Allarmi configurabili (loss > soglia, tunnel down prolungato)
5. API REST per KPI portale cliente

---

## Fase 6 — AI/ML-Ready (Quality on Demand) ⬜ NON INIZIATA

**Obiettivo (Step 5 PDF)**: layer AI/ML che adatta le policy QoS in base a telemetria real-time.

> "The characteristics of the tunnel can be adapted based on decisions coming from an AI/ML layer"

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

### Istanze operative (2026-03-04)
| Istanza | Tipo | WAN | Trasporto | Throughput |
|---------|------|-----|-----------|------------|
| mpq1-3 | single-link | WAN1-3 | QUIC | — (no modem) |
| mpq4-6 | single-link | WAN4-6 | QUIC reliable | ~50 Mbps |
| cr5/df5/bk5 | multi-tunnel | WAN5 | QUIC reliable | ~50 Mbps (isolato) |
| **mp1** | **multipath 2 WAN** | **WAN5+6** | **UDP stripe + FEC adaptive + ARQ** | **330 Mbps (picco 384)** |

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
