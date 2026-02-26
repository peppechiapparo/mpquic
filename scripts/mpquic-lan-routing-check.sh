#!/usr/bin/env bash
set -euo pipefail

MODE="${1:-check}"
TARGET="${2:-all}"

if [[ "$MODE" != "check" && "$MODE" != "fix" ]]; then
  echo "usage: $0 <check|fix> [all|1|2|3|4|5|6]" >&2
  exit 1
fi

LAN_SUBNETS=(
  "172.16.1.0/30"
  "172.16.2.0/30"
  "172.16.3.0/30"
  "172.16.4.0/30"
  "172.16.5.0/30"
  "172.16.6.0/30"
)

WAN_DEVS=("enp7s3" "enp7s4" "enp7s5" "enp7s6" "enp7s7" "enp7s8")
TUN_DEVS=("mpq1" "mpq2" "mpq3" "mpq4" "mpq5" "mpq6")
TABLES=("100" "101" "102" "103" "104" "105")
TABLE_LOOKUPS=("wan1" "wan2" "wan3" "wan4" "wan5" "wan6")
PRIOS=("1001" "1002" "1003" "1004" "1005" "1006")

degraded=0

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

tun_up() {
  ip link show dev "$1" 2>/dev/null | grep -q "UP"
}

rule_present() {
  local subnet="$1"
  local table="$2"
  local lookup_name="$3"
  local prio="$4"
  local rules
  rules="$(ip rule show)"
  echo "$rules" | grep -q "${prio}:.*from ${subnet}.*lookup ${lookup_name}" && return 0
  echo "$rules" | grep -q "${prio}:.*from ${subnet}.*lookup ${table}" && return 0
  return 1
}

default_route_kind() {
  local table="$1"
  if ip route show table "$table" | grep -q '^default dev mpq'; then
    echo "tun"
    return
  fi
  if ip route show table "$table" | grep -q '^blackhole default'; then
    echo "blackhole"
    return
  fi
  echo "missing"
}

indexes() {
  if [[ "$TARGET" == "all" ]]; then
    echo "0 1 2 3 4 5"
    return
  fi
  if [[ "$TARGET" =~ ^[1-6]$ ]]; then
    echo "$((TARGET-1))"
    return
  fi
  echo "invalid"
}

IDX_LIST="$(indexes)"
if [[ "$IDX_LIST" == "invalid" ]]; then
  echo "usage: $0 <check|fix> [all|1|2|3|4|5|6]" >&2
  exit 1
fi

if [[ "$MODE" == "fix" ]]; then
  /usr/local/sbin/mpquic-healthcheck.sh client fix || true
  systemctl restart mpquic-routing.service || true
fi

echo "[mpquic-lan-routing-check] mode=$MODE target=$TARGET"

for idx in $IDX_LIST; do
  n=$((idx + 1))
  subnet="${LAN_SUBNETS[$idx]}"
  wan="${WAN_DEVS[$idx]}"
  tun="${TUN_DEVS[$idx]}"
  table="${TABLES[$idx]}"
  lookup_name="${TABLE_LOOKUPS[$idx]}"
  prio="${PRIOS[$idx]}"

  expected="blackhole"
  if wan_usable "$wan" && tun_up "$tun"; then
    expected="tun"
  fi

  got="$(default_route_kind "$table")"
  ok_rule=0
  ok_default=0

  if rule_present "$subnet" "$table" "$lookup_name" "$prio"; then
    ok_rule=1
  fi
  if [[ "$got" == "$expected" ]]; then
    ok_default=1
  fi

  if [[ "$ok_rule" -eq 1 && "$ok_default" -eq 1 ]]; then
    echo "[OK]   lan$n subnet=$subnet table=$table default=$got"
  else
    echo "[FAIL] lan$n subnet=$subnet table=$table expected_default=$expected got=$got rule_ok=$ok_rule"
    degraded=$((degraded + 1))
  fi
done

echo "[mpquic-lan-routing-check] degraded=$degraded"
if [[ "$degraded" -gt 0 ]]; then
  exit 2
fi

exit 0
