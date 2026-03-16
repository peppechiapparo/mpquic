# Changelog implementazione (replicabile TBOX)

## 2026-03-16

### Step 4.25 — Kernel Pacing `SO_TXTIME` + `sch_fq` implementato, deployato e benchmarkato
- **Problema**: GSO (Step 4.24) emette burst a wire-speed → retransmit TCP +80%
  (176/s vs 98/s). Software pacer (`time.Sleep`) ha granularità ~1ms, inutile a
  400 Mbps che richiederebbe ~28µs inter-packet gap per shard da 1402B.
- **Soluzione**: delegare il pacing al kernel con `SO_TXTIME` + qdisc `sch_fq`:
  - Ogni pacchetto porta un **EDT (Earliest Departure Time)** in nanosecondo
  - `sch_fq` trattiene il pacchetto nella coda egress fino al timestamp EDT
  - Granularità nanosecondo vs millisecondo del software pacer
- **File nuovi**:
  - `stripe_txtime_linux.go` (155 LOC): probe, setup, SCM_TXTIME cmsg, `monoNowNs()`
  - `stripe_txtime_other.go` (17 LOC): stub non-Linux
  - `scripts/setup-fq-qdisc.sh`: installa `sch_fq` su WAN (auto-detect o manuale)
- **Modifiche core** (`stripe.go`):
  - Client: `txtimeEnabled`, `txtimeEDT[]`, `txtimeGapNs` per-pipe tracking
    - `gsoFlushPipeLocked()`: SCM_TXTIME appeso al OOB (compatibile con GSO cmsg)
    - `writePacedUDP()`: per pacchetti singoli (non-GSO)
    - Software pacer disabilitato automaticamente quando kernel pacing attivo
  - Server: `SO_TXTIME` sul listener socket, per-session EDT tracker
    - `txBatchAddLocked()`: ogni msg nel batch sendmmsg ha SCM_TXTIME individuale
    - OOB cleanup dopo flush per evitare cmsg stale
  - Log: `txtime=on/off` + `pacing=kernel@XMbps(gap=Yns)` nel messaggio ready
- **Requisiti deploy**: `sudo scripts/setup-fq-qdisc.sh` su client + server
- **Bug fix deploy**:
  - `18ac3ff`: nil pacer dereference in 6 siti → TX goroutine panic silenzioso
  - `3d2945e`: `SCM_TXTIME` costante errata (0x25→61) → sendmsg EINVAL silenzioso
- **Benchmark** (6 run × 30s, P30, -R, dual Starlink):
  - Media: **333 Mbps** (mediana 352), picco 491 Mbps
  - Confronto vs GSO only: media -0.9%, mediana +0.6%, retransmit +21%
  - Per-second CoV: 23.3% — variabilità Starlink domina
- **Decisione**: feature mantenuta attiva — stabilità canale migliorata, overhead nullo
- **Tag**: **v4.5** su commit `3d2945e`

### Fix Issue #1 — ARQ `retransmit received: 0` (bug contatore)
- **Root cause**: `addRetxReceived()` definita in `stripe_arq.go:268` ma **mai chiamata**
- **Fix**: quando `markReceived(seq)=true` e `seq < rxSeqHighest` → `addRetxReceived(1)`
  - Distingue retransmit utili (gap-filling) da duplicati reali (dup_filtered)
  - Applicato sia client che server side

### Step 4.24 — UDP GSO (`UDP_SEGMENT`) implementato
- **Scoperta**: il client TX non aveva alcun batching — ogni `SendDatagram` generava
  una `WriteToUDP` individuale (1 syscall per pacchetto). Il server aveva già `sendmmsg`.
- **Soluzione**: UDP Generic Segmentation Offload (GSO) sul client:
  - Per-pipe accumulation: shard criptati concatenati in buffer contiguo
  - `WriteMsgUDP` con ancillary `UDP_SEGMENT` → kernel split in datagrammi individuali
  - 1 syscall per pipe per batch (anziché N syscall per batch)
- **File nuovi**:
  - `stripe_gso_linux.go`: `stripeGSOProbe()`, `stripeGSOBuildOOB()`, `stripeGSOIsError()`
  - `stripe_gso_other.go`: stub per non-Linux
