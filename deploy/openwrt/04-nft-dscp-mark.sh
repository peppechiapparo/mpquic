#!/bin/sh
# =============================================================================
# 04-nft-dscp-mark.sh — nftables DSCP→fwmark per mwan3 su OpenWrt
# =============================================================================
#
# mwan3 non supporta match DSCP nativo. Questo script aggiunge regole
# nftables in /etc/nftables.d/ che marcano i pacchetti DSCP EF (46)
# con un fwmark che mwan3 può matchare.
#
# Se si usa fw4 (OpenWrt 22.03+), le custom rules vanno in
# /etc/nftables.d/ oppure in /etc/firewall.user (iptables compat).
#
# Esecuzione:
#   scp 04-nft-dscp-mark.sh root@openwrt:/tmp/
#   ssh root@openwrt 'sh /tmp/04-nft-dscp-mark.sh'
#
# =============================================================================

set -e

echo "=== MPQUIC DSCP→mark nftables rules ==="

# Creare file di regole custom per fw4
cat > /etc/nftables.d/90-mpquic-dscp.nft << 'EOF'
# MPQUIC DSCP classification
# Marca pacchetti con DSCP specifici per routing mwan3
#
# DSCP EF (46) = 0x2e → traffico critico (VoIP)
# DSCP AF41 (34) = 0x22 → traffico critico (video)
# DSCP CS1 (8) = 0x08 → traffico bulk (background)

chain mpquic_dscp_mark {
    type filter hook prerouting priority mangle - 1; policy accept;

    # DSCP EF (Expedited Forwarding) → critical
    ip dscp ef meta mark set 0x100

    # DSCP AF41 (video conferencing) → critical
    ip dscp af41 meta mark set 0x100

    # DSCP CS1 (scavenger/background) → bulk
    ip dscp cs1 meta mark set 0x300
}
EOF

echo "  [+] Created /etc/nftables.d/90-mpquic-dscp.nft"

# Restart firewall per caricare le nuove regole
echo "=== Restart firewall ==="
/etc/init.d/firewall restart

echo ""
echo "=== Done: DSCP marking rules installed ==="
echo "Verifica con: nft list chain inet fw4 mpquic_dscp_mark"
echo ""
echo "NOTA: per usare questi mark in mwan3, aggiungere nelle rules:"
echo "  uci set mwan3.rule_dscp_ef.sticky='0'"
echo "  # Il mark 0x100 viene già applicato dal nftables chain"
