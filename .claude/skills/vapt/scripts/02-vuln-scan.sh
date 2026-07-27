#!/usr/bin/env bash
# 02-vuln-scan.sh — Automated vulnerability scanning (Nuclei + Nikto)
# Richiede: VAPT_TARGET, VAPT_HOST, VAPT_RESULTS
set -e

: "${VAPT_TARGET:?Imposta VAPT_TARGET}"
: "${VAPT_RESULTS:?Imposta VAPT_RESULTS}"

COMPOSE="docker compose -f $(dirname "$0")/../docker/docker-compose.vapt.yml"
R="$VAPT_RESULTS"
T="$VAPT_TARGET"

echo "═══════════════════════════════════════"
echo "  VAPT VULN SCAN — $VAPT_HOST ($T)"
echo "═══════════════════════════════════════"

# Rileva porte aperte dal recon precedente
OPEN_PORTS=""
if [ -f "$R/nmap-tcp-quick.txt" ]; then
  OPEN_PORTS=$(grep "^[0-9]" "$R/nmap-tcp-quick.txt" | grep "open" | awk -F/ '{print $1}' | tr '\n' ',')
  echo "Porte aperte rilevate: $OPEN_PORTS"
fi

echo "[1/4] Nuclei — scan generico CVE/misconfig..."
$COMPOSE run --rm nuclei nuclei \
  -target "http://$T" \
  -target "https://$T" \
  -tags "cve,misconfig,exposure,default-login,network" \
  -severity "critical,high,medium" \
  -json-export "/results/nuclei-general.json" \
  -c 5 \
  2>&1 | tee "$R/nuclei-general.log" | grep -E "(CRITICAL|HIGH|MEDIUM|WARN|ERR)" | head -30
echo "    → $R/nuclei-general.json"

echo "[2/4] Nuclei — scan Proxmox (porta 8006)..."
$COMPOSE run --rm nuclei nuclei \
  -target "https://$T:8006" \
  -tags "proxmox,panel,default-login,exposure" \
  -json-export "/results/nuclei-proxmox.json" \
  -c 5 \
  2>&1 | grep -E "(CRITICAL|HIGH|MEDIUM|found)" | head -20 || true
echo "    → $R/nuclei-proxmox.json"

echo "[3/4] Nikto — web scan HTTP/HTTPS..."
$COMPOSE run --rm vapt-tools bash -c "
  nikto -host http://$T -Format txt -Pause 1 \
    -output /results/nikto-http.txt 2>&1
  nikto -host https://$T -ssl -Format txt -Pause 1 \
    -output /results/nikto-https.txt 2>&1
  nikto -host https://$T:8006 -ssl -Format txt -Pause 1 \
    -output /results/nikto-proxmox.txt 2>&1 || true
  echo 'Nikto done'
"
echo "    → $R/nikto-*.txt"

echo "[4/4] NVD CVE lookup per componenti noti..."
for component in "OpenWrt 21.02" "Proxmox VE 8" "ZeroTier 1.14" "dropbear SSH"; do
  encoded=$(python3 -c "import urllib.parse; print(urllib.parse.quote('$component'))")
  outfile="$R/cve-$(echo "$component" | tr ' ' '-' | tr '[:upper:]' '[:lower:]').json"
  curl -s --retry 3 \
    "https://services.nvd.nist.gov/rest/json/cves/2.0?keywordSearch=$encoded&resultsPerPage=10" \
    > "$outfile" 2>/dev/null || echo "⚠️  NVD lookup fallito per $component"
  count=$(python3 -c "import json,sys; d=json.load(open('$outfile')); print(d.get('totalResults',0))" 2>/dev/null || echo "?")
  echo "    CVE trovate per '$component': $count → $outfile"
done

echo ""
echo "✅ Vuln scan completato. Risultati in: $R"