- **Modifiche core** (`stripe.go`):
  - `stripeClientConn`: campi `gsoEnabled`, `gsoDisabled` (atomic), `gsoBufs []gsoTxPipeBuf`
  - M=0 fast path: `gsoAccumLocked()` anziché `WriteToUDP()`
  - M>0 FEC path: `sendFECGroupLocked()` usa GSO per data + parity shards
  - `FlushTxBatch()` implementato su client → `drainSendCh` lo chiama via `txBatcher`
  - `flushTxGroup()` (FEC timer) chiama `gsoFlushAllLocked()` dopo encode
  - Fallback: se `WriteMsgUDP` ritorna EIO → `gsoDisabled=1`, resend individuale
- **Config**: `stripe_disable_gso: true` per A/B testing
- **Log**: `gso=on|off` aggiunto al log "stripe client ready"
- Build + tests + vet: ✅ passati

### Step 4.24 — Benchmark GSO (7 run: 1×60s + 6×30s, P30, -R, dual Starlink)
- **Nuovo record assoluto**: **548 Mbps** (picco per-second, +9.8% vs 499 v4.3)
- **Miglior run 30s**: **400 Mbps** (+6.9% vs baseline 374 Mbps)
- **Media 6×30s**: 336 Mbps (mediana 350, senza outlier 355)
- **Retransmit TCP**: +80% vs baseline (176/s vs 98/s) — burst GSO → buffer overflow
- **Variabilità Starlink**: range 238-548 Mbps (2.3×), domina rispetto a delta GSO
- **Metriche**: FEC adattivo M=0, loss rate 0%, decrypt fail 0, asimmetria wan5/wan6 36/64%
- **Conclusione**: GSO migliora throughput come previsto (+5-10%), retransmit confermano
  necessità Step 4.25 (kernel pacing) per domare i burst

## 2026-03-15

### Tag v4.3 — Monitoring stack completo
- **Tag**: `v4.3` su commit `1a87429` — baseline prima di Fase 4c
- Monitoring stack end-to-end operativo: Prometheus (17/18 UP) + Grafana dashboard v8
- Fase 5 (Metriche + Osservabilità) completata

### Roadmap: Fase 4c — Stabilizzazione data plane
- **Obiettivo**: trasformare picco 499 Mbps in media stabile ≥400 Mbps su 30-60s
- **Analisi**: il gap picco→media è causato da burstiness software e overhead FEC,
  non da limiti di link o codice
- **Piano** (3 step):
  - Step 4.24: UDP GSO (`UDP_SEGMENT`) — kernel segmentation, riduce overhead/pkt
  - Step 4.25: Kernel pacing `SO_TXTIME` + `sch_fq` — elimina burstiness software
  - Step 4.26: Sliding Window FEC (XOR parity, N=8) — recovery minimal senza RS overhead
- **Scartato**: `SO_REUSEPORT` + per-core sharding — 24 socket indipendenti già presenti,
  profiling non mostra lock contention, Go scheduler non supporta affinity statica

### Rename tunnel: numero = WAN
- **Convenzione**: il suffisso numerico del tunnel ora corrisponde alla WAN:
  - WAN4: cr4/br4/df4 (erano cr1/br1/df1)
  - WAN5: cr5/br5/df5 (erano cr2/br2/df2)
  - WAN6: cr6/br6/df6 (erano cr3/br3/df3)
- **Decommissione Step 2.3**: rimossi cr5/df5/bk5 (old, 10.200.10.x) e mt1 —
  la subnet 10.200.10.0/24 non è più in uso
- **Rinominati**: config client/server, rt_tables, prometheus, vlan-classifier,
  isolation-test, vps-routes, install_mpquic.sh, tutta la documentazione
- **Rimosso**: `scripts/mpquic-mt-classifier.sh` (Step 2.3 classifier obsoleto)
- **Dashboard Grafana**: nessun cambiamento necessario (query basate su regex)

### Fix routing bd1 + mp1 crash loop
- **Fix rt_tables**: aggiunto `200 bd1` in `/etc/iproute2/rt_tables` (mancante dopo
  la correzione del 14/03 che ha rinominato `120 bd1` → `120 mt_cr4`)
- **Fix 27-bd1.network**: corretto `Table=120` → `Table=200` — la VLAN 17 inviava
  il traffico alla tabella `mt_cr4` anziché `bd1`, causando uscita da enp6s18
  (main table) anziché dal tunnel mp1
