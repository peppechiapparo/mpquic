# MPQUIC IP-over-QUIC POC (Debian 12)

## A) Repository base selezionato e motivazione tecnica

Scelta base: implementazione minimale diretta su `quic-go` (DATAGRAM) con schema ispirato a `connect-ip-go`.

Motivazione:
- `quic-go` è il requisito esplicito e fornisce nativamente QUIC DATAGRAM.
- Serve bind UDP su IP WAN specifica per istanza (`bind_ip`), requisito critico del tuo policy routing L3 già validato.
- `connect-ip-go` è utile come riferimento protocol-level, ma introduce semantica MASQUE non necessaria in questa fase 2 (POC 1:1 raw IP su TUN).
- `mqvpn` è un riferimento architetturale, ma per il tuo vincolo (6 sessioni indipendenti, 1 binario, YAML, systemd template) questa codebase minimale è più controllabile.

Scope deliberato:
- Solo multi-sessione parallela 1:1 (`mpquic@1`..`@6`).
- Multipath single-connection disponibile in modalità sperimentale (`multipath_enabled: true`).

## B) Implementazione minimale funzionante (1 tunnel)

Binario unico: `cmd/mpquic/main.go`
- `role: client|server` da YAML.
- QUIC DATAGRAM (`EnableDatagrams: true`).
- Bridge bidirezionale:
  - TUN -> `SendDatagram()`
  - `ReceiveDatagram()` -> TUN
- Client: bind UDP locale su `bind_ip` (supporta anche `if:<ifname>`).
- Server: listen su `bind_ip:remote_port`.

## C) Estensione a 6 istanze indipendenti

Template e config pronti:
- Client: `deploy/config/client/1..6.yaml` + `1..6.env`
- Server: `deploy/config/server/1..6.yaml` + `1..6.env`
- Systemd template: `deploy/systemd/mpquic@.service`

Mapping client WAN già preconfigurato:
- Istanza 1 -> `if:enp7s3`
- Istanza 2 -> `if:enp7s4`
- Istanza 3 -> `if:enp7s5`
- Istanza 4 -> `if:enp7s6`
- Istanza 5 -> `if:enp7s7`
- Istanza 6 -> `if:enp7s8`

## D) Configurazione, deploy e uso operativo

Per la documentazione completa, consultare:

| Argomento | Documento |
|-----------|-----------|
| Parametri config YAML | `docs/INSTALLAZIONE_TEST.md` §11 |
| Installazione client/server | `docs/INSTALLAZIONE_TEST.md` §1-6 |
| systemd template e deploy | `docs/INSTALLAZIONE_TEST.md` §18 |
| Multipath e QoS dataplane | `docs/DATAPLANE_ORCHESTRATOR.md` |
| Uso operativo e debug tunnel | `docs/TUNNEL_OPERATIONS_DEBUG.md` |
| Diagnostica lunga/crash | `docs/TUNNEL_OPERATIONS_DEBUG.md` §5 |

## G) Vincoli rispettati

- Nessuna modifica alla logica L3 esistente (source-based policy routing + NAT su WAN fisiche).
- Il tunnel si inserisce sopra: ogni istanza QUIC usa bind WAN dedicato.
- Multi-sessione 1:1 (`6` connessioni QUIC indipendenti).
- Multipath su singola connessione disponibile in modalità sperimentale (opzionale, lato client).

## File principali

- `cmd/mpquic/main.go`
- `scripts/ensure_tun.sh`
- `scripts/install_mpquic.sh`
- `deploy/systemd/mpquic@.service`
- `deploy/config/client/*.yaml`
- `deploy/config/server/*.yaml`

Guide operative aggiuntive:
- `docs/TUNNEL_OPERATIONS_DEBUG.md`

## Note operative

- TLS ora usa file persistenti (`/etc/mpquic/tls/*`) e trust esplicito client.
- `tls_insecure_skip_verify` deve restare `false` in ambienti operativi.
- MTU default TUN impostata a `1300` per ridurre frammentazione su QUIC DATAGRAM.
- Il bind `if:<ifname>` risolve l'IPv4 corrente dell'interfaccia (utile su WAN DHCP).
- Auto-heal eventi WAN supporta sia hook `ifupdown` sia `networkd-dispatcher` (hotplug carrier/DHCP).
- Smoke test multipath sperimentale: `sudo /usr/local/sbin/mpquic-multipath-smoke.sh` (template `deploy/config/client/multipath.yaml`).

## Procedura di deploy (OBBLIGATORIA)

**MAI usare `scp` per trasferire binari o config. SEMPRE passare da git.**

### 1. Commit e push dal dev box

```bash
cd /opt/TPZ/src/mpquic
git add -A && git status          # verificare cosa cambia
git commit -m "descrizione"
git push origin main
```

### 2. Aggiornare il SERVER (VPS)

```bash
ssh vps-it-mpquic
# Se servono modifiche ai file YAML, farle ORA prima di riavviare:
#   vim /etc/mpquic/instances/mp1.yaml
sudo /opt/mpquic/scripts/mpquic-update.sh /opt/mpquic
journalctl -u mpquic@mp1 --no-pager -n 50
# Ripetere journalctl per ogni tunnel da verificare
exit
```

### 3. Aggiornare il CLIENT

```bash
# Se servono modifiche ai file YAML, farle prima:
#   ssh mpquic "sudo vim /etc/mpquic/instances/mp1.yaml"
ssh mpquic "sudo /opt/mpquic/scripts/mpquic-update.sh /opt/mpquic"
```

### 4. Verifica tunnel

```bash
# Ping di base
ssh mpquic "ping -c 5 10.200.17.254"

# Verifica log di un tunnel specifico
ssh mpquic "sudo systemctl restart mpquic@mp1 && sleep 5 && sudo journalctl -u mpquic@mp1 --no-pager -n 30"

# Stats JSON
ssh mpquic "curl -s http://10.200.17.1:9090/api/v1/stats | python3 -m json.tool"
```

### Note

- Lo script `mpquic-update.sh` fa: git pull → go build → stop istanze → install binary → start istanze
- Se il server ha diverged history (dopo force push): `cd /opt/mpquic && git fetch origin && git reset --hard origin/main` poi rieseguire lo script
- Le config YAML live sono in `/etc/mpquic/instances/` (NON in `deploy/config/`)
- Le config in `deploy/config/` sono template di riferimento nel repo
