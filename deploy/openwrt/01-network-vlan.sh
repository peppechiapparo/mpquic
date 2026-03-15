#!/bin/sh
# =============================================================================
# 01-network-vlan.sh — OpenWrt VLAN devices + interfaces per MPQUIC multi-tunnel
# =============================================================================
#
# Crea 9 VLAN 802.1q (3 trunk × 3 classi) e le relative interfacce statiche.
#
# Mapping:
#   SL4 (WAN4) → eth11 → VLAN 11 (critical), 12 (bulk), 13 (default)
#   SL5 (WAN5) → eth12 → VLAN 21 (critical), 22 (bulk), 23 (default)
#   SL6 (WAN6) → eth13 → VLAN 31 (critical), 32 (bulk), 33 (default)
#
# OpenWrt IP = .2/30, TBOX gateway = .1/30
#
# Esecuzione:
#   scp 01-network-vlan.sh root@openwrt:/tmp/
#   ssh root@openwrt 'sh /tmp/01-network-vlan.sh'
#
# =============================================================================

set -e

# --- Trunk interfaces (adattare se l'hardware cambia) -----------------------
TRUNK_SL4="eth11"
TRUNK_SL5="eth12"
TRUNK_SL6="eth13"

echo "=== MPQUIC OpenWrt VLAN setup ==="
echo "Trunk SL4=$TRUNK_SL4  SL5=$TRUNK_SL5  SL6=$TRUNK_SL6"

# -----------------------------------------------------------------------------
# Helper: crea un VLAN device 802.1q + interfaccia statica
# Uso: add_vlan <trunk> <vid> <class> <wan_num> <ip> <gw> <metric>
# -----------------------------------------------------------------------------
add_vlan() {
    local trunk="$1" vid="$2" class="$3" wan="$4" ip="$5" gw="$6" metric="$7"
    local dev_name="vlan${vid}"
    local iface_name="${class}${wan}"

    echo "  [+] VLAN $vid ($class) on $trunk → $iface_name ($ip gw $gw metric $metric)"

    # --- Device 802.1q ---
    uci set network.${dev_name}=device
    uci set network.${dev_name}.type='8021q'
    uci set network.${dev_name}.ifname="${trunk}"
    uci set network.${dev_name}.vid="${vid}"
    uci set network.${dev_name}.name="${trunk}.${vid}"

    # --- Interface statica ---
    uci set network.${iface_name}=interface
    uci set network.${iface_name}.proto='static'
    uci set network.${iface_name}.device="${trunk}.${vid}"
    uci set network.${iface_name}.ipaddr="${ip}"
    uci set network.${iface_name}.netmask='255.255.255.252'
    uci set network.${iface_name}.gateway="${gw}"
    uci set network.${iface_name}.metric="${metric}"
    uci set network.${iface_name}.defaultroute='0'
}

# === SL4 / WAN4 — VLANs 11, 12, 13 =========================================
#   critical (cr4): VLAN 11, 172.16.11.0/30, metric 10
#   bulk     (br4): VLAN 12, 172.16.12.0/30, metric 30
#   default  (df4): VLAN 13, 172.16.13.0/30, metric 20

add_vlan "$TRUNK_SL4" 11 cr 1  172.16.11.2  172.16.11.1  10
add_vlan "$TRUNK_SL4" 12 br 1  172.16.12.2  172.16.12.1  30
add_vlan "$TRUNK_SL4" 13 df 1  172.16.13.2  172.16.13.1  20

# === SL5 / WAN5 — VLANs 21, 22, 23 =========================================
#   critical (cr5): VLAN 21, 172.16.21.0/30, metric 10
#   bulk     (br5): VLAN 22, 172.16.22.0/30, metric 30
#   default  (df5): VLAN 23, 172.16.23.0/30, metric 20

add_vlan "$TRUNK_SL5" 21 cr 2  172.16.21.2  172.16.21.1  10
add_vlan "$TRUNK_SL5" 22 br 2  172.16.22.2  172.16.22.1  30
add_vlan "$TRUNK_SL5" 23 df 2  172.16.23.2  172.16.23.1  20

# === SL6 / WAN6 — VLANs 31, 32, 33 =========================================
#   critical (cr6): VLAN 31, 172.16.31.0/30, metric 10
#   bulk     (br6): VLAN 32, 172.16.32.0/30, metric 30
#   default  (df6): VLAN 33, 172.16.33.0/30, metric 20

add_vlan "$TRUNK_SL6" 31 cr 3  172.16.31.2  172.16.31.1  10
add_vlan "$TRUNK_SL6" 32 br 3  172.16.32.2  172.16.32.1  30
add_vlan "$TRUNK_SL6" 33 df 3  172.16.33.2  172.16.33.1  20

# === Commit & apply ==========================================================
echo ""
echo "=== Commit network ==="
uci commit network
echo "=== Restart network ==="
/etc/init.d/network restart

echo ""
echo "=== Done: 9 VLAN devices + 9 interfaces created ==="
echo "Verifica con: uci show network | grep vlan"
