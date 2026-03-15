#!/usr/bin/env bash
# deploy-grafana.sh — Installa/aggiorna dashboard Grafana da repo
#
# Eseguire su CT 202 (10.10.11.202) dopo git pull:
#   cd /opt/mpquic && git pull && sudo ./scripts/deploy-grafana.sh
#
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DASHBOARD_SRC="$ROOT_DIR/deploy/monitoring/grafana/mpquic-dashboard.json"
DASHBOARD_DST="/var/lib/grafana/dashboards/mpquic-dashboard.json"

if [[ ! -f "$DASHBOARD_SRC" ]]; then
  echo "Errore: $DASHBOARD_SRC non trovato" >&2
  exit 1
fi

echo "━━━ Deploy Grafana Dashboard ━━━━━━━━━━━━━━━━━━━━━"
install -m 0644 "$DASHBOARD_SRC" "$DASHBOARD_DST"
echo "✅ Dashboard copiata in $DASHBOARD_DST"

if systemctl is-active --quiet grafana-server; then
  systemctl restart grafana-server
  echo "✅ grafana-server riavviato"
else
  echo "⚠ grafana-server non attivo, avvialo con: systemctl start grafana-server"
fi
