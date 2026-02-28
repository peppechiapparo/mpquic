#!/usr/bin/env bash
###############################################################################
# mpquic-vlan-classifier.sh — VLAN-based traffic classifier (Step 2.5)
#
# Routes traffic from VLAN sub-interfaces into the correct tunnel:
#
#   VLAN 11 (enp6s23.11) → cr1  (critical via WAN4/SL4)
#   VLAN 12 (enp6s23.12) → br1  (bulk     via WAN4/SL4)
#   VLAN 13 (enp6s23.13) → df1  (default  via WAN4/SL4)
#   VLAN 21 (enp7s1.21)  → cr2  (critical via WAN5/SL5)
#   VLAN 22 (enp7s1.22)  → br2  (bulk     via WAN5/SL5)
#   VLAN 23 (enp7s1.23)  → df2  (default  via WAN5/SL5)
#   VLAN 31 (enp7s2.31)  → cr3  (critical via WAN6/SL6)
#   VLAN 32 (enp7s2.32)  → br3  (bulk     via WAN6/SL6)
#   VLAN 33 (enp7s2.33)  → df3  (default  via WAN6/SL6)
#
# Classification uses 'iif' (incoming interface) based ip rules, which is
# cleaner than source-IP matching for VLAN-tagged traffic.
#
# Usage:
#   mpquic-vlan-classifier.sh apply  [set1|set2|set3|all]
#   mpquic-vlan-classifier.sh remove [set1|set2|set3|all]
#   mpquic-vlan-classifier.sh status
###############################################################################
set -euo pipefail

# ── Configuration ──────────────────────────────────────────────────────────
# Each entry: VLAN_IFACE  TUNNEL  TABLE_NAME  TABLE_ID  PRIORITY
#
# Table IDs 120-128 (avoid conflict with wan1-6=100-105, mt_cr5/df5/bk5=110-112)
# Priorities 800-808 (higher priority than wan rules at 1001-1006)

ENTRIES_SET1=(
  "enp6s23.11  cr1  mt_cr1  120  800"
  "enp6s23.12  br1  mt_br1  121  801"
  "enp6s23.13  df1  mt_df1  122  802"
)
ENTRIES_SET2=(
  "enp7s1.21   cr2  mt_cr2  123  803"
  "enp7s1.22   br2  mt_br2  124  804"
  "enp7s1.23   df2  mt_df2  125  805"
)
ENTRIES_SET3=(
  "enp7s2.31   cr3  mt_cr3  126  806"
  "enp7s2.32   br3  mt_br3  127  807"
  "enp7s2.33   df3  mt_df3  128  808"
)

# WAN gateways and devices per set, for VPS route
declare -A SET_WAN_DEV=( [set1]="enp7s6" [set2]="enp7s7" [set3]="enp7s8" )

# Management / transit networks (same as other classifier scripts)
MGMT_NETS=("10.10.10.0/24" "10.10.11.0/24")
MGMT_DEVS=("enp6s19" "enp6s18")
TRANSIT_SUPERNET="172.16.0.0/16"
TRANSIT_DEV="enp6s20"

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
  local dev="$1"
  ip -4 route show dev "$dev" default 2>/dev/null \
    | awk '/default via/{print $3}' | tail -n 1
}

ensure_rt_tables() {
  local -a entries=("$@")
  for entry in "${entries[@]}"; do
    read -r _iface _tun table_name table_id _prio <<< "$entry"
    if ! grep -qE "^${table_id}\s" /etc/iproute2/rt_tables 2>/dev/null; then
      echo "${table_id} ${table_name}" >> /etc/iproute2/rt_tables
      echo "  added rt_tables: ${table_id} ${table_name}"
    fi
  done
}

# ── Get entries for a given set ────────────────────────────────────────────
get_entries() {
  local set_name="$1"
  case "$set_name" in
    set1) printf '%s\n' "${ENTRIES_SET1[@]}" ;;
    set2) printf '%s\n' "${ENTRIES_SET2[@]}" ;;
    set3) printf '%s\n' "${ENTRIES_SET3[@]}" ;;
    all)
      printf '%s\n' "${ENTRIES_SET1[@]}"
      printf '%s\n' "${ENTRIES_SET2[@]}"
      printf '%s\n' "${ENTRIES_SET3[@]}"
      ;;
    *) echo "unknown set: $set_name (use set1|set2|set3|all)" >&2; exit 1 ;;
  esac
}

get_wan_dev_for_set() {
  case "$1" in
    set1) echo "enp7s6" ;;
    set2) echo "enp7s7" ;;
    set3) echo "enp7s8" ;;
  esac
}

set_for_entry() {
  local prio="$1"
  if (( prio >= 800 && prio <= 802 )); then echo "set1"
  elif (( prio >= 803 && prio <= 805 )); then echo "set2"
  elif (( prio >= 806 && prio <= 808 )); then echo "set3"
  fi
}

