#!/usr/bin/env bash
# deploy-prometheus.sh — Installa/aggiorna config Prometheus da repo
#
# Eseguire su CT 201 (10.10.11.201) dopo git pull:
#   cd /opt/mpquic && git pull && sudo ./scripts/deploy-prometheus.sh
#
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROM_SRC="$ROOT_DIR/deploy/monitoring/prometheus/prometheus.yml"
PROM_DST="/etc/prometheus/prometheus.yml"

if [[ ! -f "$PROM_SRC" ]]; then
  echo "Errore: $PROM_SRC non trovato" >&2
  exit 1
fi

echo "━━━ Deploy Prometheus Config ━━━━━━━━━━━━━━━━━━━━━"

# Valida config con promtool se disponibile
if command -v /opt/prometheus/promtool &>/dev/null; then
  echo "Validazione config..."
  /opt/prometheus/promtool check config "$PROM_SRC"
fi

install -m 0644 "$PROM_SRC" "$PROM_DST"
echo "✅ Config copiata in $PROM_DST"

# Hot-reload Prometheus (no restart = no loss of in-memory data)
if curl -sf -X POST http://localhost:9090/-/reload >/dev/null 2>&1; then
  echo "✅ Prometheus ricaricato (hot-reload)"
else
  echo "⚠ Hot-reload fallito, prova: systemctl restart prometheus"
fi
