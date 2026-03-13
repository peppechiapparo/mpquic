# Installazione e test (client + server)

## 1) Prerequisiti (entrambi i nodi)
```bash
sudo apt-get update
sudo apt-get install -y iproute2 systemd ca-certificates golang-go
```
Verifica:
```bash
go version
systemctl --version
ip -V
```

## 2) Build binario
```bash
cd /opt/SATCOMVAS/src/mpquic
make build
ls -l bin/mpquic
```

## 3) Installazione lato SERVER (VPS)
```bash
cd /opt/SATCOMVAS/src/mpquic
sudo ./scripts/install_server.sh
```
Verifica file:
```bash
ls -l /etc/systemd/system/mpquic@.service
ls -l /etc/mpquic/instances/{1..6}.yaml.tpl
cat /etc/mpquic/global.env
```

## 4) Installazione lato CLIENT (VM MPQUIC)
```bash
cd /opt/SATCOMVAS/src/mpquic
sudo ./scripts/install_client.sh
```
Verifica file:
```bash
ls -l /etc/mpquic/instances/{1..6}.yaml.tpl
```

## 4.1) Configurazione WAN con systemd-networkd (per-interfaccia)

### Problema

Le WAN usano DHCP per ottenere l'IP dal modem collegato (Starlink, terrestre, LTE).
In ambienti virtualizzati (Proxmox/VirtIO), quando si scollega e ricollega un cavo
su una diversa porta fisica (es. da modem terrestre a modem Starlink), la NIC
virtuale **non perde il carrier** — il DHCP client non sa che deve fare un nuovo
DISCOVER e mantiene il lease vecchio (rete sbagliata). L'unico rimedio senza
watchdog sarebbe un reboot.

### Soluzione: configurazione per-WAN + wan-watchdog

Due componenti:

1. **File `.network` individuali per WAN** — sostituiscono il singolo file condiviso.
   Ogni WAN ha la propria configurazione con `RouteMetric` dedicata, `KeepConfiguration=no`
   per rilascio IP immediato su reconfigure, e `ClientIdentifier=mac` per DHCP robusto.

2. **`wan-watchdog.service`** — daemon che ogni 15s pinga il gateway DHCP di ogni WAN.
   Se il gateway diventa irraggiungibile per 4 check consecutivi (60s), forza
   `networkctl reconfigure` sull'interfaccia per triggerare un nuovo DHCP DISCOVER.

### 4.1.1) Installazione configurazione di rete per-WAN

I file di configurazione sono in `deploy/networkd/wan/`:

