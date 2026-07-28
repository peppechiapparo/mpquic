#!/usr/bin/env bash
set -euo pipefail

# TS-012 fix (2026-07-22): questo watchdog restartava mpq6 in loop.
# Due bug rimossi:
#  1) il trigger di restart su `grep 'write tun: input/output error'` era un
#     latch: la finestra di lookback (90s) era piu' larga del periodo del timer
#     (60s), quindi ogni restart rigenerava l'errore e il giro dopo lo rivedeva
#     -> restart infinito, innescato da un singolo micro-outage Starlink.
#  2) `ensure_tun.sh` (che fa `ip link set down` + `ip tuntap del`) veniva
#     chiamato su OGNI tunnel sano ad ogni giro -> bounce dell'interfaccia viva
#     ogni 60s, con flush della rotta e write error.
# Ora il watchdog agisce solo su un guasto reale: servizio non attivo, oppure
# TUN mancante/down/senza IP. I tunnel sani non vengono toccati.

changed=0

restart_instance() {
  local inst="$1"
  systemctl restart "mpquic@${inst}.service" || true
  changed=1
}

ensure_tun_instance() {
  local tun_name="$1"
  local tun_cidr="$2"
  local tun_mtu="$3"
  /usr/local/lib/mpquic/ensure_tun.sh "$tun_name" "$tun_cidr" "$tun_mtu" >/dev/null 2>&1 || return 1
  return 0
}

tun_healthy() {
  local inst="$1"
  local env_file="/etc/mpquic/instances/${inst}.env"
  [[ -f "$env_file" ]] || return 1

  local tun_name tun_cidr
  tun_name="$(grep -E '^TUN_NAME=' "$env_file" | cut -d= -f2-)"
  tun_cidr="$(grep -E '^TUN_CIDR=' "$env_file" | cut -d= -f2-)"
  [[ -n "$tun_name" && -n "$tun_cidr" ]] || return 1

  ip link show dev "$tun_name" >/dev/null 2>&1 || return 1
  ip link show dev "$tun_name" | head -n1 | grep -q "UP" || return 1
  ip -4 addr show dev "$tun_name" | grep -q "inet ${tun_cidr}" || return 1
  return 0
}

for i in 1 2 3 4 5 6; do
  svc="mpquic@${i}.service"

  # Guasto reale #1: il servizio non e' attivo -> restart.
  if ! systemctl is-active --quiet "$svc"; then
    restart_instance "$i"
    continue
  fi

  # Guasto reale #2: il TUN manca o e' down/senza IP -> ricrea e restart.
  # Un TUN sano NON viene toccato (niente bounce ogni 60s).
  if ! tun_healthy "$i"; then
    env_file="/etc/mpquic/instances/${i}.env"
    tun_name="$(grep -E '^TUN_NAME=' "$env_file" | cut -d= -f2- || true)"
    tun_cidr="$(grep -E '^TUN_CIDR=' "$env_file" | cut -d= -f2- || true)"
    tun_mtu="$(grep -E '^TUN_MTU=' "$env_file" | cut -d= -f2- || true)"
    tun_mtu="${tun_mtu:-1300}"
    if [[ -n "$tun_name" && -n "$tun_cidr" ]]; then
      ensure_tun_instance "$tun_name" "$tun_cidr" "$tun_mtu" || true
    fi
    restart_instance "$i"
  fi
done

if [ "$changed" -eq 1 ] && systemctl list-unit-files | grep -q '^mpquic-vps-routes\.service'; then
  systemctl restart mpquic-vps-routes.service || true
fi

exit 0
