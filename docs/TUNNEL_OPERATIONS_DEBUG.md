# MPQUIC – Uso operativo e debug tunnel

Questa guida è la runbook pratica per esercire e debuggare i tunnel `mpq1..mpq6` su TBOX/MPQUIC.

## 0) Regola SSH operativa (IPS)

Host VPS canonicale: `vps-it-mpquic`.

Per evitare disconnessioni IPS, su VPS usare sempre sequenza interattiva:

```bash
ssh vps-it-mpquic
# esegui i comandi dentro la sessione
exit
```

Da evitare su VPS:

```bash
ssh vps-it-mpquic 'comando'
```

## 1) Uso operativo quotidiano

## 1.0 Aggiornamento repository (regola operativa)

Per aggiornare il software usare solo:

```bash
sudo /usr/local/sbin/mpquic-update.sh
```

Non usare `scp` per aggiornamenti standard: per evitare disallineamenti, la fonte di verità resta il repository Git.

## 1.1 Stato rapido client
```bash
for i in 1 2 3 4 5 6; do
  printf "@%s=" "$i"
  systemctl is-active mpquic@$i.service || true
done

ip -br a | egrep '^enp7s[3-8]|^mpq[1-6]'
ss -unap | grep mpquic || true
```

## 1.2 Stato rapido VPS
```bash
for i in 1 2 3 4 5 6; do
  printf "@%s=" "$i"
  systemctl is-active mpquic@$i.service || true
done

ip -br a | egrep '^mpq[1-6]'
ss -lunp | egrep '4500[1-6]' || true
```

## 1.3 Restart completo dopo restart rete

Client:
```bash
for i in 1 2 3 4 5 6; do systemctl restart mpquic@$i.service; done
systemctl restart mpquic-routing.service
systemctl restart mpquic-watchdog.service
```

VPS:
```bash
for i in 1 2 3 4 5 6; do systemctl restart mpquic@$i.service; done
systemctl restart mpquic-vps-routes.service
systemctl restart nftables
```

## 1.3-bis Problema ricorrente VM/OpenWRT (procedura iniziale)

Sintomo osservato più volte: tunnel formalmente attivi ma traffico non instradato correttamente tra VM MPQUIC e router OpenWRT finché non viene ripristinata la rete della VM.

Ordine operativo obbligatorio:
1. restart network lato VM MPQUIC (prima azione)
2. restart servizi MPQUIC/routing
3. verifiche healthcheck + route table
4. reboot VM solo se i passi 1..3 non risolvono

Client (first response):
```bash
systemctl restart networking || true
ifreload -a || true
for i in 1 2 3 4 5 6; do systemctl restart mpquic@$i.service; done
systemctl restart mpquic-routing.service
systemctl restart mpquic-watchdog.timer
/usr/local/sbin/mpquic-healthcheck.sh client fix
/usr/local/sbin/mpquic-lan-routing-check.sh fix all
```

Se ancora KO:
```bash
reboot
```

## 1.4 Check rapido strutturato (con auto-fix)

Client:
```bash
sudo /usr/local/sbin/mpquic-healthcheck.sh client check
sudo /usr/local/sbin/mpquic-healthcheck.sh client fix
```

Server:
```bash
sudo /usr/local/sbin/mpquic-healthcheck.sh server check
sudo /usr/local/sbin/mpquic-healthcheck.sh server fix
```

Regola firewall obbligatoria su VPS (nftables input policy drop):
```bash
nft list chain inet filter input
# deve esistere una riga equivalente a:
# udp dport 45001-45006 accept
```

## 1.5 Smoke test multipath (Fase 4 Step 1, sperimentale)

Config template di riferimento:
- `deploy/config/client/multipath.yaml`

Client (esegue test non distruttivo con TUN dedicata `mpqm`):
```bash
export VPS_PUBLIC_IP=<IP_VPS>
sudo /usr/local/sbin/mpquic-multipath-smoke.sh
```

