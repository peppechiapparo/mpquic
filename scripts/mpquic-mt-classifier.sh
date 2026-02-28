#!/usr/bin/env bash
###############################################################################
# mpquic-mt-classifier.sh — Multi-Tunnel traffic classifier (Step 2)
#
# Routes LAN traffic to the correct traffic-class tunnel based on source IP:
#   172.16.4.2  → cr5  (critical: VoIP, telemetry, control)
#   172.16.5.2  → df5  (default:  web, HTTPS, APIs)
#   172.16.6.2  → bk5  (bulk:     backup, sync, transfers)
#
# All three tunnels share the same WAN (enp7s7 / WAN5) and same server port
# (45010) but use distinct TUN interfaces and TUN IPs.
#
# Usage:
#   mpquic-mt-classifier.sh apply   — install rules + routes
#   mpquic-mt-classifier.sh remove  — remove rules + routes
#   mpquic-mt-classifier.sh status  — show current state
###############################################################################
set -euo pipefail

# ── Configuration ──────────────────────────────────────────────────────────
# Source IPs → class tunnel mapping
declare -A CLASS_SRC=(
  [cr5]="172.16.4.2"
  [df5]="172.16.5.2"
  [bk5]="172.16.6.2"
)

# Tunnel devices per class
declare -A CLASS_TUN=(
  [cr5]="cr5"
  [df5]="df5"
  [bk5]="bk5"
)

# Routing table IDs (must not conflict with wan1-wan6 = 100-105)
declare -A CLASS_TABLE_ID=(
  [cr5]="110"
  [df5]="111"
  [bk5]="112"
)

# Routing table names
declare -A CLASS_TABLE_NAME=(
  [cr5]="mt_cr5"
  [df5]="mt_df5"
  [bk5]="mt_bk5"
)

# Rule priorities (must be lower number = higher priority than wan rules 1001-1006)
declare -A CLASS_PRIO=(
  [cr5]="900"
  [df5]="901"
  [bk5]="902"
)

# Shared WAN for all multi-tunnel classes
MT_WAN_DEV="enp7s7"

# Management / transit networks (same as mpquic-policy-routing.sh)
MGMT_NETS=("10.10.10.0/24" "10.10.11.0/24")
MGMT_DEVS=("enp6s19" "enp6s18")
TRANSIT_SUPERNET="172.16.0.0/16"
TRANSIT_DEV="enp6s20"

# Class order for iteration
CLASSES=(cr5 df5 bk5)

# ── Helpers ────────────────────────────────────────────────────────────────
safe_ip() { ip "$@" 2>/dev/null || true; }

get_vps_ip() {
  local vps=""
  if [[ -r /etc/mpquic/global.env ]]; then
    vps="$(awk -F= '/^VPS_PUBLIC_IP=/{print $2}' /etc/mpquic/global.env | tail -n 1)"
  fi
  echo "$vps"
}

get_wan_gw() {
  ip -4 route show dev "$MT_WAN_DEV" default 2>/dev/null \
    | awk '/default via/{print $3}' | tail -n 1
}

ensure_rt_tables() {
  for cls in "${CLASSES[@]}"; do
    local id="${CLASS_TABLE_ID[$cls]}"
    local name="${CLASS_TABLE_NAME[$cls]}"
    if ! grep -qE "^${id}\s" /etc/iproute2/rt_tables 2>/dev/null; then
      echo "${id} ${name}" >> /etc/iproute2/rt_tables
      echo "  added rt_tables entry: ${id} ${name}"
    fi
  done
}

