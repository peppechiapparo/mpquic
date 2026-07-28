#!/usr/bin/env bash
# 03-container-scan.sh — Trivy scan su filesystem e container del target
# Richiede: VAPT_TARGET, VAPT_HOST, VAPT_RESULTS
# Richiede: accesso SSH al target per raccogliere configs
set -e

: "${VAPT_TARGET:?Imposta VAPT_TARGET}"
: "${VAPT_RESULTS:?Imposta VAPT_RESULTS}"

COMPOSE="docker compose -f $(dirname "$0")/../docker/docker-compose.vapt.yml"
R="$VAPT_RESULTS"
T="$VAPT_TARGET"
SSH_KEY="${VAPT_SSH_KEY:-$HOME/.ssh/id_rsa}"

echo "═══════════════════════════════════════"
echo "  VAPT CONTAINER SCAN — $VAPT_HOST ($T)"
echo "═══════════════════════════════════════"

# Crea directory staging per config raccolte
mkdir -p "$R/configs-staging"

echo "[1/4] Raccolta configurazioni via SSH..."
ssh -o StrictHostKeyChecking=no \
    -o ConnectTimeout=15 \
    -i "$SSH_KEY" \
    "root@$T" "
  tar czf /tmp/vapt-configs.tar.gz \
    /etc/config/ \
    /etc/ssh/sshd_config 2>/dev/null \
    /etc/shadow 2>/dev/null \
    /etc/passwd \
    /etc/crontabs/ 2>/dev/null \
    /etc/zerotier/ 2>/dev/null \
    /root/.ssh/ 2>/dev/null \
    2>/dev/null || true
  echo 'Archive creato'
" && \
scp -o StrictHostKeyChecking=no \
    -i "$SSH_KEY" \
    "root@$T:/tmp/vapt-configs.tar.gz" \
    "$R/configs-staging/" && \
  tar xzf "$R/configs-staging/vapt-configs.tar.gz" \
    -C "$R/configs-staging/" 2>/dev/null && \
  echo "    → Configurazioni in $R/configs-staging/" || \
  echo "⚠️  SSH non raggiungibile — skipping config collection"

echo "[2/4] Trivy config scan su configurazioni raccolte..."
if [ -d "$R/configs-staging/etc" ]; then
  $COMPOSE run --rm trivy trivy config \
    --format json \
    --output "/results/trivy-config.json" \
    /results/configs-staging/etc/ \
    2>&1 | tail -5
  echo "    → $R/trivy-config.json"
else
  echo "⚠️  Nessuna config raccolta — skip trivy config scan"
fi

echo "[3/4] Raccolta lista Docker images dal target..."
DOCKER_IMAGES=$(ssh -o StrictHostKeyChecking=no \
    -o ConnectTimeout=10 \
    -i "$SSH_KEY" \
    "root@$T" \
    "docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null" 2>/dev/null || echo "")

if [ -n "$DOCKER_IMAGES" ]; then
  echo "$DOCKER_IMAGES" > "$R/docker-images.txt"
  echo "[4/4] Trivy scan sulle immagini Docker..."
  echo "$DOCKER_IMAGES" | while read -r img; do
    safe_name=$(echo "$img" | tr ':/' '--')
    $COMPOSE run --rm trivy trivy image \
      --format json \
      --output "/results/trivy-image-$safe_name.json" \
      --severity HIGH,CRITICAL \
      "$img" 2>&1 | grep -E "(Total|HIGH|CRITICAL|Scanning)" | head -5
    echo "    → $R/trivy-image-$safe_name.json"
  done
else
  echo "    Nessuna immagine Docker trovata sul target — skip"
fi

echo ""
echo "✅ Container scan completato. Risultati in: $R"
