#!/usr/bin/env bash
set -euo pipefail

WAN_DEVS=("enp7s3" "enp7s4" "enp7s5" "enp7s6" "enp7s7" "enp7s8")
INSTANCES=("1" "2" "3" "4" "5" "6")
# L2 multi-tunnel per classi (WAN4-6): cr/br/df
CLASS_WAN_DEVS=(
  "enp7s6" "enp7s6" "enp7s6"
  "enp7s7" "enp7s7" "enp7s7"
  "enp7s8" "enp7s8" "enp7s8"
)
CLASS_INSTANCES=("cr4" "br4" "df4" "cr5" "br5" "df5" "cr6" "br6" "df6")
PEER_FAIL_DIR="/run/mpquic/watchdog-peer-fail"
PEER_FAIL_THRESHOLD=2

mkdir -p "$PEER_FAIL_DIR"

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

tun_healthy() {
  local inst="$1"
  local env_file="/etc/mpquic/instances/${inst}.env"

  if [[ ! -f "$env_file" ]]; then
    return 1
  fi

  local tun_name
  local tun_cidr
  tun_name="$(grep -E '^TUN_NAME=' "$env_file" | cut -d= -f2-)"
  tun_cidr="$(grep -E '^TUN_CIDR=' "$env_file" | cut -d= -f2-)"

  if [[ -z "$tun_name" || -z "$tun_cidr" ]]; then
    return 1
  fi

  ip link show dev "$tun_name" >/dev/null 2>&1 || return 1
  ip link show dev "$tun_name" | head -n1 | grep -q "UP" || return 1
  ip -4 addr show dev "$tun_name" | grep -q "inet ${tun_cidr}" || return 1

  return 0
}

derive_peer_from_cidr() {
  local tun_cidr="$1"
  local local_ip="${tun_cidr%/*}"
  local last_octet="${local_ip##*.}"
  local prefix="${local_ip%.*}"

  if [[ "$last_octet" == "1" ]]; then
    echo "${prefix}.2"
    return 0
  fi
  if [[ "$last_octet" == "2" ]]; then
    echo "${prefix}.1"
    return 0
  fi

  return 1
}

peer_reachable() {
  local inst="$1"
  local env_file="/etc/mpquic/instances/${inst}.env"

  if [[ ! -f "$env_file" ]]; then
    return 1
  fi

  local tun_name
  local tun_cidr
  local tun_peer
  tun_name="$(grep -E '^TUN_NAME=' "$env_file" | cut -d= -f2- || true)"
  tun_cidr="$(grep -E '^TUN_CIDR=' "$env_file" | cut -d= -f2- || true)"
  tun_peer="$(grep -E '^TUN_PEER=' "$env_file" | cut -d= -f2- || true)"

  if [[ -z "$tun_name" || -z "$tun_cidr" ]]; then
    return 1
  fi

  if [[ -z "$tun_peer" ]]; then
    tun_peer="$(derive_peer_from_cidr "$tun_cidr" || true)"
  fi

  if [[ -z "$tun_peer" ]]; then
    return 1
  fi

  ping -I "$tun_name" -c 1 -W 1 "$tun_peer" >/dev/null 2>&1
}

peer_fail_file() {
  local inst="$1"
  echo "${PEER_FAIL_DIR}/${inst}.count"
}

reset_peer_fail() {
  local inst="$1"
  rm -f "$(peer_fail_file "$inst")"
}

inc_peer_fail() {
  local inst="$1"
  local file
  local val=0
  file="$(peer_fail_file "$inst")"
  if [[ -f "$file" ]]; then
    val="$(cat "$file" 2>/dev/null || echo 0)"
  fi
  val=$((val + 1))
  echo "$val" > "$file"
  echo "$val"
}

manage_instance() {
  local dev="$1"
  local inst="$2"
  local stop_when_wan_down="${3:-1}"
  local svc="mpquic@${inst}.service"

  if wan_usable "$dev"; then
    if ! systemctl is-active --quiet "$svc"; then
      systemctl restart "$svc" || true
      reset_peer_fail "$inst"
      changed=1
    elif ! tun_healthy "$inst"; then
      systemctl restart "$svc" || true
      reset_peer_fail "$inst"
      changed=1
    elif peer_reachable "$inst"; then
      reset_peer_fail "$inst"
    else
      fails="$(inc_peer_fail "$inst")"
      if (( fails >= PEER_FAIL_THRESHOLD )); then
        systemctl restart "$svc" || true
        reset_peer_fail "$inst"
        changed=1
      fi
    fi
  else
    if [[ "$stop_when_wan_down" == "1" ]] && systemctl is-active --quiet "$svc"; then
      systemctl stop "$svc" || true
      changed=1
    fi
    reset_peer_fail "$inst"
  fi
}

changed=0

for idx in "${!WAN_DEVS[@]}"; do
  dev="${WAN_DEVS[$idx]}"
  inst="${INSTANCES[$idx]}"
  manage_instance "$dev" "$inst" 1
done

# --- L2 class tunnels: cr/br/df su WAN4-6 ---
for idx in "${!CLASS_WAN_DEVS[@]}"; do
  dev="${CLASS_WAN_DEVS[$idx]}"
  inst="${CLASS_INSTANCES[$idx]}"
  manage_instance "$dev" "$inst" 1
done

# --- mp1: multipath stripe (wan5=enp7s7 + wan6=enp7s8) ---
# mp1 non ha un singolo WAN device: si monitora con logica dedicata.
# Non lo fermiamo se un solo WAN è down (stripe gestisce internamente).
# Lo riavviamo solo se: almeno un WAN è usabile AND (servizio inattivo OR
# TUN degradata OR peer irraggiungibile per >= PEER_FAIL_THRESHOLD cicli).
MP1_WAN_DEVS=("enp7s7" "enp7s8")
MP1_SVC="mpquic@mp1.service"
MP1_INST="mp1"

mp1_any_wan_usable() {
  local dev
  for dev in "${MP1_WAN_DEVS[@]}"; do
    if wan_usable "$dev"; then
      return 0
    fi
  done
  return 1
}

if mp1_any_wan_usable; then
  if ! systemctl is-active --quiet "$MP1_SVC"; then
    systemctl restart "$MP1_SVC" || true
    reset_peer_fail "$MP1_INST"
    changed=1
  elif ! tun_healthy "$MP1_INST"; then
    systemctl restart "$MP1_SVC" || true
    reset_peer_fail "$MP1_INST"
    changed=1
  elif peer_reachable "$MP1_INST"; then
    reset_peer_fail "$MP1_INST"
  else
    fails="$(inc_peer_fail "$MP1_INST")"
    if (( fails >= PEER_FAIL_THRESHOLD )); then
      systemctl restart "$MP1_SVC" || true
      reset_peer_fail "$MP1_INST"
      changed=1
    fi
  fi
else
  # Entrambe le WAN down: reset counter, non toccare il servizio.
  # Quando tornerà almeno un WAN, il blocco sopra lo riavvierà se necessario.
  reset_peer_fail "$MP1_INST"
fi

if [ "$changed" -eq 1 ] && systemctl list-unit-files | grep -q '^mpquic-routing\.service'; then
  systemctl restart mpquic-routing.service || true
fi

exit 0