# ── Apply ──────────────────────────────────────────────────────────────────
do_apply() {
  local target="${1:-all}"
  echo "=== VLAN Classifier: APPLY ($target) ==="

  local vps_ip
  vps_ip="$(get_vps_ip)"

  # Collect all entries for the requested sets
  local -a sets_to_apply=()
  case "$target" in
    all)  sets_to_apply=(set1 set2 set3) ;;
    set*) sets_to_apply=("$target") ;;
  esac

  for set_name in "${sets_to_apply[@]}"; do
    local wan_dev
    wan_dev="$(get_wan_dev_for_set "$set_name")"
    local wan_gw
    wan_gw="$(get_wan_gw "$wan_dev")"

    echo ""
    echo "  --- $set_name (WAN=$wan_dev, GW=${wan_gw:-?}) ---"

    mapfile -t entries < <(get_entries "$set_name")
    ensure_rt_tables "${entries[@]}"

    for entry in "${entries[@]}"; do
      read -r iface tun table_name table_id prio <<< "$entry"

      echo "  [$tun] iface=$iface table=$table_name prio=$prio"

      # Flush and rebuild routing table
      safe_ip route flush table "$table_name"

      # Management + transit routes
      for i in 0 1; do
        safe_ip route add "${MGMT_NETS[$i]}" dev "${MGMT_DEVS[$i]}" table "$table_name"
      done
      safe_ip route add "$TRANSIT_SUPERNET" dev "$TRANSIT_DEV" table "$table_name"

      # VPS route via WAN gateway
      if [[ -n "$vps_ip" && -n "$wan_gw" ]]; then
        safe_ip route add "${vps_ip}/32" via "$wan_gw" dev "$wan_dev" table "$table_name"
      fi

      # Default route via tunnel
      if ip link show dev "$tun" 2>/dev/null | grep -q "UP"; then
        safe_ip route add default dev "$tun" table "$table_name"
        echo "    default → dev ${tun} ✓"
      else
        safe_ip route add blackhole default table "$table_name"
        echo "    default → blackhole (${tun} not UP)"
      fi

      # Policy rule: incoming interface → table
      safe_ip rule del iif "$iface" priority "$prio"
      ip rule add iif "$iface" lookup "$table_name" priority "$prio"
      echo "    rule: iif ${iface} lookup ${table_name} prio ${prio} ✓"

      # Masquerade: NAT traffic leaving via class tunnel
      if command -v nft >/dev/null 2>&1; then
        if ! nft list chain ip nat postrouting 2>/dev/null | grep -q "oifname \"${tun}\" masquerade"; then
          nft add rule ip nat postrouting oifname "${tun}" masquerade
          echo "    nft: masquerade oifname ${tun} ✓"
        else
          echo "    nft: masquerade oifname ${tun} (already present)"
        fi
      fi
    done
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
  local target="${1:-all}"
  echo "=== VLAN Classifier: REMOVE ($target) ==="

  mapfile -t entries < <(get_entries "$target")

  for entry in "${entries[@]}"; do
    read -r iface tun table_name table_id prio <<< "$entry"

    safe_ip rule del iif "$iface" priority "$prio"
    safe_ip route flush table "$table_name"

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

    echo "  [$tun] removed rule iif=${iface} prio=${prio}, flushed ${table_name}, removed nft masq"
  done

  # Persist nftables rules
  if command -v nft >/dev/null 2>&1 && [[ -f /etc/nftables.conf ]]; then
    nft list ruleset > /etc/nftables.conf
  fi

  echo "=== Classifier removed ==="
}

# ── Status ─────────────────────────────────────────────────────────────────
do_status() {
  echo "=== VLAN Classifier: STATUS ==="

  mapfile -t all_entries < <(get_entries all)

  for entry in "${all_entries[@]}"; do
    read -r iface tun table_name table_id prio <<< "$entry"

    local tun_state="DOWN"
    if ip link show dev "$tun" 2>/dev/null | grep -q "UP"; then
      tun_state="UP"
    fi

    local vlan_state="DOWN"
    if ip link show dev "$iface" 2>/dev/null | grep -q "UP"; then
      vlan_state="UP"
    fi

    local rule_ok="NO"
    if ip rule show | grep -q "iif ${iface}"; then
      rule_ok="YES"
    fi

    local default_route
    default_route="$(ip route show table "$table_name" default 2>/dev/null || echo 'none')"

    printf "  %-4s  vlan=%-14s(%s)  tun=%-4s(%s)  rule=%s  default=%s\n" \
      "[$tun]" "$iface" "$vlan_state" "$tun" "$tun_state" "$rule_ok" "$default_route"
  done

  echo ""
  echo "  Relevant ip rules (prio 800-808):"
  ip rule show | grep -E "prio(rity)? (80[0-8])" || echo "    (none)"
}

# ── Main ───────────────────────────────────────────────────────────────────
ACTION="${1:-status}"
TARGET="${2:-all}"

case "$ACTION" in
  apply)  do_apply  "$TARGET" ;;
  remove) do_remove "$TARGET" ;;
  status) do_status ;;
  *)
    echo "usage: $0 {apply|remove|status} [set1|set2|set3|all]" >&2
    echo ""
    echo "  Sets:"
    echo "    set1  WAN4/SL4: cr1 + br1 + df1 (VLANs 11/12/13)"
    echo "    set2  WAN5/SL5: cr2 + br2 + df2 (VLANs 21/22/23)"
    echo "    set3  WAN6/SL6: cr3 + br3 + df3 (VLANs 31/32/33)"
    echo "    all   All sets (default)"
    exit 1
    ;;
esac
