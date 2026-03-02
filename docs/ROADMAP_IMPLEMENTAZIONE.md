# Roadmap implementazione MPQUIC

*Allineata al documento "QUIC over Starlink TSPZ" — aggiornata 2026-03-01*

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

#### Fase 1 — Matrice A/B sicurezza Stripe (IN CORSO)

**Scopo**: verificare in modo misurabile se MAC/rekey introducono regressione
di throughput/stabilità rispetto al baseline storico.

**Test case (stesse condizioni di rete e iperf):**
- `T1`: `auth=off`, `rekey=off` (baseline riferimento)
- `T2`: `auth=on`, `rekey=off` (costo MAC puro)
- `T3`: `auth=on`, `rekey=600s` (config target attuale)
- `T4`: `auth=on`, `rekey=30s` (stress rekey)

**Metriche obbligatorie per ogni test:**
- Throughput iperf3 (`-R` e upload) + retry/retransmission
- Telemetria path/class (`tx_pkts`, `rx_pkts`, `tx_err`, `rx_err`, `fails`)
- Contatori sicurezza (`auth_fail`, `replay_drop`) se disponibili
- Eventi sessione (`path recovered/down`, reconnect, timeout)

**Criterio decisionale Fase 1:**
- Se `T2/T3/T4` degradano nettamente vs `T1`, priorità a ottimizzazione path
  sicurezza (MAC verify/rekey).
- Se degrado è presente già in `T1`, priorità a scheduler/dispatch/FEC contention.

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

### Implementazione (stripe.go — 1320 righe)

| Componente | File | Righe | Descrizione |
|-----------|------|-------|-------------|
| Wire protocol | stripe.go | ~100 | Header 16 byte, encode/decode, costanti |
| FEC group | stripe.go | ~50 | Accumulatore shard con stato present/received |
| Client conn | stripe.go | ~350 | N UDP socket, FEC encode TX, decode RX, keepalive |
| Server listener | stripe.go | ~400 | UDP listener, session management, GC |
| Server DC | stripe.go | ~150 | `datagramConn` per return-path server→client |
| Helpers | stripe.go | ~30 | parseTUNIP, ipToUint32 |
| Unit tests | stripe_test.go | 188 | Header, FEC group, helpers, 9 test |
| Main integration | main.go | ~100 | Config, registerStripe, path init, server startup |

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

### Step 4.10 — Stripe Security Hardening 🔄 IN CORSO (priorità alta)

**Perché ora**: con `transport: stripe` il canale non eredita automaticamente la
sicurezza TLS di QUIC; il focus diventa autenticazione + confidenzialità + anti-replay
del protocollo UDP stripe.

**Checklist implementativa**:
1. **Threat model minimale** (spoofing, replay, injection, hijack sessione)
2. **Auth messaggi stripe** con MAC per-packet (chiave condivisa per sessione) ✅ baseline implementata (`stripe_auth_key`)
3. **Anti-replay window** su `(session, groupSeq, shardIdx)` con finestra scorrevole ✅ baseline implementata su DATA/PARITY
4. **Rotazione chiavi** (rekey periodico) + scadenza sessione forzata ✅ baseline rekey per-epoch implementata
5. **Opzione cifratura payload stripe** (AEAD) senza rompere FEC
6. **Metriche sicurezza** (drop auth fail, replay drop, rekey count) in Fase 5 ✅ `auth_fail`, `replay_drop` baseline loggate lato stripe server
7. **Test dedicati**: unit + netem + packet injection test in lab

### Step 4.11 — Stripe Confidentiality (TLS-like target) ⬜ PROPOSTO

**Perché lo vogliamo fare**:
- L'attuale MAC + anti-replay protegge integrità/autenticazione, ma non la
  **confidenzialità** del payload stripe.
- Su path QUIC la sicurezza è già TLS 1.3; su path stripe vogliamo un livello
  il più possibile analogo per ridurre gap di rischio operativo.

**Decisione tecnica**:
- **Opzione A (preferita se fattibile)**: handshake con proprietà TLS 1.3
  equivalenti (scambio chiavi effimere, key confirmation, PFS) e cifratura AEAD.
- **Opzione B (baseline realistica)**: AEAD AES-GCM (o ChaCha20-Poly1305) con
  nonce/sequence robusti, key schedule di sessione e rekey controllato.

**Nota pratica**:
- Integrare direttamente un handshake TLS 1.3 pieno su protocollo UDP custom
  stripe non è banale senza introdurre uno stack dedicato; per questo la
  baseline operativa resta AEAD + key schedule, mantenendo compatibilità FEC.

**Done criteria Step 4.11**:
- [ ] Payload stripe cifrato end-to-end (AEAD)
- [ ] Nonce univoco per shard e protezione riuso nonce
- [ ] Key schedule di sessione con rekey periodico e key confirmation
- [ ] Overhead prestazionale entro target (degradazione ≤10% vs stripe attuale)

**Done criteria Step 4.10**:
- [x] Pacchetti stripe non autenticati rifiutati dal server (quando `stripe_auth_key` è configurata)
- [x] Replay di shard vecchi rifiutato (window su DATA/PARITY)
- [x] Session rekey baseline per-epoch implementata (chiavi derivate da `stripe_auth_key`)
- [ ] Benchmark throughput degradazione ≤10% rispetto stripe attuale

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
- Stripe sessions: 3 (wan4, wan5, wan6) × 4 pipe = 12 pipe totali

### Istanze operative (2026-03-01)
| Istanza | Tipo | WAN | Trasporto | Throughput |
|---------|------|-----|-----------|------------|
| mpq1-3 | single-link | WAN1-3 | QUIC | — (no modem) |
| mpq4-6 | single-link | WAN4-6 | QUIC reliable | ~50 Mbps |
| cr5/df5/bk5 | multi-tunnel | WAN5 | QUIC reliable | ~50 Mbps (isolato) |
| **mp1** | **multipath 3 WAN** | **WAN4+5+6** | **UDP stripe + FEC** | **303 Mbps** |

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
