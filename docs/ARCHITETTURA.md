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
- `cmd/mpquic/main.go`: dataplane TUN <-> QUIC DATAGRAM
- `deploy/systemd/mpquic@.service`: template servizio
- `deploy/config/client/{1..6}.yaml`
- `deploy/config/server/{1..6}.yaml`
- `scripts/ensure_tun.sh`: creazione/config TUN persistente e idempotente
- `scripts/mpquic-healthcheck.sh`: check strutturato per ruolo (`client|server`) con auto-recovery opzionale
- `scripts/mpquic-lan-routing-check.sh`: validazione/fix routing LAN->tunnel (`check|fix`, target `1..6|all`)
- `scripts/install_client.sh`: installazione lato client
- `scripts/install_server.sh`: installazione lato server

## Parametri configurazione per istanza
Ogni YAML include:
- `role`: `client` o `server`
- `bind_ip`: IP locale o `if:<ifname>`
- `remote_addr`: endpoint remoto (richiesto lato client)
- `remote_port`: porta UDP/QUIC istanza
- `tun_name`: nome TUN
- `tun_cidr`: CIDR locale TUN
- `log_level`: `debug|info|error`

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

### Livello 3: Multi-path per tunnel (FUTURO)
Un singolo tunnel può usare N link per resilienza:
- Bonding: aggregazione bandwidth su più WAN
- Backup: failover automatico
- Duplicazione: pacchetti critici su più link simultaneamente

Richiede QUIC Multipath (RFC 9443) o implementazione applicativa.
Il codice `multipathConn` esistente implementa una versione applicativa di questo livello.

## Architettura multipath applicativa (codice esistente, client)

Quando `multipath_enabled: true`, il client non usa più il singolo blocco `bind_ip/remote_addr/remote_port`, ma crea una sessione logica con N path definiti in `multipath_paths`.

**Nota**: questo codice è stato scritto per il Livello 3 (bonding) e verrà riadattato quando il server multi-connessione (Livello 2) sarà pronto. Non è stato ancora testato su infra reale.

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

## Limiti deliberati (fase corrente)
- Multipath in singola connessione QUIC disponibile in modalità sperimentale (scheduler path-aware con priorità/peso e fail-cooldown)
- Nessun endpoint/API di controllo dinamico runtime (oggi policy caricata da YAML a startup)
- TLS server self-signed runtime (POC)
