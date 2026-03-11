# Changelog implementazione (replicabile TBOX)

## 2026-03-11

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

### Step 2.2 — Client istanze per-classe + classifier (`058ddca`, `477d08d`)
- 3 istanze client: cr5 (critical), df5 (default), bk5 (bulk) sulla stessa WAN5
- Deploy configs: `deploy/config/client/{cr5,df5,bk5}.yaml` + `.env`
- Server config: `deploy/config/server/mt1.yaml` (porta 45010, TUN mt1, /24)
- `scripts/mpquic-mt-classifier.sh`: classificazione source-IP con ip rule + routing tables
- Masquerade per-tunnel (NAT src → TUN IP) per return-path server
- Persistenza nftables in `/etc/nftables.conf`

### Step 2.3 — Deploy e test end-to-end
- VPS: `mpquic@mt1` su porta 45010, NFT aperta, TUN mt1 10.200.10.254/24
- Client: 3 servizi `mpquic@{cr5,df5,bk5}` attivi, TUN UP con IP corretti
- Test OpenWrt: `mwan3 use SLx ping` → traffico classificato → VPS → internet → reply ✓
- tcpdump verificato su entrambi i lati: flusso completo bidirezionale su tutti e 3 i tunnel
- RTT ~14ms su WAN5

### Step 2.4 — Test isolamento con netem + iperf3 (2026-02-28)
- **RTT isolation**: netem 10%/30% loss su br2 → cr2 e df2 a 0% loss
- **Throughput isolation**: iperf3 con device binding (`-B IP%dev`)
  - Baseline: cr2=50.2, br2=48.1, df2=50.0 Mbps
  - 10% loss br2: cr2=50.2 (±0%), br2=2.3 (−95%), df2=50.2 (±0%)
  - 30% loss br2: cr2=50.2 (±0%), br2=0.4 (−99%), df2=49.8 (±0%)
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
