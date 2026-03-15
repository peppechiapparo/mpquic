#!/usr/bin/env bash
set -euo pipefail

safe() { "$@" 2>/dev/null || true; }

# Single-link tunnels (1:1 WAN↔tunnel)
safe ip route replace 172.16.1.0/30 dev mpq1
safe ip route replace 172.16.2.0/30 dev mpq2
safe ip route replace 172.16.3.0/30 dev mpq3
safe ip route replace 172.16.4.0/30 dev mpq4
safe ip route replace 172.16.5.0/30 dev mpq5
safe ip route replace 172.16.6.0/30 dev mpq6

# Multi-tunnel per link (Step 2.5: VLAN transit subnets → mt tunnels)
# WAN4 classes: cr4/br4/df4 via mt4
safe ip route replace 172.16.11.0/30 dev mt4
safe ip route replace 172.16.12.0/30 dev mt4
safe ip route replace 172.16.13.0/30 dev mt4

# WAN5 classes: cr5/br5/df5 via mt5
safe ip route replace 172.16.21.0/30 dev mt5
safe ip route replace 172.16.22.0/30 dev mt5
safe ip route replace 172.16.23.0/30 dev mt5

# WAN6 classes: cr6/br6/df6 via mt6
safe ip route replace 172.16.31.0/30 dev mt6
safe ip route replace 172.16.32.0/30 dev mt6
safe ip route replace 172.16.33.0/30 dev mt6

exit 0
