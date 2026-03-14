#!/usr/bin/env bash
# setup-prometheus.sh — Installa e configura Prometheus dentro CT 201
#
# Uso:
#   pct exec 201 -- bash < setup-prometheus.sh
#   oppure: copiare dentro il container e lanciare come root

set -euo pipefail

PROM_VERSION="2.53.4"
PROM_USER="prometheus"
PROM_DIR="/opt/prometheus"
PROM_DATA="/var/lib/prometheus"
PROM_CONF="/etc/prometheus"
RETENTION="30d"
SCRAPE_INTERVAL="5s"

echo "━━━ Setup Prometheus ${PROM_VERSION} ━━━━━━━━━━━━━━━━━━━━━"

# ── 1. Pacchetti base ────────────────────────────────────
apt-get update -qq
apt-get install -y -qq curl tar ca-certificates > /dev/null

# ── 2. Crea utente ───────────────────────────────────────
if ! id "$PROM_USER" &>/dev/null; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "$PROM_USER"
fi

# ── 3. Download e installa ───────────────────────────────
cd /tmp
ARCHIVE="prometheus-${PROM_VERSION}.linux-amd64.tar.gz"
if [[ ! -f "$ARCHIVE" ]]; then
    echo "⬇ Download Prometheus ${PROM_VERSION} ..."
    curl -sSLO "https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/${ARCHIVE}"
fi

tar xzf "$ARCHIVE"
mkdir -p "$PROM_DIR" "$PROM_DATA" "$PROM_CONF"

cp "prometheus-${PROM_VERSION}.linux-amd64/prometheus" "$PROM_DIR/"
cp "prometheus-${PROM_VERSION}.linux-amd64/promtool"   "$PROM_DIR/"
chmod +x "$PROM_DIR/prometheus" "$PROM_DIR/promtool"

chown -R "$PROM_USER:$PROM_USER" "$PROM_DATA"

# ── 4. Configurazione Prometheus ─────────────────────────
# Se non esiste già prometheus.yml, creare quello di default MPQUIC
if [[ ! -f "$PROM_CONF/prometheus.yml" ]]; then
cat > "$PROM_CONF/prometheus.yml" << 'PROMYML'
global:
  scrape_interval: 5s
  evaluation_interval: 5s
  scrape_timeout: 4s

scrape_configs:
  - job_name: "prometheus"
    static_configs:
      - targets: ["localhost:9090"]

  - job_name: "mpquic-server"
    static_configs:
      - targets: ["10.200.17.254:9090"]
        labels:
          instance_name: "mp1"
          role: "server"
          site: "vps"

  - job_name: "mpquic-client"
    static_configs:
      - targets: ["10.200.17.1:9090"]
        labels:
          instance_name: "mp1"
          role: "client"
          site: "client"
          transport: "stripe"
      - targets: ["10.200.14.1:9090"]
        labels:
          instance_name: "cr1"
          role: "client"
          site: "client"
          transport: "quic"
      - targets: ["10.200.16.1:9090"]
        labels:
          instance_name: "cr2"
          role: "client"
          site: "client"
          transport: "quic"
      - targets: ["10.200.10.1:9090"]
        labels:
          instance_name: "cr3"
          role: "client"
          site: "client"
          transport: "quic"
      - targets: ["10.200.15.1:9090"]
        labels:
          instance_name: "cr5"
          role: "client"
          site: "client"
          transport: "quic"
PROMYML
    echo "✅ prometheus.yml creato con target MPQUIC"
else
    echo "ℹ prometheus.yml già presente, skip"
fi

# ── 5. Systemd unit ──────────────────────────────────────
cat > /etc/systemd/system/prometheus.service << EOF
[Unit]
Description=Prometheus Monitoring
Documentation=https://prometheus.io/docs/
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=${PROM_USER}
Group=${PROM_USER}
ExecStart=${PROM_DIR}/prometheus \\
    --config.file=${PROM_CONF}/prometheus.yml \\
    --storage.tsdb.path=${PROM_DATA} \\
    --storage.tsdb.retention.time=${RETENTION} \\
    --web.listen-address=0.0.0.0:9090 \\
    --web.enable-lifecycle \\
    --log.level=info
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

# ── 6. Avvio ─────────────────────────────────────────────
systemctl daemon-reload
systemctl enable prometheus
systemctl start prometheus

echo ""
echo "━━━ Prometheus installato e avviato ━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "  Web UI:   http://10.10.11.201:9090"
echo "  Config:   ${PROM_CONF}/prometheus.yml"
echo "  Data:     ${PROM_DATA}"
echo "  Scrape:  every ${SCRAPE_INTERVAL}"
echo ""
echo "  Verifica: systemctl status prometheus"
echo "  Targets:  http://10.10.11.201:9090/targets"
echo ""
echo "⚠ Assicurarsi che le route verso 10.200.x.0/24 siano configurate!"
echo "  ip route add 10.200.0.0/16 via 10.10.11.100"