- **Fix ip rule 1017**: aggiornata live `from 172.16.17.0/30 lookup bd1` (era mt_cr4)
- **Fix mp1 crash loop (450 restart)**: il servizio `mpquic@mp1` andava in crash per
  `ExecStartPost` che eseguiva `ip route replace ... table bd1` ma la tabella bd1
  non era definita in rt_tables → exit 255 → service killed
- **Repo**: aggiunti `deploy/networkd/bd1/` (26-vlan17.netdev, 27-bd1.network),
  `deploy/networkd/rt_tables`, `deploy/systemd/mpquic@mp1.service.d/bd1-route.conf`
- **Docs**: aggiornato ID tabella bd1 da 120 a 200 in NOTA_TECNICA e INSTALLAZIONE_TEST

### Fix `metrics_listen: auto` mancante in mpq4/5/6
- **Root cause**: i template `{4,5,6}.yaml` (single-link tunnel) non contenevano
  la direttiva `metrics_listen: auto` — presente solo nei class tunnel (cr/br/df)
- **Sintomo**: mpq4/5/6 non esponevano endpoint HTTP su `:9090`, Prometheus li segnava DOWN
- **Fix**: aggiunto `metrics_listen: auto` nei config client e server per 4.yaml, 5.yaml, 6.yaml
- **Deploy**: sed sui `.yaml.tpl` in `/etc/mpquic/instances/`, re-render, restart `mpquic@{4,5,6}`
- **Verifica**: `ss -tlnp` mostra listening su `:9090`, `curl` ritorna metriche Prometheus
- **Prometheus**: 17/18 target UP (mt4 DOWN — problema lato VPS, non correlato)
- **Commit**: `af14a2d`

### Dashboard Grafana v8: fix leggibilità e regex stale
- **Stato Tunnel**: regex `cr.*` → `cr[456]`, `br.*|bk.*` → `br[456]`, `df.*` → `df[456]`,
  `mpq.*` → `mpq[456]` — impedisce match su vecchie time series in Prometheus TSDB
- **Server**: query `up{job="mpquic-server"}` → filtro esplicito `instance_name=~"mp1|mt[456]"`
- **Uptime**: filtrato a `job="mpquic-client"`, `instance_name=~"mp1|mpq[456]"` (4 valori),
  layout verticale. Fix duplicato mp1 (appariva sia come client che server senza filtro job)
- **TX/RX totale**: da 18 valori individuali a singolo `sum()` aggregato
- **Commit**: `c07b825` (fix completo), `a217474` (fix duplicato mp1 in Uptime)

## 2026-03-14

### Step 2.5: Multi-tunnel VLAN — install script, deploy e verifica end-to-end
- **`install_mpquic.sh` aggiornato** per installazione completa Step 2.5 su nuove TBOX:
  - Client: installa VLAN `.netdev`/`.network` in `/etc/systemd/network/`, config
    multi-tunnel (cr4-3/br4-3/df4-3), classifier, e abilita tutti i servizi
  - Server: installa config mt4/mt5/mt6, apre porte NFT (45014-45016),
    configura forward e NAT per `mt*` tunnel e subnet 10.200.{10,14,15,16}.0/24
- **VPS routes** (`mpquic-vps-routes.sh`): aggiunte route di ritorno per VLAN transit
  subnets 172.16.{11-13,21-23,31-33}.0/30 via mt4/mt5/mt6
- **VPS nftables** (`mpquic-vps.nft`): forward per `mt*` tunnel + NAT per subnet
  multi-tunnel e VLAN transit
- **Config fix**: aggiunto `metrics_listen: auto` a br4, df4, br5, df5, br6, df6
- **Deploy client**: VLAN networkd (9 .netdev + 9 .network), LAN trunk con VLAN= lines,
  ip rules 800-808, rt_tables 120-128, VLAN classifier applicato
- **Deploy VPS**: forward rules mt4/mt5/mt6 ↔ eth0, VLAN transit routes (9 subnets),
  nftables salvato in `/etc/nftables.conf`, `mpquic-vps-routes.service` riavviato