Atteso:
- output `smoke test PASS`
- nel log compaiono `connected multipath paths=` e almeno un `path up name=`

Verifica telemetria path-level (client):
```bash
journalctl -u mpquic@4.service -n 200 --no-pager | grep 'path telemetry' || true
```

Chiavi da leggere nel log telemetrico:
- `state=up|down`
- `tx_pkts`, `rx_pkts`
- `tx_err`, `rx_err`
- `fails`, `cooldown_until`, `last_up`, `last_down`

Tuning QoS path-aware (attuale):
- regola `priority` e `weight` in `multipath_paths`
- riavvia il processo che usa quella config
- verifica nei log che i path desiderati risultino preferiti/stabili

Controllo VPS (sequenza SSH obbligatoria):
```bash
ssh vps-it-mpquic
ss -lunp | egrep '4500[4-6]'
journalctl -u mpquic@4.service -n 30 --no-pager
journalctl -u mpquic@5.service -n 30 --no-pager
journalctl -u mpquic@6.service -n 30 --no-pager
exit
```

## 2) Mapping e comportamento atteso

- `LAN1 (172.16.1.0/30)` -> `mpq1` -> QUIC `udp/45001` su `enp7s3`
- `LAN2 (172.16.2.0/30)` -> `mpq2` -> QUIC `udp/45002` su `enp7s4`
- `LAN3 (172.16.3.0/30)` -> `mpq3` -> QUIC `udp/45003` su `enp7s5`
- `LAN4 (172.16.4.0/30)` -> `mpq4` -> QUIC `udp/45004` su `enp7s6`
- `LAN5 (172.16.5.0/30)` -> `mpq5` -> QUIC `udp/45005` su `enp7s7`
- `LAN6 (172.16.6.0/30)` -> `mpq6` -> QUIC `udp/45006` su `enp7s8`

Nessun failover cross-tunnel: se WANx non è disponibile, il tunnel x deve fermarsi (o restare non connesso).

## 3) Debug per sintomo

## 3.0 Multipath: rumore "superseded" durante test

Se esegui smoke multipath su porte già usate da istanze `mpquic@X` attive, il server può chiudere sessioni precedenti con evento `superseded`.

Per test pulito:
1. stop temporaneo delle istanze client in conflitto con le porte usate dal multipath
2. esecuzione smoke test
3. riavvio istanze baseline

## 3.1 Tunnel `active` ma non passa traffico

Client:
```bash
journalctl -u mpquic@4.service -n 80 --no-pager
ip route show table 103
ip route show table 104
ip route show table 105
```

Controlla:
- presenza `default dev mpqX` nella tabella corretta
- presenza route `/32` verso VPS sulla WAN associata

VPS:
```bash
ip route get 172.16.4.2
ip route get 172.16.5.2
ip route get 172.16.6.2
nft list ruleset | sed -n '1,220p'
```

Controlla:
- ritorno verso LAN su `mpqX` (non su `eth0`)
- NAT attivo su `eth0` per subnet `172.16.x.0/30`
- apertura UDP `45001-45006` nella chain `inet filter input` (altrimenti client in timeout continuo)

## 3.2 Messaggio `no ipv4 found on enp7sX`

Significa che la WAN associata non ha IPv4 DHCP valida.

Check:
```bash
ip -4 -br a show dev enp7s3
ip -4 -br a show dev enp7s4
ip -4 -br a show dev enp7s5
ip -4 -br a show dev enp7s6
ip -4 -br a show dev enp7s7
ip -4 -br a show dev enp7s8
```

Se mancante: il tunnel relativo non potrà connettersi finché WAN non torna up.

## 3.3 Verifica 1:1 reale (no cross-tunnel)

Esempio LAN4:

OpenWRT:
```bash
mwan3 use SL4 ping 8.8.8.8
```

