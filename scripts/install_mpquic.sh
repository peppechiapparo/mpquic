#!/usr/bin/env bash
set -euo pipefail

ROLE="${1:-}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ "$ROLE" != "client" && "$ROLE" != "server" ]]; then
  echo "usage: $0 <client|server>" >&2
  exit 1
fi

install -d /usr/local/bin
install -d /usr/local/lib/mpquic
install -d /usr/local/sbin
install -d /etc/mpquic/instances
install -d /etc/mpquic/tls

install -m 0755 "$ROOT_DIR/bin/mpquic" /usr/local/bin/mpquic
install -m 0755 "$ROOT_DIR/scripts/ensure_tun.sh" /usr/local/lib/mpquic/ensure_tun.sh
install -m 0755 "$ROOT_DIR/scripts/render_config.sh" /usr/local/lib/mpquic/render_config.sh
install -m 0755 "$ROOT_DIR/scripts/generate_tls_certs.sh" /usr/local/lib/mpquic/generate_tls_certs.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-tunnel-watchdog.sh" /usr/local/lib/mpquic/mpquic-tunnel-watchdog.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-server-watchdog.sh" /usr/local/lib/mpquic/mpquic-server-watchdog.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-if-event.sh" /usr/local/lib/mpquic/mpquic-if-event.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-healthcheck.sh" /usr/local/sbin/mpquic-healthcheck.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-update.sh" /usr/local/sbin/mpquic-update.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-lan-routing-check.sh" /usr/local/sbin/mpquic-lan-routing-check.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-multipath-smoke.sh" /usr/local/sbin/mpquic-multipath-smoke.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-controlapi-lb-failover-test.sh" /usr/local/sbin/mpquic-controlapi-lb-failover-test.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-long-diagnostics.sh" /usr/local/sbin/mpquic-long-diagnostics.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-postmortem.sh" /usr/local/sbin/mpquic-postmortem.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-postmortem-remote.sh" /usr/local/sbin/mpquic-postmortem-remote.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-vps-routes.sh" /usr/local/sbin/mpquic-vps-routes.sh
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic@.service" /etc/systemd/system/mpquic@.service
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic-watchdog.service" /etc/systemd/system/mpquic-watchdog.service
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic-watchdog.timer" /etc/systemd/system/mpquic-watchdog.timer
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic-server-watchdog.service" /etc/systemd/system/mpquic-server-watchdog.service
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic-server-watchdog.timer" /etc/systemd/system/mpquic-server-watchdog.timer
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic-vps-routes.service" /etc/systemd/system/mpquic-vps-routes.service

if [[ "$ROLE" == "client" ]]; then
  install -m 0755 "$ROOT_DIR/scripts/mpquic-policy-routing.sh" /usr/local/sbin/mpquic-policy-routing.sh
  install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic-routing.service" /etc/systemd/system/mpquic-routing.service

  install -d /etc/network/if-up.d /etc/network/if-post-down.d
  install -m 0755 "$ROOT_DIR/deploy/hooks/mpquic-ifupdown-hook" /etc/network/if-up.d/mpquic-auto
  install -m 0755 "$ROOT_DIR/deploy/hooks/mpquic-ifupdown-hook" /etc/network/if-post-down.d/mpquic-auto

  if command -v networkctl >/dev/null 2>&1 || [[ -d /etc/networkd-dispatcher ]]; then
    install -d /etc/networkd-dispatcher/routable.d
    install -d /etc/networkd-dispatcher/configured.d
    install -d /etc/networkd-dispatcher/degraded.d
    install -d /etc/networkd-dispatcher/off.d
    install -d /etc/networkd-dispatcher/no-carrier.d
    install -m 0755 "$ROOT_DIR/deploy/hooks/mpquic-networkd-dispatcher-hook" /etc/networkd-dispatcher/routable.d/50-mpquic-auto
    install -m 0755 "$ROOT_DIR/deploy/hooks/mpquic-networkd-dispatcher-hook" /etc/networkd-dispatcher/configured.d/50-mpquic-auto
    install -m 0755 "$ROOT_DIR/deploy/hooks/mpquic-networkd-dispatcher-hook" /etc/networkd-dispatcher/degraded.d/50-mpquic-auto
    install -m 0755 "$ROOT_DIR/deploy/hooks/mpquic-networkd-dispatcher-hook" /etc/networkd-dispatcher/off.d/50-mpquic-auto
    install -m 0755 "$ROOT_DIR/deploy/hooks/mpquic-networkd-dispatcher-hook" /etc/networkd-dispatcher/no-carrier.d/50-mpquic-auto
  fi
fi

for i in 1 2 3 4 5 6; do
  install -m 0644 "$ROOT_DIR/deploy/config/$ROLE/$i.yaml" "/etc/mpquic/instances/$i.yaml.tpl"
  install -m 0644 "$ROOT_DIR/deploy/config/$ROLE/$i.env" "/etc/mpquic/instances/$i.env"
done

if [[ ! -f /etc/mpquic/global.env ]]; then
  install -m 0644 "$ROOT_DIR/deploy/config/global.env" /etc/mpquic/global.env
fi

if [[ "$ROLE" == "server" ]]; then
  if [[ ! -s /etc/mpquic/tls/server.crt || ! -s /etc/mpquic/tls/server.key || ! -s /etc/mpquic/tls/ca.crt ]]; then
    /usr/local/lib/mpquic/generate_tls_certs.sh /etc/mpquic/tls mpquic-server 825
  fi

  if command -v nft >/dev/null 2>&1; then
    if nft list chain inet filter input >/dev/null 2>&1; then
      if ! nft list chain inet filter input | grep -q 'udp dport 45001-45006 accept'; then
        nft add rule inet filter input udp dport 45001-45006 accept
      fi
      if [[ -f /etc/nftables.conf ]]; then
        nft list ruleset > /etc/nftables.conf
      fi
      systemctl restart nftables >/dev/null 2>&1 || true
    fi
  fi
fi

systemctl daemon-reload
for i in 1 2 3 4 5 6; do
  systemctl enable mpquic@"$i".service
done
if [[ "$ROLE" == "client" ]]; then
  systemctl enable --now mpquic-routing.service
  systemctl enable --now mpquic-watchdog.timer
  systemctl disable --now mpquic-server-watchdog.timer >/dev/null 2>&1 || true
else
  systemctl enable --now mpquic-server-watchdog.timer
  systemctl enable --now mpquic-vps-routes.service
  systemctl disable --now mpquic-watchdog.timer >/dev/null 2>&1 || true
fi

echo "Installed role=$ROLE. Set VPS_PUBLIC_IP in /etc/mpquic/global.env and edit /etc/mpquic/instances/*.yaml.tpl if needed."
