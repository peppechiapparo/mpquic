#!/usr/bin/env bash
set -euo pipefail

# Return-route per-/30 dei client, DERIVATE dai file d'istanza.
# Ogni /etc/mpquic/instances/<i>.env dichiara la sua RETURN_SUBNETS (le subnet
# LAN del client il cui ritorno de-mascherato va instradato dentro il tunnel).
# Prima questa era una lista scritta a mano qui dentro, che si disallineava dai
# tunnel reali (es. mp1 e mt1 mancanti). Ora la fonte di verità è l'env
# dell'istanza: aggiungi un tunnel con la sua RETURN_SUBNETS e la rotta compare
# da sola, senza toccare questo script.
#
# Uso:
#   mpquic-vps-routes.sh            -> tutte le istanze (boot / keeper)
#   mpquic-vps-routes.sh <istanza>  -> solo quella (ExecStartPost=... %i)
#
# Nota: i tunnel di test senza LAN dietro (es. mt1, che maschera la propria
# /24 10.200.10.0/24) non dichiarano RETURN_SUBNETS: il loro ritorno è la
# rotta connessa del TUN, gia' creata da ensure_tun.sh.

INSTANCES_DIR=/etc/mpquic/instances

safe() { "$@" 2>/dev/null || true; }

install_for() {
  local env_file="$1"
  [ -f "$env_file" ] || return 0

  local tun subs
  tun="$(grep -E '^TUN_NAME=' "$env_file" | cut -d= -f2- || true)"
  subs="$(grep -E '^RETURN_SUBNETS=' "$env_file" | cut -d= -f2- | tr -d '"' || true)"

  [ -n "$tun" ] || return 0
  [ -n "$subs" ] || return 0

  local sub
  for sub in $subs; do
    safe ip route replace "$sub" dev "$tun"
  done
}

if [ "$#" -ge 1 ] && [ -n "${1:-}" ]; then
  install_for "$INSTANCES_DIR/$1.env"
else
  for f in "$INSTANCES_DIR"/*.env; do
    install_for "$f"
  done
fi

exit 0
