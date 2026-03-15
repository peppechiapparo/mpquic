#!/bin/sh
# =============================================================================
# 02-firewall-zones.sh — OpenWrt firewall zones per VLAN MPQUIC
# =============================================================================
#
# Crea 9 firewall zones (una per VLAN/tunnel class) con forwarding da LAN.
# Ogni zona contiene una sola interfaccia VLAN e ha MASQUERADE + forwarding.
#
# Zone create: wan_cr1..3, wan_br1..3, wan_df1..3
#
# Dipendenze: eseguire DOPO 01-network-vlan.sh
#
# Esecuzione:
#   scp 02-firewall-zones.sh root@openwrt:/tmp/
#   ssh root@openwrt 'sh /tmp/02-firewall-zones.sh'
#
# =============================================================================

set -e

echo "=== MPQUIC OpenWrt Firewall Zones ==="

# -----------------------------------------------------------------------------
# Helper: crea una zona firewall + forwarding da LAN
# Uso: add_zone <zone_name> <iface_name>
# -----------------------------------------------------------------------------
add_zone() {
    local zone_name="$1" iface_name="$2"

    echo "  [+] Zone $zone_name → interface $iface_name"

    uci set firewall.${zone_name}=zone
    uci set firewall.${zone_name}.name="${zone_name}"
    uci set firewall.${zone_name}.network="${iface_name}"
    uci set firewall.${zone_name}.input='REJECT'
    uci set firewall.${zone_name}.output='ACCEPT'
    uci set firewall.${zone_name}.forward='REJECT'
    uci set firewall.${zone_name}.masq='1'
    uci set firewall.${zone_name}.mtu_fix='1'

    # Forwarding: LAN → questa zona (traffico LAN esce via tunnel MPQUIC)
    uci set firewall.fwd_lan_${zone_name}=forwarding
    uci set firewall.fwd_lan_${zone_name}.src='lan'
    uci set firewall.fwd_lan_${zone_name}.dest="${zone_name}"
}

# === WAN4 / SL4 — cr4, br4, df4 =============================================
add_zone wan_cr1 cr4
add_zone wan_br1 br4
add_zone wan_df1 df4

# === WAN5 / SL5 — cr5, br5, df5 =============================================
add_zone wan_cr2 cr5
add_zone wan_br2 br5
add_zone wan_df2 df5

# === WAN6 / SL6 — cr6, br6, df6 =============================================
add_zone wan_cr3 cr6
add_zone wan_br3 br6
add_zone wan_df3 df6

# === Commit & apply ==========================================================
echo ""
echo "=== Commit firewall ==="
uci commit firewall
echo "=== Restart firewall ==="
/etc/init.d/firewall restart

echo ""
echo "=== Done: 9 firewall zones + 9 LAN forwardings created ==="
echo "Verifica con: uci show firewall | grep wan_cr\\|wan_br\\|wan_df"
