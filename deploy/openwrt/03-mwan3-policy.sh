#!/bin/sh
# =============================================================================
# 03-mwan3-policy.sh — OpenWrt mwan3 interfaces, members, policies e rules
# =============================================================================
#
# Configura mwan3 per distribuire il traffico LAN verso i 9 tunnel MPQUIC
# in base alla classe di traffico:
#
#   critical → cr4 + cr5 + cr6 (VoIP, telemetria, DSCP EF)
#   default  → df4 + df5 + df6 (web, HTTPS, API)
#   bulk     → br4 + br5 + br6 (backup, sync, download)
#
# Ogni classe usa 3 interfacce (una per WAN) in load-balance.
# Failover: se un tunnel è DOWN, il traffico migra sugli altri 2.
#
# Dipendenze: eseguire DOPO 01-network-vlan.sh e 02-firewall-zones.sh
#
# Esecuzione:
#   scp 03-mwan3-policy.sh root@openwrt:/tmp/
#   ssh root@openwrt 'sh /tmp/03-mwan3-policy.sh'
#
# =============================================================================

set -e

echo "=== MPQUIC OpenWrt mwan3 Configuration ==="

# =============================================================================
# 1) INTERFACES — tracking con ping verso TBOX gateway (.1)
# =============================================================================
echo ""
echo "--- mwan3 interfaces ---"

add_mwan3_iface() {
    local name="$1" track_ip="$2" metric="$3"

    echo "  [+] Interface $name → track $track_ip (metric $metric)"

    uci set mwan3.${name}=interface
    uci set mwan3.${name}.enabled='1'
    uci set mwan3.${name}.family='ipv4'
    uci set mwan3.${name}.proto='static'
    uci set mwan3.${name}.track_ip="${track_ip}"
    uci set mwan3.${name}.track_method='ping'
    uci set mwan3.${name}.reliability='1'
    uci set mwan3.${name}.count='3'
    uci set mwan3.${name}.size='56'
    uci set mwan3.${name}.timeout='3'
    uci set mwan3.${name}.interval='5'
    uci set mwan3.${name}.failure_interval='3'
    uci set mwan3.${name}.recovery_interval='3'
    uci set mwan3.${name}.down='3'
    uci set mwan3.${name}.up='3'
    uci set mwan3.${name}.metric="${metric}"
}

# WAN4 / SL4
add_mwan3_iface cr4 172.16.11.1 10
add_mwan3_iface br4 172.16.12.1 30
add_mwan3_iface df4 172.16.13.1 20

# WAN5 / SL5
add_mwan3_iface cr5 172.16.21.1 10
add_mwan3_iface br5 172.16.22.1 30
add_mwan3_iface df5 172.16.23.1 20

# WAN6 / SL6
add_mwan3_iface cr6 172.16.31.1 10
add_mwan3_iface br6 172.16.32.1 30
add_mwan3_iface df6 172.16.33.1 20


# =============================================================================
# 2) MEMBERS — peso per bilanciamento load-balance intra-classe
# =============================================================================
echo ""
echo "--- mwan3 members ---"

add_member() {
    local name="$1" iface="$2" metric="$3" weight="$4"

    echo "  [+] Member $name → $iface (metric=$metric weight=$weight)"

    uci set mwan3.${name}=member
    uci set mwan3.${name}.interface="${iface}"
    uci set mwan3.${name}.metric="${metric}"
    uci set mwan3.${name}.weight="${weight}"
}

# Critical class members (load-balanced, same priority)
add_member m_cr1 cr4 1 10
add_member m_cr2 cr5 1 10
add_member m_cr3 cr6 1 10

# Default class members
add_member m_df1 df4 1 10
add_member m_df2 df5 1 10
add_member m_df3 df6 1 10

# Bulk class members
add_member m_br1 br4 1 10
add_member m_br2 br5 1 10
add_member m_br3 br6 1 10


# =============================================================================
# 3) POLICIES — ogni classe mappa ai suoi 3 members
# =============================================================================
echo ""
echo "--- mwan3 policies ---"

echo "  [+] Policy pol_critical → m_cr1 + m_cr2 + m_cr3"
uci set mwan3.pol_critical=policy
uci add_list mwan3.pol_critical.use_member='m_cr1'
uci add_list mwan3.pol_critical.use_member='m_cr2'
uci add_list mwan3.pol_critical.use_member='m_cr3'
uci set mwan3.pol_critical.last_resort='default'

