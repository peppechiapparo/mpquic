#!/usr/bin/env bash
###############################################################################
# mpquic-update.sh — Full update: pull, build, stop, install, restart
#
# Usage:
#   mpquic-update.sh [REPO_DIR]
#
# Auto-detects all running mpquic@ instances and restarts them after
# installing the new binary. Works on both server (VPS) and client.
###############################################################################
set -euo pipefail

REPO_DIR="${1:-/opt/mpquic}"
GO_BIN=""

# ── Helpers ────────────────────────────────────────────────────────────────
log()  { echo "[mpquic-update] $*"; }
die()  { echo "[mpquic-update] ERROR: $*" >&2; exit 1; }

find_go() {
  # Prefer /usr/local/go (manually installed, usually newer) over system go
  if [[ -x /usr/local/go/bin/go ]]; then
    GO_BIN="/usr/local/go/bin/go"
  elif command -v go >/dev/null 2>&1; then
    GO_BIN="$(command -v go)"
  else
    die "go not found. Install Go 1.22+ first."
  fi
  log "go=$GO_BIN ($($GO_BIN version 2>&1 | head -1))"
}

# Discover all enabled mpquic@ instances from systemd
list_instances() {
  systemctl list-units --type=service --all --no-legend 'mpquic@*' \
    | awk '{print $1}' \
    | sed 's/mpquic@//;s/\.service//' \
    | sort
}

# ── Pre-flight checks ─────────────────────────────────────────────────────
[[ -d "$REPO_DIR/.git" ]] || die "git repo not found at $REPO_DIR"
cd "$REPO_DIR"

if [[ -n "$(git status --porcelain)" ]]; then
  die "repo has local changes, aborting. Run 'git stash' or 'git checkout -- .' first."
fi

OLD_HEAD="$(git rev-parse --short HEAD)"
log "repo=$REPO_DIR"
log "remote=$(git remote get-url origin)"
log "current=$OLD_HEAD"

# ── Step 1: Pull ──────────────────────────────────────────────────────────
log "--- pulling ---"
git fetch origin
git pull --ff-only

NEW_HEAD="$(git rev-parse --short HEAD)"
if [[ "$OLD_HEAD" == "$NEW_HEAD" ]]; then
  log "already up-to-date ($NEW_HEAD), proceeding with rebuild+restart."
else
  log "updated: $OLD_HEAD → $NEW_HEAD"
  git --no-pager log --oneline "${OLD_HEAD}..${NEW_HEAD}" | sed 's/^/  /'
fi

# ── Step 2: Build ─────────────────────────────────────────────────────────
log "--- building ---"
find_go
PATH="$(dirname "$GO_BIN"):$PATH" make build
log "binary: $(ls -lh bin/mpquic | awk '{print $5, $9}')"

# ── Step 3: Discover instances ────────────────────────────────────────────
mapfile -t INSTANCES < <(list_instances)
if [[ ${#INSTANCES[@]} -eq 0 ]]; then
  log "no running mpquic@ instances found, just installing binary."
fi
log "instances: ${INSTANCES[*]:-none}"

# ── Step 4: Stop all instances ────────────────────────────────────────────
if [[ ${#INSTANCES[@]} -gt 0 ]]; then
  log "--- stopping ${#INSTANCES[@]} instance(s) ---"
  # Stop all instances in parallel for faster shutdown
  for inst in "${INSTANCES[@]}"; do
    systemctl stop "mpquic@${inst}" &
  done
  wait
  # Brief wait to ensure binary is released
  sleep 1
fi

# ── Step 5: Install binary + scripts + systemd units ──────────────────────
log "--- installing ---"
cp bin/mpquic /usr/local/bin/mpquic
cp scripts/ensure_tun.sh    /usr/local/lib/mpquic/ensure_tun.sh    2>/dev/null || true
cp scripts/render_config.sh /usr/local/lib/mpquic/render_config.sh 2>/dev/null || true
chmod 0755 /usr/local/bin/mpquic
# Install updated systemd service file
if [[ -f deploy/systemd/mpquic@.service ]]; then
  cp deploy/systemd/mpquic@.service /etc/systemd/system/mpquic@.service
  log "systemd unit updated"
fi
log "binary installed to /usr/local/bin/mpquic"

# ── Step 6: Restart all instances ─────────────────────────────────────────
if [[ ${#INSTANCES[@]} -gt 0 ]]; then
  log "--- starting ${#INSTANCES[@]} instance(s) ---"
  systemctl daemon-reload
  for inst in "${INSTANCES[@]}"; do
    systemctl start "mpquic@${inst}"
  done
  sleep 2

  # Quick status check
  local_fail=0
  for inst in "${INSTANCES[@]}"; do
    state="$(systemctl is-active "mpquic@${inst}" 2>/dev/null || echo 'unknown')"
    if [[ "$state" != "active" ]]; then
      log "  ⚠ mpquic@${inst}: $state"
      local_fail=1
    else
      log "  ✓ mpquic@${inst}: active"
    fi
  done
  if [[ $local_fail -eq 1 ]]; then
    log "WARNING: some instances failed to start. Check 'journalctl -u mpquic@<name>'."
  fi
fi

log "=== update complete: $NEW_HEAD ==="
