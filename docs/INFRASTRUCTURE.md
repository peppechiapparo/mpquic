# MPQUIC Infrastructure Reference

> **REGOLA CRITICA**: mpquic gira SOLO su VM MPQUIC (10.10.11.100) e VPS (172.238.232.223).  
> **OpenWrt (10.10.11.254) è SOLO un router** — esegue mwan3, nftables, LuCI.  
> Non eseguire MAI comandi mpquic, systemctl mpquic@, o diagnostica tunnel su OpenWrt.

---

## 1. Inventario Host

| Host | IP | Ruolo | OS | SSH |
|------|-----|-------|-----|-----|
| **VM MPQUIC** (client) | 10.10.11.100 | Tunnel client, binari mpquic, servizi systemd | Debian 12 (Proxmox VM 200) | `ssh root@10.10.11.100` |
| **OpenWrt** (router) | 10.10.11.254 | Router, mwan3, nftables, VLAN trunk, LuCI | OpenWrt 24.10 x86_64 | `ssh root@10.10.11.254` |
| **VPS Server** | 172.238.232.223 | Tunnel server, binari mpquic server-side | Ubuntu 24.04 | `ssh vps-it-mpquic` (no one-liner, IPS attiva) |
| **Proxmox Host** | 10.10.11.2 | Hypervisor (VM 200, CT 201, CT 202) | Proxmox VE 8.x | `ssh root@10.10.11.2` |
| **Prometheus** | 10.10.11.201 | Monitoraggio metriche | Debian 12 LXC (CT 201) | — |
| **Grafana** | 10.10.11.202 | Dashboard | Debian 12 LXC (CT 202) | — |

---

## 2. Cosa gira DOVE

### VM MPQUIC (10.10.11.100) — il cuore del sistema

| Componente | Descrizione |
|-----------|-------------|
| `mpquic@{1..6}` | Servizi systemd, 1 tunnel QUIC single-path per WAN (porte 45001-45006) |
| `mpquic@mp1` | Tunnel multipath stripe bonding (WAN5+WAN6, porte 45017/46017) |
| `mpquic@cr4..6, br4..6, df4..6` | 9 tunnel VLAN multi-tunnel-per-link |
| `mpquic-mgmt` | Daemon gestione API REST (:8080) |
| `mpquic-routing.service` | Policy routing per binding WAN |
| `mpquic-watchdog` | Health check + auto-recovery tunnel |
| `wan-watchdog` | Rileva cambio IP DHCP sulle WAN Starlink |
| `systemd-networkd` | Configurazione rete (WAN DHCP, LAN static) |

**Dispositivi TUN** (creati qui, NON su OpenWrt):
- `mpq1`..`mpq6` — tunnel single-path
- `mp1` — tunnel multipath bonding
- `cr4`..`cr6`, `br4`..`br6`, `df4`..`df6` — tunnel multi-class VLAN

**Path repo**: `/opt/mpquic` (produzione), `/opt/SATCOMVAS/src/mpquic` (dev)

### OpenWrt (10.10.11.254) — solo routing

| Componente | Descrizione |
|-----------|-------------|
| mwan3 | Multi-WAN policy routing + health tracking |
| nftables/fw4 | Firewall, DSCP marking per traffic class |
| VLAN trunk | 9 VLAN per classi di traffico (cr/br/df × WAN4-6) |
| LuCI + rpcd | Interfaccia web, chiama API mpquic-mgmt su VM |

**NON gira su OpenWrt**: mpquic, systemd, tunnel TUN, mpquic-mgmt.

### VPS Server (172.238.232.223) — endpoint remoto

| Componente | Descrizione |
|-----------|-------------|
| `mpquic@{1..6}` | Server-side dei tunnel QUIC |
| `mpquic@mp1` | Server-side multipath bonding |
| `mpquic@mt1, mt4, mt5, mt6` | Server-side multi-conn per stack VLAN/classi |
| nftables | Firewall con IPS (attenzione: no SSH one-liner) |

Nota: i tunnel `cr/br/df` sulla VM MPQUIC possono essere lasciati spenti se non
usati nella demo; questo non indica un fault se pianificato.

---

## 3. Interfacce di Rete — VM MPQUIC

