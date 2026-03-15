#!/bin/sh
# =============================================================================
# 99-remove-vlan.sh — Rimuove tutta la configurazione VLAN MPQUIC da OpenWrt
# =============================================================================
#
# Script di cleanup: rimuove devices, interfaces, firewall zones,
# mwan3 config e nftables rules creati dagli script 01-04.
#
# Esecuzione:
#   scp 99-remove-vlan.sh root@openwrt:/tmp/
#   ssh root@openwrt 'sh /tmp/99-remove-vlan.sh'
#
# =============================================================================

set -e

echo "=== MPQUIC OpenWrt VLAN cleanup ==="

# --- Network: VLAN devices ---
echo "--- Removing VLAN devices ---"
for vid in 11 12 13 21 22 23 31 32 33; do
    uci -q delete network.vlan${vid} 2>/dev/null && echo "  [-] device vlan${vid}" || true
done

# --- Network: interfaces ---
echo "--- Removing interfaces ---"
for iface in cr4 br4 df4 cr5 br5 df5 cr6 br6 df6; do
    uci -q delete network.${iface} 2>/dev/null && echo "  [-] interface ${iface}" || true
done

uci commit network

# --- Firewall: zones + forwardings ---
echo "--- Removing firewall zones ---"
for z in wan_cr1 wan_br1 wan_df1 wan_cr2 wan_br2 wan_df2 wan_cr3 wan_br3 wan_df3; do
    uci -q delete firewall.${z} 2>/dev/null && echo "  [-] zone ${z}" || true
    uci -q delete firewall.fwd_lan_${z} 2>/dev/null && echo "  [-] forwarding fwd_lan_${z}" || true
done

uci commit firewall

# --- mwan3: interfaces, members, policies, rules ---
echo "--- Removing mwan3 config ---"
for iface in cr4 br4 df4 cr5 br5 df5 cr6 br6 df6; do
    uci -q delete mwan3.${iface} 2>/dev/null && echo "  [-] mwan3 interface ${iface}" || true
done

for m in m_cr1 m_cr2 m_cr3 m_df1 m_df2 m_df3 m_br1 m_br2 m_br3; do
    uci -q delete mwan3.${m} 2>/dev/null && echo "  [-] mwan3 member ${m}" || true
done

for p in pol_critical pol_default pol_bulk; do
    uci -q delete mwan3.${p} 2>/dev/null && echo "  [-] mwan3 policy ${p}" || true
done

for r in rule_sip rule_rtp rule_dscp_ef rule_dns rule_ssh rule_http rule_https rule_https_alt rule_catchall; do
    uci -q delete mwan3.${r} 2>/dev/null && echo "  [-] mwan3 rule ${r}" || true
done

uci commit mwan3

# --- nftables DSCP rules ---
echo "--- Removing nftables DSCP rules ---"
rm -f /etc/nftables.d/90-mpquic-dscp.nft && echo "  [-] /etc/nftables.d/90-mpquic-dscp.nft" || true

# === Restart services ========================================================
echo ""
echo "=== Restarting services ==="
/etc/init.d/network restart
/etc/init.d/firewall restart
/etc/init.d/mwan3 restart 2>/dev/null || true

echo ""
echo "=== Done: all MPQUIC VLAN config removed ==="
