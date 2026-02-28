#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# setup-network.sh — configure systemd-networkd based networking for MPQUIC VM
#
# Creates a reproducible network layout on a fresh Debian 12 VM with 14 NICs:
#
#   MGMT1  enp6s18   10.10.11.100/24  (default gateway, DNS)
#   MGMT2  enp6s19   10.10.10.100/24  (no gateway)
#   LAN1   enp6s20   172.16.1.1/30   (transit to modem 1)
#   LAN2   enp6s21   172.16.2.1/30   (transit to modem 2)
#   LAN3   enp6s22   172.16.3.1/30   (transit to modem 3)
#   LAN4   enp6s23   172.16.4.1/30   (transit to modem 4)
#   LAN5   enp7s1    172.16.5.1/30   (transit to modem 5)
#   LAN6   enp7s2    172.16.6.1/30   (transit to modem 6)
#   WAN1   enp7s3    DHCP             (modem 1 uplink)
#   WAN2   enp7s4    DHCP             (modem 2 uplink)
#   WAN3   enp7s5    DHCP             (modem 3 uplink)
#   WAN4   enp7s6    DHCP             (modem 4 uplink)
#   WAN5   enp7s7    DHCP             (modem 5 uplink)
#   WAN6   enp7s8    DHCP             (modem 6 uplink)
#
# Requirements:
#   - Debian 12 with systemd >= 252
#   - 14 NICs matching the names above (Proxmox VirtIO)
#
# Usage:
#   sudo ./setup-network.sh [OPTIONS]
#
# Options:
#   --mgmt1-ip ADDR     MGMT1 IP (default: 10.10.11.100/24)
#   --mgmt1-gw ADDR     MGMT1 gateway (default: 10.10.11.1)
#   --mgmt2-ip ADDR     MGMT2 IP (default: 10.10.10.100/24)
#   --dns SERVERS        Comma-separated DNS servers
#                        (default: 10.150.19.1,1.1.1.1,8.8.8.8)
#   --dry-run            Show what would be done without applying
#   -y, --yes            Skip confirmation prompt
#   -h, --help           Show this help
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

# ── defaults ─────────────────────────────────────────────────────────────────
MGMT1_IP="10.10.11.100/24"
MGMT1_GW="10.10.11.1"
MGMT2_IP="10.10.10.100/24"
DNS_SERVERS="10.150.19.1,1.1.1.1,8.8.8.8"
DRY_RUN=0
AUTO_YES=0

NETWORKD_DIR="/etc/systemd/network"
RT_TABLES="/etc/iproute2/rt_tables"
RESOLV_CONF="/etc/resolv.conf"

# ── NIC mapping ──────────────────────────────────────────────────────────────
MGMT1_DEV="enp6s18"
MGMT2_DEV="enp6s19"

LAN_DEVS=("enp6s20" "enp6s21" "enp6s22" "enp6s23" "enp7s1" "enp7s2")
LAN_IPS=("172.16.1.1/30" "172.16.2.1/30" "172.16.3.1/30"
         "172.16.4.1/30" "172.16.5.1/30" "172.16.6.1/30")

WAN_DEVS=("enp7s3" "enp7s4" "enp7s5" "enp7s6" "enp7s7" "enp7s8")

# ── functions ────────────────────────────────────────────────────────────────
usage() {
  sed -n '/^# Usage:/,/^# ─/{ /^# ─/d; s/^# //; p }' "$0"
  exit 0
}

log()  { printf '[setup-network] %s\n' "$*"; }
warn() { printf '[setup-network] WARN: %s\n' "$*" >&2; }
die()  { printf '[setup-network] ERROR: %s\n' "$*" >&2; exit 1; }

write_file() {
  local path="$1" content="$2"
  if (( DRY_RUN )); then
    log "[dry-run] would create $path:"
    printf '%s\n' "$content" | sed 's/^/  /'
    echo
  else
    printf '%s\n' "$content" > "$path"
    log "created $path"
  fi
}

