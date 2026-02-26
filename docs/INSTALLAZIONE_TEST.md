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
dataplane_config_file: ./dataplane.yaml
```

E crea `dataplane.yaml` nello stesso path del file applicativo:
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

Esempio verifica:
```bash
curl -sS -H 'Authorization: Bearer change-me' http://127.0.0.1:19090/healthz
curl -sS -H 'Authorization: Bearer change-me' http://127.0.0.1:19090/dataplane
```

### Verifica operativa
Dopo riavvio istanza multipath, controlla:
```bash
journalctl -u mpquic@4.service -n 200 --no-pager | egrep 'path telemetry|class telemetry'
```

Per schema completo, pattern QoS e flusso orchestrator esterno: `docs/DATAPLANE_ORCHESTRATOR.md`.

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
