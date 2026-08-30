#!/bin/sh
# =============================================================================
# 06-phy-interfaces.sh — canali fisici di riserva LTE_PHY / STARLINK_PHY
# =============================================================================
#
# Crea su OpenWrt le due interfacce che portano il traffico DIRETTAMENTE sul
# canale fisico della VM mpquic (fallback esplicito, fuori dai tunnel), come
# in esercizio su IBLEA-M (TS-023 banco, TS-026 IBLEA, TS-032 ROMARS,
# TS-034 ricostruzione TBOX-EVO):
#
#   LTE_PHY       eth12.95  172.16.95.2/30  gw 172.16.95.1 (VM enp7s1.95 -> phy5 -> enp7s7)
#   STARLINK_PHY  eth13.96  172.16.96.2/30  gw 172.16.96.1 (VM enp7s2.96 -> phy6 -> enp7s8)
#
# Lato VM devono esistere i file di deploy/networkd/phy/ (VLAN 95/96, tabelle
# phy5/phy6 con rule 1095/1096 e pavimento blackhole) e lo script canonico di
# policy routing che popola il default delle tabelle phy dal lease DHCP.
#
# Tracking mwan3 come su IBLEA-M (TS-029: count 3 / timeout 3, per non
# dichiarare offline un satellitare per un singolo ping perso). I membri
# vengono aggiunti IN CODA (metrica 6/7) alle policy elencate in POLICIES:
# sono l'ultima risorsa dietro tunnel e uplink "veri". Le VLAN 95/96 vengono
# create da OpenWrt per nome (eth13.96), senza sezione device esplicita:
# e' cosi' anche su IBLEA-M.
#
# Idempotente: rieseguirlo non duplica membri ne' voci di zona.
#
# Esecuzione:
#   scp 06-phy-interfaces.sh root@openwrt:/tmp/
#   ssh root@openwrt 'sh /tmp/06-phy-interfaces.sh'
#
# Nota OpenWrt 24.10: `mwan3 reload` non esiste e `mwan3 ifup` NON crea i
# tracker delle interfacce nuove; serve `mwan3 restart` (lo fa lo script).
#
# =============================================================================

set -eu

TRUNK_LTE="${TRUNK_LTE:-eth12}"          # link fisico verso LAN5 (enp7s1) della VM
TRUNK_STARLINK="${TRUNK_STARLINK:-eth13}" # link fisico verso LAN6 (enp7s2) della VM
WAN_ZONE="${WAN_ZONE:-STARLINK}"          # nome della zona firewall wan (masq)
POLICIES="${POLICIES:-BONDING FAILOVER}"  # policy mwan3 che ricevono i membri in coda

add_iface() {
  name=$1; dev=$2; ip=$3; gw=$4; metric=$5
  echo "  [+] network.$name -> $dev $ip/30 gw $gw metric $metric"
  uci set network.$name=interface
  uci set network.$name.proto='static'
  uci set network.$name.device="$dev"
  uci set network.$name.ipaddr="$ip"
  uci set network.$name.netmask='255.255.255.252'
  uci set network.$name.gateway="$gw"
  uci set network.$name.metric="$metric"

  echo "  [+] mwan3.$name (tracking count 3 / timeout 3 / interval 30, up 5 / down 5)"
  uci set mwan3.$name=interface
  uci set mwan3.$name.enabled='1'
  uci set mwan3.$name.initial_state='online'
  uci set mwan3.$name.family='ipv4'
  uci set mwan3.$name.track_method='ping'
  uci set mwan3.$name.reliability='1'
  uci set mwan3.$name.count='3'
  uci set mwan3.$name.timeout='3'
  uci set mwan3.$name.interval='30'
  uci set mwan3.$name.failure_interval='5'
  uci set mwan3.$name.recovery_interval='5'
  uci set mwan3.$name.down='5'
  uci set mwan3.$name.up='5'
  uci -q delete mwan3.$name.track_ip || true
  uci add_list mwan3.$name.track_ip='8.8.8.8'
  uci add_list mwan3.$name.track_ip='1.1.1.1'
}

add_member() {
  member=$1; iface=$2; metric=$3
  echo "  [+] mwan3.$member (interface $iface metric $metric)"
  uci set mwan3.$member=member
  uci set mwan3.$member.interface="$iface"
  uci set mwan3.$member.metric="$metric"
  uci set mwan3.$member.weight='1'
  for pol in $POLICIES; do
    if ! uci -q get mwan3.$pol.use_member | grep -qw "$member"; then
      echo "      -> in coda a policy $pol"
      uci add_list mwan3.$pol.use_member="$member"
    fi
  done
}

zone_add() {
  net=$1
  idx=$(uci show firewall | grep -E "^firewall\.@zone\[[0-9]+\]\.name='$WAN_ZONE'" | sed -E 's/.*\[([0-9]+)\].*/\1/' | head -1)
  [ -n "$idx" ] || { echo "  [!] zona firewall $WAN_ZONE non trovata"; exit 1; }
  if ! uci -q get firewall.@zone[$idx].network | grep -qw "$net"; then
    echo "  [+] firewall zona $WAN_ZONE += $net"
    uci add_list firewall.@zone[$idx].network="$net"
  fi
}

echo "=== MPQUIC canali fisici PHY (LTE_PHY / STARLINK_PHY) ==="
mkdir -p /root/bak-phy && cp -a /etc/config/network /etc/config/mwan3 /etc/config/firewall /root/bak-phy/
echo "  backup in /root/bak-phy/"

echo "--- interfacce ---"
add_iface LTE_PHY      "$TRUNK_LTE.95"      172.16.95.2 172.16.95.1 23
add_iface STARLINK_PHY "$TRUNK_STARLINK.96" 172.16.96.2 172.16.96.1 25

echo "--- membri mwan3 ---"
add_member STARLINK_PHY_M6_W1 STARLINK_PHY 6
add_member LTE_PHY_M7_W1      LTE_PHY      7

echo "--- firewall ---"
zone_add STARLINK_PHY
zone_add LTE_PHY

uci commit network; uci commit mwan3; uci commit firewall

echo "--- applicazione ---"
/etc/init.d/network reload
sleep 4
/etc/init.d/firewall reload >/dev/null 2>&1 || true
mwan3 restart >/dev/null 2>&1
sleep 8

echo "--- verifica ---"
ip -br addr | grep -E "$TRUNK_LTE\.95|$TRUNK_STARLINK\.96" || echo "  [!] VLAN non presenti"
ping -c 2 -W 1 172.16.95.1 >/dev/null 2>&1 && echo "  near-hop LTE_PHY 172.16.95.1: ok" || echo "  near-hop LTE_PHY: NO (lato VM enp7s1.95 assente?)"
ping -c 2 -W 1 172.16.96.1 >/dev/null 2>&1 && echo "  near-hop STARLINK_PHY 172.16.96.1: ok" || echo "  near-hop STARLINK_PHY: NO (lato VM enp7s2.96 assente?)"
mwan3 status | grep -E "_PHY"
echo ""
echo "Collaudo del percorso completo (deve arrivare a internet via VM/phyN):"
echo "  mwan3 use STARLINK_PHY ping -c 5 8.8.8.8     # NON usare 'ping -I eth13.96': con mwan3 esce da un'altra WAN"
echo "Rollback: cp /root/bak-phy/* /etc/config/ && /etc/init.d/network reload && mwan3 restart"
