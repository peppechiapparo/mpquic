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

# ── ip rules: delete stale, re-add current ───────────────────────────────
# Rules are deleted and re-added to pick up any source IP changes from DHCP.
# The brief window where a rule is absent only affects new connections from
# LAN subnets, not active tunnel sockets.
for idx in $(seq 0 5); do
  safe_ip ip rule del from "${LAN_SUBNETS[$idx]}" priority "${RULE_PRIOS[$idx]}"
  safe_ip ip rule del priority "${SRC_RULE_PRIOS[$idx]}"
  safe_ip ip rule del priority "${REMOTE_RULE_PRIOS[$idx]}"
done

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

  if wan_usable "$dev" && have_tun_up "$tun"; then
    gw="$(gw_for_dev "$dev")"

    if [ -n "$gw" ] && is_ipv4_lit "$rip"; then
      # Atomically replaces any stale VPS host route (e.g. after DHCP gateway change)
      safe_ip ip route replace "${rip}/32" via "$gw" dev "$dev" table "$table"
    fi

    # Atomically replaces blackhole default (if present) with tunnel default
    safe_ip ip route replace default dev "$tun" table "$table"
  else
    # WAN or TUN is down: replace default with blackhole (atomic)
    safe_ip ip route replace blackhole default table "$table"
    # Remove stale VPS host route if present
    if is_ipv4_lit "$rip"; then
      safe_ip ip route del "${rip}/32" table "$table"
    fi
  fi

  safe_ip ip rule add from "$subnet" lookup "$table" priority "$prio"

  if [ "$ENFORCE_WAN_SOURCE" = "1" ]; then
    if [ -n "$src_ip" ]; then
      safe_ip ip rule add from "${src_ip}/32" lookup "$table" priority "$src_prio"
    fi
  fi

  if [ -n "$src_ip" ] && is_ipv4_lit "$rip"; then
    safe_ip ip rule add from "${src_ip}/32" to "${rip}/32" lookup "$table" priority "$remote_prio"
  fi
done

exit 0
