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
- Nessun multipath single-connection.

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

## D) Parametri config

Ogni YAML contiene:
- `role`
- `bind_ip`
- `remote_addr`
- `remote_port`
- `tun_name`
- `tun_cidr`
- `log_level`
- `tls_cert_file` (server)
- `tls_key_file` (server)
- `tls_ca_file` (client)
- `tls_server_name` (client)
- `tls_insecure_skip_verify` (client)

Esempio (client, istanza 1):
```yaml
role: client
bind_ip: if:enp7s3
remote_addr: VPS_PUBLIC_IP
remote_port: 45001
tun_name: mpq1
tun_cidr: 10.200.1.1/30
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
```

## E) systemd unit template

File: `deploy/systemd/mpquic@.service`
- `EnvironmentFile=/etc/mpquic/instances/%i.env`
- `ExecStartPre`: crea/configura TUN via script persistente idempotente
- `ExecStart`: avvia binario con `/etc/mpquic/instances/%i.yaml`
- `Restart=always`
- `enable` previsto per `mpquic@1..6`

## F) Procedura operativa step-by-step (verificabile)

### 0. Prerequisiti Debian 12 (client e server)

```bash
sudo apt-get update
sudo apt-get install -y iproute2 systemd ca-certificates golang-go
```
Verifica:
```bash
go version
ip -V
systemctl --version
```

### 1. Build binario

```bash
cd /opt/SATCOMVAS/src/mpquic
make build
```
Verifica:
```bash
ls -l bin/mpquic
file bin/mpquic
```

### 2. Installazione lato CLIENT

```bash
sudo ./scripts/install_mpquic.sh client
```
Verifica:
```bash
ls -l /etc/systemd/system/mpquic@.service
ls -l /etc/mpquic/instances/{1..6}.yaml
```

### 3. Installazione lato SERVER (VPS)

Copia repo sul server e poi:
```bash
sudo ./scripts/install_mpquic.sh server
```
Verifica:
```bash
ls -l /etc/mpquic/instances/{1..6}.yaml
```

### 4. Configurazione endpoint

#### Client
Aggiorna `remote_addr` in tutti i file `/etc/mpquic/instances/1..6.yaml` con IP pubblico VPS.

Verifica:
```bash
grep -R "remote_addr" /etc/mpquic/instances/*.yaml
```

#### Server
Se vuoi bind dedicato, modifica `bind_ip` nei file server da `0.0.0.0` a IP specifico.

Verifica:
```bash
grep -R "bind_ip" /etc/mpquic/instances/*.yaml
```

### 4.1 Materiale TLS persistente (obbligatorio)

#### Server
```bash
sudo /usr/local/lib/mpquic/generate_tls_certs.sh /etc/mpquic/tls mpquic-server 825
sudo ls -l /etc/mpquic/tls
```

#### Client
Copia `/etc/mpquic/tls/ca.crt` dal server al client in `/etc/mpquic/tls/ca.crt`.

Verifica:
```bash
ls -l /etc/mpquic/tls/ca.crt
grep -R "tls_" /etc/mpquic/instances/*.yaml
```

### 5. Bring-up iniziale 1 tunnel (POC minimo)

#### Server
```bash
sudo systemctl enable --now mpquic@1.service
sudo systemctl status mpquic@1.service --no-pager
sudo ss -lunp | grep 45001
```

#### Client
```bash
sudo systemctl enable --now mpquic@1.service
sudo systemctl status mpquic@1.service --no-pager
ip -br a show dev mpq1
```

Verifica end-to-end:
- Da client verso server TUN peer:
```bash
ping -I mpq1 -c 3 10.200.1.2
```
- Sul client, verifica bind WAN corretto:
```bash
sudo ss -unap | grep mpquic
```
Controlla che la socket UDP della istanza `@1` abbia `src` su IP WAN1 (interfaccia `enp7s3`).

### 6. Estensione a 6 tunnel

#### Server
```bash
for i in 1 2 3 4 5 6; do sudo systemctl enable --now mpquic@$i.service; done
```
Verifica:
```bash
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
sudo ss -lunp | egrep '4500[1-6]'
```

#### Client
```bash
for i in 1 2 3 4 5 6; do sudo systemctl enable --now mpquic@$i.service; done
```
Verifica:
```bash
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
ip -br a | grep '^mpq'
sudo ss -unap | grep mpquic
```

### 7. Verifica persistenza reboot

```bash
sudo reboot
```
Dopo reboot:
```bash
for i in 1 2 3 4 5 6; do systemctl is-enabled mpquic@$i.service; systemctl is-active mpquic@$i.service; done
ip -br a | grep '^mpq'
```

## G) Vincoli rispettati

- Nessuna modifica alla logica L3 esistente (source-based policy routing + NAT su WAN fisiche).
- Il tunnel si inserisce sopra: ogni istanza QUIC usa bind WAN dedicato.
- Multi-sessione 1:1 (`6` connessioni QUIC indipendenti).
- Nessun multipath su singola connessione.

## File principali

- `cmd/mpquic/main.go`
- `scripts/ensure_tun.sh`
- `scripts/install_mpquic.sh`
- `deploy/systemd/mpquic@.service`
- `deploy/config/client/*.yaml`
- `deploy/config/server/*.yaml`

## Note operative

- TLS ora usa file persistenti (`/etc/mpquic/tls/*`) e trust esplicito client.
- `tls_insecure_skip_verify` deve restare `false` in ambienti operativi.
- MTU default TUN impostata a `1300` per ridurre frammentazione su QUIC DATAGRAM.
- Il bind `if:<ifname>` risolve l'IPv4 corrente dell'interfaccia (utile su WAN DHCP).