Client (deve vedere traffico su `mpq4`):
```bash
tcpdump -ni mpq4
tcpdump -ni mpq5
tcpdump -ni mpq6
```

Atteso:
- pacchetti nel solo `mpq4`
- assenza di pacchetti equivalenti su `mpq5/mpq6`

Verifica incapsulamento QUIC su WAN4:
```bash
tcpdump -ni enp7s6 udp port 45004
```

## 3.4 Auto-heal non interviene dopo flap WAN

Check:
```bash
systemctl is-active mpquic-watchdog.timer
systemctl status mpquic-watchdog.timer --no-pager
journalctl -u mpquic-watchdog.service -n 50 --no-pager
ls -l /etc/network/if-up.d/mpquic-auto /etc/network/if-post-down.d/mpquic-auto
```

Recovery:
```bash
systemctl restart mpquic-watchdog.timer
systemctl restart mpquic-watchdog.service
```

Copertura attuale watchdog client:
- single-path: `1..6`
- multipath: `mp1`
- multi-class VLAN: `cr4..6`, `br4..6`, `df4..6`

## 3.5 Su VPS i tunnel restano down

Check:
```bash
systemctl is-active mpquic-server-watchdog.timer
systemctl status mpquic-server-watchdog.timer --no-pager
journalctl -u mpquic-server-watchdog.service -n 50 --no-pager
for i in 1 2 3 4 5 6 mp1 mt1 mt4 mt5 mt6; do
  printf "@%s=" "$i"
  systemctl is-active mpquic@$i.service || true
done
```

Nota: il watchdog verifica sia `is-active` sia salute TUN (`TUN_NAME/TUN_CIDR` presenti e interfaccia `UP`).
Se il processo è attivo ma il tunnel è rotto (es. `write tun: input/output error`), forza restart dell'istanza.

Recovery:
```bash
for i in 1 2 3 4 5 6 mp1 mt1 mt4 mt5 mt6; do systemctl restart mpquic@$i.service; done
systemctl restart mpquic-vps-routes.service
systemctl restart mpquic-server-watchdog.timer
systemctl restart mpquic-server-watchdog.service
```

## 3.6 Tunnel cr/br/df in stato down

Check rapido su VM MPQUIC:
```bash
for s in cr4 cr5 cr6 br4 br5 br6 df4 df5 df6; do
  printf "%s: enabled=%s active=%s\n" \
    "$s" \
    "$(systemctl is-enabled mpquic@$s.service 2>/dev/null || echo no)" \
    "$(systemctl is-active mpquic@$s.service 2>/dev/null || echo down)"
done
```

Restart mirato:
```bash
for s in cr4 cr5 cr6 br4 br5 br6 df4 df5 df6; do
  systemctl restart mpquic@$s.service
done
```

Nota: per la demo, i tunnel cr/br/df possono essere lasciati intenzionalmente spenti.
In quel caso un alert Grafana su queste istanze va marcato come expected/maintenance.

## 4) TLS debug

## 4.1 File certificati

Server:
```bash
ls -l /etc/mpquic/tls/server.crt /etc/mpquic/tls/server.key /etc/mpquic/tls/ca.crt
```

Client:
```bash
ls -l /etc/mpquic/tls/ca.crt
grep -R "tls_" /etc/mpquic/instances/*.yaml.tpl /etc/mpquic/instances/*.yaml
```

## 4.2 Errori tipici

- `x509: certificate signed by unknown authority`
  - `ca.crt` assente/non allineato sul client
- `tls: failed to find any PEM data`
  - file certificato/chiave corrotti o path errato
- mismatch `tls_server_name`
  - CN certificato diverso da valore in YAML client

## 5) Raccolta evidenze per troubleshooting

