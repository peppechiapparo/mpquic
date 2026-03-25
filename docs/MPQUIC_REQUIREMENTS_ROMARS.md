# Requisiti Tecnici — Integrazione VM Tunnel MPQUIC

**Progetto**: ROMARS  
**Data**: 2026-06-11  
**Versione**: 1.0  
**Classificazione**: Riservato — Solo per il fornitore designato  

---

## Indice

1. [Scopo del Documento](#1-scopo-del-documento)
2. [Architettura di Riferimento](#2-architettura-di-riferimento)
3. [Specifiche VM](#3-specifiche-vm)
4. [Interfacce di Rete](#4-interfacce-di-rete)
5. [VLAN](#5-vlan)
6. [Indirizzamento IP](#6-indirizzamento-ip)
7. [Routing e Policy Routing](#7-routing-e-policy-routing)
8. [NAT e Firewall (nftables)](#8-nat-e-firewall-nftables)
9. [Tunnel — Nomenclatura e Addressing](#9-tunnel--nomenclatura-e-addressing)
10. [Configurazione Tunnel (YAML)](#10-configurazione-tunnel-yaml)
11. [Gestione Servizi (systemd)](#11-gestione-servizi-systemd)
12. [Watchdog e Health-Check](#12-watchdog-e-health-check)
13. [Management REST API](#13-management-rest-api)
14. [Metriche e Prometheus](#14-metriche-e-prometheus)
15. [Validazione e Test](#15-validazione-e-test)
16. [Supporto L3](#16-supporto-l3)
17. [Deliverable e Acceptance Criteria](#17-deliverable-e-acceptance-criteria)

---

## 1. Scopo del Documento

Questo documento definisce i requisiti tecnici che la VM tunnel MPQUIC fornita
dal fornitore deve soddisfare per integrarsi nell'infrastruttura TBOX/OpenWrt
del progetto ROMARS.

La VM deve essere **funzionalmente intercambiabile** con l'attuale slot VM della
TBOX: stesse interfacce di rete, stessa nomenclatura, stesso piano di
indirizzamento, stesse API di gestione, stessi formati metriche.

Il fornitore è libero nella scelta dell'implementazione interna (linguaggio,
librerie, architettura del codice) purché le interfacce esterne — rete, API,
metriche, servizi systemd — siano compatibili con quanto specificato di seguito.

---

## 2. Architettura di Riferimento

```
┌─────────────────────────────────────────────────────────────────┐
│                       TBOX (Server fisico)                      │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │                    VM Tunnel (Debian 12)                  │  │
│  │                                                           │  │
│  │   6× WAN (DHCP)         ─────────>   Internet (Starlink) │  │
│  │   6× LAN (static /30)  <─────────   OpenWrt router       │  │
│  │   2× MGMT (static)     <─────────>  Rete gestione        │  │
│  │                                                           │  │
│  │   Tunnel QUIC client per ogni WAN ──> VPS Server remoto   │  │
│  │   TUN interfaces (mpqN) ──> routing policy ──> LAN       │  │
│  │                                                           │  │
│  │   Management API (REST) ──> LuCI / orchestrator           │  │
│  │   Metrics (HTTP/JSON)   ──> Prometheus / Grafana          │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │                 OpenWrt Router (x86_64)                   │  │
│  │                                                           │  │
│  │   6× LAN ports ─── /30 point-to-point ──> VM tunnel      │  │
│  │   VLANs           ─── tagged traffic  ──> VM tunnel      │  │
│  │   Policy routing   ─── per-subnet      ──> LAN→WAN map   │  │
│  │   LuCI GUI         ─── dashboard + config via API        │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                          │
                     Internet/Starlink
                          │
                          ▼
              ┌───────────────────────┐
              │    VPS Server remoto  │
              │    (Linux, pubblica)  │
              │                       │
              │  Binario tunnel       │
              │  (role: server)       │
              │  6× TUN + listener    │
              └───────────────────────┘
```

### Flusso dati

1. Traffico LAN arriva alla VM tramite interfacce LAN punto-punto /30
2. Policy routing nella VM inoltra il traffico verso la TUN associata
3. Il processo tunnel incapsula i pacchetti IP in QUIC DATAGRAM
4. Il socket UDP è bindata sull'interfaccia WAN corretta
5. Il pacchetto attraversa Internet verso il VPS
6. Il VPS decapsula e inoltra (o viceversa per il ritorno)

---

## 3. Specifiche VM

| Parametro | Valore |
|-----------|--------|
| **Hypervisor** | KVM/QEMU (libvirt) |
| **SO** | Debian 12 (bookworm) x86_64 |
| **vCPU** | Minimo 2 (assegnati: i7-10710U 1.10GHz) |
| **RAM** | Minimo 4 GB |
| **Disco** | Minimo 20 GB (SSD) |
| **NIC fisiche (passthrough)** | 14 (vedi § 4) |
| **Kernel** | >= 6.1 (supporto TUN multi_queue, nftables, SO_BINDTODEVICE) |
| **Init** | systemd |
| **Network manager** | systemd-networkd (NO NetworkManager) |
| **Firewall** | nftables (NO iptables legacy) |
| **Pacchetti minimi** | iproute2, nftables, systemd, curl, jq, inetutils-ping |

### Requisiti kernel

- `CONFIG_TUN=m` (modulo tun)
- `CONFIG_NF_TABLES=y` (nftables)
- `net.core.rmem_max >= 7340032` (7 MB — buffer socket UDP)
- `net.core.wmem_max >= 7340032`

---

## 4. Interfacce di Rete

La VM dispone di **14 NIC fisiche** in PCI passthrough, suddivise in 3 gruppi funzionali.

### 4.1 WAN — Uplink Internet (6 interfacce)

| Interfaccia | Nome logico | Connettività | Addressing |
|-------------|-------------|--------------|------------|
| `enp7s3` | WAN1 | Uplink ISP #1 | DHCP |
| `enp7s4` | WAN2 | Uplink ISP #2 | DHCP |
| `enp7s5` | WAN3 | Uplink ISP #3 | DHCP |
| `enp7s6` | WAN4 | Uplink ISP #4 | DHCP |
| `enp7s7` | WAN5 | Starlink #1 | DHCP (CGNAT 100.x.x.x) |
| `enp7s8` | WAN6 | Starlink #2 | DHCP (CGNAT 100.x.x.x) |

> **Nota**: gli IP WAN sono dinamici (DHCP). Il software tunnel deve gestire
> il binding per interfaccia (`SO_BINDTODEVICE`) o per IP sorgente, con
> tolleranza al cambio IP WAN.

### 4.2 LAN — Collegamento verso OpenWrt (6 interfacce)

| Interfaccia | Nome logico | IP VM | IP OpenWrt | Subnet |
|-------------|-------------|-------|------------|--------|
| `enp6s20` | LAN1 | `172.16.1.1` | `172.16.1.2` | `/30` |
| `enp6s21` | LAN2 | `172.16.2.1` | `172.16.2.2` | `/30` |
| `enp6s22` | LAN3 | `172.16.3.1` | `172.16.3.2` | `/30` |
| `enp6s23` | LAN4 | `172.16.4.1` | `172.16.4.2` | `/30` |
| `enp7s1`  | LAN5 | `172.16.5.1` | `172.16.5.2` | `/30` |
| `enp7s2`  | LAN6 | `172.16.6.1` | `172.16.6.2` | `/30` |

Ogni coppia LAN forma un link point-to-point /30 tra la VM e OpenWrt.

### 4.3 MGMT — Gestione (2 interfacce)

| Interfaccia | Rete | IP |
|-------------|------|-----|
| `enp6s18` | Management primaria | `10.10.11.100/24` |
| `enp6s19` | Management secondaria | `10.10.10.100/24` |

L'API di gestione (§ 13) deve essere raggiungibile sulla rete MGMT primaria.

---

## 5. VLAN

Alcune interfacce LAN trasportano traffico tagged (802.1Q). La VM deve creare
le sotto-interfacce VLAN corrispondenti.

### 5.1 VLAN su LAN4 (`enp6s23`)

| VLAN ID | Sotto-interfaccia | IP VM |
|---------|-------------------|-------|
| 11 | `enp6s23.11` | `172.16.11.1/30` |
| 12 | `enp6s23.12` | `172.16.12.1/30` |
| 13 | `enp6s23.13` | `172.16.13.1/30` |

### 5.2 VLAN su LAN5 (`enp7s1`)

| VLAN ID | Sotto-interfaccia | IP VM |
|---------|-------------------|-------|
| 21 | `enp7s1.21` | `172.16.21.1/30` |
| 22 | `enp7s1.22` | `172.16.22.1/30` |
| 23 | `enp7s1.23` | `172.16.23.1/30` |

### 5.3 VLAN su LAN6 (`enp7s2`)

| VLAN ID | Sotto-interfaccia | IP VM |
|---------|-------------------|-------|
| 31 | `enp7s2.31` | `172.16.31.1/30` |
| 32 | `enp7s2.32` | `172.16.32.1/30` |
| 33 | `enp7s2.33` | `172.16.33.1/30` |

### 5.4 VLAN su LAN1 (`enp6s20`)

| VLAN ID | Sotto-interfaccia | IP VM |
|---------|-------------------|-------|
| 17 | `enp6s20.17` | `172.16.17.1/30` |

### Configurazione VLAN (systemd-networkd)

Per ogni VLAN serve:
1. Un file `.netdev` che definisce il dispositivo VLAN
2. Un file `.network` che assegna l'IP

Esempio per VLAN 11 su `enp6s23`:

```ini
# /etc/systemd/network/25-vlan11.netdev
[NetDev]
Name=enp6s23.11
Kind=vlan

[VLAN]
Id=11
```

```ini
# /etc/systemd/network/26-vlan11.network
[Match]
Name=enp6s23.11

[Network]
Address=172.16.11.1/30
```

Il parent interface (`enp6s23.network`) deve includere:

```ini
[Network]
VLAN=enp6s23.11
VLAN=enp6s23.12
VLAN=enp6s23.13
```

---

## 6. Indirizzamento IP

### 6.1 Riepilogo subnet

| Ruolo | Subnet | Gateway | Note |
|-------|--------|---------|------|
| WAN1-6 | DHCP | DHCP-assegnato | IP dinamici |
| LAN1 | `172.16.1.0/30` | — | point-to-point |
| LAN2 | `172.16.2.0/30` | — | point-to-point |
| LAN3 | `172.16.3.0/30` | — | point-to-point |
| LAN4 | `172.16.4.0/30` | — | point-to-point |
| LAN5 | `172.16.5.0/30` | — | point-to-point |
| LAN6 | `172.16.6.0/30` | — | point-to-point |
| VLAN 11 | `172.16.11.0/30` | — | su LAN4 |
| VLAN 12 | `172.16.12.0/30` | — | su LAN4 |
| VLAN 13 | `172.16.13.0/30` | — | su LAN4 |
| VLAN 21 | `172.16.21.0/30` | — | su LAN5 |
| VLAN 22 | `172.16.22.0/30` | — | su LAN5 |
| VLAN 23 | `172.16.23.0/30` | — | su LAN5 |
| VLAN 31 | `172.16.31.0/30` | — | su LAN6 |
| VLAN 32 | `172.16.32.0/30` | — | su LAN6 |
| VLAN 33 | `172.16.33.0/30` | — | su LAN6 |
| VLAN 17 | `172.16.17.0/30` | — | su LAN1 |
| MGMT1 | `10.10.11.0/24` | `10.10.11.254` | gestione |
| MGMT2 | `10.10.10.0/24` | — | gestione secondaria |
| TUN 1-6 | `10.200.{1-6}.0/30` | — | tunnel single-path |
| TUN cr/br/df | `10.200.{14-16}.0/24` | — | tunnel multi-class |
| TUN mp1 | `10.200.17.0/24` | — | tunnel multipath |

### 6.2 Piano TUN (dettaglio)

| Tunnel | TUN name | Client IP | Server IP | Porta server |
|--------|----------|-----------|-----------|-------------|
| 1 | `mpq1` | `10.200.1.1` | `10.200.1.2` | `45001` |
| 2 | `mpq2` | `10.200.2.1` | `10.200.2.2` | `45002` |
| 3 | `mpq3` | `10.200.3.1` | `10.200.3.2` | `45003` |
| 4 | `mpq4` | `10.200.4.1` | `10.200.4.2` | `45004` |
| 5 | `mpq5` | `10.200.5.1` | `10.200.5.2` | `45005` |
| 6 | `mpq6` | `10.200.6.1` | `10.200.6.2` | `45006` |
| cr4 | `tun-cr4` | `10.200.14.1` | — | `45014` |
| br4 | `tun-br4` | `10.200.14.5` | — | `45014` |
| df4 | `tun-df4` | `10.200.14.9` | — | `45014` |
| cr5 | `tun-cr5` | `10.200.15.1` | — | `45015` |
| br5 | `tun-br5` | `10.200.15.5` | — | `45015` |
| df5 | `tun-df5` | `10.200.15.9` | — | `45015` |
| cr6 | `tun-cr6` | `10.200.16.1` | — | `45016` |
| br6 | `tun-br6` | `10.200.16.5` | — | `45016` |
| df6 | `tun-df6` | `10.200.16.9` | — | `45016` |
| mp1 | `mp1` | `10.200.17.1` | `10.200.17.254` | `46017` |

> **Nota**: i tunnel cr/br/df (traffic-class) per lo stesso link condividono la
> stessa porta server e lo stesso TUN server-side, con routing interno basato
> su IP sorgente. Questo approccio è opzionale per il fornitore nella prima fase.

---

## 7. Routing e Policy Routing

### 7.1 Tabelle di routing personalizzate

La VM deve definire le seguenti tabelle in `/etc/iproute2/rt_tables`:

```
# Single-path tunnel routing
100     wan1
101     wan2
102     wan3
103     wan4
104     wan5
105     wan6

# Multi-tunnel class routing (opzionale fase 2)
120     mt_cr4
121     mt_br4
122     mt_df4
123     mt_cr5
124     mt_br5
125     mt_df5
126     mt_cr6
127     mt_br6
128     mt_df6

# Multipath bonding
200     bd1
```

### 7.2 Routing per tabella WAN

Per ogni WAN `N` (1-6), il traffico verso il VPS deve uscire dalla WAN corretta.

```bash
# Esempio per WAN5 (enp7s7)
ip route add default via <GW_WAN5> dev enp7s7 table wan5
```

Il gateway WAN è assegnato via DHCP e deve essere scoperto dinamicamente.

### 7.3 Policy rules (source-based routing)

Ogni subnet LAN interna che deve usare un tunnel specifico ha una regola:

```bash
# Traffico dalla LAN5 -> esce su WAN5
ip rule add from 172.16.5.0/30 lookup wan5 priority 100

# Traffico dalla LAN6 -> esce su WAN6
ip rule add from 172.16.6.0/30 lookup wan6 priority 100
```

### 7.4 Routing TUN → WAN (ritorno traffico)

Per ogni tunnel single-path, il traffico di ritorno dal VPS entra sulla TUN e
deve raggiungere la LAN corretta:

```bash
# Il server VPS (10.200.5.2) raggiunga il client via WAN5
ip route add 10.200.5.2 via <GW_WAN5> dev enp7s7 table wan5
ip rule add from 10.200.5.0/30 lookup wan5 priority 50
```

### 7.5 Default route

La tabella `main` deve avere almeno una default route verso una WAN attiva:

```bash
ip route add default via <GW_WANX> dev enp7sX
```

La scelta della WAN default è a discrezione dell'implementazione.

---

## 8. NAT e Firewall (nftables)

### 8.1 Regola NAT obbligatoria

Tutto il traffico in uscita sulle interfacce WAN e tunnel deve essere mascherato (SNAT).

```nft
table ip nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;

        # WAN interfaces
        oifname "enp7s3" masquerade
        oifname "enp7s4" masquerade
        oifname "enp7s5" masquerade
        oifname "enp7s6" masquerade
        oifname "enp7s7" masquerade
        oifname "enp7s8" masquerade

        # Tunnel TUN interfaces (single-path)
        oifname "mpq1" masquerade
        oifname "mpq2" masquerade
        oifname "mpq3" masquerade
        oifname "mpq4" masquerade
        oifname "mpq5" masquerade
        oifname "mpq6" masquerade

        # Tunnel TUN interfaces (multi-class)
        oifname "tun-cr4" masquerade
        oifname "tun-br4" masquerade
        oifname "tun-df4" masquerade
        oifname "tun-cr5" masquerade
        oifname "tun-br5" masquerade
        oifname "tun-df5" masquerade
        oifname "tun-cr6" masquerade
        oifname "tun-br6" masquerade
        oifname "tun-df6" masquerade

        # Multipath
        oifname "mp1" masquerade
    }
}
```

### 8.2 IP forwarding

Deve essere abilitato:

```bash
sysctl -w net.ipv4.ip_forward=1
```

Persistente in `/etc/sysctl.d/99-mpquic.conf`:

```
net.ipv4.ip_forward = 1
net.core.rmem_max = 7340032
net.core.wmem_max = 7340032
```

### 8.3 Firewall aggiuntivo

Nessuna regola `filter` aggiuntiva è richiesta in questa fase.
Il traffico tra LAN, TUN e WAN deve poter transitare liberamente.

---

## 9. Tunnel — Nomenclatura e Addressing

### 9.1 Nomenclatura tunnel

Il sistema di naming è **fisso** e deve essere rispettato esattamente.

#### Single-path (1 tunnel per WAN)

| Istanza | Nome TUN | WAN | Porta VPS | TUN CIDR |
|---------|----------|-----|-----------|----------|
| `1` | `mpq1` | enp7s3 | 45001 | `10.200.1.1/30` |
| `2` | `mpq2` | enp7s4 | 45002 | `10.200.2.1/30` |
| `3` | `mpq3` | enp7s5 | 45003 | `10.200.3.1/30` |
| `4` | `mpq4` | enp7s6 | 45004 | `10.200.4.1/30` |
| `5` | `mpq5` | enp7s7 | 45005 | `10.200.5.1/30` |
| `6` | `mpq6` | enp7s8 | 45006 | `10.200.6.1/30` |

#### Multi-tunnel per link (3 classi traffico per WAN)

| Istanza | Nome TUN | WAN | Porta VPS | TUN CIDR | Classe |
|---------|----------|-----|-----------|----------|--------|
| `cr4` | `tun-cr4` | enp7s6 | 45014 | `10.200.14.1/30` | critical |
| `br4` | `tun-br4` | enp7s6 | 45014 | `10.200.14.5/30` | browsing |
| `df4` | `tun-df4` | enp7s6 | 45014 | `10.200.14.9/30` | default |
| `cr5` | `tun-cr5` | enp7s7 | 45015 | `10.200.15.1/30` | critical |
| `br5` | `tun-br5` | enp7s7 | 45015 | `10.200.15.5/30` | browsing |
| `df5` | `tun-df5` | enp7s7 | 45015 | `10.200.15.9/30` | default |
| `cr6` | `tun-cr6` | enp7s8 | 45016 | `10.200.16.1/30` | critical |
| `br6` | `tun-br6` | enp7s8 | 45016 | `10.200.16.5/30` | browsing |
| `df6` | `tun-df6` | enp7s8 | 45016 | `10.200.16.9/30` | default |

#### Multipath bonding

| Istanza | Nome TUN | WAN | Porta VPS | TUN CIDR |
|---------|----------|-----|-----------|----------|
| `mp1` | `mp1` | enp7s6+7+8 | 46017 | `10.200.17.1/24` |

### 9.2 Interfacce TUN

Ogni tunnel richiede un dispositivo TUN Linux dedicato. Il dispositivo deve:

- Essere creato prima dell'avvio del processo tunnel
- Avere MTU configurabile (default: 1280)
- Supportare `multi_queue` per prestazioni multi-core
- Avere l'IP assegnato come da piano (§ 6.2)

Esempio di creazione:

```bash
ip tuntap add dev mpq5 mode tun multi_queue
ip addr add 10.200.5.1/30 dev mpq5
ip link set mpq5 mtu 1280 up
```

---

## 10. Configurazione Tunnel (YAML)

Ogni istanza tunnel deve avere un file YAML di configurazione in
`/etc/mpquic/instances/`. Il nome del file identifica l'istanza.

### 10.1 Schema configurazione single-path

```yaml
# /etc/mpquic/instances/5.yaml
role: client
bind_ip: "if:enp7s7"           # Binding per interfaccia (SO_BINDTODEVICE)
remote_addr: "<VPS_PUBLIC_IP>"  # IP pubblico del server VPS
remote_port: 45005
tun_name: mpq5
tun_cidr: "10.200.5.1/30"
tun_mtu: 1280
log_level: info
metrics_port: 9095              # HTTP metrics (porta unica per istanza)
```

### 10.2 Schema configurazione multi-tunnel (traffic class)

```yaml
# /etc/mpquic/instances/cr5.yaml
role: client
bind_ip: "if:enp7s7"
remote_addr: "<VPS_PUBLIC_IP>"
remote_port: 45015
tun_name: tun-cr5
tun_cidr: "10.200.15.1/30"
tun_mtu: 1280
log_level: info
metrics_port: 9115
```

### 10.3 Schema configurazione multipath

```yaml
# /etc/mpquic/instances/mp1.yaml
role: client
tun_name: mp1
tun_cidr: "10.200.17.1/24"
tun_mtu: 1280
multipath_enabled: true
multipath_policy: balanced       # priority | failover | balanced
log_level: info
metrics_port: 9117

multipath_paths:
  - name: wan4
    bind_ip: "if:enp7s6"
    remote_addr: "<VPS_PUBLIC_IP>"
    remote_port: 46017
    priority: 10
    weight: 1
    transport: stripe            # quic | stripe
    # parametri stripe (se transport=stripe)
    stripe_pipes: 4
    stripe_fec_data_shards: 10
    stripe_fec_parity_shards: 2
    stripe_fec_mode: adaptive

  - name: wan5
    bind_ip: "if:enp7s7"
    remote_addr: "<VPS_PUBLIC_IP>"
    remote_port: 46017
    priority: 10
    weight: 1
    transport: stripe
    stripe_pipes: 4
    stripe_fec_data_shards: 10
    stripe_fec_parity_shards: 2
    stripe_fec_mode: adaptive

  - name: wan6
    bind_ip: "if:enp7s8"
    remote_addr: "<VPS_PUBLIC_IP>"
    remote_port: 46017
    priority: 10
    weight: 1
    transport: stripe
    stripe_pipes: 4
    stripe_fec_data_shards: 10
    stripe_fec_parity_shards: 2
    stripe_fec_mode: adaptive
```

### 10.4 Variabili ambiente (.env)

Ogni istanza può avere un file `.env` associato in `/etc/mpquic/instances/`:

```bash
# /etc/mpquic/instances/5.env
TUN_NAME=mpq5
TUN_CIDR=10.200.5.1/30
TUN_PEER=10.200.5.2
TUN_MTU=1280
VPS_PUBLIC_IP=<indirizzo_vps>
```

Un file `global.env` definisce variabili comuni a tutte le istanze:

```bash
# /etc/mpquic/instances/global.env
VPS_PUBLIC_IP=<indirizzo_vps>
```

### 10.5 Parametri YAML — Lista completa

| Parametro | Tipo | Obbligatorio | Descrizione |
|-----------|------|-------------|-------------|
| `role` | string | Sì | `client` o `server` |
| `bind_ip` | string | Sì (client) | IP o `if:<ifname>` per SO_BINDTODEVICE |
| `remote_addr` | string | Sì (client) | IP del server VPS |
| `remote_port` | int | Sì | Porta UDP server (es. 45005) |
| `listen_addr` | string | Sì (server) | Indirizzo di ascolto (es. `0.0.0.0`) |
| `listen_port` | int | Sì (server) | Porta di ascolto |
| `tun_name` | string | Sì | Nome interfaccia TUN |
| `tun_cidr` | string | Sì | CIDR dell'interfaccia TUN |
| `tun_mtu` | int | No | MTU della TUN (default: 1280) |
| `log_level` | string | No | `debug`, `info`, `warn`, `error` |
| `metrics_port` | int | No | Porta HTTP metriche (0 = disabilitato) |
| `multipath_enabled` | bool | No | Abilita modalità multipath |
| `multipath_policy` | string | No | `priority`, `failover`, `balanced` |
| `multipath_paths` | array | Se multipath | Lista path (vedi § 10.3) |
| `congestion` | string | No | `bbr`, `cubic` (default: bbr) |

---

## 11. Gestione Servizi (systemd)

### 11.1 Template unit

Il software tunnel deve essere gestito tramite un systemd template unit
`mpquic@.service` che permette istanze multiple:

```ini
[Unit]
Description=MPQUIC tunnel instance %i
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
EnvironmentFile=-/etc/mpquic/instances/global.env
EnvironmentFile=-/etc/mpquic/instances/%i.env

# Pre-start: crea/configura TUN e renderizza config
ExecStartPre=/usr/local/bin/ensure_tun.sh
ExecStartPre=/usr/local/bin/render_config.sh /etc/mpquic/instances/%i.yaml.tpl /etc/mpquic/instances/%i.yaml

ExecStart=/usr/local/bin/mpquic -config /etc/mpquic/instances/%i.yaml

Restart=always
RestartSec=3
LimitNOFILE=65535
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

### 11.2 Comandi di gestione

```bash
# Avvio singola istanza
systemctl start mpquic@5

# Stop singola istanza
systemctl stop mpquic@5

# Restart
systemctl restart mpquic@5

# Stato
systemctl is-active mpquic@5

# Abilitazione al boot
systemctl enable mpquic@5

# Log
journalctl -u mpquic@5 -f
```

### 11.3 Script helper

#### `ensure_tun.sh`

Script idempotente che crea la TUN se non esiste, assegna IP e MTU.
Leggoe le variabili `TUN_NAME`, `TUN_CIDR`, `TUN_MTU` dall'ambiente.

```bash
#!/bin/bash
set -euo pipefail
: "${TUN_NAME:?}"
: "${TUN_CIDR:?}"
: "${TUN_MTU:=1280}"

if ! ip link show "$TUN_NAME" &>/dev/null; then
    ip tuntap add dev "$TUN_NAME" mode tun multi_queue
fi

ip addr flush dev "$TUN_NAME" 2>/dev/null || true
ip addr add "$TUN_CIDR" dev "$TUN_NAME"
ip link set "$TUN_NAME" mtu "$TUN_MTU" up
```

#### `render_config.sh`

Script che sostituisce variabili nel template YAML:

```bash
#!/bin/bash
set -euo pipefail
TPL="$1"
OUT="$2"
[[ -f "$TPL" ]] || exit 0
envsubst < "$TPL" > "$OUT"
```

---

## 12. Watchdog e Health-Check

### 12.1 Timer systemd

Un timer systemd deve eseguire un check periodico (ogni 60 secondi):

```ini
# /etc/systemd/system/mpquic-watchdog.timer
[Unit]
Description=MPQUIC tunnel watchdog timer

[Timer]
OnBootSec=120
OnUnitActiveSec=60
RandomizedDelaySec=5

[Install]
WantedBy=timers.target
```

```ini
# /etc/systemd/system/mpquic-watchdog.service
[Unit]
Description=MPQUIC tunnel watchdog

[Service]
Type=oneshot
ExecStart=/usr/local/bin/mpquic-tunnel-watchdog.sh
```

### 12.2 Logica watchdog

Lo script di watchdog deve implementare i seguenti controlli per ogni istanza tunnel abilitata:

1. **WAN carrier check**: verifica che l'interfaccia WAN associata abbia carrier
   (`cat /sys/class/net/<ifname>/carrier`)
2. **TUN existence check**: verifica che l'interfaccia TUN esista e sia UP
3. **Peer reachability**: ping verso il peer tunnel (es. `10.200.5.2`) con timeout breve
4. **Threshold-based restart**: restart dell'istanza solo dopo N fallimenti consecutivi
   (consigliato: 3), per evitare flapping

```
Per ogni istanza abilitata:
  1. Se WAN non ha carrier → skip (non riavviare, attenderne il ripristino)
  2. Se TUN non esiste → restart immediato
  3. Se peer non risponde → incrementa contatore
     - Se contatore >= THRESHOLD → restart + reset contatore
     - Altrimenti → log warning e attesa
  4. Se peer risponde → reset contatore
```

### 12.3 File contatore

Il contatore di fallimenti deve essere persistito per sopravvivere a restart
del watchdog. Formato consigliato:

```
/run/mpquic-watchdog/<instance>.fail_count
```

---

## 13. Management REST API

La VM deve esporre una REST API HTTP(S) sulla rete di gestione, compatibile
con il contratto definito di seguito.

### 13.1 Informazioni generali

| Parametro | Valore |
|-----------|--------|
| **Bind address** | `10.10.11.100:8080` (MGMT primaria) |
| **Protocollo** | HTTP (fase 1), HTTPS/TLS 1.2+ (fase 2) |
| **Autenticazione** | Bearer token (`Authorization: Bearer <token>`) |
| **Content-Type** | `application/json` |
| **CORS** | Opzionale, configurabile |

### 13.2 Sicurezza

- Il token deve avere lunghezza minima 16 caratteri
- Il confronto del token deve essere constant-time (prevenzione timing attack)
- Rate limiting: max 10 tentativi falliti per IP in finestra di 5 minuti
- Header di sicurezza obbligatori nelle risposte:
  - `X-Content-Type-Options: nosniff`
  - `X-Frame-Options: DENY`
  - `Cache-Control: no-store`
- Input validation: i nomi tunnel devono matchare `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`
- Body size limit: 512 KB

### 13.3 Endpoint

#### `GET /api/v1/health`

Health check globale.

**Risposta** (200 OK):

```json
{
  "ok": true,
  "version": "1.0.0",
  "hostname": "tbox-mpquic",
  "os": "linux",
  "arch": "amd64",
  "tunnels_total": 16,
  "tunnels_running": 4,
  "tunnels_stopped": 12,
  "tunnels_failed": 0,
  "timestamp": "2026-06-11T10:00:00Z"
}
```

#### `GET /api/v1/tunnels`

Lista tutte le istanze tunnel con stato corrente.

**Risposta** (200 OK):

```json
{
  "tunnels": [
    {
      "name": "5",
      "status": "running",
      "uptime_sec": 3600,
      "tun_name": "mpq5",
      "tun_cidr": "10.200.5.1/30",
      "transport": "quic",
      "config_file": "/etc/mpquic/instances/5.yaml"
    },
    {
      "name": "mp1",
      "status": "running",
      "uptime_sec": 7200,
      "tun_name": "mp1",
      "tun_cidr": "10.200.17.1/24",
      "transport": "stripe",
      "config_file": "/etc/mpquic/instances/mp1.yaml"
    }
  ]
}
```

Campi obbligatori per ogni tunnel:

| Campo | Tipo | Descrizione |
|-------|------|-------------|
| `name` | string | Nome istanza (es. `5`, `cr5`, `mp1`) |
| `status` | string | `running`, `stopped`, `failed` |
| `uptime_sec` | int | Secondi dall'avvio (0 se non running) |
| `tun_name` | string | Nome interfaccia TUN |
| `tun_cidr` | string | CIDR assegnato alla TUN |
| `transport` | string | `quic` o `stripe` |
| `config_file` | string | Path assoluto al file YAML |

#### `GET /api/v1/tunnels/{name}`

Dettaglio singola istanza. Stessi campi di cui sopra.

#### `POST /api/v1/tunnels/{name}/start`

Avvia l'istanza tunnel.

**Risposta** (200 OK):

```json
{
  "ok": true,
  "action": "start",
  "tunnel": "5",
  "status": "running"
}
```

#### `POST /api/v1/tunnels/{name}/stop`

Ferma l'istanza tunnel. Stesso formato risposta.

#### `POST /api/v1/tunnels/{name}/restart`

Riavvia l'istanza tunnel. Stesso formato risposta.

#### `GET /api/v1/tunnels/{name}/config`

Legge la configurazione corrente dell'istanza.

**Risposta** (200 OK):

```json
{
  "tunnel": "5",
  "config": {
    "role": "client",
    "bind_ip": "if:enp7s7",
    "remote_addr": "1.2.3.4",
    "remote_port": 45005,
    "tun_name": "mpq5",
    "tun_cidr": "10.200.5.1/30",
    "tun_mtu": 1280,
    "log_level": "info",
    "metrics_port": 9095
  },
  "param_categories": {
    "A": ["log_level", "metrics_port"],
    "B": ["tun_mtu", "congestion"],
    "C": ["role", "bind_ip", "remote_addr", "remote_port", "tun_name", "tun_cidr"]
  }
}
```

Categorie parametro:
- **A (hot-reload)**: modificabili senza restart
- **B (restart)**: richiedono restart dell'istanza
- **C (server-coupled)**: richiedono coordinamento con il server VPS, modifica bloccata via API

#### `PATCH /api/v1/tunnels/{name}/config`

Modifica parziale della configurazione.

Per client che non supportano PATCH (es. wget, uclient-fetch), è supportato:
`POST` con header `X-HTTP-Method-Override: PATCH`.

**Request body**:

```json
{
  "log_level": "debug",
  "tun_mtu": 1400
}
```

**Risposta** (200 OK):

```json
{
  "ok": true,
  "tunnel": "5",
  "fields_applied": ["log_level", "tun_mtu"],
  "needs_restart": true,
  "restart_applied": false
}
```

Query parameter opzionale: `?auto_restart=true` per trigger automatico del restart.

#### `POST /api/v1/tunnels/{name}/config/validate`

Valida un patch di configurazione senza applicarlo.

**Risposta** (200 OK se valido):

```json
{
  "ok": true,
  "needs_restart": true
}
```

**Risposta** (400 Bad Request se invalido):

```json
{
  "ok": false,
  "error": "field 'remote_addr' is category C (server-coupled), modification blocked",
  "blocked_fields": ["remote_addr"],
  "needs_restart": false
}
```

#### `GET /api/v1/tunnels/{name}/metrics`

Proxy verso le metriche dell'istanza tunnel.

Risposta: passthrough del JSON restituito dall'endpoint metriche dell'istanza
(vedi § 14).

#### `GET /api/v1/tunnels/{name}/logs?lines=100&level=error`

Ultimi log dell'istanza da journald.

| Query param | Default | Descrizione |
|-------------|---------|-------------|
| `lines` | 100 | Numero righe (max 10000) |
| `level` | (tutti) | Filtra: `error`, `warning` |

**Risposta** (200 OK):

```json
{
  "tunnel": "5",
  "lines": 100,
  "level": "error",
  "output": "Jun 11 10:00:01 tbox mpquic[1234]: ERROR: ..."
}
```

#### `GET /api/v1/metrics`

Metriche aggregate di tutti i tunnel.

**Risposta** (200 OK):

```json
{
  "tunnels": {
    "5": { "total_tx_bytes": 123456, "total_rx_bytes": 789012, ... },
    "6": { "total_tx_bytes": 1000, "total_rx_bytes": 2000, ... },
    "mp1": { "total_tx_bytes": 5000000, "total_rx_bytes": 8000000, ... }
  }
}
```

#### `GET /api/v1/system/info`

Informazioni di sistema.

**Risposta** (200 OK):

```json
{
  "mgmt_version": "1.0.0",
  "mpquic_version": "v2.3.0",
  "git_commit": "abc1234 fix: ...",
  "go_version": "go1.22.0",
  "hostname": "tbox-mpquic",
  "os": "linux",
  "arch": "amd64",
  "num_cpu": 2,
  "uptime": "up 5 days, 3 hours",
  "timestamp": "2026-06-11T10:00:00Z"
}
```

#### `GET /api/v1/system/logs/{name}?lines=100&level=error`

Stessa semantica di `/api/v1/tunnels/{name}/logs` (endpoint alternativo).

---

## 14. Metriche e Prometheus

### 14.1 Endpoint metriche per istanza

Ogni istanza tunnel deve esporre un endpoint HTTP JSON sulla porta `metrics_port`
configurata nel YAML.

**Endpoint**: `GET http://127.0.0.1:<metrics_port>/api/v1/stats`

### 14.2 Porte metriche assegnate

| Istanza | Porta |
|---------|-------|
| 1 | 9091 |
| 2 | 9092 |
| 3 | 9093 |
| 4 | 9094 |
| 5 | 9095 |
| 6 | 9096 |
| cr4 | 9114 |
| br4 | 9114 |
| df4 | 9114 |
| cr5 | 9115 |
| br5 | 9115 |
| df5 | 9115 |
| cr6 | 9116 |
| br6 | 9116 |
| df6 | 9116 |
| mp1 | 9117 |

> **Nota**: i tunnel multi-class che condividono lo stesso server process
> possono condividere la stessa porta metriche.

### 14.3 Formato metriche — Single-path

```json
{
  "tunnel": "5",
  "role": "client",
  "status": "running",
  "uptime_sec": 3600,
  "tun_name": "mpq5",
  "tun_cidr": "10.200.5.1/30",
  "transport": "quic",
  "global": {
    "total_tx_bytes": 123456,
    "total_rx_bytes": 789012,
    "total_tx_pkts": 1023,
    "total_rx_pkts": 6542,
    "tx_rate_bps": 1234567,
    "rx_rate_bps": 7890123,
    "uptime_seconds": 3600
  }
}
```

### 14.4 Formato metriche — Multipath

```json
{
  "tunnel": "mp1",
  "role": "client",
  "status": "running",
  "uptime_sec": 7200,
  "tun_name": "mp1",
  "tun_cidr": "10.200.17.1/24",
  "transport": "stripe",
  "paths": [
    {
      "name": "wan4",
      "state": "up",
      "tx_bytes": 1000000,
      "rx_bytes": 2000000,
      "tx_pkts": 500,
      "rx_pkts": 1000,
      "rtt_us": 25000,
      "loss_pct": 0.1,
      "consecutive_fails": 0
    },
    {
      "name": "wan5",
      "state": "up",
      "tx_bytes": 1500000,
      "rx_bytes": 2500000
    }
  ],
  "global": {
    "total_tx_bytes": 5000000,
    "total_rx_bytes": 8000000,
    "total_tx_pkts": 2500,
    "total_rx_pkts": 4000,
    "active_paths": 3,
    "total_paths": 3,
    "uptime_seconds": 7200
  },
  "fec": {
    "groups_encoded": 10000,
    "groups_decoded_ok": 9900,
    "groups_decoded_fail": 2,
    "effective_parity": 2,
    "mode": "adaptive"
  }
}
```

### 14.5 Prometheus scraping

Le metriche devono essere compatibili con scraping Prometheus. L'approccio
consigliato è un exporter esterno che converte il JSON in formato Prometheus
text exposition, oppure un endpoint nativo `/metrics` in formato Prometheus.

Metriche minime attese (naming convention Prometheus):

```
# HELP mpquic_tunnel_tx_bytes_total Total bytes transmitted
# TYPE mpquic_tunnel_tx_bytes_total counter
mpquic_tunnel_tx_bytes_total{instance="5",tun="mpq5"} 123456

# HELP mpquic_tunnel_rx_bytes_total Total bytes received
# TYPE mpquic_tunnel_rx_bytes_total counter
mpquic_tunnel_rx_bytes_total{instance="5",tun="mpq5"} 789012

# HELP mpquic_tunnel_tx_pkts_total Total packets transmitted
# TYPE mpquic_tunnel_tx_pkts_total counter
mpquic_tunnel_tx_pkts_total{instance="5",tun="mpq5"} 1023

# HELP mpquic_tunnel_rx_pkts_total Total packets received
# TYPE mpquic_tunnel_rx_pkts_total counter
mpquic_tunnel_rx_pkts_total{instance="5",tun="mpq5"} 6542

# HELP mpquic_tunnel_up Tunnel operational status (1=up, 0=down)
# TYPE mpquic_tunnel_up gauge
mpquic_tunnel_up{instance="5",tun="mpq5"} 1

# HELP mpquic_path_state Multipath path state (1=up, 0=down)
# TYPE mpquic_path_state gauge
mpquic_path_state{instance="mp1",path="wan4"} 1
mpquic_path_state{instance="mp1",path="wan5"} 1
mpquic_path_state{instance="mp1",path="wan6"} 1
```

---

## 15. Validazione e Test

### 15.1 Test funzionali obbligatori

| # | Test | Criterio di successo |
|---|------|----------------------|
| T1 | **Connettività single-path** | Ping dal VPS verso `10.200.N.1` per N=1..6 con RTT < 200ms |
| T2 | **Throughput single-path** | iperf3 TCP attraverso tunnel: >= 40 Mbps per link Starlink |
| T3 | **Isolamento traffico** | Loss artificiale (tc netem 10%) su un tunnel non impatta i tunnel adiacenti |
| T4 | **Failover multipath** | Disconnect WAN→WAN5: traffico migra su WAN4/6 in < 15s, packet loss < 5% |
| T5 | **Bonding throughput** | iperf3 attraverso mp1 con 3 WAN: >= 150 Mbps aggregati |
| T6 | **Watchdog recovery** | Kill processo mpquic@5: watchdog lo riavvia entro 120s |
| T7 | **API health** | `GET /api/v1/health` ritorna 200 con `ok: true` |
| T8 | **API tunnel lifecycle** | stop/start/restart via API cambiano stato systemd |
| T9 | **Metriche non-zero** | Dopo traffico iperf3, TX/RX bytes > 0 per ogni tunnel attivo |
| T10 | **NAT masquerade** | Traffico dalla LAN OpenWrt esce verso Internet con IP WAN corretto |

### 15.2 Procedura iperf3

```bash
# Sul VPS (server iperf3)
iperf3 -s -p 5201

# Sulla VM tunnel (client iperf3) — test tunnel 5
iperf3 -c 10.200.5.2 -p 5201 -t 30 -P 4

# Test multipath mp1
iperf3 -c 10.200.17.254 -p 5201 -t 30 -P 8
```

### 15.3 Test Grafana

Il fornitore deve dimostrare che le metriche sono visibili su una dashboard
Grafana con:
- Pannello per-tunnel con TX/RX bytes rate
- Pannello stato tunnel (running/stopped/failed)
- Pannello path multipath (stato, RTT, loss)

### 15.4 Test k6 (API load)

L'API di gestione deve sostenere almeno:
- 100 richieste/secondo su `GET /api/v1/health`
- 50 richieste/secondo su `GET /api/v1/tunnels`
- Risposta P95 < 200ms

Esempio script k6:

```javascript
import http from 'k6/http';
import { check } from 'k6';

export const options = {
  vus: 10,
  duration: '60s',
};

const BASE = 'http://10.10.11.100:8080';
const TOKEN = '<auth_token>';

export default function () {
  const res = http.get(`${BASE}/api/v1/health`, {
    headers: { Authorization: `Bearer ${TOKEN}` },
  });
  check(res, {
    'status 200': (r) => r.status === 200,
    'body ok': (r) => JSON.parse(r.body).ok === true,
  });
}
```

---

## 16. Supporto L3

### 16.1 Scope del supporto

Il fornitore deve garantire supporto di Livello 3 (L3) per i seguenti
componenti:

| Componente | Descrizione |
|------------|-------------|
| Software tunnel | Bug fix, patch di sicurezza, aggiornamenti funzionali |
| Management API | Manutenzione endpoint, fix compatibilità |
| Watchdog | Correzioni logica health-check |
| Configurazione rete | Assistenza su routing, NAT, VLAN |

### 16.2 SLA

| Metrica | Valore target |
|---------|--------------|
| Tempo di risposta (P1 — tunnel down) | < 4 ore lavorative |
| Tempo di risposta (P2 — degradazione) | < 8 ore lavorative |
| Tempo di risposta (P3 — richiesta info) | < 2 giorni lavorativi |
| Uptime tunnel (target mensile) | >= 99.5% |
| Manutenzione preventiva | Inclusa |
| Aggiornamento software | Trimestrale (minimo) |

### 16.3 Canali di comunicazione

- Ticketing via piattaforma condivisa (JIRA/GitLab/equivalente)
- Accesso SSH alla VM per troubleshooting remoto
- Documentazione aggiornamenti via changelog versionato

---

## 17. Deliverable e Acceptance Criteria

### 17.1 Deliverable attesi

| # | Deliverable | Formato |
|---|-------------|---------|
| D1 | Immagine VM Debian 12 pre-configurata | QCOW2 / OVA |
| D2 | Binario tunnel (client + server) | Eseguibile Linux amd64 |
| D3 | Configurazione systemd | File .service + .timer |
| D4 | Script helper (ensure_tun, watchdog, render) | Bash scripts |
| D5 | Management API daemon | Eseguibile Linux amd64 |
| D6 | Documentazione tecnica | Markdown |
| D7 | Risultati test (§ 15) | Report con evidenze |

### 17.2 Acceptance Criteria

La VM fornita sarà accettata quando:

1. **Tutti i 10 test funzionali (T1-T10)** sono superati
2. **L'API risponde correttamente** a tutti gli endpoint definiti in § 13
3. **Le metriche** sono coerenti con il formato definito in § 14
4. **La nomenclatura** di interfacce, tunnel, file config rispetta esattamente
   quanto definito in §§ 4, 9, 10
5. **Il watchdog** rileva e recupera automaticamente un tunnel in errore
6. **La VM si avvia al boot** con tutti i tunnel configurati attivi
7. **Il piano di indirizzamento** (§ 6) è rispettato senza deviazioni
8. **Le regole NAT** (§ 8) sono funzionanti e verificate con traceroute

### 17.3 Timeline

| Fase | Descrizione | Durata stimata |
|------|-------------|----------------|
| Fase 1 | Single-path (6 tunnel) + API + metriche | 4 settimane |
| Fase 2 | Multi-tunnel traffic-class (9 tunnel) | 3 settimane |
| Fase 3 | Multipath bonding + FEC | 4 settimane |
| Fase 4 | Integration test + acceptance | 2 settimane |

---

*Fine del documento.*
