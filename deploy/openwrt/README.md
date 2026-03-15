# OpenWrt MPQUIC VLAN Deploy Scripts

Script UCI per configurare OpenWrt come classificatore di traffico per
l'architettura MPQUIC multi-tunnel (Step 2.5).

## Prerequisiti

- OpenWrt 22.03+ con fw4 (nftables)
- Pacchetto `mwan3` installato (`opkg install mwan3 luci-app-mwan3`)
- Connettività fisica tra OpenWrt e TBOX sulle porte SL4/SL5/SL6

## Mapping interfacce

| OpenWrt | Device | TBOX LAN | TBOX Device | Subnet transit |
|---------|--------|----------|-------------|----------------|
| SL4     | eth11  | LAN4     | enp6s23     | 172.16.4.0/30  |
| SL5     | eth12  | LAN5     | enp7s1      | 172.16.5.0/30  |
| SL6     | eth13  | LAN6     | enp7s2      | 172.16.6.0/30  |

## VLAN Mapping

| VLAN | Classe   | OpenWrt IF  | IP OpenWrt    | IP TBOX (gw)  | Tunnel |
|------|----------|-------------|---------------|---------------|--------|
| 11   | critical | eth11.11    | 172.16.11.2   | 172.16.11.1   | cr1    |
| 12   | bulk     | eth11.12    | 172.16.12.2   | 172.16.12.1   | br1    |
| 13   | default  | eth11.13    | 172.16.13.2   | 172.16.13.1   | df1    |
| 21   | critical | eth12.21    | 172.16.21.2   | 172.16.21.1   | cr2    |
| 22   | bulk     | eth12.22    | 172.16.22.2   | 172.16.22.1   | br2    |
| 23   | default  | eth12.23    | 172.16.23.2   | 172.16.23.1   | df2    |
| 31   | critical | eth13.31    | 172.16.31.2   | 172.16.31.1   | cr3    |
| 32   | bulk     | eth13.32    | 172.16.32.2   | 172.16.32.1   | br3    |
| 33   | default  | eth13.33    | 172.16.33.2   | 172.16.33.1   | df3    |

## Ordine esecuzione

```bash
# 1. VLAN devices + interfacce statiche
sh /tmp/01-network-vlan.sh

# 2. Firewall zones + forwarding da LAN
sh /tmp/02-firewall-zones.sh

# 3. mwan3 interfaces + members + policies + rules
sh /tmp/03-mwan3-policy.sh

# 4. (Opzionale) DSCP marking via nftables
sh /tmp/04-nft-dscp-mark.sh
```

## Cleanup

Per rimuovere tutta la configurazione MPQUIC:

```bash
sh /tmp/99-remove-vlan.sh
```

## Classificazione traffico

| Classe     | Policy        | Protocolli                                     |
|------------|---------------|-------------------------------------------------|
| **critical** | pol_critical | SIP (UDP 5060), RTP (UDP 10000-20000), DNS, SSH |
| **default**  | pol_default  | HTTP (80), HTTPS (443), HTTPS-alt (8443)         |
| **bulk**     | pol_bulk     | Tutto il resto (catch-all)                       |

Ogni policy bilancia il traffico su 3 tunnel (uno per WAN) in load-balance.
Se un tunnel va DOWN, mwan3 lo rileva tramite ping e redistribuisce il traffico.

## Adattamento per altre TBOX

Modificare le variabili trunk in `01-network-vlan.sh`:

```bash
TRUNK_SL4="eth11"   # ← adattare al device fisico dell'OpenWrt
TRUNK_SL5="eth12"
TRUNK_SL6="eth13"
```

Il resto della configurazione (VLAN ID, subnet, classi) è identico per ogni TBOX.