- **Verifica end-to-end**: 9/9 class tunnel UP + ping peer VPS OK:
  - WAN4 (SL4, terrestrial): cr4/br4/df4 → 10.200.14.254 — ~110ms RTT
  - WAN5 (SL5, Starlink): cr5/br5/df5 → 10.200.15.254 — ~13ms RTT
  - WAN6 (SL6, Starlink): cr6/br6/df6 → 10.200.16.254 — ~19ms RTT
- **Fix rt_tables**: corretto entry stale `120 bd1` → `120 mt_cr4`
- **Documentazione**: sezione 23 in INSTALLAZIONE_TEST.md (procedura completa)

### Fase 5 Metriche: completata
- Roadmap aggiornata: Fase 5 ✅ COMPLETATA (Prometheus CT 201 + Grafana CT 202)
- Fase 5.2 documentata con dettagli deployment, 9 scrape target, bug fix
- Fix `mpquic_session_pipes`: gauge corretto (non più contatore cumulativo)

## 2025-03-12

### Step 4.23: TUN Multiqueue (IFF_MULTI_QUEUE) — per-session fd
- **Profiling-driven**: TUN write al 26.9% CPU + TUN read al 7.5% = 34.4% totale I/O TUN.
  Con singolo fd, reader e N writer serializzati su stessa coda kernel.
- Linux TUN multiqueue (kernel 3.8+): `IFF_MULTI_QUEUE` permette N fd indipendenti
  sullo stesso device, ognuno con coda kernel propria.