echo "  [+] Policy pol_default → m_df1 + m_df2 + m_df3"
uci set mwan3.pol_default=policy
uci add_list mwan3.pol_default.use_member='m_df1'
uci add_list mwan3.pol_default.use_member='m_df2'
uci add_list mwan3.pol_default.use_member='m_df3'
uci set mwan3.pol_default.last_resort='default'

echo "  [+] Policy pol_bulk → m_br1 + m_br2 + m_br3"
uci set mwan3.pol_bulk=policy
uci add_list mwan3.pol_bulk.use_member='m_br1'
uci add_list mwan3.pol_bulk.use_member='m_br2'
uci add_list mwan3.pol_bulk.use_member='m_br3'
uci set mwan3.pol_bulk.last_resort='default'


# =============================================================================
# 4) RULES — classificazione traffico per policy
# =============================================================================
echo ""
echo "--- mwan3 rules ---"

# ---- CRITICAL: VoIP + telemetria + DSCP EF ----------------------------------

echo "  [+] Rule: SIP (UDP 5060) → pol_critical"
uci set mwan3.rule_sip=rule
uci set mwan3.rule_sip.proto='udp'
uci set mwan3.rule_sip.dest_port='5060'
uci set mwan3.rule_sip.use_policy='pol_critical'

echo "  [+] Rule: RTP (UDP 10000-20000) → pol_critical"
uci set mwan3.rule_rtp=rule
uci set mwan3.rule_rtp.proto='udp'
uci set mwan3.rule_rtp.dest_port='10000-20000'
uci set mwan3.rule_rtp.use_policy='pol_critical'

echo "  [+] Rule: DSCP EF (46) → pol_critical"
uci set mwan3.rule_dscp_ef=rule
uci set mwan3.rule_dscp_ef.proto='all'
uci set mwan3.rule_dscp_ef.use_policy='pol_critical'
# Nota: mwan3 non supporta match DSCP nativo.
# Per DSCP EF usare iptables/nftables mark + ip rule, oppure
# aggiungere custom iptables rule in /etc/firewall.user:
#   iptables -t mangle -A PREROUTING -m dscp --dscp 46 -j MARK --set-mark 0x100
# e poi matchare il mark nella rule mwan3.
# Alternativa: usare nftables con `meta mark` (vedi 04-nft-dscp-mark.sh)

echo "  [+] Rule: DNS (UDP 53) → pol_critical"
uci set mwan3.rule_dns=rule
uci set mwan3.rule_dns.proto='udp'
uci set mwan3.rule_dns.dest_port='53'
uci set mwan3.rule_dns.use_policy='pol_critical'

echo "  [+] Rule: SSH (TCP 22) → pol_critical"
uci set mwan3.rule_ssh=rule
uci set mwan3.rule_ssh.proto='tcp'
uci set mwan3.rule_ssh.dest_port='22'
uci set mwan3.rule_ssh.use_policy='pol_critical'

# ---- DEFAULT: web, HTTPS, API -----------------------------------------------

echo "  [+] Rule: HTTP (TCP 80) → pol_default"
uci set mwan3.rule_http=rule
uci set mwan3.rule_http.proto='tcp'
uci set mwan3.rule_http.dest_port='80'
uci set mwan3.rule_http.use_policy='pol_default'

echo "  [+] Rule: HTTPS (TCP 443) → pol_default"
uci set mwan3.rule_https=rule
uci set mwan3.rule_https.proto='tcp'
uci set mwan3.rule_https.dest_port='443'
uci set mwan3.rule_https.use_policy='pol_default'

echo "  [+] Rule: HTTPS-alt (TCP 8443) → pol_default"
uci set mwan3.rule_https_alt=rule
uci set mwan3.rule_https_alt.proto='tcp'
uci set mwan3.rule_https_alt.dest_port='8443'
uci set mwan3.rule_https_alt.use_policy='pol_default'

# ---- BULK: tutto il resto → catch-all ---------------------------------------

echo "  [+] Rule: catch-all → pol_bulk"
uci set mwan3.rule_catchall=rule
uci set mwan3.rule_catchall.proto='all'
uci set mwan3.rule_catchall.use_policy='pol_bulk'
# IMPORTANTE: questa rule deve essere l'ULTIMA nella lista.
# Le rule mwan3 sono valutate in ordine; la prima che matcha vince.


# =============================================================================
# 5) Commit & apply
# =============================================================================
echo ""
echo "=== Commit mwan3 ==="
uci commit mwan3
echo "=== Restart mwan3 ==="
/etc/init.d/mwan3 restart

echo ""
echo "=== Done: 9 interfaces + 9 members + 3 policies + rules ==="
echo "Verifica con:"
echo "  mwan3 status"
echo "  mwan3 interfaces"
echo "  uci show mwan3"
