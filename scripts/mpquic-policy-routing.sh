#!/usr/bin/env bash
set -euo pipefail

LAN_SUBNETS=(
  "172.16.1.0/30"
  "172.16.2.0/30"
  "172.16.3.0/30"
  "172.16.4.0/30"
  "172.16.5.0/30"
  "172.16.6.0/30"
)

WAN_DEVS=(
  "enp7s3"
  "enp7s4"
  "enp7s5"
  "enp7s6"
  "enp7s7"
  "enp7s8"
)

TUN_DEVS=(
  "mpq1"
  "mpq2"
  "mpq3"
  "mpq4"
  "mpq5"
  "mpq6"
)

WAN_TABLES=("100" "101" "102" "103" "104" "105")
RULE_PRIOS=("1001" "1002" "1003" "1004" "1005" "1006")
SRC_RULE_PRIOS=("1101" "1102" "1103" "1104" "1105" "1106")
REMOTE_RULE_PRIOS=("1201" "1202" "1203" "1204" "1205" "1206")

MGMT_NETS=("10.10.10.0/24" "10.10.11.0/24")
MGMT_DEVS=("enp6s19" "enp6s18")

TRANSIT_SUPERNET="172.16.0.0/16"
TRANSIT_DEV="enp6s20"

WAIT_SECS=2
ENFORCE_WAN_SOURCE="${MPQUIC_ENFORCE_WAN_SOURCE:-0}"

# WAN i cui pacchetti LAN escono DIRETTI sul WAN fisico (col masquerade per-oif
# gia' presente), invece di essere instradati dentro mpqN. Lista dev separata da
# spazi. TS-017 (IBLEA-M): STARLINK su enp7s8 va diretto dopo aver tolto STRIPES
# dal data-path del client. mp1/bd1 non e' toccato (gestito fuori da questo script).
BYPASS_WANS="${MPQUIC_BYPASS_WANS:-enp7s8}"

is_bypass() {
  local d
  for d in $BYPASS_WANS; do
    [[ "$d" == "$1" ]] && return 0
  done
  return 1
}

lan_dev_for_subnet() {
  # Return the local dev holding the .1 of the given /30 (the interface facing
  # the OpenWrt router for that WAN), e.g. 172.16.6.0/30 -> enp7s2.
  local base="${1%/*}"
  local pfx="${base%.*}."
  ip -4 -o addr show 2>/dev/null | awk -v p="$pfx" 'index($4, p)==1 {print $2; exit}'
}

have_ipv4() {
  ip -4 addr show dev "$1" 2>/dev/null | grep -q "inet "
}

have_carrier() {
  local dev="$1"
  local carrier_file="/sys/class/net/${dev}/carrier"
  if [[ -r "$carrier_file" ]]; then
    [[ "$(cat "$carrier_file" 2>/dev/null || echo 0)" = "1" ]]
    return
  fi
  ip link show dev "$dev" 2>/dev/null | grep -q "LOWER_UP"
}

wan_usable() {
  local dev="$1"
  have_ipv4 "$dev" && have_carrier "$dev"
}

have_tun_up() {
  ip link show dev "$1" 2>/dev/null | grep -q "UP"
}

ipv4_for_dev() {
  ip -4 -o addr show dev "$1" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n 1
}

gw_for_dev() {
  local dev="$1"
  local gw=""
  # Primary: kernel routing table (works with systemd-networkd and dhclient)
  gw="$(ip -4 route show dev "$dev" default 2>/dev/null | awk '/default via/{print $3}' | tail -n 1)"
  # Fallback: dhclient lease file (legacy, may be stale after networkd migration)
  if [ -z "$gw" ]; then
    local lease="/var/lib/dhcp/dhclient.${dev}.leases"
    if [ -r "$lease" ]; then
      gw="$(awk '/option routers /{gsub(";","",$3); print $3}' "$lease" | tail -n 1)"
    fi
  fi
  echo "$gw"
}

