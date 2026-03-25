#!/bin/bash
# install-luci-app.sh — Deploy luci-app-mpquic to OpenWrt router
#
# Usage:
#   ./install-luci-app.sh [openwrt_host] [tbox_host] [mgmt_token]
#
# Defaults:
#   openwrt_host = 10.10.11.254
#   tbox_host    = 10.10.11.100
#   mgmt_token   = (read from TBOX /etc/mpquic/mgmt.env)
#
# Prerequisites on OpenWrt:
#   opkg update && opkg install curl
#
# This script:
#   1. Installs rpcd plugin (/usr/libexec/rpcd/mpquic)
#   2. Installs ACL (/usr/share/rpcd/acl.d/luci-app-mpquic.json)
#   3. Installs LuCI menu (/usr/share/luci/menu.d/luci-app-mpquic.json)
#   4. Installs LuCI views (/www/luci-static/resources/view/mpquic/)
#   5. Creates UCI config (/etc/config/mpquic) with TBOX address + token
#   6. Restarts rpcd + uhttpd

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OWRT="${1:-10.10.11.254}"
TBOX="${2:-10.10.11.100}"
TOKEN="${3:-}"

echo "=== luci-app-mpquic installer ==="
echo "  OpenWrt:  $OWRT"
echo "  TBOX:     $TBOX"

# ── Verify OpenWrt connectivity ──────────────────────────────────────────
echo ""
echo "[1/7] Checking OpenWrt connectivity..."
ssh "root@${OWRT}" 'echo OK' || { echo "ERROR: cannot SSH to ${OWRT}"; exit 1; }

# ── Ensure curl is installed ─────────────────────────────────────────────
echo "[2/7] Ensuring curl is installed on OpenWrt..."
ssh "root@${OWRT}" 'command -v curl >/dev/null 2>&1 || (opkg update && opkg install curl)'

# ── Install rpcd plugin ──────────────────────────────────────────────────
echo "[3/7] Installing rpcd plugin..."
scp "${SCRIPT_DIR}/root/usr/libexec/rpcd/mpquic" "root@${OWRT}:/usr/libexec/rpcd/mpquic"
ssh "root@${OWRT}" 'chmod 0755 /usr/libexec/rpcd/mpquic'

# ── Install ACL ──────────────────────────────────────────────────────────
echo "[4/7] Installing ACL..."
scp "${SCRIPT_DIR}/root/usr/share/rpcd/acl.d/luci-app-mpquic.json" \
    "root@${OWRT}:/usr/share/rpcd/acl.d/luci-app-mpquic.json"

# ── Install LuCI menu ───────────────────────────────────────────────────
echo "[5/7] Installing LuCI menu + views..."
scp "${SCRIPT_DIR}/root/usr/share/luci/menu.d/luci-app-mpquic.json" \
    "root@${OWRT}:/usr/share/luci/menu.d/luci-app-mpquic.json"

# Install views
ssh "root@${OWRT}" 'mkdir -p /www/luci-static/resources/view/mpquic'
scp "${SCRIPT_DIR}/htdocs/luci-static/resources/view/mpquic/dashboard.js" \
    "root@${OWRT}:/www/luci-static/resources/view/mpquic/dashboard.js"
scp "${SCRIPT_DIR}/htdocs/luci-static/resources/view/mpquic/config.js" \
    "root@${OWRT}:/www/luci-static/resources/view/mpquic/config.js"

# ── Configure UCI ────────────────────────────────────────────────────────
echo "[6/7] Configuring UCI (/etc/config/mpquic)..."
if [ -z "$TOKEN" ]; then
    echo "    No token provided — reading from TBOX..."
    TOKEN=$(ssh "root@${OWRT}" "ssh satcom@${TBOX} 'sudo grep MGMT_AUTH_TOKEN /etc/mpquic/mgmt.env | cut -d= -f2-'" 2>/dev/null || true)
fi

if [ -z "$TOKEN" ]; then
    echo "    WARNING: Could not read token. Set manually:"
    echo "    ssh root@${OWRT} \"uci set mpquic.api.token='YOUR_TOKEN'; uci commit mpquic\""
else
    echo "    Token acquired (${#TOKEN} chars)"
fi

# Write or update UCI config
ssh "root@${OWRT}" bash -s <<UCIEOF
#!/bin/sh
# Create config if missing
[ -f /etc/config/mpquic ] || touch /etc/config/mpquic

uci -q batch <<EOB
delete mpquic.api
set mpquic.api=api
set mpquic.api.host='${TBOX}'
set mpquic.api.port='8080'
set mpquic.api.proto='http'
set mpquic.api.timeout='10'
set mpquic.api.token='${TOKEN}'
commit mpquic
EOB
echo "    UCI config written"
UCIEOF

# ── Restart services ─────────────────────────────────────────────────────
echo "[7/7] Restarting rpcd + uhttpd..."
ssh "root@${OWRT}" '/etc/init.d/rpcd restart && /etc/init.d/uhttpd restart'

echo ""
echo "=== Installation complete ==="
echo ""
echo "LuCI will show 'Services → MPQUIC Tunnels' after login."
echo "URL: http://${OWRT}/cgi-bin/luci/admin/services/mpquic/dashboard"
echo ""
echo "Verify rpcd plugin:"
echo "  ssh root@${OWRT} 'ubus call mpquic health'"
echo ""