# ── parse args ───────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --mgmt1-ip) MGMT1_IP="$2"; shift 2 ;;
    --mgmt1-gw) MGMT1_GW="$2"; shift 2 ;;
    --mgmt2-ip) MGMT2_IP="$2"; shift 2 ;;
    --dns)      DNS_SERVERS="$2"; shift 2 ;;
    --dry-run)  DRY_RUN=1; shift ;;
    -y|--yes)   AUTO_YES=1; shift ;;
    -h|--help)  usage ;;
    *)          die "unknown option: $1" ;;
  esac
done

# ── preflight ────────────────────────────────────────────────────────────────
if (( ! DRY_RUN )); then
  [[ "$(id -u)" -eq 0 ]] || die "must run as root (use sudo)"
fi

# Check systemd-networkd exists
command -v networkctl &>/dev/null || die "systemd-networkd not found"

if (( ! AUTO_YES && ! DRY_RUN )); then
  log "This will configure systemd-networkd for the MPQUIC VM."
  log "  MGMT1: ${MGMT1_DEV} → ${MGMT1_IP} gw ${MGMT1_GW}"
  log "  MGMT2: ${MGMT2_DEV} → ${MGMT2_IP}"
  log "  LAN:   ${LAN_DEVS[*]} → 172.16.x.1/30"
  log "  WAN:   ${WAN_DEVS[*]} → DHCP"
  log "  DNS:   ${DNS_SERVERS}"
  echo
  read -rp "Continue? [y/N] " answer
  [[ "$answer" =~ ^[Yy]$ ]] || { log "Aborted."; exit 1; }
fi

# ── step 1: disable conflicting network managers ────────────────────────────
log "=== Step 1: Disable conflicting network managers ==="

for svc in networking NetworkManager; do
  if systemctl is-enabled "$svc" &>/dev/null 2>&1; then
    if (( DRY_RUN )); then
      log "[dry-run] would mask $svc.service"
    else
      systemctl stop "$svc" 2>/dev/null || true
      systemctl mask "$svc" 2>/dev/null || true
      log "masked $svc.service"
    fi
  fi
done

# Remove stale dhclient leases that can poison gateway detection
if (( ! DRY_RUN )); then
  rm -f /var/lib/dhcp/dhclient.enp*.leases 2>/dev/null || true
  log "cleaned stale dhclient lease files"
fi

# Rename legacy interfaces file if present
if [[ -f /etc/network/interfaces ]] && (( ! DRY_RUN )); then
  mv /etc/network/interfaces /etc/network/interfaces.bak.$(date +%s) 2>/dev/null || true
  log "renamed /etc/network/interfaces → .bak"
fi

# ── step 2: enable systemd-networkd ─────────────────────────────────────────
log "=== Step 2: Enable systemd-networkd ==="

if (( ! DRY_RUN )); then
  systemctl unmask systemd-networkd 2>/dev/null || true
  systemctl enable --now systemd-networkd
  log "systemd-networkd enabled and started"
fi

# ── step 3: create networkd unit files ───────────────────────────────────────
log "=== Step 3: Create networkd configuration files ==="

if (( ! DRY_RUN )); then
  mkdir -p "$NETWORKD_DIR"
  # Remove any existing .network files to start clean
  rm -f "$NETWORKD_DIR"/*.network 2>/dev/null || true
fi

# Build DNS lines
IFS=',' read -ra DNS_LIST <<< "$DNS_SERVERS"
DNS_LINES=""
for d in "${DNS_LIST[@]}"; do
  DNS_LINES+="DNS=${d}"$'\n'
done

# MGMT1 — primary management (with gateway + DNS)
write_file "$NETWORKD_DIR/01-mgmt1.network" \
"[Match]
Name=${MGMT1_DEV}

[Network]
Address=${MGMT1_IP}
Gateway=${MGMT1_GW}
${DNS_LINES}LinkLocalAddressing=no
IPv6AcceptRA=no

[Route]
Destination=$(echo "$MGMT1_IP" | sed 's|\.[0-9]*/|.0/|')
Scope=link"

