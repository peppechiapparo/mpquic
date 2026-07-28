#!/usr/bin/env bash
# start-tools.sh — Avvia e prebuild i container VAPT
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/../docker/docker-compose.vapt.yml"

export VAPT_TARGET="${VAPT_TARGET:-10.10.11.254}"
export VAPT_HOST="${VAPT_HOST:-TBOX-EVO}"
export VAPT_RESULTS="${VAPT_RESULTS:-/tmp/vapt-results/$(date +%Y%m%d_%H%M%S)}"

mkdir -p "$VAPT_RESULTS"
echo "📁 Results: $VAPT_RESULTS"
echo "🎯 Target:  $VAPT_TARGET ($VAPT_HOST)"

echo "🔨 Building vapt-tools image..."
docker compose -f "$COMPOSE_FILE" build vapt-tools

echo "🚀 Starting containers..."
docker compose -f "$COMPOSE_FILE" up -d

echo "⏳ Aggiornamento template Nuclei..."
docker compose -f "$COMPOSE_FILE" run --rm nuclei nuclei -update-templates 2>&1 | tail -3

echo "✅ Tool VAPT pronti."
echo ""
echo "Variabili da esportare nella sessione corrente:"
echo "  export VAPT_TARGET=\"$VAPT_TARGET\""
echo "  export VAPT_HOST=\"$VAPT_HOST\""
echo "  export VAPT_RESULTS=\"$VAPT_RESULTS\""
