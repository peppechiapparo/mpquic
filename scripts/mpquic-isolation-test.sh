#!/usr/bin/env bash
###############################################################################
# mpquic-isolation-test.sh — Step 2.4: Demonstrate tunnel isolation under loss
#
# This script proves that independent QUIC tunnels isolate packet loss:
# injecting loss on ONE tunnel does NOT degrade the others.
#
# Test plan:
#   1. BASELINE: measure RTT + throughput on all 3 tunnels (cr2/br2/df2 on WAN5)
#   2. LOSS on br2: inject 10% loss via netem on br2 TUN interface
#   3. MEASURE under loss: re-measure all 3 tunnels
#   4. Compare: cr2 and df2 should be unaffected by br2 loss
#   5. Cleanup: remove netem qdisc
#
# Requires:
#   - iperf3 on client (already installed)
#   - iperf3 server running on VPS (script starts it via SSH or uses public)
#   - tc/netem on client
#
# Usage:
#   mpquic-isolation-test.sh [baseline|loss|cleanup|full]
#
# We test on WAN5 set (cr2/br2/df2) because it has the best RTT (~14ms).
###############################################################################
set -euo pipefail

# ── Configuration ──────────────────────────────────────────────────────────
VPS_TUN_IP_MT5="10.200.15.254"       # VPS-side TUN IP for mt5 subnet
TUNNELS=(cr2 br2 df2)
TUN_IPS=(10.200.15.1 10.200.15.5 10.200.15.9)
LOSS_TARGET="br2"                     # inject loss on this tunnel
LOSS_PERCENT="10"                     # % packet loss to inject
IPERF_DURATION=10                     # seconds per iperf3 test
IPERF_PORT_BASE=5201                  # VPS iperf3 server port
RESULTS_DIR="/tmp/mpquic-isolation-results"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"

# ── Helpers ────────────────────────────────────────────────────────────────
log()  { echo "[test] $*"; }
hr()   { echo "────────────────────────────────────────────────────────────"; }

ensure_root() {
  if [[ $EUID -ne 0 ]]; then
    echo "This script must be run as root (for tc netem)" >&2
    exit 1
  fi
}

start_vps_iperf() {
  log "Starting iperf3 servers on VPS (ports 5201-5203)..."
  # Kill any existing iperf3 servers on VPS
  ssh -o ConnectTimeout=5 root@172.238.232.223 \
    'pkill -f "iperf3 -s" 2>/dev/null || true; sleep 0.5;
     iperf3 -s -p 5201 -D;
     iperf3 -s -p 5202 -D;
     iperf3 -s -p 5203 -D;
     echo "iperf3 servers started on 5201-5203"' 2>/dev/null || {
    log "WARNING: could not start iperf3 on VPS via SSH."
    log "Please start manually: iperf3 -s -p 5201 -D && iperf3 -s -p 5202 -D && iperf3 -s -p 5203 -D"
    return 1
  }
}

stop_vps_iperf() {
  ssh -o ConnectTimeout=5 root@172.238.232.223 \
    'pkill -f "iperf3 -s" 2>/dev/null || true' 2>/dev/null || true
}

# ── Measurement functions ──────────────────────────────────────────────────

measure_rtt() {
  local tun="$1" peer="$2"
  # 20 pings, extract avg RTT
  local out
  out="$(ping -I "$tun" -c 20 -i 0.2 -W 2 "$peer" 2>&1)"
  local avg
  avg="$(echo "$out" | awk -F'/' '/rtt min\/avg\/max/{print $5}')"
  local loss
  loss="$(echo "$out" | awk -F', ' '/packet loss/{print $3}' | tr -d ' ')"
  echo "${avg:-?}ms loss=${loss:-?}"
}

measure_throughput() {
  local tun="$1" tun_ip="$2" port="$3" label="$4" outfile="$5"
  log "  iperf3 $label: ${tun} → ${VPS_TUN_IP_MT5}:${port} (${IPERF_DURATION}s)..."
  iperf3 -c "$VPS_TUN_IP_MT5" -p "$port" -B "$tun_ip" \
    -t "$IPERF_DURATION" -J 2>/dev/null > "$outfile" || true

  # Extract summary from JSON
  local bps retransmits
  bps="$(python3 -c "
import json, sys
d = json.load(open('$outfile'))
e = d.get('end',{}).get('sum_sent',{})
print(f\"{e.get('bits_per_second',0)/1e6:.2f} Mbps  retransmits={e.get('retransmits','?')}\")" 2>/dev/null || echo "parse error")"
  echo "$bps"
}

# ── Phases ─────────────────────────────────────────────────────────────────