| File | Interfaccia | RouteMetric | Note |
|------|-------------|-------------|------|
| `10-wan1.network` | enp7s3 | 101 | WAN1 |
| `11-wan2.network` | enp7s4 | 102 | WAN2 |
| `12-wan3.network` | enp7s5 | 103 | WAN3 |
| `13-wan4.network` | enp7s6 | 104 | WAN4 (fibra/terrestre) |
| `14-wan5.network` | enp7s7 | 105 | WAN5 (Starlink #1) |
| `15-wan6.network` | enp7s8 | 106 | WAN6 (Starlink #2) |

Esempio contenuto (`14-wan5.network`):
```ini
# WAN5 — enp7s7 (Starlink #1)
[Match]
Name=enp7s7

[Network]
DHCP=yes
IPv6AcceptRA=no
LinkLocalAddressing=no
KeepConfiguration=no

[DHCP]
RouteMetric=105
UseDNS=no
UseRoutes=yes
SendRelease=yes
ClientIdentifier=mac

[Link]
RequiredForOnline=no
```

Opzioni chiave:
- **`KeepConfiguration=no`** — rimuove l'IP vecchio immediatamente su reconfigure
- **`SendRelease=yes`** — invia DHCP RELEASE prima del nuovo DISCOVER
- **`ClientIdentifier=mac`** — identifica il client per MAC (più robusto del DUID condiviso)
- **`RequiredForOnline=no`** — le WAN senza modem non bloccano il boot
- **`RouteMetric` diverso** — evita conflitti nella routing table tra WAN

Deploy:
```bash
# Rimuovi il vecchio file condiviso (se presente)
sudo rm -f /etc/systemd/network/10-wan.network

# Installa i file per-WAN
sudo cp deploy/networkd/wan/*.network /etc/systemd/network/

# Ricarica configurazione
sudo networkctl reload
```

Verifica:
```bash
# Ogni WAN deve mostrare il proprio Network File
networkctl status enp7s7  # → Network File: /etc/systemd/network/14-wan5.network
networkctl status enp7s8  # → Network File: /etc/systemd/network/15-wan6.network
```

### 4.1.2) Installazione wan-watchdog (auto-recovery DHCP)

Il watchdog rileva automaticamente quando il gateway DHCP di una WAN diventa
irraggiungibile e forza un DHCP re-discover, senza necessità di reboot.

**Flusso operativo:**
```
Cable swap → gateway vecchio irraggiungibile → 4 ping falliti (60s)
  → networkctl reconfigure → DHCP DISCOVER → nuovo IP dal modem collegato
  → WAN operativa (~70s totale)
```

Installazione:
```bash
# Script
sudo cp scripts/wan-watchdog.sh /usr/local/bin/
sudo chmod +x /usr/local/bin/wan-watchdog.sh

# Service systemd
sudo cp deploy/systemd/wan-watchdog.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now wan-watchdog.service
```

Verifica:
```bash
sudo systemctl status wan-watchdog.service
# Deve mostrare: Active: active (running)

# Log del watchdog
journalctl -u wan-watchdog.service -f
```

Esempio di log durante un cable swap:
```
wan-watchdog: enp7s7: gateway 10.150.19.1 UNREACHABLE (1/4)
wan-watchdog: enp7s7: gateway 10.150.19.1 UNREACHABLE (2/4)
wan-watchdog: enp7s7: gateway 10.150.19.1 UNREACHABLE (3/4)
wan-watchdog: enp7s7: gateway 10.150.19.1 UNREACHABLE (4/4)
wan-watchdog: enp7s7: *** RECONFIGURE *** reason: gateway 10.150.19.1 unreachable for 60s
wan-watchdog: enp7s7: old gateway=10.150.19.1, old addr=10.150.19.116
wan-watchdog: enp7s7: new gateway=100.64.0.1, new addr=100.110.241.142
```

### 4.1.3) Riconfigurazione manuale WAN (wan-reconfigure.sh)

Per forzare un DHCP re-discover immediato (senza attendere il watchdog):

```bash
# Singola interfaccia
sudo /opt/mpquic/scripts/wan-reconfigure.sh enp7s7

# Tutte le WAN
sudo /opt/mpquic/scripts/wan-reconfigure.sh
```

### 4.1.4) Configurazione watchdog (opzionale)

I parametri del watchdog sono configurabili via variabili d'ambiente nel service file.
Decommentare le righe desiderate in `/etc/systemd/system/wan-watchdog.service`:

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `WAN_INTERFACES` | `enp7s3 ... enp7s8` | Interfacce da monitorare |
| `CHECK_INTERVAL` | `15` | Secondi tra un check e l'altro |
| `FAIL_THRESHOLD` | `4` | Check falliti prima di reconfigure (4 × 15s = 60s) |
| `COOLDOWN` | `120` | Secondi minimo tra due reconfigure sulla stessa iface |
| `PING_TIMEOUT` | `3` | Timeout singolo ping in secondi |

Per applicare le modifiche: `sudo systemctl daemon-reload && sudo systemctl restart wan-watchdog`

## 5) Parametrizzazione endpoint
### Client
Imposta IP pubblico VPS una sola volta (vale per tutte le istanze):
```bash
sudo sed -i 's/^VPS_PUBLIC_IP=.*/VPS_PUBLIC_IP=172.238.232.223/' /etc/mpquic/global.env
cat /etc/mpquic/global.env
```
Verifica:
```bash
grep -R "remote_addr" /etc/mpquic/instances/*.yaml.tpl
```

### Server
Opzionale: bind dedicato (`bind_ip`) al posto di `0.0.0.0`.

## 5.1) Configurazione dataplane multipath (completa)

Per policy avanzate (`critical/default/bulk`, classifier L3/L4, duplication) sono supportati due modelli:

### Modello consigliato: file dataplane dedicato
Nel file applicativo client (es. `/etc/mpquic/instances/multipath.yaml.tpl`) aggiungi:
```yaml
dataplane_config_file: /etc/mpquic/instances/dataplane.yaml
```

E crea/copia `dataplane.yaml` in path assoluto:
```bash
sudo install -m 0644 /opt/mpquic/deploy/config/client/dataplane.yaml /etc/mpquic/instances/dataplane.yaml
```

Contenuto esempio:
```yaml
default_class: default
classes:
	default:
		scheduler_policy: balanced
	critical:
		scheduler_policy: failover
		preferred_paths: [wan4, wan5]
		duplicate: true
		duplicate_copies: 2
	bulk:
		scheduler_policy: balanced
		excluded_paths: [wan4]
classifiers:
	- name: voip
		class: critical
		protocol: udp
		dst_ports: ["5060", "10000-20000"]
		dscp: [46]
	- name: backup
		class: bulk
		protocol: tcp
		dst_ports: ["5001-6000"]
```

### Modello alternativo: dataplane inline nello YAML applicativo
Nel medesimo file YAML client, usa sezione `dataplane:` con la stessa struttura di cui sopra.

### Precedenza di configurazione
Se presenti entrambe:
- `dataplane` inline
- `dataplane_config_file`

il file dedicato (`dataplane_config_file`) ha precedenza.

### Control API orchestrator (opzionale)
Nel file client multipath puoi abilitare API locale per validare/applicare policy dataplane:
```yaml
control_api_listen: 127.0.0.1:19090
control_api_auth_token: change-me
```

Generazione token (consigliata):
```bash
TOKEN="$(openssl rand -hex 32)"
echo "$TOKEN"
```

Sostituisci `change-me` con il token nel file YAML e riavvia istanza:
```bash
sudo systemctl restart mpquic@4.service
```

Esempio verifica:
```bash
TOKEN="<token_generato>"
curl -sS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:19090/healthz
curl -sS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:19090/dataplane
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/yaml' --data-binary @/etc/mpquic/instances/dataplane.yaml http://127.0.0.1:19090/dataplane/validate
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/yaml' --data-binary @/etc/mpquic/instances/dataplane.yaml http://127.0.0.1:19090/dataplane/apply
```

### Verifica operativa
Dopo riavvio istanza multipath, controlla:
```bash
journalctl -u mpquic@4.service -n 200 --no-pager | egrep 'path telemetry|class telemetry'
```

Per schema completo, pattern QoS e flusso orchestrator esterno: `docs/DATAPLANE_ORCHESTRATOR.md`.

### Test automatico Control API + Load-balancing + Failover (mpq3/mpq4/mpq5)

Script pronto:
```bash
sudo /usr/local/sbin/mpquic-controlapi-lb-failover-test.sh 4 vps-it-mpquic
```

Con trigger traffico da OpenWRT (`mwan3 use SL1/SL2/SL3`):
```bash
sudo /usr/local/sbin/mpquic-controlapi-lb-failover-test.sh 4 vps-it-mpquic openwrt-host
```

Cosa fa:
1. backup config `4.yaml.tpl`
2. applica config test multipath su path `wan3/wan4/wan5` con Control API locale
3. verifica API (`/healthz`, `/dataplane`)
4. misura distribuzione traffico su UDP `45003/45004/45005` (load-balancing)
5. simula failover fermando `mpquic@4` lato VPS e rimisura il traffico
6. riporta automaticamente la configurazione originale al termine

Output:
- `/tmp/mpquic-lb-capture-4.txt`
- `/tmp/mpquic-failover-capture-4.txt`
- riepilogo finale su stdout

## 6) Test incrementale: prima 1 tunnel
### Server
```bash
sudo systemctl enable --now mpquic@1.service
sudo systemctl --no-pager --full status mpquic@1.service
sudo ss -lunp | grep 45001
```

### Client
```bash
sudo systemctl enable --now mpquic@1.service
sudo systemctl --no-pager --full status mpquic@1.service
ip -br a show dev mpq1
sudo ss -unap | grep mpquic
```

Ping di test (client -> server tunnel peer):
```bash
ping -I mpq1 -c 3 10.200.1.2
```

## 7) Estensione ai 6 tunnel
### Server
```bash
for i in 1 2 3 4 5 6; do sudo systemctl enable --now mpquic@$i.service; done
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
sudo ss -lunp | egrep '4500[1-6]'
```

### Client
```bash
for i in 1 2 3 4 5 6; do sudo systemctl enable --now mpquic@$i.service; done
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
ip -br a | grep '^mpq'
sudo ss -unap | grep mpquic
```

## 8) Troubleshooting rapido
Log istanza:
```bash
journalctl -u mpquic@1.service -n 100 --no-pager
```

Procedura iniziale consigliata (issue ricorrente interfaccia VM/OpenWRT):
1. **non riavviare subito la VM**
2. restart network stack lato client VM MPQUIC
3. restart istanze `mpquic@*` + routing/watchdog
4. rieseguire check/fix
5. solo se ancora KO: reboot VM client (e in ultima istanza anche VPS)

Esempio rapido lato client:
```bash
sudo systemctl restart networking || true
sudo ifreload -a || true
sudo systemctl restart mpquic@1.service mpquic@2.service mpquic@3.service mpquic@4.service mpquic@5.service mpquic@6.service
sudo systemctl restart mpquic-routing.service
sudo systemctl restart mpquic-watchdog.timer
sudo /usr/local/sbin/mpquic-healthcheck.sh client fix
sudo /usr/local/sbin/mpquic-lan-routing-check.sh fix all
```

Esempio rapido lato server:
```bash
sudo systemctl restart mpquic@1.service mpquic@2.service mpquic@3.service mpquic@4.service mpquic@5.service mpquic@6.service
sudo /usr/local/sbin/mpquic-healthcheck.sh server fix
```

Check + auto-fix lato client:
```bash
sudo /usr/local/sbin/mpquic-healthcheck.sh client fix
```

Check + auto-fix lato server:
```bash
sudo /usr/local/sbin/mpquic-healthcheck.sh server fix
```

Diagnostica lunga (cattura eventi intermittenti/crash):

Client:
```bash
sudo /usr/local/sbin/mpquic-long-diagnostics.sh client 21600 20
```

Server:
```bash
sudo /usr/local/sbin/mpquic-long-diagnostics.sh server 21600 20
```

Output:
```bash
ls -lh /var/log/mpquic-diag-*/
```

Post-mortem automatico (dopo crash/flap):
```bash
sudo /usr/local/sbin/mpquic-postmortem.sh \
	/var/log/mpquic-diag-client-<timestamp> \
	/var/log/mpquic-diag-server-<timestamp> \
	/tmp/mpquic-postmortem.txt
```

Uso rapido (ultime cartelle disponibili):
```bash
sudo /usr/local/sbin/mpquic-postmortem.sh > /tmp/mpquic-postmortem-latest.txt
```

Post-mortem cross-host (client + server remoti, consigliato):
```bash
/usr/local/sbin/mpquic-postmortem-remote.sh \
	mpquic vps-it-mpquic /tmp/mpquic-postmortem-remote.txt
```

## 9) Persistenza al reboot
```bash
sudo reboot
```
Dopo reboot:
```bash
for i in 1 2 3 4 5 6; do systemctl is-enabled mpquic@$i.service; systemctl is-active mpquic@$i.service; done
ip -br a | grep '^mpq'
```

## 10) Checklist roadmap (temporanea, da rimuovere a completamento)

### 10.1 VPS (sessione dedicata)
```bash
ssh vps-it-mpquic
sudo /usr/local/sbin/mpquic-update.sh
sudo /usr/local/sbin/mpquic-healthcheck.sh server fix
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
ss -lunp | egrep '4500[1-6]'
exit
```

### 10.2 Client (sessione dedicata)
```bash
ssh mpquic
sudo /usr/local/sbin/mpquic-update.sh
ip -4 -br a show dev enp7s3
ip -4 -br a show dev enp7s4
sudo systemctl restart mpquic@1.service mpquic@2.service mpquic@3.service mpquic@4.service mpquic@5.service mpquic@6.service
sudo /usr/local/sbin/mpquic-healthcheck.sh client fix
sudo /usr/local/sbin/mpquic-lan-routing-check.sh fix 1
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
ip -br a | egrep '^mpq[1-6]'
ping -I mpq1 -c 3 10.200.1.2
ping -I mpq2 -c 3 10.200.2.2
sudo tcpdump -ni enp7s3 udp port 45001 -c 20
exit
```

### 10.3 Fasi successive immediate (dopo LAN1 validata)
```bash
# estensione a LAN2..LAN6 (stessa logica Fase 3)
sudo /usr/local/sbin/mpquic-lan-routing-check.sh check all

# test resilienza modem unplug
sudo /usr/local/sbin/mpquic-healthcheck.sh client check
```

---

## 11) Riferimento completo attributi YAML

Questa sezione documenta **ogni attributo** supportato nei file YAML di configurazione
delle istanze MPQUIC, organizzati per categoria.

### 11.1 Attributi globali (presenti in ogni YAML)

| Attributo | Valori | Obbligatorio | Descrizione |
|-----------|--------|:------------:|-------------|
| `role` | `client` / `server` | ✅ | Ruolo dell'istanza |
| `tun_name` | stringa (es. `mpq4`, `mp1`, `cr5`) | ✅ | Nome interfaccia TUN Linux |
| `tun_cidr` | CIDR (es. `10.200.4.1/30`) | ✅ | Indirizzo IP e subnet della TUN |
| `log_level` | `debug` / `info` / `error` | ✅ | Livello di logging |
| `metrics_listen` | `auto` / `<ip>:<porta>` / (vuoto) | No | Indirizzo di ascolto server metriche. `auto` = deriva IP da `tun_cidr` + porta 9090. Espone `/metrics` (Prometheus) e `/api/v1/stats` (JSON) |

### 11.2 Attributi di rete e connessione

| Attributo | Valori | Obbligatorio | Descrizione |
|-----------|--------|:------------:|-------------|
| `bind_ip` | IP o `if:<ifname>` | Client: ✅ | IP sorgente per il socket UDP. Con `if:` risolve l'IP dall'interfaccia e applica `SO_BINDTODEVICE` |
| `remote_addr` | IP o hostname | Client: ✅ | Indirizzo del server (può usare `VPS_PUBLIC_IP` come placeholder) |
| `remote_port` | intero (es. `45004`) | ✅ | Porta UDP del listener QUIC server |

**Nota su `bind_ip`**:
- `192.168.1.100`: bind solo all'IP (senza SO_BINDTODEVICE)
- `if:enp7s6`: risolve il primo IPv4 di `enp7s6`, applica SO_BINDTODEVICE (raccomandato per multi-WAN)
- `0.0.0.0`: bind su tutte le interfacce (solo server)

### 11.3 Attributi TLS

| Attributo | Valori | Obbligatorio | Descrizione |
|-----------|--------|:------------:|-------------|
| `tls_ca_file` | path (es. `/etc/mpquic/tls/ca.crt`) | Client: ✅ | Certificato CA per verifica server |
| `tls_cert_file` | path (es. `/etc/mpquic/tls/server.crt`) | Server: ✅ | Certificato TLS server |
| `tls_key_file` | path (es. `/etc/mpquic/tls/server.key`) | Server: ✅ | Chiave privata TLS server |
| `tls_server_name` | stringa (es. `mpquic-server`) | Client: ✅ | CN (Common Name) o SAN atteso nel certificato server |
| `tls_insecure_skip_verify` | `true` / `false` | No | Disabilita verifica certificato (solo per test, **mai in produzione**) |

### 11.4 Attributi trasporto e congestion control

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `congestion_algorithm` | `cubic` / `bbr` | `cubic` | Algoritmo di congestion control QUIC |
| `transport_mode` | `datagram` / `reliable` | `datagram` | Modalità trasporto: `datagram` = QUIC DATAGRAM frames (unreliable); `reliable` = QUIC streams (ritrasmissione) |

**Raccomandazione**: usare **sempre** `transport_mode: reliable` su link satellitari.
`datagram` è utile solo per applicazioni UDP real-time che gestiscono la loss internamente.

### 11.5 Attributi multi-connessione server

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `multi_conn_enabled` | `true` / `false` | `false` | Se `true`, il server accetta N connessioni QUIC sulla stessa porta (necessario per multi-tunnel per link e multipath) |

### 11.6 Attributi multipath (client)

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `multipath_enabled` | `true` / `false` | `false` | Abilita la modalità multipath (N path verso lo stesso server) |
| `multipath_policy` | `priority` / `failover` / `balanced` | `priority` | Policy di selezione path TX |

**Policy multipath**:
- `priority`: seleziona il path con score migliore (priority + penalty + weight)
- `failover`: usa il path con priority più bassa (valore numerico), fallback sui successivi
- `balanced`: distribuisce il traffico su tutti i path attivi con round-robin flow-hash

### 11.7 Attributi `multipath_paths[]` (client)

Array di path, ciascuno con:

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `name` | stringa (es. `wan4`) | ✅ obbligatorio | Etichetta operativa del path (usata in log e telemetria) |
| `bind_ip` | IP o `if:<ifname>` | ✅ obbligatorio | IP sorgente / interfaccia WAN per questo path |
| `remote_addr` | IP o hostname | ✅ obbligatorio | Indirizzo IP del server |
| `remote_port` | intero | ✅ obbligatorio | Porta UDP del listener server |
| `priority` | intero ≥ 1 | `1` | Priorità (valore più basso = più preferito). Per failover: primary=1, backup=2 |
| `weight` | intero ≥ 1 | `1` | Peso di preferenza. Per `balanced`, pesi uguali = distribuzione uniforme |
| `pipes` | intero ≥ 1 | `1` | Numero di socket UDP paralleli per il path. Con `transport: stripe`, ogni pipe è una sessione Starlink indipendente |
| `transport` | `quic` / `stripe` | `quic` | Tipo di trasporto per il path. `stripe` usa UDP raw + FEC, `quic` usa connessione QUIC standard |

### 11.8 Attributi stripe (trasporto UDP + FEC + ARQ)

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `stripe_port` | intero (es. `46017`) | `remote_port + 1000` | Porta UDP del listener stripe sul server |
| `stripe_data_shards` | intero (es. `10`) | `10` | K — numero shards dati per gruppo FEC. Anche con FEC disabilitato (M=0), K è usato come soglia nel protocollo RX per distinguere pacchetti diretti (GroupDataN < K) da gruppi FEC completi. **Deve essere coerente tra client e server.** |
| `stripe_parity_shards` | intero (es. `2`) | `2` | M — numero shards parità Reed-Solomon. Con K=10, M=2: tolleranza 16.7% loss. In modalità `adaptive`, l'encoder RS viene pre-creato con questo valore anche se M effettivo parte da 0 |
| `stripe_fec_mode` | `always` / `adaptive` / `off` | `always` | Modalità FEC: `always` = M fisso, ogni gruppo ha K+M shards; `adaptive` = parte da M=0 (nessuna parità, invio diretto), sale a M configurato se rilevata perdita; `off` = M=0 permanente, nessun encoder RS creato |
| `stripe_arq` | `true` / `false` | `false` | Abilita Hybrid ARQ con NACK selettivo. Il receiver rileva gap di sequenza e invia NACK bitmap al sender, che ritrasmette solo i pacchetti mancanti. Attivo solo quando effectiveM=0. Overhead ~0% in assenza di loss |
| `stripe_pacing_rate` | intero (Mbps) | `0` (disabilitato) | Rate limiter token bucket per sessione. **Sconsigliato**: `time.Sleep()` su Linux ha granularità ~1-4ms, causando regressione del throughput fino al 40%. Lasciare a 0 |
| `stripe_enabled` | `true` / `false` | `false` | Solo server: abilita il listener UDP stripe |

**Formula FEC**: può recuperare fino a M shards persi su K+M totali.
Con K=10, M=2: gruppo di 12 shards, tolleranza 2 shards persi (16.7%).
Aumentando M si migliora la resilienza al costo di più overhead di rete.

**Configurazione raccomandata per Starlink**: `stripe_fec_mode: adaptive` + `stripe_arq: true`.
In condizioni normali (loss < 1%), FEC adattivo opera con M=0 (zero overhead) e ARQ
ritrasmette selettivamente i rari pacchetti persi. Se la perdita aumenta significativamente,
FEC adattivo può passare automaticamente a M=2 come fallback.
Benchmark dual Starlink 24 pipe: **354 Mbps** media, picco 390 Mbps (+48% vs baseline 239 Mbps).

**Nota critica**: `stripe_fec_mode` **deve essere identico su client e server**.
Se il client usa `off` ma il server ha `adaptive`, il server può inviare gruppi
FEC con parità che il client non sa decodificare. Dopo qualsiasi modifica,
riavviare **entrambi** i nodi.

### 11.9 Attributi dataplane e QoS (avanzati)

| Attributo | Valori | Default | Descrizione |
|-----------|--------|---------|-------------|
| `dataplane_config_file` | path assoluto | — | File YAML esterno con configurazione dataplane (ha precedenza su inline) |
| `control_api_listen` | `host:port` (es. `127.0.0.1:19090`) | — | Endpoint API REST locale per controllo runtime |
| `control_api_auth_token` | stringa | — | Token Bearer per autenticazione API |

Per schema completo dataplane: `docs/DATAPLANE_ORCHESTRATOR.md`.

---

### 11.10 Esempio completo: client single-link (mpq4)

Configurazione minima per un tunnel QUIC singolo su WAN4:

```yaml
# /etc/mpquic/instances/4.yaml.tpl
role: client
bind_ip: if:enp7s6
remote_addr: VPS_PUBLIC_IP
remote_port: 45004
tun_name: mpq4
tun_cidr: 10.200.4.1/30
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
```

Corrispondente server:

```yaml
# /etc/mpquic/instances/4.yaml.tpl (VPS)
role: server
bind_ip: 0.0.0.0
remote_port: 45004
tun_name: mpq4
tun_cidr: 10.200.4.2/30
log_level: info
tls_cert_file: /etc/mpquic/tls/server.crt
tls_key_file: /etc/mpquic/tls/server.key
```

### 11.11 Esempio completo: client multi-tunnel per link (cr5/df5/bk5)

Tre tunnel sullo stesso link WAN5, ciascuno per una classe di traffico:

```yaml
# /etc/mpquic/instances/cr5.yaml.tpl — VoIP, telemetria
role: client
bind_ip: if:enp7s7
remote_addr: VPS_PUBLIC_IP
remote_port: 45010
tun_name: cr5
tun_cidr: 10.200.10.1/24
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
```

```yaml
# /etc/mpquic/instances/df5.yaml.tpl — Web, HTTPS, API
role: client
bind_ip: if:enp7s7
remote_addr: VPS_PUBLIC_IP
remote_port: 45010
tun_name: df5
tun_cidr: 10.200.10.5/24
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
```

```yaml
# /etc/mpquic/instances/bk5.yaml.tpl — Backup, sync
role: client
bind_ip: if:enp7s7
remote_addr: VPS_PUBLIC_IP
remote_port: 45010
tun_name: bk5
tun_cidr: 10.200.10.9/24
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
```

Corrispondente server (unica istanza multi-conn):

```yaml
# /etc/mpquic/instances/mt1.yaml.tpl (VPS)
role: server
bind_ip: 0.0.0.0
remote_port: 45010
multi_conn_enabled: true
tun_name: mt1
tun_cidr: 10.200.10.254/24
log_level: info
tls_cert_file: /etc/mpquic/tls/server.crt
tls_key_file: /etc/mpquic/tls/server.key
```

### 11.12 Esempio completo: client multipath stripe dual Starlink (mp1)

Configurazione produzione — bonding 2 link Starlink con stripe + FEC adattivo + ARQ:

```yaml
# /etc/mpquic/instances/mp1.yaml (client)
role: client
multipath_enabled: true
multipath_policy: balanced
tun_name: mp1
tun_cidr: 10.200.17.1/24
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
congestion_algorithm: bbr
transport_mode: reliable
stripe_port: 46017
stripe_data_shards: 10
stripe_parity_shards: 2
stripe_fec_mode: adaptive
stripe_arq: true
multipath_paths:
- name: wan5
  bind_ip: if:enp7s7
  remote_addr: 172.238.232.223
  remote_port: 45017
  priority: 1
  weight: 1
  pipes: 12
  transport: stripe
- name: wan6
  bind_ip: if:enp7s8
  remote_addr: 172.238.232.223
  remote_port: 45017
  priority: 1
  weight: 1
  pipes: 12
  transport: stripe
```

Corrispondente server:

```yaml
# /etc/mpquic/instances/mp1.yaml (VPS)
role: server
bind_ip: 0.0.0.0
remote_port: 45017
multi_conn_enabled: true
stripe_enabled: true
stripe_port: 46017
stripe_data_shards: 10
stripe_parity_shards: 2
stripe_fec_mode: adaptive
stripe_arq: true
tun_name: mp1
tun_cidr: 10.200.17.254/24
log_level: info
tls_cert_file: /etc/mpquic/tls/server.crt
tls_key_file: /etc/mpquic/tls/server.key
```

### 11.13 Esempio: failover primario/backup

```yaml
# Failover: wan5 primario, wan6 backup
multipath_policy: failover
multipath_paths:
- name: wan5
  bind_ip: if:enp7s7
  remote_addr: 172.238.232.223
  remote_port: 45017
  priority: 1       # primario
  weight: 1
- name: wan6
  bind_ip: if:enp7s8
  remote_addr: 172.238.232.223
  remote_port: 45017
  priority: 2       # backup
  weight: 1
```

---

## 12) File .env per istanza

Ogni istanza systemd richiede un file `.env` accanto al `.yaml.tpl`:

```bash
# /etc/mpquic/instances/mp1.env
TUN_NAME=mp1
TUN_CIDR=10.200.17.1/24
TUN_MTU=1300
```

Il file `.env` è letto da `EnvironmentFile=` nel service systemd e usato da
`ensure_tun.sh` per creare la TUN prima dell'avvio del processo.

```bash
# /etc/mpquic/instances/4.env (esempio single-link)
TUN_NAME=mpq4
TUN_CIDR=10.200.4.1/30
TUN_MTU=1300
```

### File globale `/etc/mpquic/global.env`

Variabili condivise da tutte le istanze:

```bash
VPS_PUBLIC_IP=172.238.232.223
```

La variabile `VPS_PUBLIC_IP` viene sostituita nei template `.yaml.tpl` dallo
script `render_config.sh` durante l'`ExecStartPre` del servizio.

---

## 13) Configurazione di rete client — interfacce e routing

### 13.1 Layout interfacce (Debian 12, systemd-networkd)

La VM client ha 16 interfacce di rete suddivise in 4 ruoli:

| Gruppo | Interfacce | Ruolo | IP |
|--------|------------|-------|-----|
| MGMT | enp6s18, enp6s19 | Management SSH | 10.10.11.100, 10.10.10.100 |
| LAN | enp6s20-23, enp7s1-2 | Transit verso OpenWrt | 172.16.{1-6}.1/30 |
| VLAN | enp6s20.17 | Transit dedicato mp1 (bd1) | 172.16.17.1/30 |
| WAN | enp7s3-8 | Uplink Starlink (DHCP) | Dinamici (CGNAT/privato) |
| TUN | mpq1-6, mp1, cr5, etc. | Tunnel MPQUIC | 10.200.x.x |

### 13.2 Configurazione interfacce con systemd-networkd

La VM client usa `systemd-networkd` come backend di rete. I file di configurazione
sono in `/etc/systemd/network/` e vengono forniti dal progetto in `deploy/networkd/`.

#### MGMT e LAN (statiche)

Le interfacce di management e LAN usano IP statici configurati nei file
`01-mgmt1.network`, `02-mgmt2.network`, `20-lan1.network` ... `25-lan6.network`.

#### WAN (DHCP — file per-interfaccia)

Ogni WAN ha il proprio file `.network` individuale (vedi §4.1 per dettagli):

```bash
/etc/systemd/network/10-wan1.network   # enp7s3 — WAN1 (metric 101)
/etc/systemd/network/11-wan2.network   # enp7s4 — WAN2 (metric 102)
/etc/systemd/network/12-wan3.network   # enp7s5 — WAN3 (metric 103)
/etc/systemd/network/13-wan4.network   # enp7s6 — WAN4 (metric 104)
/etc/systemd/network/14-wan5.network   # enp7s7 — WAN5 (metric 105)
/etc/systemd/network/15-wan6.network   # enp7s8 — WAN6 (metric 106)
```

I file vengono installati con:
```bash
sudo cp deploy/networkd/wan/*.network /etc/systemd/network/
sudo networkctl reload
```

**Importante**: non usare un singolo file condiviso per tutte le WAN. La configurazione
per-interfaccia è necessaria per il corretto funzionamento del `wan-watchdog` e per
avere `RouteMetric` diversificate.

### 13.3 Rendere permanente la configurazione di rete

La configurazione in `/etc/systemd/network/` è già persistente. Dopo un reboot:

```bash
# Verifica tutte le interfacce sono up con IP
ip -br a | egrep 'enp6s|enp7s'

# Verifica specifiche WAN (DHCP)
ip -4 -br a show dev enp7s6   # WAN4
ip -4 -br a show dev enp7s7   # WAN5
ip -4 -br a show dev enp7s8   # WAN6

# Verifica che ogni WAN usi il proprio file .network
networkctl status enp7s7   # → Network File: /etc/systemd/network/14-wan5.network
```

Il servizio `wan-watchdog.service` (vedi §4.1.2) garantisce la recovery automatica
del lease DHCP anche in caso di cable swap senza carrier loss.

---

## 14) Configurazione nftables (NAT) — Client

### 14.1 File di configurazione

```bash
# /etc/nftables.conf (client)
table ip nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;

        # === NAT sulle WAN fisiche (Starlink) ===
        oifname "enp7s3" masquerade
        oifname "enp7s4" masquerade
        oifname "enp7s5" masquerade
        oifname "enp7s6" masquerade
        oifname "enp7s7" masquerade
        oifname "enp7s8" masquerade

        # === NAT sui tunnel single-link ===
        oifname "mpq1" masquerade
        oifname "mpq2" masquerade
        oifname "mpq3" masquerade
        oifname "mpq4" masquerade
        oifname "mpq5" masquerade
        oifname "mpq6" masquerade

        # === NAT sui tunnel avanzati ===
        oifname "cr5" masquerade
        oifname "df5" masquerade
        oifname "bk5" masquerade
        oifname "mp1" masquerade
    }
}
```

**Perché MASQUERADE su ogni interfaccia?**

- **WAN**: il traffico che esce dalle WAN fisiche verso Internet ha IP sorgente
  delle LAN transit (172.16.x.x) → va NATtato con l'IP WAN
- **Tunnel**: il traffico che entra nei tunnel ha IP sorgente LAN → va NATtato
  con l'IP TUN del tunnel per il ritorno corretto dal VPS
- **mp1**: il tunnel multipath aggrega traffico da LAN instradato via
  `ip route replace default dev mp1 table wan1` → va NATtato

### 14.2 Installazione e persistenza

```bash
# Installare nftables
sudo apt-get install -y nftables

# Copiare la configurazione
sudo cp /etc/nftables.conf /etc/nftables.conf.bak  # backup
sudo nano /etc/nftables.conf                         # editare

# Applicare
sudo nft -f /etc/nftables.conf

# Abilitare al boot
sudo systemctl enable --now nftables

# Verifica
sudo nft list ruleset
```

### 14.3 nftables VPS (server)

```bash
# /etc/nftables.conf (VPS) — estratto rilevante
table inet filter {
    chain input {
        type filter hook input priority filter; policy drop;

        # conntrack
        ct state established,related accept
        iif "lo" accept

        # SSH
        tcp dport 22 accept

        # QUIC tunnel ports
        udp dport 45001-45006 accept
        udp dport 45010 accept
        udp dport 45017 accept

        # Stripe UDP port
        udp dport 46017 accept

        # Tunnel interfaces (everything from TUN is trusted)
        iifname "mpq*" accept
        iifname "mt*" accept
        iifname "mp*" accept

        # iperf3 (test)
        tcp dport 5201 accept
    }

    chain forward {
        type filter hook forward priority filter; policy accept;
    }
}

table ip nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "eth0" masquerade
    }
}
```

**Rendere permanente sul VPS:**

```bash
# Dopo ogni modifica
sudo nft list ruleset | sudo tee /etc/nftables.conf
sudo systemctl enable nftables
```

---

## 15) Tabelle di routing — Client

### 15.1 Policy routing source-based (1:1)

Ogni LAN transit è instradato nel tunnel corrispondente tramite policy routing:

```
Sorgente 172.16.1.0/30 → table wan1 → default dev mpq1
Sorgente 172.16.2.0/30 → table wan2 → default dev mpq2
...
Sorgente 172.16.6.0/30 → table wan6 → default dev mpq6
```

Le tabelle sono gestite dallo script `mpquic-policy-routing.sh` attivato dal
service `mpquic-routing.service`.

### 15.2 Definizione tabelle in `/etc/iproute2/rt_tables`

```bash
# /etc/iproute2/rt_tables — aggiungere:
100 wan1
101 wan2
102 wan3
103 wan4
104 wan5
105 wan6
120 bd1
```

### 15.3 Regole e route per tunnel single-link

Per ogni WAN attiva (es. WAN4, tabella 103):

```bash
# Rule: traffico da LAN4 → table wan4
ip rule add from 172.16.4.0/30 table wan4 priority 1004

# Route: default via tunnel mpq4
ip route replace default dev mpq4 table wan4

# Route: raggiungere il VPS via WAN4 (necessario per il tunnel stesso)
GATEWAY=$(ip route show dev enp7s6 | awk '/default/ {print $3}')
ip route replace 172.238.232.223/32 via "$GATEWAY" dev enp7s6 table wan4
```

### 15.4 Route per tunnel multipath mp1 — tabella bd1

Il tunnel multipath mp1 utilizza una tabella di routing dedicata `bd1` (ID 120)
con VLAN 17 su enp6s20, completamente indipendente dalle tabelle wan1–wan6 e
dallo script watchdog `mpquic-lan-routing-check.sh`.

**Infrastruttura VLAN 17 (systemd-networkd):**

```ini
# /etc/systemd/network/26-vlan17.netdev
[NetDev]
Name=enp6s20.17
Kind=vlan

[VLAN]
Id=17
```

```ini
# /etc/systemd/network/27-bd1.network
[Match]
Name=enp6s20.17

[Network]
Address=172.16.17.1/30
LinkLocalAddressing=no
IPv6AcceptRA=no

[RoutingPolicyRule]
From=172.16.17.0/30
Table=120
Priority=1017
```

```ini
# /etc/systemd/network/20-lan1.network — aggiungere sotto [Network]:
VLAN=enp6s20.17
```

**Route bd1 (persistenti tramite systemd drop-in):**

```ini
# /etc/systemd/system/mpquic@mp1.service.d/bd1-route.conf
[Service]
ExecStartPost=/bin/sh -c "sleep 1 && ip route replace default dev mp1 table bd1 && ip route replace 172.16.17.0/30 dev enp6s20.17 table bd1"
```

**Schema routing:**

```
Sorgente 172.16.17.0/30 → rule 1017 → table bd1 → default dev mp1
                                                   → 172.16.17.0/30 dev enp6s20.17
```

**VPS — route di ritorno:**

```bash
ip route replace 172.16.17.0/30 dev mp1
```

**Verifica:**

```bash
# Client
ip route show table bd1
ip rule show | grep 1017

# VPS
ip route show | grep 172.16.17
```

### 15.5 Route VPS (server)

```bash
# Route di ritorno per ogni LAN transit
ip route add 172.16.1.0/30 dev mpq1
ip route add 172.16.2.0/30 dev mpq2
ip route add 172.16.3.0/30 dev mpq3
ip route add 172.16.4.0/30 dev mpq4
ip route add 172.16.5.0/30 dev mpq5
ip route add 172.16.6.0/30 dev mpq6

# Route per subnet mp1 (multipath, tabella bd1)
ip route add 172.16.17.0/30 dev mp1
```

### 15.6 Rendere le route permanenti

**Metodo 1: service systemd (raccomandato)**

Le route sono gestite dai service dedicati:

```bash
# Client: policy routing
sudo systemctl enable --now mpquic-routing.service

# VPS: route di ritorno
sudo systemctl enable --now mpquic-vps-routes.service
```

Questi service vengono eseguiti dopo l'avvio dei tunnel e ri-applicano tutte le
route necessarie.

**Metodo 2: post-up in /etc/network/interfaces**

Per route aggiuntive non gestite dai service:

```bash
auto enp7s6
iface enp7s6 inet dhcp
    post-up ip route replace default dev mpq4 table wan4 2>/dev/null || true
```

### 15.7 Verifica stato route dopo reboot

```bash
# Client — rule policies
ip rule show | egrep '100[1-6]'

# Client — route tables
for t in wan1 wan2 wan3 wan4 wan5 wan6; do
    echo "=== $t ==="
    ip route show table "$t"
done

# VPS — route di ritorno
ip route show | egrep '172\.16\.[1-6]\.0/30|10\.200\.'
```

---

## 16) Forwarding IP (server VPS)

Il forwarding IPv4 è necessario perché il VPS fa da gateway Internet per il
traffico tunnellizzato.

```bash
# Abilita forwarding permanente
echo 'net.ipv4.ip_forward = 1' | sudo tee /etc/sysctl.d/99-mpquic-forward.conf
sudo sysctl --system

# Verifica
sysctl net.ipv4.ip_forward
# Atteso: net.ipv4.ip_forward = 1
```

---

## 16.1) Tuning UDP Socket Buffers (entrambi i nodi)

MPQUIC configura automaticamente buffer da 7 MB per ogni socket UDP stripe
tramite `SetReadBuffer()` / `SetWriteBuffer()`. Tuttavia il kernel Linux
limita il buffer massimo ai valori di `net.core.rmem_max` e `net.core.wmem_max`
(default ~208 KB). Senza questo tuning, la chiamata viene silenziosamente
troncata e le burst Starlink possono causare drop a livello kernel.

```bash
# Imposta buffer max 7 MB (= 7340032 bytes, valore usato anche da quic-go)
cat <<'EOF' | sudo tee /etc/sysctl.d/99-mpquic-buffers.conf
# MPQUIC stripe: socket buffer 7 MB per prevenire drop durante burst Starlink
net.core.rmem_max = 7340032
net.core.wmem_max = 7340032
# Opzionale: aumenta anche il default (per tutti i socket, non solo mpquic)
net.core.rmem_default = 1048576
net.core.wmem_default = 1048576
EOF

sudo sysctl --system
```

**Verifica**:
```bash
sysctl net.core.rmem_max net.core.wmem_max
# Atteso:
# net.core.rmem_max = 7340032
# net.core.wmem_max = 7340032
```

**Nota**: questa configurazione è necessaria sia sul client che sul server VPS.
Senza di essa, i buffer effettivi restano a ~208 KB nonostante il codice
richieda 7 MB. Il tuning è persistente (sopravvive al reboot).

---

## 17) Certificati TLS

### 17.1 Generazione CA e certificati (una tantum)

```bash
# Genera CA
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout ca.key -out ca.crt -days 3650 -nodes \
  -subj "/CN=MPQUIC CA"

# Genera chiave + CSR server
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout server.key -out server.csr -nodes \
  -subj "/CN=mpquic-server"

# Firma con SAN
cat > san.ext <<EOF
subjectAltName = DNS:mpquic-server
EOF
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key \
  -CAcreateserial -out server.crt -days 3650 -extfile san.ext

# Cleanup
rm -f server.csr san.ext ca.srl
```

### 17.2 Distribuzione

```bash
# Server VPS
sudo install -d /etc/mpquic/tls
sudo install -m 0600 server.key /etc/mpquic/tls/server.key
sudo install -m 0644 server.crt /etc/mpquic/tls/server.crt
sudo install -m 0644 ca.crt /etc/mpquic/tls/ca.crt

# Client
sudo install -d /etc/mpquic/tls
sudo install -m 0644 ca.crt /etc/mpquic/tls/ca.crt
```

### 17.3 Verifica

```bash
# Verificare che il CN corrisponda a tls_server_name
openssl x509 -in /etc/mpquic/tls/server.crt -noout -subject -ext subjectAltName
# Atteso: CN = mpquic-server, SAN: DNS:mpquic-server
```

---

## 18) Servizio systemd — template e funzionamento

### 18.1 Template `mpquic@.service`

```ini
[Unit]
Description=MPQUIC tunnel instance %i
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-/etc/mpquic/global.env
EnvironmentFile=/etc/mpquic/instances/%i.env
ExecStartPre=/bin/sh -c '/usr/local/lib/mpquic/ensure_tun.sh "$TUN_NAME" "$TUN_CIDR" "${TUN_MTU:-1300}"'
ExecStartPre=/bin/sh -c '/usr/local/lib/mpquic/render_config.sh "%i"'
ExecStart=/usr/local/bin/mpquic --config /run/mpquic/%i.yaml
ExecStopPost=-/bin/sh -c 'ip link set dev "$TUN_NAME" down 2>/dev/null || true'
Restart=always
RestartSec=2
TimeoutStopSec=15
KillMode=mixed
KillSignal=SIGTERM
User=root
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
NoNewPrivileges=true
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

### 18.2 Flusso di avvio di un'istanza

1. `systemd` legge `global.env` (VPS_PUBLIC_IP) e `instances/%i.env` (TUN_NAME, TUN_CIDR, TUN_MTU)
2. `ensure_tun.sh` crea la TUN se non esiste, assegna IP e MTU, porta up
3. `render_config.sh` sostituisce `VPS_PUBLIC_IP` nel template `.yaml.tpl` e genera `/run/mpquic/%i.yaml`
4. `mpquic --config /run/mpquic/%i.yaml` avvia il processo con la configurazione renderizzata
5. Al termine, `ExecStopPost` porta down la TUN

### 18.3 Comandi operativi

```bash
# Avviare un'istanza
sudo systemctl start mpquic@mp1.service

# Avviare e abilitare al boot
sudo systemctl enable --now mpquic@mp1.service

# Fermare
sudo systemctl stop mpquic@mp1.service

# Restart
sudo systemctl restart mpquic@mp1.service

# Log
journalctl -u mpquic@mp1.service -n 100 --no-pager -f

# Stato tutte le istanze
for i in 1 2 3 4 5 6 mp1 cr5 df5 bk5; do
    printf "%-6s " "$i"
    systemctl is-active mpquic@$i.service 2>/dev/null || echo inactive
done
```

---

## 19) Aggiornamento software — `mpquic-update.sh`

Lo script di aggiornamento è il modo raccomandato per aggiornare il software:

```bash
sudo /usr/local/sbin/mpquic-update.sh
```

**Flusso completo:**

1. `git pull` dal repository
2. Se lo script stesso è cambiato → `exec` della nuova versione con `MPQUIC_UPDATE_SKIP_PULL=1`
3. `go build` del binario
4. Discovery delle istanze attive
5. Stop parallelo di tutte le istanze
6. `pkill -9` di eventuali processi residui
7. `rm -f` + `cp` del nuovo binario (evita ETXTBSY)
8. Install del template systemd aggiornato
9. Start parallelo di tutte le istanze
10. Health check post-deploy

---

## 20) Checklist post-installazione completa

### Client
```bash
# 1. Certificati
ls -l /etc/mpquic/tls/ca.crt

# 2. Configurazioni istanze
ls -l /etc/mpquic/instances/*.yaml.tpl /etc/mpquic/instances/*.env

# 3. Global env
cat /etc/mpquic/global.env

# 4. Binario
/usr/local/bin/mpquic --version 2>&1 || /usr/local/bin/mpquic --help 2>&1 | head -1

# 5. Servizi attivi
for i in 1 2 3 4 5 6 mp1; do
    printf "%-6s " "$i"
    systemctl is-active mpquic@$i.service 2>/dev/null || echo inactive
done

# 6. TUN interfaces
ip -br a | egrep 'mpq[1-6]|mp1|cr5|df5|bk5'

# 7. nftables
sudo nft list ruleset | head -30

# 8. Routing tables
ip rule show | egrep '100[1-6]'

# 9. Connettività tunnel
ping -I mp1 -c 3 10.200.17.254

# 10. Throughput
iperf3 -c 10.200.17.254 -p 5201 -t 5 -P 4 -R --bind-dev mp1
```

### Server VPS
```bash
# 1. Certificati
ls -l /etc/mpquic/tls/server.crt /etc/mpquic/tls/server.key /etc/mpquic/tls/ca.crt

# 2. Forwarding
sysctl net.ipv4.ip_forward

# 3. nftables
sudo nft list chain inet filter input | egrep '4500|4601|mpq|mt|mp'

# 4. Servizi attivi
for i in 1 2 3 4 5 6 mp1; do
    printf "%-6s " "$i"
    systemctl is-active mpquic@$i.service 2>/dev/null || echo inactive
done

# 5. Porte in ascolto
ss -lunp | egrep '4500|4601'

# 6. Route di ritorno
ip route show | egrep '172\.16\.[1-6]\.0/30|10\.200\.'
```

## 21) Metriche e osservabilità (Fase 5)

### 21.1 Architettura

Ogni istanza mpquic può esporre metriche su un server HTTP dedicato, **vincolato
all'IP tunnel** (non esposto su Internet). Gli endpoint disponibili sono:

| Endpoint | Formato | Uso |
|----------|---------|-----|
| `/metrics` | Prometheus text exposition | Scraping da Prometheus/Grafana |
| `/api/v1/stats` | JSON strutturato | Portali, script, AI/ML feedback |

Il binding sull'IP tunnel garantisce che le metriche siano raggiungibili **solo
attraverso il tunnel crittografato MPQUIC**, senza alcuna porta esposta su
Internet.

### 21.2 Configurazione

Aggiungere `metrics_listen: auto` al file YAML dell'istanza:

```yaml
# Server (es. /etc/mpquic/instances/mt4.yaml.tpl)
role: server
bind_ip: 0.0.0.0
remote_port: 45014
multi_conn_enabled: true
tun_name: mt4
tun_cidr: 10.200.14.254/24
metrics_listen: auto          # ← deriva 10.200.14.254:9090 da tun_cidr
log_level: info
tls_cert_file: /etc/mpquic/tls/server.crt
tls_key_file: /etc/mpquic/tls/server.key
```

```yaml
# Client (es. /etc/mpquic/instances/cr1.yaml.tpl)
role: client
bind_ip: if:enp7s6
remote_addr: VPS_PUBLIC_IP
remote_port: 45014
tun_name: cr1
tun_cidr: 10.200.14.1/24
metrics_listen: auto          # ← deriva 10.200.14.1:9090 da tun_cidr
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
```

**Valori possibili per `metrics_listen`:**

| Valore | Risultato |
|--------|-----------|
| `auto` | Estrae l'IP da `tun_cidr` e usa porta 9090 (raccomandato) |
| `10.200.14.254:9090` | Bind esplicito a IP e porta |
| (vuoto/assente) | Metriche disabilitate per questa istanza |

### 21.3 Installazione config sulle macchine

Dopo aver modificato i template nel repository:

```bash
# 1. Push delle modifiche
cd /opt/mpquic
git add deploy/config/
git commit -m "config: add metrics_listen to instances"
git push origin main
```

**Sul server VPS:**
```bash
cd /opt/mpquic && git pull

# Copia i config aggiornati (mt1 ha .tpl, mt4/5/6 hanno .yaml)
for i in mt1; do
  cp deploy/config/server/$i.yaml /etc/mpquic/instances/$i.yaml.tpl
done
for i in mt4 mt5 mt6; do
  cp deploy/config/server/$i.yaml /etc/mpquic/instances/$i.yaml
done

# Rebuild + restart
bash scripts/mpquic-update.sh /opt/mpquic
```

**Sul client:**
```bash
cd /opt/mpquic && git pull

# Copia i config aggiornati
for i in cr1 cr2 cr3 cr5; do
  cp deploy/config/client/$i.yaml /etc/mpquic/instances/$i.yaml.tpl
done

# Rebuild + restart
sudo bash scripts/mpquic-update.sh /opt/mpquic
```

### 21.4 Verifica

```bash
# Dal server → metriche server mt4 (Prometheus format)
curl http://10.200.14.254:9090/metrics

# Dal server → metriche server mt4 (JSON)
curl http://10.200.14.254:9090/api/v1/stats

# Verifica che la porta NON sia raggiungibile da Internet
# (questo deve fallire — nessuna porta esposta)
curl --connect-timeout 3 http://<VPS_PUBLIC_IP>:9090/metrics
# curl: (28) Connection timed out  ← OK, corretto

# Dal client → metriche server (attraverso il tunnel)
curl http://10.200.14.254:9090/api/v1/stats

# Dal server → metriche client cr1 (attraverso il tunnel)
curl http://10.200.14.1:9090/api/v1/stats
```

**Output atteso (JSON):**
```json
{
  "role": "server",
  "version": "4.2",
  "uptime_sec": 35.18,
  "sessions": [...],
  "total_tx_bytes": 123456,
  "total_rx_bytes": 789012,
  "total_tx_pkts": 100,
  "total_rx_pkts": 200
}
```

### 21.5 Metriche Prometheus esposte

**Globali:**

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `mpquic_uptime_seconds` | gauge | Uptime del processo |
| `mpquic_tx_bytes_total` | counter | Byte trasmessi totali |
| `mpquic_rx_bytes_total` | counter | Byte ricevuti totali |
| `mpquic_tx_packets_total` | counter | Pacchetti trasmessi totali |
| `mpquic_rx_packets_total` | counter | Pacchetti ricevuti totali |

**Per sessione (server, label: `session`, `peer`):**

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `mpquic_session_tx_bytes` | counter | Byte TX per sessione |
| `mpquic_session_rx_bytes` | counter | Byte RX per sessione |
| `mpquic_session_tx_packets` | counter | Pacchetti TX per sessione |
| `mpquic_session_rx_packets` | counter | Pacchetti RX per sessione |
| `mpquic_session_pipes` | gauge | Pipe attive per sessione |
| `mpquic_session_adaptive_m` | gauge | Parità FEC corrente (M) |
| `mpquic_session_fec_encoded` | counter | Gruppi FEC codificati (TX) |
| `mpquic_session_fec_recovered` | counter | Gruppi FEC recuperati (RX) |
| `mpquic_session_arq_nack_sent` | counter | NACK ARQ inviati |
| `mpquic_session_arq_retx_recv` | counter | Ritrasmissioni ARQ ricevute |
| `mpquic_session_arq_dup_filtered` | counter | Duplicati filtrati |
| `mpquic_session_loss_rate_pct` | gauge | Loss rate riportata dal peer (0-100) |
| `mpquic_session_uptime_seconds` | gauge | Uptime sessione |
| `mpquic_session_decrypt_fail` | counter | Fallimenti decrittazione |

**Per path (client, label: `path`, `bind`):**

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `mpquic_path_alive` | gauge | Path attivo (1) o down (0) |
| `mpquic_path_tx_packets` | counter | Pacchetti TX per path |
| `mpquic_path_rx_packets` | counter | Pacchetti RX per path |
| `mpquic_path_stripe_tx_bytes` | counter | Byte stripe TX per path |
| `mpquic_path_stripe_rx_bytes` | counter | Byte stripe RX per path |
| `mpquic_path_stripe_fec_recovered` | counter | Recuperi FEC stripe per path |

### 21.6 Note di sicurezza

- Il server metriche è **separato** da pprof (che resta su `--pprof 127.0.0.1:6060`, solo per debug)
- Il binding sull'IP tunnel (10.200.x.y) garantisce che **non** sia raggiungibile dall'esterno
- Non è necessaria alcuna regola nftables aggiuntiva: il tunnel stesso è la protezione
- Per ulteriore hardening, si può aggiungere una regola nftables:
  ```bash
  nft add rule inet filter input ip saddr != 10.200.0.0/16 tcp dport 9090 drop
  ```
