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

## 2) Mapping e comportamento atteso

- `LAN1 (172.16.1.0/30)` -> `mpq1` -> QUIC `udp/45001` su `enp7s3`
- `LAN2 (172.16.2.0/30)` -> `mpq2` -> QUIC `udp/45002` su `enp7s4`
- `LAN3 (172.16.3.0/30)` -> `mpq3` -> QUIC `udp/45003` su `enp7s5`
- `LAN4 (172.16.4.0/30)` -> `mpq4` -> QUIC `udp/45004` su `enp7s6`
- `LAN5 (172.16.5.0/30)` -> `mpq5` -> QUIC `udp/45005` su `enp7s7`
- `LAN6 (172.16.6.0/30)` -> `mpq6` -> QUIC `udp/45006` su `enp7s8`

Nessun failover cross-tunnel: se WANx non è disponibile, il tunnel x deve fermarsi (o restare non connesso).

## 3) Debug per sintomo

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
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service || true; done
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
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service || true; done
ip -br a | egrep '^mpq[1-6]|^eth0'
ip route show | egrep '172\.16\.[1-6]\.0/30|10\.200\.'
nft list ruleset | sed -n '1,220p'
```