- Implementazione:
  - `runServerMultiConn`: TUN aperto con `MultiQueue: true` (fd #1 per reader)
  - `stripeSession`: nuovo campo `tunFd *water.Interface` — fd dedicato per-session
  - Session creation: `water.New(MultiQueue: true, Name: tunName)` apre fd aggiuntivo
  - `tunWriter`: usa `sess.tunFd.Write()` (fd dedicato, non condiviso)
  - Session cleanup (GC + Close): chiusura per-session fd
  - Fallback: se multiqueue fd fails, usa fd condiviso (backward compatible)
- Con dual Starlink: 3 fd paralleli (1 reader + 2 writer per wan5/wan6)
- File modificati: `main.go` (MultiQueue: true), `stripe.go` (per-session fd + cleanup)

### Step 4.21: tunWriter batch-drain + reduce per-packet mutex
- **Profiling-driven**: pprof mostra `tunWriter → os.File.Write` al **26.3%** CPU,
  `runtime.findRunnable` (scheduling) al **9.76%**, touchPath/learnRoute con mutex per pacchetto.
- Limitazione TUN: `/dev/net/tun` accetta 1 pacchetto IP per `write()` —
  `writev` concatena in un unico pacchetto, non supporta multi-packet batch.
- Implementazione batch-drain rxCh (stesso pattern di `drainSendCh`):
  - Blocking receive 1 pacchetto, non-blocking drain di tutti i pacchetti in coda
  - Tight write loop: `tun.Write()` per ogni pacchetto senza re-scheduling goroutine
  - `touchPath` chiamato **1 volta per batch** (non per pacchetto) — elimina N-1 RLock
  - `learnRoute` chiamato solo se `srcIP ≠ peerIP` — skip per traffico dalla stessa sorgente
- Impatto atteso: scheduling da 11.2% → ~8-9%, mutex contention quasi azzerata
- File modificati: `stripe.go` (rewrite `tunWriter`)

## 2025-03-11

### Step 4.20: Batch TX via sendmmsg — server-side
- **Profiling-driven**: pprof CPU 60s mostra TX path (SendDatagram→WriteToUDP→sendto)
  consuma il **45% del tempo CPU server** — 1 syscall sendto per ogni pacchetto.
- Implementazione `sendmmsg` via `ipv4.PacketConn.WriteBatch()`:
  - `stripeSession` accumula pacchetti criptati in batch di 8 (`stripeBatchSize`)
  - Flush automatico quando batch pieno, o da `drainSendCh` dopo batch-drain
  - `drainSendCh` modificato: blocking recv → non-blocking drain → FlushTxBatch
  - Copre M=0 fast path E M>0 FEC path (data + parity shards)
  - Timer FEC flush include anche batch flush per non lasciare pacchetti pendenti
- Interfaccia `txBatcher` con type assertion (non modifica `datagramConn`)
- Impatto atteso: riduzione syscall TX di ~8× → libera ~40% del tempo CPU server
- File modificati: `stripe.go` (batch add/flush/init), `main.go` (drainSendCh + txBatcher)

### Step 4.19: pprof profiling + analisi bottleneck COMPLETATO
- Flag `--pprof :6060` per CPU/memory profiling runtime via `net/http/pprof`.
- Profilo CPU catturato sotto carico reale (iperf3 -R -P20, 60s, 86.56s CPU).
- Risultati: TX syscall **45%**, TUN write **23%**, scheduling 14%, crypto 4.6%, RX 5.2%.
- Il server è completamente **I/O bound** (66.8% in syscall.Syscall6).
- ROADMAP e NOTA_TECNICA aggiornate con tabella profiling e nuove priorità.

### Step 4.18: RX Reorder Buffer — ❌ NEGATIVO (revert 1b010a9)
- Implementazione completa con 3 tuning testati (default/window24-1ms/window16-200µs).
- Tutti i tuning peggiorativi: -13% a -16% throughput, retransmit fino a +234%.
- Root cause: jitter artificiale dal buffer confonde smoothed-RTT di TCP → CC backoff.
- **Codice completamente rimosso**, codebase torna a v4.1.

### Step 4.19: pprof profiling support
- Flag `--pprof :6060` per CPU/memory profiling runtime via `net/http/pprof`.
- Prerequisito per ottimizzazioni data-driven (Step 4.20 UDP GSO, Step 4.21 UDP GRO).

### Step 4.14: FEC per dimensione pacchetto — ❌ NEGATIVO (revert)
- Benchmark dual Starlink: 331 Mbps media (-6.5% vs baseline 354), retransmit +186%.
- Root cause: il codice FEC skip è dead code in modalità adaptive M=0 (il 99% del
  tempo operativo). Il fast path M=0 in `SendDatagram()` ritorna prima del blocco skip.
- Nessun beneficio misurabile, complessità aggiunta inutile.
- **Codice revertito** (`git revert ca4f179`), codebase torna a v4.1.

### Deploy: wan-watchdog auto DHCP re-discover
- Daemon systemd che monitora i gateway DHCP delle WAN ogni 15s.
- Se il gateway diventa irraggiungibile per 60s (4 check), forza `networkctl reconfigure`.
- Risolve il problema delle NIC VirtIO che non perdono carrier su cable-swap.
- File: `scripts/wan-watchdog.sh`, `deploy/systemd/wan-watchdog.service`.

### Deploy: configurazione di rete per-WAN
- Sostituito il singolo `10-wan.network` con 6 file individuali per WAN.
- `KeepConfiguration=no`, `SendRelease=yes`, `ClientIdentifier=mac`.
- `RouteMetric` diversificata per evitare conflitti routing.
- File: `deploy/networkd/wan/10-wan1.network` ... `15-wan6.network`.
- Script manuale: `scripts/wan-reconfigure.sh`.

## 2026-03-10

### Step 4.14: FEC per dimensione pacchetto (skip small packets) — IMPLEMENTATO poi REVERTITO
- Quando `effectiveM > 0` (FEC attivo), i pacchetti più piccoli di `fecMinSize`
  (default 300 byte) vengono inviati direttamente senza accumulo FEC.
- Evita il padding di pacchetti piccoli (ACK TCP ~52B, DNS ~80B, keepalive)
  a ~1402B all'interno di un FEC group, con >90% di spreco banda.
- Implementato sia nel client `SendDatagram()` che nel server `SendDatagram()`.
- Compatibile con ARQ (pacchetti skipped salvati in `arqTx`).
- Configurabile via YAML: `stripe_fec_min_size: 300` (default), `-1` per disabilitare.
- Contatore `txFECSkip` (atomic) per telemetria.
- Nessuna modifica RX necessaria: il receiver gestisce già `GroupDataN < K` come
  consegna diretta.
- **Revertito l'11 marzo** — vedi sopra.

### Fix: re-register connectionTable on re-key (`89ab73f`)
- Dopo riavvio client, il `pathConn` nella connectionTable del server manteneva
  un `lastRecv` stantio dalla sessione precedente. `dispatch()` lo considerava
  "stale" (soglia 3s) e scartava silenziosamente il traffico di ritorno.
- Fix: al re-key e al reconnect, viene ricreato il `stripeServerDC` e chiamato
  `registerStripe()` con `lastRecv = time.Now()`.
- Aggiunto lock `txMu` per update `txCipher` (data race con `drainSendCh`).
- `pendingKeys.Delete()` dopo re-key per evitare re-key spurii.
- Logging diagnostico rate-limited (1/s) per TX drop e dispatch falliti.

### Fix: reset ARQ/FEC state on re-key (`6ca7052`)
- Dopo riavvio client, il server manteneva `arqRxTracker.base` dalla sessione
  precedente. Il nuovo client riparte da `txSeq=0`, ma tutti i pacchetti con
  `seq < base` venivano rifiutati come "troppo vecchi" da `markReceived()`.
- Sintomo: ritardo progressivo nel ripristino del ping (+8s ad ogni riavvio).
- Fix: al re-key e al reconnect, reset completo di `arqRx`, `arqTx`, `rxGroups`,
  `rxSeqHighest`, counters RX/TX, e `txSeq`/`txPipe`/`txGroup` del server.
- Risultato: dopo il fix, il ping risponde immediatamente ad ogni riavvio
  (verificato 4 riavvii consecutivi con 0 pacchetti persi da stale state).

## 2026-03-01

### Cleanup diagnostico (`c15b235`)
- Rimosse log temporanee `[DIAG]` da `cmd/mpquic/main.go` in `registerStripe()`:
  - `CREATED group`
  - `REPLACED path`
  - `APPENDED path`
- Rimosse log temporanee `[DIAG]` da `dispatch()`:
  - `SINGLE path`
  - `MULTI paths`
- Eliminato anche il contatore di sampling diagnostico `dispatchCounter` (non più usato)
- Verifica locale: `go test ./cmd/mpquic` → `ok`

## 2026-02-28 — Fase 2: Multi-tunnel per link ✅

### Step 2.1 — Server multi-connessione (`b0bbddf`)
- `connectionTable` con mapping `peer_TUN_IP → quic.Connection`
- `runServerMultiConn()`: accetta N connessioni concorrenti sulla stessa porta
- Auto-registrazione peer dal primo pacchetto (src IP bytes 12-15)
- `learnRoute()` per return-path su traffico non-NATtato (`b93155c`)
- Config flag `multi_conn_enabled: true`

### Step 2.2-2.3 — Test multi-tunnel su WAN5 (DECOMMISSIONATO)
> **Nota**: Questi step usavano i nomi cr5/df5/bk5 sulla subnet 10.200.10.0/24
> con server mt1 (porta 45010). Sono stati **decommissionati** il 15/03/2026
> con il rename che unifica la numerazione tunnel=WAN. I nomi cr5/df5 ora
> identificano i tunnel WAN5 rinominati (ex cr2/df2, subnet 10.200.15.x).

- 3 istanze client: cr5 (critical), df5 (default), bk5 (bulk) sulla stessa WAN5
- Deploy configs: `deploy/config/client/{cr5,df5,bk5}.yaml` + `.env`
- Server config: `deploy/config/server/mt1.yaml` (porta 45010, TUN mt1, /24)
- `scripts/mpquic-mt-classifier.sh`: classificazione source-IP con ip rule + routing tables
- Masquerade per-tunnel (NAT src → TUN IP) per return-path server
- Persistenza nftables in `/etc/nftables.conf`

### Step 2.3 — Deploy e test end-to-end (DECOMMISSIONATO)
- VPS: `mpquic@mt1` su porta 45010, NFT aperta, TUN mt1 10.200.10.254/24
- Client: 3 servizi `mpquic@{cr5,df5,bk5}` attivi, TUN UP con IP corretti
- Test OpenWrt: `mwan3 use SLx ping` → traffico classificato → VPS → internet → reply ✓
- tcpdump verificato su entrambi i lati: flusso completo bidirezionale su tutti e 3 i tunnel
- RTT ~14ms su WAN5

### Step 2.4 — Test isolamento con netem + iperf3 (2026-02-28)
- **RTT isolation**: netem 10%/30% loss su br5 → cr5 e df5 a 0% loss
- **Throughput isolation**: iperf3 con device binding (`-B IP%dev`)
  - Baseline: cr5=50.2, br5=48.1, df5=50.0 Mbps
  - 10% loss br5: cr5=50.2 (±0%), br5=2.3 (−95%), df5=50.2 (±0%)
  - 30% loss br5: cr5=50.2 (±0%), br5=0.4 (−99%), df5=49.8 (±0%)
- **Conclusione**: isolamento perfetto tra tunnel sulla stessa WAN
- VPS nftables: aggiunta regola `iifname "mt*" accept` + `tcp dport 5201`
- Nota: subnet /24 condivisa richiede binding esplicito per-device nei test

### Step 2.5 — Architettura 9 tunnel VLAN (8a6923e)

### Strumenti
- `scripts/mpquic-update.sh` (`f1ddffb`): update automatico VPS/client (pull → build → stop → install → restart)
- `scripts/mpquic-mt-classifier.sh`: apply/remove/status per regole classifier

### Prossimo step
- **Step 2.5**: Generalizzazione 3 WAN × 3 classi = 9 tunnel
- Classificazione per VLAN (non più per source-IP): OpenWrt tagga traffico → VLAN sub-interfaces → tunnel dedicato
- Schema: VLAN XY (X=LAN, Y=classe) → tunnel crX/brX/dfX

---

## 2026-02-28

### Bug fix critici
- **Server goroutine leak** (`73474a9`): ogni riconnessione client lasciava un goroutine reader TUN stantio che rubava pacchetti di ritorno. Fix: singolo reader TUN condiviso via channel + `runServerTunnel()` context-aware.
- **Gateway detection avvelenato** (`3ac4036`): `gw_for_dev()` nel routing script consultava prima i file lease dhclient (stantii dopo migrazione a networkd) che contenevano gateway di interfacce sbagliate. Fix: inversione priorità — prima `ip route` (kernel), poi dhclient come fallback legacy.

### Risultati
- **Tutti e 3 i tunnel attivi ora bidirezionali**: mpq4 (~108ms), mpq5 (~13ms), mpq6 (~34ms)
- mpq5 era rotto da giorni per la combinazione dei due bug sopra

### Network migration
- Migrazione completa da ifupdown a systemd-networkd (11 file .network)
- DNS statico con chattr +i su resolv.conf
- Rimossi file lease dhclient stantii
- Script `setup-network.sh` per replica su nuove VM

### Roadmap aggiornata
- Fase 1 (baseline multi-link 1:1): **COMPLETATA** — 3/3 WAN attive con tunnel bidirezionali
- Roadmap riscritta e allineata ai 5 step del documento TSPZ
- Chiarita distinzione chiave: **multi-link** (1 tunnel/WAN, ✅ DONE) vs **multi-tunnel per link** (N tunnel/WAN per classe traffico, PROSSIMO) vs **multi-path per tunnel** (bonding/failover, FUTURO)
- Prossimo step: Fase 2 — multi-tunnel per link con server multi-connessione e nftables classifier

## 2026-02-25

### Step completati
- Implementazione tunnel QUIC 1:1 su 6 istanze (`mpquic@1..@6`).
- Validazione operativa end-to-end su 3 WAN attive (`@4`, `@5`, `@6`).
- Policy routing client aggiornata a `LANx -> mpqx` senza fallback.
- VPS configurata con forwarding + NAT persistenti (`nftables`) e route di ritorno su `mpq1..mpq6`.
- Hardening TLS nel binario:
  - server con `tls_cert_file` + `tls_key_file` obbligatori
  - client con trust CA (`tls_ca_file`) e `tls_insecure_skip_verify: false`
  - helper per generazione certificati persistenti.

### Artefatti principali
- `scripts/mpquic-policy-routing.sh`
- `scripts/mpquic-vps-routes.sh`
- `scripts/generate_tls_certs.sh`
- `deploy/nftables/mpquic-vps.nft`
- `deploy/systemd/mpquic-routing.service`
- `deploy/systemd/mpquic-vps-routes.service`
- `docs/OPERATIVE_ROUTING_NAT.md`

### Stato roadmap
- Baseline 1:1: `3/6` tunnel attivi (WAN4/5/6).
- Blocco residuo: WAN1/2/3 senza IPv4 DHCP lato client.
- Prossimo obiettivo: estendere validazione a `@1..@3` appena WAN disponibili, poi iniziare design multipath su connessione logica unica.