Client:
```bash
date
hostname
for i in 1 2 3 4 5 6 mp1 cr4 cr5 cr6 br4 br5 br6 df4 df5 df6; do systemctl is-active mpquic@$i.service || true; done
ip -br a | egrep '^enp7s[3-8]|^mpq[1-6]'
ip rule show | egrep '100[1-6]'
ip route show table 100
ip route show table 101
ip route show table 102
ip route show table 103
ip route show table 104
ip route show table 105
ss -unap | grep mpquic || true
```

VPS:
```bash
date
hostname
for i in 1 2 3 4 5 6 mp1 mt1 mt4 mt5 mt6; do systemctl is-active mpquic@$i.service || true; done
ip -br a | egrep '^mpq[1-6]|^eth0'
ip route show | egrep '172\.16\.[1-6]\.0/30|10\.200\.'
nft list ruleset | sed -n '1,220p'
```

---

## Appendice A – Installazione watchdog

### A.1 Client — tunnel watchdog + hook eventi interfaccia

```bash
sudo install -m 0755 scripts/mpquic-tunnel-watchdog.sh /usr/local/lib/mpquic/mpquic-tunnel-watchdog.sh
sudo install -m 0755 scripts/mpquic-if-event.sh /usr/local/lib/mpquic/mpquic-if-event.sh
sudo install -m 0644 deploy/systemd/mpquic-watchdog.service /etc/systemd/system/mpquic-watchdog.service
sudo install -m 0644 deploy/systemd/mpquic-watchdog.timer /etc/systemd/system/mpquic-watchdog.timer
sudo install -m 0755 deploy/hooks/mpquic-ifupdown-hook /etc/network/if-up.d/mpquic-auto
sudo install -m 0755 deploy/hooks/mpquic-ifupdown-hook /etc/network/if-post-down.d/mpquic-auto
sudo systemctl daemon-reload
sudo systemctl enable --now mpquic-watchdog.timer
```

Verifica:
```bash
systemctl is-active mpquic-watchdog.timer
journalctl -u mpquic-watchdog.service -n 20 --no-pager
```

### A.2 VPS — server watchdog

```bash
sudo install -m 0755 scripts/mpquic-server-watchdog.sh /usr/local/lib/mpquic/mpquic-server-watchdog.sh
sudo install -m 0644 deploy/systemd/mpquic-server-watchdog.service /etc/systemd/system/mpquic-server-watchdog.service
sudo install -m 0644 deploy/systemd/mpquic-server-watchdog.timer /etc/systemd/system/mpquic-server-watchdog.timer
sudo systemctl daemon-reload
sudo systemctl enable --now mpquic-server-watchdog.timer
```

Verifica:
```bash
systemctl is-active mpquic-server-watchdog.timer
journalctl -u mpquic-server-watchdog.service -n 50 --no-pager
```

---

## Test chaos mp1 fast failover A+E

> Combo A+E (commit `4e36d0e` + fixup `e18dd08`): keepalive 1s, healthCheckLoop
> 500 ms, soglia degraded 3 s, recovery 1 s. Obiettivo: blackhole ≤ 3.5 s su
> failover stripe mp1 (Starlink wan5+wan6) anche se carrier resta UP ma il
> backhaul è morto.

Le ricette seguenti vanno eseguite **in produzione dal tech-lead** durante una
run iperf3 (o ping ad alta cadenza) attraverso `mp1`. Tutti i comandi
distruttivi includono il cleanup; eseguire sempre il cleanup anche in caso di
errore nel test.

### Recipe 1 — `nft` drop UDP su una WAN (carrier rimane UP)

Esegui sull'OpenWrt CPE, mentre un iperf3 su `mp1` è in corso lato server:

```bash
# Setup chaos: blocca il traffico UDP della pipe 0 su wan6 (sport 6201).
# Adatta sport al pipe configurato (vedi cmd/mpquic/stripe.go: stripeBaseSport).
sudo nft add table inet chaos
sudo nft 'add chain inet chaos out { type filter hook output priority 0; }'
sudo nft 'add rule inet chaos out oifname "wan6" udp sport 6201 drop'

# ... lascia girare 60 s e raccogli metriche (vedi Recipe 4) ...

# Cleanup OBBLIGATORIO
sudo nft delete table inet chaos
```

