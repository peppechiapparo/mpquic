# Changelog implementazione (replicabile TBOX)

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