# ── Apply ──────────────────────────────────────────────────────────────────
do_apply() {
  echo "=== Multi-Tunnel Classifier: APPLY ==="
  ensure_rt_tables

  local vps_ip
  vps_ip="$(get_vps_ip)"
  local wan_gw
  wan_gw="$(get_wan_gw)"

  echo "  VPS_IP=${vps_ip:-<unset>}  WAN_GW=${wan_gw:-<unset>}  WAN_DEV=${MT_WAN_DEV}"

  for cls in "${CLASSES[@]}"; do
    local src="${CLASS_SRC[$cls]}"
    local tun="${CLASS_TUN[$cls]}"
    local table="${CLASS_TABLE_NAME[$cls]}"
    local prio="${CLASS_PRIO[$cls]}"

    echo ""
    echo "  [$cls] src=${src} tun=${tun} table=${table} prio=${prio}"

    # Flush table
    safe_ip route flush table "$table"

    # Management + transit routes
    for i in 0 1; do
      safe_ip route add "${MGMT_NETS[$i]}" dev "${MGMT_DEVS[$i]}" table "$table"
    done
    safe_ip route add "$TRANSIT_SUPERNET" dev "$TRANSIT_DEV" table "$table"

    # VPS route via WAN gateway (for QUIC control traffic from this host)
    if [[ -n "$vps_ip" && -n "$wan_gw" ]]; then
      safe_ip route add "${vps_ip}/32" via "$wan_gw" dev "$MT_WAN_DEV" table "$table"
    fi

    # Default route via tunnel
    if ip link show dev "$tun" 2>/dev/null | grep -q "UP"; then
      safe_ip route add default dev "$tun" table "$table"
      echo "    default → dev ${tun} ✓"
    else
      safe_ip route add blackhole default table "$table"
      echo "    default → blackhole (${tun} not UP)"
    fi

    # Policy rule: source IP → table
    safe_ip rule del from "${src}/32" priority "$prio"
    ip rule add from "${src}/32" lookup "$table" priority "$prio"
    echo "    rule: from ${src} lookup ${table} prio ${prio} ✓"

    # Masquerade: NAT traffic leaving via class tunnel so VPS sees
    # src=TUN_IP (e.g. 10.200.10.1) instead of LAN IP (172.16.x.x).
    # This is essential for the server connectionTable return-path lookup.
    if command -v nft >/dev/null 2>&1; then
      if ! nft list chain ip nat postrouting 2>/dev/null | grep -q "oifname \"${tun}\" masquerade"; then
        nft add rule ip nat postrouting oifname "${tun}" masquerade
        echo "    nft: masquerade oifname ${tun} ✓"
      else
        echo "    nft: masquerade oifname ${tun} (already present)"
      fi
    fi
  done

  # Persist nftables rules
  if command -v nft >/dev/null 2>&1 && [[ -f /etc/nftables.conf ]]; then
    nft list ruleset > /etc/nftables.conf
    echo ""
    echo "  nftables rules saved to /etc/nftables.conf"
  fi

  echo ""
  echo "=== Classifier applied ==="
}

# ── Remove ─────────────────────────────────────────────────────────────────
do_remove() {
  echo "=== Multi-Tunnel Classifier: REMOVE ==="

  for cls in "${CLASSES[@]}"; do
    local src="${CLASS_SRC[$cls]}"
    local tun="${CLASS_TUN[$cls]}"
    local table="${CLASS_TABLE_NAME[$cls]}"
    local prio="${CLASS_PRIO[$cls]}"

    safe_ip rule del from "${src}/32" priority "$prio"
    safe_ip route flush table "$table"

    # Remove masquerade rule for this tunnel
    if command -v nft >/dev/null 2>&1; then
      local handle
      handle="$(nft -a list chain ip nat postrouting 2>/dev/null \
        | grep "oifname \"${tun}\" masquerade" \
        | awk '{print $NF}')"
      if [[ -n "$handle" ]]; then
        nft delete rule ip nat postrouting handle "$handle"
      fi
    fi

    echo "  [$cls] removed rule prio=${prio}, flushed table=${table}, removed nft masq"
  done

  # Persist nftables rules
  if command -v nft >/dev/null 2>&1 && [[ -f /etc/nftables.conf ]]; then
    nft list ruleset > /etc/nftables.conf
  fi

  echo "=== Classifier removed ==="
}

# ── Status ─────────────────────────────────────────────────────────────────
do_status() {
  echo "=== Multi-Tunnel Classifier: STATUS ==="

  for cls in "${CLASSES[@]}"; do
    local src="${CLASS_SRC[$cls]}"
    local tun="${CLASS_TUN[$cls]}"
    local table="${CLASS_TABLE_NAME[$cls]}"
    local prio="${CLASS_PRIO[$cls]}"

    local tun_state="DOWN"
    if ip link show dev "$tun" 2>/dev/null | grep -q "UP"; then
      tun_state="UP"
    fi

    local rule_ok="NO"
    if ip rule show | grep -q "from ${src}"; then
      rule_ok="YES"
    fi

    local default_route
    default_route="$(ip route show table "$table" default 2>/dev/null || echo 'none')"

    printf "  %-4s  src=%-14s  tun=%-4s(%s)  rule=%s  default=%s\n" \
      "[$cls]" "$src" "$tun" "$tun_state" "$rule_ok" "$default_route"
  done

  echo ""
  echo "  Relevant ip rules:"
  ip rule show | grep -E "prio(rity)? (900|901|902)" || echo "    (none)"
}

# ── Main ───────────────────────────────────────────────────────────────────
case "${1:-status}" in
  apply)  do_apply  ;;
  remove) do_remove ;;
  status) do_status ;;
  *)
    echo "usage: $0 {apply|remove|status}" >&2
    exit 1
    ;;
esac