# MGMT2 — secondary management (no gateway)
write_file "$NETWORKD_DIR/02-mgmt2.network" \
"[Match]
Name=${MGMT2_DEV}

[Network]
Address=${MGMT2_IP}
LinkLocalAddressing=no
IPv6AcceptRA=no"

# WAN interfaces — single match-all file, DHCP
write_file "$NETWORKD_DIR/10-wan.network" \
"[Match]
Name=${WAN_DEVS[*]}

[Network]
DHCP=yes
IPv6AcceptRA=no
LinkLocalAddressing=no

[DHCP]
RouteMetric=100
UseDNS=no
UseRoutes=yes"

# LAN transit interfaces — one file each with static /30
for i in "${!LAN_DEVS[@]}"; do
  n=$((i + 1))
  prio=$((20 + i))
  write_file "$NETWORKD_DIR/${prio}-lan${n}.network" \
"[Match]
Name=${LAN_DEVS[$i]}

[Network]
Address=${LAN_IPS[$i]}
LinkLocalAddressing=no
IPv6AcceptRA=no"
done

# ── step 4: routing tables ──────────────────────────────────────────────────
log "=== Step 4: Configure routing tables ==="

RT_ENTRIES=(
  "100 wan1"
  "101 wan2"
  "102 wan3"
  "103 wan4"
  "104 wan5"
  "105 wan6"
)

if (( DRY_RUN )); then
  log "[dry-run] would ensure routing tables in ${RT_TABLES}:"
  for entry in "${RT_ENTRIES[@]}"; do
    log "  $entry"
  done
else
  for entry in "${RT_ENTRIES[@]}"; do
    if ! grep -qF "$entry" "$RT_TABLES" 2>/dev/null; then
      echo "$entry" >> "$RT_TABLES"
      log "added rt_table: $entry"
    else
      log "rt_table already present: $entry"
    fi
  done
fi

# ── step 5: static DNS (resolv.conf) ────────────────────────────────────────
log "=== Step 5: Configure static DNS ==="

if (( DRY_RUN )); then
  log "[dry-run] would write ${RESOLV_CONF} with: ${DNS_SERVERS}"
else
  # Remove immutable flag if set (from previous run)
  chattr -i "$RESOLV_CONF" 2>/dev/null || true

  # Unlink if it's a symlink (e.g. to systemd-resolved stub)
  if [[ -L "$RESOLV_CONF" ]]; then
    rm -f "$RESOLV_CONF"
    log "removed resolv.conf symlink"
  fi

  {
    for d in "${DNS_LIST[@]}"; do
      echo "nameserver ${d}"
    done
  } > "$RESOLV_CONF"

  # Lock it so networkd/resolvconf don't overwrite
  chattr +i "$RESOLV_CONF"
  log "wrote ${RESOLV_CONF} (immutable)"
fi

# ── step 6: disable systemd-resolved if active ──────────────────────────────
log "=== Step 6: Disable systemd-resolved (if active) ==="

if systemctl is-active systemd-resolved &>/dev/null 2>&1; then
  if (( DRY_RUN )); then
    log "[dry-run] would stop and mask systemd-resolved"
  else
    systemctl stop systemd-resolved
    systemctl mask systemd-resolved
    log "masked systemd-resolved"
  fi
else
  log "systemd-resolved not active, skipping"
fi

# ── step 7: apply configuration ─────────────────────────────────────────────
log "=== Step 7: Apply configuration ==="

if (( ! DRY_RUN )); then
  networkctl reload 2>/dev/null || systemctl restart systemd-networkd
  log "networkd reloaded"
  sleep 3

  # Show result
  log "=== Current network state ==="
  networkctl --no-pager
  echo
  ip -br addr
fi

log ""
log "=== Network setup complete ==="
log ""
log "Next steps:"
log "  1. Verify connectivity:  ping -c 2 ${MGMT1_GW}"
log "  2. Install mpquic:       install_mpquic.sh client"
log "  3. Start tunnels:        systemctl start mpquic@{1..6}"
log "  4. Rebuild routing:      mpquic-policy-routing.sh"
