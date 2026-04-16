#!/bin/sh
# =============================================================================
# 05-mwan3-bond.sh — mwan3 interface for mp1 bonding tunnel (BOND1)
# =============================================================================
#
# Configura mwan3 per il tunnel bonding mp1 (stripe multipath wan5+wan6).
# Il TUN device 'mp1' è creato dinamicamente dal binario mpquic.
#
# Problema risolto: BOND1 risultava OFFLINE perché mwan3 non aveva una
# configurazione di health check corretta per l'interfaccia mp1.
#
# Prerequisiti:
#   - mpquic con config mp1.yaml in esecuzione (crea il TUN device mp1)
#   - Server mp1 raggiungibile su 10.200.17.254
#   - Script 03-mwan3-policy.sh già eseguito (per le policy base)
#
# Esecuzione:
#   scp 05-mwan3-bond.sh root@openwrt:/tmp/
#   ssh root@openwrt 'sh /tmp/05-mwan3-bond.sh'
#
# =============================================================================

set -eu

# --- Configurazione ---
BOND_IFACE="BOND1"
TUN_DEV="mp1"
TUN_CLIENT_IP="10.200.17.1"
TUN_SERVER_IP="10.200.17.254"
TUN_NETMASK="255.255.255.0"

echo "=== MPQUIC mwan3 BOND1 Configuration ==="
echo ""

# =============================================================================
# 1) NETWORK — interfaccia statica per il TUN device mp1
# =============================================================================
echo "--- Network interface ---"
echo "  [+] Interface ${BOND_IFACE} → device ${TUN_DEV} (${TUN_CLIENT_IP}/24)"

uci set network.${BOND_IFACE}=interface
uci set network.${BOND_IFACE}.proto='static'
uci set network.${BOND_IFACE}.device="${TUN_DEV}"
uci set network.${BOND_IFACE}.ipaddr="${TUN_CLIENT_IP}"
uci set network.${BOND_IFACE}.netmask="${TUN_NETMASK}"
# Non impostare gateway: il routing è gestito da mwan3 e dal tunnel stesso.
# defaultroute=0 evita che OpenWrt aggiunga una default route via mp1.
uci set network.${BOND_IFACE}.defaultroute='0'
# peerdns=0: il DNS non deve venire da questo tunnel
uci set network.${BOND_IFACE}.peerdns='0'

uci commit network
echo "  → network committed"

# =============================================================================
# 2) FIREWALL — aggiungere BOND1 alla zona wan (per mwan3 routing)
# =============================================================================
echo ""
echo "--- Firewall zone ---"

# Cerca la zona 'wan' e aggiungi BOND1 se non già presente
ZONE_WAN=$(uci show firewall | grep "=zone" | grep -v "@" | while read -r line; do
    section=$(echo "$line" | cut -d'=' -f1)
    name=$(uci get "${section}.name" 2>/dev/null || true)
    if [ "$name" = "wan" ]; then
        echo "${section}" | sed 's/\.name$//' | sed 's/firewall\.//'
        break
    fi
done)

if [ -n "${ZONE_WAN}" ]; then
    CURRENT=$(uci get "firewall.${ZONE_WAN}.network" 2>/dev/null || true)
    if echo "$CURRENT" | grep -qw "${BOND_IFACE}"; then
        echo "  [=] ${BOND_IFACE} already in firewall zone wan"
    else
        echo "  [+] Adding ${BOND_IFACE} to firewall zone wan (${ZONE_WAN})"
        uci add_list "firewall.${ZONE_WAN}.network=${BOND_IFACE}"
        uci commit firewall
        echo "  → firewall committed"
    fi
else
    echo "  [!] WARNING: zona 'wan' non trovata. Aggiungere manualmente:"
    echo "      uci add_list firewall.@zone[1].network='${BOND_IFACE}'"
fi

# =============================================================================
# 3) MWAN3 — interfaccia con health check via ping al server TUN
# =============================================================================
echo ""
echo "--- mwan3 interface ---"
echo "  [+] Interface ${BOND_IFACE} → track ${TUN_SERVER_IP}"

uci set mwan3.${BOND_IFACE}=interface
uci set mwan3.${BOND_IFACE}.enabled='1'
uci set mwan3.${BOND_IFACE}.family='ipv4'
uci set mwan3.${BOND_IFACE}.proto='static'
uci set mwan3.${BOND_IFACE}.track_ip="${TUN_SERVER_IP}"
uci set mwan3.${BOND_IFACE}.track_method='ping'
uci set mwan3.${BOND_IFACE}.reliability='1'
uci set mwan3.${BOND_IFACE}.count='3'
uci set mwan3.${BOND_IFACE}.size='56'
uci set mwan3.${BOND_IFACE}.timeout='4'
uci set mwan3.${BOND_IFACE}.interval='10'
uci set mwan3.${BOND_IFACE}.failure_interval='5'
uci set mwan3.${BOND_IFACE}.recovery_interval='5'
uci set mwan3.${BOND_IFACE}.down='3'
uci set mwan3.${BOND_IFACE}.up='3'
# Metrica alta: il bonding è il tunnel primario
uci set mwan3.${BOND_IFACE}.metric='5'

# =============================================================================
# 4) MWAN3 — member e policy per BOND1
# =============================================================================
echo ""
echo "--- mwan3 member ---"
echo "  [+] Member m_bond1 → ${BOND_IFACE} (metric=1 weight=10)"

uci set mwan3.m_bond1=member
uci set mwan3.m_bond1.interface="${BOND_IFACE}"
uci set mwan3.m_bond1.metric='1'
uci set mwan3.m_bond1.weight='10'

echo ""
echo "--- mwan3 policy ---"
echo "  [+] Policy pol_bond → m_bond1 (fallback to default route)"

uci set mwan3.pol_bond=policy
uci add_list mwan3.pol_bond.use_member='m_bond1'
uci set mwan3.pol_bond.last_resort='default'

# =============================================================================
# 5) Commit & apply
# =============================================================================
echo ""
echo "=== Commit mwan3 ==="
uci commit mwan3

echo "=== Reload network & mwan3 ==="
/etc/init.d/network reload
sleep 3
/etc/init.d/mwan3 restart

echo ""
echo "=== Done: BOND1 (mp1 tunnel) configured in mwan3 ==="
echo ""
echo "Verifica:"
echo "  mwan3 status              # BOND1 deve essere ONLINE"
echo "  mwan3 interfaces          # Mostra stato di tutte le interfacce"
echo "  ping -I mp1 ${TUN_SERVER_IP}   # Ping diretto via tunnel"
echo ""
echo "Note:"
echo "  - Il track_ip (${TUN_SERVER_IP}) è il server-side del tunnel mp1"
echo "  - Il tunnel mp1 deve essere UP (mpquic running) per il ping"
echo "  - Se BOND1 resta OFFLINE, verificare:"
echo "    1) ping -I mp1 ${TUN_SERVER_IP}  → deve rispondere"
echo "    2) ip link show mp1              → deve essere UP"
echo "    3) ip addr show mp1              → deve avere ${TUN_CLIENT_IP}/24"
echo "    4) logread | grep mwan3          → errori di tracking"