remote_ip_for_idx() {
  local idx="$1"
  local n=$((idx+1))
  local ip=""
  local cfg_runtime="/run/mpquic/${n}.yaml"
  local cfg_template="/etc/mpquic/instances/${n}.yaml.tpl"
  local cfg_legacy="/etc/mpquic/instances/${n}.yaml"

  if [ -r "$cfg_runtime" ]; then
    ip="$(awk -F': *' '/^remote_addr:/{print $2}' "$cfg_runtime" | tr -d '"' | tail -n 1)"
  fi

  if [ -z "$ip" ] && [ -r "$cfg_template" ]; then
    ip="$(awk -F': *' '/^remote_addr:/{print $2}' "$cfg_template" | tr -d '"' | tail -n 1)"
  fi

  if [ -z "$ip" ] && [ -r "$cfg_legacy" ]; then
    ip="$(awk -F': *' '/^remote_addr:/{print $2}' "$cfg_legacy" | tr -d '"' | tail -n 1)"
  fi

  if [ "$ip" = "VPS_PUBLIC_IP" ] && [ -r /etc/mpquic/global.env ]; then
    ip="$(awk -F= '/^VPS_PUBLIC_IP=/{print $2}' /etc/mpquic/global.env | tail -n 1)"
  fi

  echo "$ip"
}

is_ipv4_lit() {
  [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]
}

safe_ip() { "$@" 2>/dev/null || true; }

sleep "$WAIT_SECS"

# ── ip rules ──────────────────────────────────────────────────────────────
# TS-020: la wan rule (prio 1001-1006, "from 172.16.N.0/30 lookup wanN") e'
# statica: il vecchio del+add apriva una finestra in cui la rule mancava e il
# /30 cadeva in main, uscendo dal default fisico in bypass del tunnel. Con il
# watchdog in churn su tunnel morti la finestra restava quasi sempre aperta.
#
# Le rule src/remote (1101-1106, 1201-1206) dipendono dal DHCP, ma anche per
# loro il "cancella tutte in testa, riaggiungi in coda al giro" e' vietato:
# lasciava le rule remote assenti per secondi a ogni run, i socket QUIC
# attivi perdevano la route (EPERM su sendmsg), le istanze riconnettevano
# ricreando il TUN e networkd, riconfigurando il link appena ricomparso,
# spazzava anche le rule statiche (storm osservato sul banco, 2026-07-27).
# replace_prio_rule confronta la rule esistente con quella voluta: se uguale
# non tocca nulla; se diversa fa del+add adiacenti (finestra sub-ms).

# replace_prio_rule <prio> <table> <table_name> <src_cidr> [<dst_cidr>]
# Con src vuoto rimuove la rule alla priorita' data (stato "non deve esserci").
replace_prio_rule() {
  local rprio="$1" rtable="$2" rtname="$3" rsrc="$4" rdst="${5:-}"
  local cur want_name want_num
  cur="$(ip rule show priority "$rprio" 2>/dev/null | head -n1 | sed "s/^${rprio}:[[:space:]]*//")"
  if [ -z "$rsrc" ]; then
    [ -n "$cur" ] && safe_ip ip rule del priority "$rprio"
    return 0
  fi
  # ip rule show stampa gli host senza /32 e la tabella per nome se mappata
  if [ -n "$rdst" ]; then
    want_name="from ${rsrc%/32} to ${rdst%/32} lookup ${rtname}"
    want_num="from ${rsrc%/32} to ${rdst%/32} lookup ${rtable}"
  else
    want_name="from ${rsrc%/32} lookup ${rtname}"
    want_num="from ${rsrc%/32} lookup ${rtable}"
  fi
  case "$cur" in
    "$want_name"|"$want_num") return 0 ;;
  esac
  [ -n "$cur" ] && safe_ip ip rule del priority "$rprio"
  if [ -n "$rdst" ]; then
    safe_ip ip rule add from "$rsrc" to "$rdst" lookup "$rtable" priority "$rprio"
  else
    safe_ip ip rule add from "$rsrc" lookup "$rtable" priority "$rprio"
  fi
}

