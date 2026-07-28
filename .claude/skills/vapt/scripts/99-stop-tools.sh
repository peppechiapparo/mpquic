#!/usr/bin/env bash
# 99-stop-tools.sh — Ferma e rimuove i container VAPT
set -e

COMPOSE="docker compose -f $(dirname "$0")/../docker/docker-compose.vapt.yml"

echo "🛑 Fermando container VAPT..."
$COMPOSE down

echo "✅ Container VAPT fermati."
echo ""
echo "I volumi (template nuclei, cache trivy) vengono mantenuti per riuso."
echo "Per rimuovere anche i volumi: docker compose -f <path>/docker-compose.vapt.yml down -v"
