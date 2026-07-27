#!/usr/bin/env bash
# 01-recon.sh — Reconnaissance completa sul target VAPT
# Richiede: VAPT_TARGET, VAPT_HOST, VAPT_RESULTS
set -e

: "${VAPT_TARGET:?Imposta VAPT_TARGET (es: export VAPT_TARGET=10.10.11.254)}"
: "${VAPT_RESULTS:?Imposta VAPT_RESULTS (es: export VAPT_RESULTS=/tmp/vapt-results/20250101)}"

COMPOSE="docker compose -f $(dirname "$0")/../docker/docker-compose.vapt.yml"
R="$VAPT_RESULTS"
T="$VAPT_TARGET"

echo "═══════════════════════════════════════"
echo "  VAPT RECON — $VAPT_HOST ($T)"
echo "═══════════════════════════════════════"

# Timeout nmap (aumentare per target VSAT con alta latenza)
NMAP_TIMING="${NMAP_TIMING:-T4}"  # T2 per VSAT

echo "[1/6] Port scan TCP rapido (top 1000)..."
$COMPOSE run --rm nmap nmap -sS -sV -O \
  --version-intensity 7 \
  --open -"$NMAP_TIMING" \
  -oN "$R/nmap-tcp-quick.txt" \
  -oX "$R/nmap-tcp-quick.xml" \
  "$T" 2>&1 | tail -3
echo "    → $R/nmap-tcp-quick.txt"

echo "[2/6] Port scan TCP completo (65535 porte)..."
$COMPOSE run --rm nmap nmap -sS -p- -"$NMAP_TIMING" \
  -oN "$R/nmap-tcp-full.txt" \
  "$T" 2>&1 | tail -3
echo "    → $R/nmap-tcp-full.txt"

echo "[3/6] UDP scan porte critiche..."
$COMPOSE run --rm nmap nmap -sU \
  -p 53,67,68,69,123,161,500,4500,9993,9994,9995,9996 \
  -oN "$R/nmap-udp.txt" \
  "$T" 2>&1 | tail -3
echo "    → $R/nmap-udp.txt"

echo "[4/6] Service detection dettagliata + NSE scripts..."
$COMPOSE run --rm nmap nmap -sV --version-all -sC \
  -oN "$R/nmap-service-detail.txt" \
  "$T" 2>&1 | tail -3
echo "    → $R/nmap-service-detail.txt"

echo "[5/6] SSH Audit..."
$COMPOSE run --rm vapt-tools \
  bash -c "ssh-audit $T > /results/ssh-audit.txt 2>&1; echo 'Done'"
echo "    → $R/ssh-audit.txt"

echo "[6/6] SSL/TLS Audit..."
$COMPOSE run --rm vapt-tools \
  bash -c "
    sslscan --no-colour $T:443 > /results/ssl-audit.txt 2>&1
    sslscan --no-colour $T:8006 >> /results/ssl-audit.txt 2>&1
    echo 'Done'
  "
echo "    → $R/ssl-audit.txt"

echo ""
echo "✅ Recon completata. Risultati in: $R"
echo "   File generati:"
ls -lh "$R"/*.txt "$R"/*.xml 2>/dev/null | awk '{print "   " $NF " (" $5 ")"}'