# ── Route tables: use "replace" instead of "flush + add" ─────────────────
# "ip route replace" atomically updates or inserts a route without creating
# a blackout window.  The old "flush + add" pattern would briefly remove all
# routes from the table, causing active QUIC sockets to get EPERM on sendmsg
# (kernel rejects the send because the route/interface index changed under
# the socket), which triggered a cascade of tunnel restarts → routing reload
# → more restarts.
for idx in $(seq 0 5); do
  table="${WAN_TABLES[$idx]}"
  dev="${WAN_DEVS[$idx]}"
  tun="${TUN_DEVS[$idx]}"
  subnet="${LAN_SUBNETS[$idx]}"
  prio="${RULE_PRIOS[$idx]}"
  src_prio="${SRC_RULE_PRIOS[$idx]}"
  remote_prio="${REMOTE_RULE_PRIOS[$idx]}"

  rip="$(remote_ip_for_idx "$idx")"
  src_ip="$(ipv4_for_dev "$dev")"

  # Management and transit routes: always present, replace is idempotent
  safe_ip ip route replace "${MGMT_NETS[0]}" dev "${MGMT_DEVS[0]}" table "$table"
  safe_ip ip route replace "${MGMT_NETS[1]}" dev "${MGMT_DEVS[1]}" table "$table"
  safe_ip ip route replace "$TRANSIT_SUPERNET" dev "$TRANSIT_DEV" table "$table"
  # Specific /30 back to the router on its own bridge (more specific than the
  # transit /16). Without this, return traffic to the router's per-WAN /30
  # follows 172.16.0.0/16 dev enp6s20 onto the wrong bridge and is dropped
  # -> heavy loss / mwan3 flapping. Mirrors table bd1.
  lan_dev="$(lan_dev_for_subnet "$subnet")"
  if [ -n "$lan_dev" ]; then
    safe_ip ip route replace "$subnet" dev "$lan_dev" scope link table "$table"
  fi

  gw="$(gw_for_dev "$dev")"

  # VPS host route: tied to the WAN's own state only, NOT to the tunnel's.
  # This table is per-WAN, not per-tunnel: other mpquic instances bound to
  # the same physical interface (e.g. mp1 on enp7s8 pinned into wan6 via a
  # remote ip rule) rely on this route to reach the VPS even while mpqN is
  # down. Coupling it to have_tun_up caused a 18.4s stripe blackout on mp1
  # during a `systemctl stop mpquic@6` test (IBLEA-M, 2026-07-22): the
  # networkd-dispatcher hook re-ran this script, saw mpq6 down, and deleted
  # the shared host route + set a blackhole default in wan6 out from under
  # mp1's still-alive pipes.
  if wan_usable "$dev"; then
    if [ -n "$gw" ] && is_ipv4_lit "$rip"; then
      # Atomically replaces any stale VPS host route (e.g. after DHCP gateway change)
      safe_ip ip route replace "${rip}/32" via "$gw" dev "$dev" table "$table"
    fi
  else
    # Remove stale VPS host route if present
    if is_ipv4_lit "$rip"; then
      safe_ip ip route del "${rip}/32" table "$table"
    fi
  fi

  # Default route: bypass WAN -> diretto sul fisico; altrimenti dentro il tunnel.
  if is_bypass "$dev"; then
    # TS-017 bypass: il traffico LAN esce DIRETTO dal WAN fisico (no tunnel).
    # L'egress e' mascherato dalla regola nft per-oif gia' esistente. Non dipende
    # da have_tun_up: mpqN puo' anche essere fermo.
    if wan_usable "$dev" && [ -n "$gw" ]; then
      safe_ip ip route replace default via "$gw" dev "$dev" table "$table"
    else
      safe_ip ip route replace blackhole default table "$table"
    fi
  elif wan_usable "$dev" && have_tun_up "$tun"; then
    # Atomically replaces blackhole default (if present) with tunnel default
    safe_ip ip route replace default dev "$tun" table "$table"
  else
    # WAN or TUN is down: replace default with blackhole (atomic)
    safe_ip ip route replace blackhole default table "$table"
  fi

  # TS-020: add-if-absent, la rule statica non si tocca se c'e' gia'
  ip rule show | grep -q "^${prio}:" || safe_ip ip rule add from "$subnet" lookup "$table" priority "$prio"

  # Rule DHCP-dipendenti: aggiornate solo se cambiate davvero (vedi commento
  # in testa alla sezione). Il nome tabella serve per il confronto con
  # l'output di ip rule show, che stampa il nome quando rt_tables lo mappa.
  tname="wan$((idx+1))"

  if [ "$ENFORCE_WAN_SOURCE" = "1" ] && [ -n "$src_ip" ]; then
    replace_prio_rule "$src_prio" "$table" "$tname" "${src_ip}/32"
  else
    replace_prio_rule "$src_prio" "$table" "$tname" ""
  fi

  if [ -n "$src_ip" ] && is_ipv4_lit "$rip"; then
    replace_prio_rule "$remote_prio" "$table" "$tname" "${src_ip}/32" "${rip}/32"
  else
    replace_prio_rule "$remote_prio" "$table" "$tname" ""
  fi
done

exit 0
