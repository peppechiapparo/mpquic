#!/bin/bash
# setup-fq-qdisc.sh — Install sch_fq qdisc on WAN interfaces for SO_TXTIME pacing.
#
# SO_TXTIME requires sch_fq (Fair Queue) qdisc on egress to hold packets until
# their Earliest Departure Time (EDT). Without sch_fq, SCM_TXTIME timestamps
# are silently ignored and packets depart immediately (defeating kernel pacing).
#
# Usage:
#   sudo ./setup-fq-qdisc.sh                 # auto-detect WAN interfaces
#   sudo ./setup-fq-qdisc.sh enp7s7 enp7s8   # explicit interface list
#
# Safety: idempotent — safe to run multiple times. Replaces root qdisc with fq.
# To revert: tc qdisc replace dev <iface> root pfifo_fast
#
# Prerequisite: Linux kernel >= 4.19 (sch_fq built-in on Debian 12 / Ubuntu 24.04)

set -euo pipefail

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${GREEN}[fq-setup]${NC} $*"; }
warn() { echo -e "${YELLOW}[fq-setup]${NC} $*"; }
err()  { echo -e "${RED}[fq-setup]${NC} $*" >&2; }

if [[ $EUID -ne 0 ]]; then
    err "Must run as root (need tc permissions)"
    exit 1
fi

# Determine interface list
if [[ $# -gt 0 ]]; then
    INTERFACES=("$@")
else
    # Auto-detect: all non-loopback, non-tun, physical or veth interfaces
    # that have a default route or are in the mpquic config bind list.
    # Heuristic: look for interfaces matching enp*, eth*, wlan*
    mapfile -t INTERFACES < <(
        ip -o link show | awk -F': ' '{print $2}' | \
        grep -E '^(enp|eth|wlan|ens|bond|venet)' | \
        sort -u
    )
    if [[ ${#INTERFACES[@]} -eq 0 ]]; then
        err "No WAN interfaces detected. Specify them explicitly."
        exit 1
    fi
fi

log "Target interfaces: ${INTERFACES[*]}"

APPLIED=0
FAILED=0

for iface in "${INTERFACES[@]}"; do
    # Verify interface exists
    if ! ip link show "$iface" &>/dev/null; then
        warn "Interface $iface does not exist — skipping"
        continue
    fi

    # Check current qdisc
    current=$(tc qdisc show dev "$iface" root 2>/dev/null | head -1 || true)

    if echo "$current" | grep -q "fq "; then
        log "$iface: sch_fq already active — no change"
        ((APPLIED++))
        continue
    fi

    # Replace root qdisc with fq
    if tc qdisc replace dev "$iface" root fq 2>/dev/null; then
        log "$iface: sch_fq installed (was: ${current:-none})"
        ((APPLIED++))
    else
        err "$iface: failed to install sch_fq"
        ((FAILED++))
    fi
done

echo ""
log "Done: $APPLIED interfaces with sch_fq, $FAILED failed"

# Show result
for iface in "${INTERFACES[@]}"; do
    if ip link show "$iface" &>/dev/null; then
        echo "  $iface: $(tc qdisc show dev "$iface" root 2>/dev/null | head -1)"
    fi
done

if [[ $FAILED -gt 0 ]]; then
    exit 1
fi