| Gruppo | Interfacce | Ruolo | IP |
|--------|-----------|-------|-----|
| MGMT | enp6s18, enp6s19 | Management/SSH | 10.10.11.100, 10.10.10.100 |
| LAN transit | enp6s20-23, enp7s1-2 | Collegamento verso OpenWrt | 172.16.{1-6}.1/30 |
| VLAN | enp6s20.17 | Transit dedicato mp1/BOND1 | 172.16.17.1/30 |
| WAN | enp7s3-8 | Uplink Starlink (DHCP, CGNAT) | Dinamico |

### Mappatura WAN → Interfaccia
| WAN | Interfaccia | Tipo |
|-----|-----------|------|
| WAN1 | enp7s3 | Starlink |
| WAN2 | enp7s4 | Starlink |
| WAN3 | enp7s5 | Starlink |
| WAN4 | enp7s6 | Starlink |
| WAN5 | enp7s7 | Starlink |
| WAN6 | enp7s8 | Starlink |

---

## 4. Topologia di Rete

```
              Internet
                 |
         ┌───────────────┐
         │  VPS Server    │
         │ 172.238.232.223│
         └───────┬────────┘
                 │ QUIC tunnels (UDP)
     ┌───────────┼───────────┐
     │ WAN5      │ WAN6      │ ... WAN1-4
     │ enp7s7    │ enp7s8    │
┌────┴───────────┴───────────┴────┐
│         VM MPQUIC               │
│         10.10.11.100            │
│  TUN: mpq1-6, mp1, cr/br/df    │
│  Services: mpquic@*, mgmt, etc  │
└────────────┬────────────────────┘
             │ LAN transit 172.16.x.0/30
             │ VLAN trunk (enp6s20.17 per mp1)
┌────────────┴────────────────────┐
│         OpenWrt Router          │
│         10.10.11.254            │
│  mwan3, nftables, VLAN, LuCI   │
│  Interfaces: SL1-6, BOND1      │
└────────────┬────────────────────┘
             │ LAN
         Rete locale
```

---

## 5. mwan3 su OpenWrt — Stato attuale

### Interfacce mwan3
| Interfaccia | Stato | Tracking | Note |
|------------|-------|----------|------|
| SL1-SL4 | OFFLINE | — | WAN 1-4 non attivi |
| SL5 | ONLINE | ping 8.8.8.8 + 1.1.1.1 | Starlink attivo |
| SL6 | ONLINE | ping 8.8.8.8 + 1.1.1.1 | Starlink attivo |
| BOND1 | ONLINE | ping 8.8.8.8 + 1.1.1.1 (interval=30) | Tunnel bonding mp1, uptime 765h+ |

### Config BOND1 attuale (UCI)
```
mwan3.BOND1=interface
mwan3.BOND1.initial_state='online'
mwan3.BOND1.track_ip='8.8.8.8' '1.1.1.1'
mwan3.BOND1.interval='30'
mwan3.BOND1.down='5'
mwan3.BOND1.up='5'
```

### Policy mwan3
- **BALANCED**: usa tutti gli SL + BOND1 con pesi/metriche
- **FAILOVER**: failover tra SL e BOND1

---

## 6. Procedure di Deploy

### Aggiornare mpquic su VM MPQUIC o VPS
```bash
# Su VM MPQUIC:
ssh root@10.10.11.100
sudo /opt/mpquic/scripts/mpquic-update.sh /opt/mpquic

# Su VPS (sessione interattiva, NO one-liner per IPS):
ssh vps-it-mpquic
sudo /opt/mpquic/scripts/mpquic-update.sh /opt/mpquic
```

### Deployare script OpenWrt
```bash
scp deploy/openwrt/XX-script.sh root@10.10.11.254:/tmp/
ssh root@10.10.11.254 'sh /tmp/XX-script.sh'
```

### Ricaricare mwan3
```bash
ssh root@10.10.11.254 '/etc/init.d/mwan3 restart'
```

---

## 7. Regole Operative

1. **Mai** eseguire `mpquic`, `systemctl mpquic@*`, `ip link show mpqX` su OpenWrt
2. **Mai** SSH one-liner su VPS (IPS blocca) — usare sessione interattiva
3. I device TUN (`mpq1-6`, `mp1`, `cr/br/df*`) esistono SOLO su VM MPQUIC
4. OpenWrt vede i tunnel come interfacce di rete via transit VLAN, non come device locali
5. Per diagnostica tunnel: `ssh root@10.10.11.100` e poi `ip link`, `wg show`, `journalctl -u mpquic@*`
6. Per diagnostica routing mwan3: `ssh root@10.10.11.254` e poi `mwan3 interfaces`, `mwan3 policies`
