#!/usr/bin/env bash
# setup-grafana.sh — Installa e configura Grafana OSS dentro CT 202
#
# Uso:
#   pct exec 202 -- bash < setup-grafana.sh
#   oppure: copiare dentro il container e lanciare come root

set -euo pipefail

GRAFANA_VERSION="11.6.0"

echo "━━━ Setup Grafana OSS ${GRAFANA_VERSION} ━━━━━━━━━━━━━━━━━"

# ── 1. Pacchetti base ────────────────────────────────────
apt-get update -qq
apt-get install -y -qq curl ca-certificates gnupg > /dev/null

# ── 2. Repo Grafana APT ─────────────────────────────────
if [[ ! -f /etc/apt/sources.list.d/grafana.list ]]; then
    echo "➕ Aggiunta repository Grafana ..."
    curl -fsSL https://apt.grafana.com/gpg.key | gpg --dearmor -o /usr/share/keyrings/grafana-archive-keyring.gpg
    echo "deb [signed-by=/usr/share/keyrings/grafana-archive-keyring.gpg] https://apt.grafana.com stable main" \
        > /etc/apt/sources.list.d/grafana.list
    apt-get update -qq
fi

# ── 3. Installa Grafana ─────────────────────────────────
apt-get install -y -qq grafana > /dev/null
echo "✅ Grafana installato"

# ── 4. Datasource Prometheus (provisioning automatico) ───
mkdir -p /etc/grafana/provisioning/datasources
cat > /etc/grafana/provisioning/datasources/prometheus.yml << 'DS'
apiVersion: 1

datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://10.10.11.201:9090
    isDefault: true
    editable: true
    jsonData:
      timeInterval: "5s"
      httpMethod: POST
DS
echo "✅ Datasource Prometheus configurato (auto-provisioned)"

# ── 5. Dashboard provisioning ────────────────────────────
mkdir -p /etc/grafana/provisioning/dashboards
mkdir -p /var/lib/grafana/dashboards

cat > /etc/grafana/provisioning/dashboards/mpquic.yml << 'DPROV'
apiVersion: 1

providers:
  - name: "MPQUIC"
    orgId: 1
    type: file
    disableDeletion: false
    updateIntervalSeconds: 30
    allowUiUpdates: true
    options:
      path: /var/lib/grafana/dashboards
      foldersFromFilesStructure: false
DPROV

echo "✅ Dashboard provider configurato"

# ── 6. Installa dashboard MPQUIC ─────────────────────────
# Il file JSON della dashboard va copiato separatamente:
#   cp mpquic-dashboard.json /var/lib/grafana/dashboards/
# (vedi deploy/monitoring/grafana/mpquic-dashboard.json)
echo "ℹ Copiare mpquic-dashboard.json in /var/lib/grafana/dashboards/"

# ── 7. Configurazione Grafana ────────────────────────────
# Impostazioni principali in /etc/grafana/grafana.ini
# Modifica le impostazioni più rilevanti:
sed -i 's/^;http_port = 3000/http_port = 3000/' /etc/grafana/grafana.ini
sed -i 's/^;http_addr =$/http_addr = 0.0.0.0/' /etc/grafana/grafana.ini

# ── 8. Avvio ─────────────────────────────────────────────
systemctl daemon-reload
systemctl enable grafana-server
systemctl start grafana-server

echo ""
echo "━━━ Grafana installato e avviato ━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "  Web UI:     http://10.10.11.202:3000"
echo "  Login:      admin / admin (cambiar al primo accesso)"
echo "  Datasource: Prometheus (http://10.10.11.201:9090) — auto-provisioned"
echo ""
echo "  Prossimi step:"
echo "    1. Copiare mpquic-dashboard.json in /var/lib/grafana/dashboards/"
echo "    2. Accedere alla Web UI e verificare il datasource"
echo "    3. Aprire la dashboard 'MPQUIC Overview'"
echo ""
echo "  Verifica: systemctl status grafana-server"
