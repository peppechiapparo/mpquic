# Roadmap implementazione MPQUIC

*Allineata al documento "QUIC over Starlink TSPZ" — aggiornata 2026-02-28*

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
| **2** | Traffico distribuito per applicazione, non per pacchetto | Multi-tunnel per link | **🔄 IN PROGRESS** (2.1-2.4 ✅) |
| **3** | BBRv3 / Wave congestion control | CC per tunnel | **⬜ NOT STARTED** |
| **4** | Bonding, Backup, Duplicazione | Multi-path per tunnel | **⬜ NOT STARTED** |
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

## Fase 3 — BBRv3 Congestion Control ⬜ NON INIZIATA

**Obiettivo (Step 3 PDF)**: sostituire Cubic (default quic-go) con BBRv3 per ottimizzare
throughput su canali LEO con alta variabilità RTT e loss non da congestione.

> "Cubic: slow and decreasing. BBRv3 is relatively fast."
> "Congestion control of Wave was created for high packet loss environments"

### Motivazione (dal PDF §10-12)
- **Cubic**: lento a crescere, interpreta OGNI loss come congestione → rallenta drasticamente
- **BBRv3**: model-based CC, stima bandwidth e RTT, non rallenta su loss isolati
- **Wave** (futuro): CC ottimizzato specificamente per canali LEO altamente variabili

### Stato tecnico
- quic-go v0.48.2 usa Cubic come default, NESSUNA API CC pubblica
- quic-go v0.59+ (richiede Go 1.24+) potrebbe avere API CC estensibili
- Nota: QUIC DATAGRAM sfrutta CC solo per flow control, non per packet-level reliability

### Opzioni di implementazione
1. **Upgrade quic-go + Go** a versioni più recenti
2. **Fork quic-go** con BBR sender abilitato
3. **CC applicativo** sopra QUIC datagram: pacing + bandwidth estimation a livello tunnel
4. **Diverso CC per classe**: critical usa pacing conservativo, bulk usa BBR aggressivo

### Steps
1. Upgrade Go 1.22 → 1.24, quic-go v0.48 → v0.59+
2. Valutare API CC in quic-go recente
3. PoC: BBR vs Cubic su canale Starlink reale, singolo tunnel
4. A/B test con metriche comparative (throughput, recovery time)
5. Parametrizzare in config YAML (`congestion_control: cubic|bbr`)
6. Benchmark su profili loss/jitter simulati (netem) e reali
7. Stretch: prototipo "Wave-like" (pacing su bandwidth estimation, ignora loss isolati)

---

## Fase 4 — Multi-path per tunnel: Bonding, Backup, Duplicazione ⬜ NON INIZIATA

**Obiettivo (Step 4 PDF)**: un singolo tunnel "applicativo" può usare N link fisici per
resilienza e aggregazione bandwidth.

> "New services: Bonding, Backup, Duplications"

### Prerequisiti
- Fase 2 completata (multi-tunnel funzionante)
- quic-go con supporto Multipath QUIC (RFC 9443) — oppure implementazione applicativa

### Servizi target

| Servizio | Meccanismo | Caso d'uso |
|----------|-----------|------------|
| **Bonding** | Un tunnel usa 2+ WAN, pacchetti distribuiti round-robin/weighted | Max throughput |
| **Active Backup** | Tunnel su WAN primaria, failover su WAN secondaria | Low latency + resilienza |
| **Duplicazione** | Pacchetti critico inviati su 2+ WAN simultaneamente | Zero-loss per VoIP/controllo |

### Opzioni implementative
1. **QUIC Multipath nativo** (RFC 9443): una connessione QUIC, N path di rete
   - Pro: standard IETF, gestione path integrata nel protocollo
   - Contro: richiede quic-go recente + Go 1.24+
2. **Applicativo**: N connessioni QUIC (una per WAN), aggregazione nella nostra `multipathConn`
   - Pro: funziona con quic-go attuale, più controllo
   - Contro: overhead applicativo, duplicazione gestita manualmente (già implementata nel codice)
3. **Ibrido**: multipath nativo dove possibile, fallback applicativo

### Il codice "multipath" esistente
Il codice `multipathConn` già presente implementa Opzione 2 a livello applicativo:
- N connessioni QUIC su N WAN diverse → singola TUN
- Scheduler path-aware con priority/weight/failover
- Duplicazione per classi critiche
- Reconnect con backoff e recovery automatica

**Gap**: non è mai stato testato sulla infra reale e ha un problema di routing server-side
(ogni path punta a una porta server diversa con TUN diversa). Va riadattato per usare
server multi-connessione (Fase 2) come base.

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

## Infrastruttura (2026-02-28)

### Client VM (VMID 200, Debian 12)
| Interfaccia | Ruolo | IP | Stato |
|-------------|-------|----|-------|
| enp6s18 | MGMT1 | 10.10.11.100/24 | ✅ |
| enp6s19 | MGMT2 | 10.10.10.100/24 | ✅ |
| enp6s20-23 | LAN1-4 | 172.16.{1-4}.1/30 | ✅ |
| enp7s1-2 | LAN5-6 | 172.16.{5-6}.1/30 | ✅ |
| enp7s3-5 | WAN1-3 | — | No modem |
| enp7s6 | WAN4 | 192.168.1.100 | ✅ mpq4 ~108ms |
| enp7s7 | WAN5 | 10.150.19.95 | ✅ mpq5 ~13ms |
| enp7s8 | WAN6 | 100.64.86.226 | ✅ mpq6 ~34ms |

### Server VPS (Ubuntu 24.04, 172.238.232.223)
- 6 listener QUIC (45001-45006)
- NFT: UDP 45001-45006 accept
- Route di ritorno su mpq1-mpq6

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
