# luci-app-mpquic — LuCI UI for MPQUIC tunnel management
#
# Provides:
#   - rpcd plugin: bridges ubus → TBOX Management API (HTTP)
#   - LuCI views: Dashboard + Configuration editor
#   - UCI config: /etc/config/mpquic (TBOX address + auth token)
#
# Installation (manual — without OpenWrt build system):
#   ./install.sh [openwrt_ip] [tbox_ip] [auth_token]
#
# Architecture:
#
#   ┌──────────────────────────────────────────────────────────┐
#   │ OpenWrt 24.10 (10.10.11.254)                             │
#   │                                                          │
#   │  Browser ──▶ LuCI JS ──▶ ubus/rpcd ──▶ rpcd/mpquic     │
#   │                                            │             │
#   │                                     curl (HTTP)          │
#   └────────────────────────────────────────│─────────────────┘
#                                            │
#   ┌────────────────────────────────────────▼─────────────────┐
#   │ TBOX (10.10.11.100)                                      │
#   │                                                          │
#   │  mpquic-mgmt :8080 ──▶ systemctl / YAML / metrics       │
#   └──────────────────────────────────────────────────────────┘
#
# Files:
#   root/usr/libexec/rpcd/mpquic                      rpcd plugin (shell)
#   root/usr/share/rpcd/acl.d/luci-app-mpquic.json    ACL grants
#   root/usr/share/luci/menu.d/luci-app-mpquic.json   LuCI menu entries
#   root/etc/config/mpquic                             UCI config template
#   htdocs/luci-static/resources/view/mpquic/          LuCI JS views
#     dashboard.js                                     Dashboard + tunnel table
#     config.js                                        Configuration editor
#
# Dependencies:
#   curl                (opkg install curl)
#   jsonfilter          (built-in on OpenWrt)
#   rpcd                (built-in)
#   luci-base           (built-in)
#
# Security notes:
#   - Auth token stored in /etc/config/mpquic (root-readable only)
#   - rpcd enforces ACL — only admin users can access mpquic object
#   - Token is never exposed to browser; rpcd plugin injects it server-side
#   - TBOX API listen address should be LAN-only (10.10.11.100, not 0.0.0.0)
