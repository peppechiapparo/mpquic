# Operativa routing 1:1 e NAT VPS

Questa guida rende persistente la logica validata in test:
- Client: `LANx -> mpqx` (no failover)
- VPS: forward + NAT verso Internet
- VPS: route di ritorno per `172.16.1.0/30 ... 172.16.6.0/30` su `mpq1..mpq6`

## 1) Client MPQUIC — policy routing 1:1

Installazione script + service:
```bash
sudo install -m 0755 scripts/mpquic-policy-routing.sh /usr/local/sbin/mpquic-policy-routing.sh
sudo install -m 0644 deploy/systemd/mpquic-routing.service /etc/systemd/system/mpquic-routing.service
sudo systemctl daemon-reload
sudo systemctl enable --now mpquic-routing.service
```

Verifica:
```bash
systemctl is-active mpquic-routing.service
ip rule show | egrep '100[1-6]'
ip route show table 103
ip route show table 104
ip route show table 105
```

Atteso con WAN4/5/6 disponibili:
- table 103: `default dev mpq4` + route `/32` verso VPS su `enp7s6`
- table 104: `default dev mpq5` + route `/32` verso VPS su `enp7s7`
- table 105: `default dev mpq6` + route `/32` verso VPS su `enp7s8`
- table 100..102: `blackhole default` (finché WAN1..3 non hanno IPv4)

## 2) VPS — NAT e forwarding persistenti

Abilita forwarding persistente:
```bash
echo 'net.ipv4.ip_forward = 1' | sudo tee /etc/sysctl.d/99-mpquic-forward.conf
sudo sysctl --system
```

Installa `nftables` policy:
```bash
sudo install -d /etc
sudo install -m 0644 deploy/nftables/mpquic-vps.nft /etc/nftables.conf
sudo nft -f /etc/nftables.conf
sudo systemctl enable --now nftables
```

Verifica:
```bash
systemctl is-active nftables
nft list ruleset | sed -n '1,220p'
```

## 3) VPS — route di ritorno LAN sui tunnel

Installazione script + service:
```bash
sudo install -m 0755 scripts/mpquic-vps-routes.sh /usr/local/sbin/mpquic-vps-routes.sh
sudo install -m 0644 deploy/systemd/mpquic-vps-routes.service /etc/systemd/system/mpquic-vps-routes.service
sudo systemctl daemon-reload
sudo systemctl enable --now mpquic-vps-routes.service
```

Verifica:
```bash
systemctl is-active mpquic-vps-routes.service
ip route get 172.16.4.2
ip route get 172.16.5.2
ip route get 172.16.6.2
```

Atteso:
- `172.16.4.2 dev mpq4`
- `172.16.5.2 dev mpq5`
- `172.16.6.2 dev mpq6`

## 4) Test funzionale 1:1

Da OpenWRT (esempio):
```bash
mwan3 use SL4 ping 8.8.8.8
mwan3 use SL5 ping 8.8.4.4
mwan3 use SL6 ping 1.1.1.1
```

Da client MPQUIC:
```bash
tcpdump -ni mpq4
tcpdump -ni mpq5
tcpdump -ni mpq6
```

Verifica incapsulamento QUIC su WAN client:
```bash
tcpdump -ni enp7s6 udp port 45004
tcpdump -ni enp7s7 udp port 45005
tcpdump -ni enp7s8 udp port 45006
```
