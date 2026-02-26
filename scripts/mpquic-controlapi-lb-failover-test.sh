#!/usr/bin/env bash
set -euo pipefail

INSTANCE_ID="${1:-4}"
SERVER_HOST="${2:-vps-it-mpquic}"
OPENWRT_HOST="${3:-}"
WORK_DIR="/etc/mpquic/instances"
TPL_FILE="$WORK_DIR/${INSTANCE_ID}.yaml.tpl"
TPL_BACKUP="$WORK_DIR/${INSTANCE_ID}.yaml.tpl.bak.controlapi.$(date +%s)"
DATAPLANE_FILE="$WORK_DIR/dataplane.controlapi-test.yaml"
TOKEN_FILE="/tmp/mpquic-controlapi-token-${INSTANCE_ID}.txt"
API_ADDR="127.0.0.1:19090"
API_BASE="http://${API_ADDR}"

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  exec sudo -E "$0" "$@"
fi

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "comando richiesto non trovato: $1" >&2
    exit 1
  }
}

require_cmd systemctl
require_cmd curl
require_cmd tcpdump
require_cmd ssh

cleanup() {
  local rc=$?
  if [[ -f "$TPL_BACKUP" ]]; then
    cp -f "$TPL_BACKUP" "$TPL_FILE"
    systemctl restart "mpquic@${INSTANCE_ID}.service" >/dev/null 2>&1 || true
    rm -f "$TPL_BACKUP"
  fi
  if [[ -f "$DATAPLANE_FILE" ]]; then
    rm -f "$DATAPLANE_FILE"
  fi
  if [[ -f "$TOKEN_FILE" ]]; then
    rm -f "$TOKEN_FILE"
  fi
  ssh "$SERVER_HOST" "sudo systemctl start mpquic@4.service" >/dev/null 2>&1 || true
  exit "$rc"
}
trap cleanup EXIT

echo "[test] backup template ${TPL_FILE} -> ${TPL_BACKUP}"
cp -f "$TPL_FILE" "$TPL_BACKUP"

TOKEN="$(openssl rand -hex 32)"
echo "$TOKEN" > "$TOKEN_FILE"
chmod 600 "$TOKEN_FILE"

cat > "$TPL_FILE" <<'EOF'
role: client
multipath_enabled: true
multipath_policy: priority
dataplane_config_file: /etc/mpquic/instances/dataplane.controlapi-test.yaml
control_api_listen: 127.0.0.1:19090
control_api_auth_token: __TOKEN__
multipath_paths:
  - name: wan3
    bind_ip: if:enp7s5
    remote_addr: VPS_PUBLIC_IP
    remote_port: 45003
    priority: 10
    weight: 1
  - name: wan4
    bind_ip: if:enp7s6
    remote_addr: VPS_PUBLIC_IP
    remote_port: 45004
    priority: 10
    weight: 1
  - name: wan5
    bind_ip: if:enp7s7
    remote_addr: VPS_PUBLIC_IP
    remote_port: 45005
    priority: 10
    weight: 1
tun_name: mpq4
tun_cidr: 10.200.4.1/30
log_level: info
tls_ca_file: /etc/mpquic/tls/ca.crt
tls_server_name: mpquic-server
tls_insecure_skip_verify: false
EOF
sed -i "s|__TOKEN__|${TOKEN}|g" "$TPL_FILE"

cat > "$DATAPLANE_FILE" <<'EOF'
default_class: default
classes:
  default:
    scheduler_policy: balanced
    preferred_paths: [wan3, wan4, wan5]
  critical:
    scheduler_policy: failover
    preferred_paths: [wan4, wan3, wan5]
classifiers:
  - name: critical-icmp
    class: critical
    protocol: icmp
EOF

echo "[test] restart instance mpquic@${INSTANCE_ID}"
systemctl restart "mpquic@${INSTANCE_ID}.service"
sleep 2

echo "[test] wait control API ${API_ADDR}"
for _ in {1..20}; do
  if curl -sS -H "Authorization: Bearer ${TOKEN}" "$API_BASE/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

if ! curl -sS -H "Authorization: Bearer ${TOKEN}" "$API_BASE/healthz" >/dev/null 2>&1; then
  echo "[test] control API non raggiungibile" >&2
  systemctl --no-pager --full status "mpquic@${INSTANCE_ID}.service" || true
  journalctl -u "mpquic@${INSTANCE_ID}.service" -n 80 --no-pager || true
  exit 2
fi

echo "[test] control API ok"
curl -sS -H "Authorization: Bearer ${TOKEN}" "$API_BASE/dataplane" >/dev/null

generate_traffic() {
  if [[ -n "$OPENWRT_HOST" ]]; then
    echo "[test] traffic via OPENWRT host=$OPENWRT_HOST"
    ssh "$OPENWRT_HOST" "mwan3 use SL1 ping -c 20 -W 1 1.1.1.1 >/dev/null 2>&1 || true"
    ssh "$OPENWRT_HOST" "mwan3 use SL2 ping -c 20 -W 1 8.8.8.8 >/dev/null 2>&1 || true"
    ssh "$OPENWRT_HOST" "mwan3 use SL3 ping -c 20 -W 1 9.9.9.9 >/dev/null 2>&1 || true"
  else
    echo "[test] traffic locale di fallback su mpq4"
    ping -I mpq4 -c 30 -W 1 10.200.4.2 >/dev/null 2>&1 || true
  fi
}

capture_ports() {
  local outfile="$1"
  timeout 14 tcpdump -ni any 'udp and (dst port 45003 or dst port 45004 or dst port 45005)' -c 500 2>/dev/null |
    awk '/\.45003:/{p3++} /\.45004:/{p4++} /\.45005:/{p5++} END{printf("p45003=%d p45004=%d p45005=%d\n", p3+0, p4+0, p5+0)}' > "$outfile"
}

echo "[test] STEP1 load-balancing capture"
LB_FILE="/tmp/mpquic-lb-capture-${INSTANCE_ID}.txt"
capture_ports "$LB_FILE" &
CPID=$!
sleep 1
generate_traffic
wait "$CPID" || true
echo "[test] load-balancing result: $(cat "$LB_FILE")"

echo "[test] STEP2 failover (stop server mpquic@4)"
ssh "$SERVER_HOST" "sudo systemctl stop mpquic@4.service"

FAILOVER_DP="/tmp/dataplane-failover-${INSTANCE_ID}.yaml"
cat > "$FAILOVER_DP" <<'EOF'
default_class: default
classes:
  default:
    scheduler_policy: failover
    preferred_paths: [wan4, wan3, wan5]
  critical:
    scheduler_policy: failover
    preferred_paths: [wan4, wan3, wan5]
classifiers:
  - name: critical-icmp
    class: critical
    protocol: icmp
EOF

curl -sS -X POST -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/yaml" --data-binary @"$FAILOVER_DP" "$API_BASE/dataplane/apply" >/dev/null
rm -f "$FAILOVER_DP"

FAIL_FILE="/tmp/mpquic-failover-capture-${INSTANCE_ID}.txt"
capture_ports "$FAIL_FILE" &
CPID=$!
sleep 1
generate_traffic
wait "$CPID" || true
echo "[test] failover result: $(cat "$FAIL_FILE")"

echo "[test] restart server mpquic@4"
ssh "$SERVER_HOST" "sudo systemctl start mpquic@4.service"

echo "[test] summary"
echo "  load-balance: $(cat "$LB_FILE")"
echo "  failover:     $(cat "$FAIL_FILE")"
echo "  token file:   $TOKEN_FILE"
echo "  API:          $API_BASE"
echo "[test] done (rollback automatico al termine)"