### Recipe 2 — `tc netem` con loss 100 % su una pipe

Equivalente più aggressivo: disabilita interamente il device per il flusso UDP.

```bash
sudo tc qdisc add dev wan6 root netem loss 100%
# ... 60 s di test ...
sudo tc qdisc del dev wan6 root
```

> Nota: `tc netem` impatta anche eventuali flow non-stripe sul device. Preferire
> `nft` (Recipe 1) quando la WAN è condivisa.

### Recipe 3 — Acceptance criteria

| Metrica | Soglia | Sorgente |
|---|---|---|
| Detection blackhole `mp1` | ≤ 3.5 s | `mpquic_path_degraded_since_seconds` ≥ 3 (poi ricovero) |
| Loss totale finestra 60 s, policy `balanced`, 1 path su 2 down | ≤ 5 % | `iperf3 -u` o `ping` |
| Tempo di fail-back dopo cleanup | ≤ 2 s | `mpquic_path_failback_total` += 1 |
| Restart del servizio durante chaos | nessuno | `systemctl show mpquic@mp1 -p NRestarts` invariato |
| Deadlock / panic | nessuno | `journalctl -u mpquic@mp1 --since "5 min ago" \| grep -E "panic\|fatal"` |
| Throughput iperf3 (recovery dopo chaos) | ≥ 80 % nominal entro 5 s | `iperf3 -i 1` |

### Recipe 4 — Lettura metriche live durante il test

```bash
watch -n 1 'curl -s http://10.10.11.100:9090/metrics \
  | grep -E "mpquic_path_(alive|degraded|last_rx|blackhole|failover|failback)" \
  | head -40'
```

Snapshot sincronizzato con il chaos (eseguire in parallelo a Recipe 1/2):

```bash
for i in $(seq 1 60); do
  ts=$(date -u +%H:%M:%S)
  echo "=== T+${i}s ${ts} ==="
  curl -s http://10.10.11.100:9090/metrics \
    | grep -E "^mpquic_path_(alive|degraded|degraded_since_seconds|blackhole_seconds_total|failover_total|failback_total)\{path=\"mp1\"" 
  sleep 1
done | tee /tmp/chaos_mp1_metrics.log
```

Esempio di output atteso al momento del flap:

```
mpquic_path_alive{path="mp1",bind="if:enp7s8"} 1
mpquic_path_degraded{path="mp1",bind="if:enp7s8"} 1
mpquic_path_degraded_since_seconds{path="mp1",bind="if:enp7s8"} 3.215
mpquic_path_failover_total{path="mp1",bind="if:enp7s8"} 1
mpquic_path_failback_total{path="mp1",bind="if:enp7s8"} 0
```

Dopo il cleanup (entro ≤ 2 s):

```
mpquic_path_degraded{path="mp1",bind="if:enp7s8"} 0
mpquic_path_degraded_since_seconds{path="mp1",bind="if:enp7s8"} 0
mpquic_path_failback_total{path="mp1",bind="if:enp7s8"} 1
mpquic_path_blackhole_seconds_total{path="mp1",bind="if:enp7s8"} 3.215
```

### Note operative

- Il `mpquic-tunnel-watchdog` ha la sua soglia di restart (vedi
  `/etc/default/mpquic-watchdog`); verificare che non scatti un restart durante
  i 60 s di chaos. Se scatta, il test è invalidato (rumore esterno).
- Lo stesso test va ripetuto contro `wan5` (mirror simmetrico) per validare
  entrambi i path.
- I valori `mpquic_path_blackhole_seconds_total` sono cumulativi: confrontare il
  delta pre/post chaos, non il valore assoluto.