do_baseline() {
  log "=== PHASE 1: BASELINE (no loss) ==="
  hr
  mkdir -p "$RESULTS_DIR"

  log "RTT measurements (20 pings each):"
  for i in "${!TUNNELS[@]}"; do
    local tun="${TUNNELS[$i]}"
    local result
    result="$(measure_rtt "$tun" "$VPS_TUN_IP_MT5")"
    printf "  %-4s  %s\n" "$tun" "$result"
    echo "baseline_rtt ${tun} ${result}" >> "${RESULTS_DIR}/baseline_${TIMESTAMP}.txt"
  done

  log ""
  log "Throughput measurements (${IPERF_DURATION}s each):"
  local ports=(5201 5202 5203)
  for i in "${!TUNNELS[@]}"; do
    local tun="${TUNNELS[$i]}"
    local tun_ip="${TUN_IPS[$i]}"
    local port="${ports[$i]}"
    local outfile="${RESULTS_DIR}/baseline_iperf_${tun}_${TIMESTAMP}.json"
    local result
    result="$(measure_throughput "$tun" "$tun_ip" "$port" "baseline" "$outfile")"
    printf "  %-4s  %s\n" "$tun" "$result"
    echo "baseline_tput ${tun} ${result}" >> "${RESULTS_DIR}/baseline_${TIMESTAMP}.txt"
  done

  hr
  log "Baseline saved to ${RESULTS_DIR}/baseline_${TIMESTAMP}.txt"
}

do_inject_loss() {
  log "=== PHASE 2: INJECT ${LOSS_PERCENT}% LOSS on ${LOSS_TARGET} ==="
  hr

  # Remove any existing qdisc first
  tc qdisc del dev "$LOSS_TARGET" root 2>/dev/null || true

  # Add netem loss
  tc qdisc add dev "$LOSS_TARGET" root netem loss "${LOSS_PERCENT}%"
  log "Applied: tc qdisc add dev ${LOSS_TARGET} root netem loss ${LOSS_PERCENT}%"

  # Verify
  tc qdisc show dev "$LOSS_TARGET"
  hr
}

do_measure_under_loss() {
  log "=== PHASE 3: MEASURE UNDER LOSS (${LOSS_PERCENT}% on ${LOSS_TARGET}) ==="
  hr

  # Confirm netem is active
  local active
  active="$(tc qdisc show dev "$LOSS_TARGET" | grep netem || true)"
  if [[ -z "$active" ]]; then
    log "ERROR: netem not active on ${LOSS_TARGET}. Run 'loss' phase first."
    exit 1
  fi
  log "Confirmed: $active"
  log ""

  log "RTT measurements (20 pings each):"
  for i in "${!TUNNELS[@]}"; do
    local tun="${TUNNELS[$i]}"
    local marker=""
    [[ "$tun" == "$LOSS_TARGET" ]] && marker=" ← LOSS INJECTED"
    local result
    result="$(measure_rtt "$tun" "$VPS_TUN_IP_MT5")"
    printf "  %-4s  %s%s\n" "$tun" "$result" "$marker"
    echo "loss_rtt ${tun} ${result}" >> "${RESULTS_DIR}/loss_${TIMESTAMP}.txt"
  done

  log ""
  log "Throughput measurements (${IPERF_DURATION}s each):"
  local ports=(5201 5202 5203)
  for i in "${!TUNNELS[@]}"; do
    local tun="${TUNNELS[$i]}"
    local tun_ip="${TUN_IPS[$i]}"
    local port="${ports[$i]}"
    local marker=""
    [[ "$tun" == "$LOSS_TARGET" ]] && marker=" ← LOSS INJECTED"
    local outfile="${RESULTS_DIR}/loss_iperf_${tun}_${TIMESTAMP}.json"
    local result
    result="$(measure_throughput "$tun" "$tun_ip" "$port" "under-loss" "$outfile")"
    printf "  %-4s  %s%s\n" "$tun" "$result" "$marker"
    echo "loss_tput ${tun} ${result}" >> "${RESULTS_DIR}/loss_${TIMESTAMP}.txt"
  done

  hr
  log "Results under loss saved to ${RESULTS_DIR}/loss_${TIMESTAMP}.txt"
}

do_cleanup() {
  log "=== CLEANUP: removing netem from ${LOSS_TARGET} ==="
  tc qdisc del dev "$LOSS_TARGET" root 2>/dev/null || true
  log "Done. Verifying:"
  tc qdisc show dev "$LOSS_TARGET"
}

do_full() {
  start_vps_iperf || true
  sleep 2

  do_baseline
  echo ""

  do_inject_loss
  sleep 2

  do_measure_under_loss
  echo ""

  do_cleanup
  echo ""

  log "=== SUMMARY ==="
  hr
  log "Baseline results:"
  cat "${RESULTS_DIR}/baseline_${TIMESTAMP}.txt" 2>/dev/null | sed 's/^/  /'
  log ""
  log "Under ${LOSS_PERCENT}% loss on ${LOSS_TARGET}:"
  cat "${RESULTS_DIR}/loss_${TIMESTAMP}.txt" 2>/dev/null | sed 's/^/  /'
  hr
  log ""
  log "Key finding: cr2 and df2 RTT/throughput should be UNCHANGED"
  log "while br2 shows degraded throughput and/or higher retransmits."

  stop_vps_iperf || true
}

# ── Main ───────────────────────────────────────────────────────────────────
ensure_root

case "${1:-full}" in
  baseline)   start_vps_iperf || true; sleep 1; do_baseline ;;
  loss)       do_inject_loss ;;
  measure)    do_measure_under_loss ;;
  cleanup)    do_cleanup ;;
  full)       do_full ;;
  *)
    echo "usage: $0 {baseline|loss|measure|cleanup|full}" >&2
    echo "  full = baseline + inject loss + measure + cleanup"
    exit 1
    ;;
esac
