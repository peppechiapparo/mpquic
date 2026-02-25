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
install -d /etc/mpquic/instances
install -d /etc/mpquic/tls

install -m 0755 "$ROOT_DIR/bin/mpquic" /usr/local/bin/mpquic
install -m 0755 "$ROOT_DIR/scripts/ensure_tun.sh" /usr/local/lib/mpquic/ensure_tun.sh
install -m 0755 "$ROOT_DIR/scripts/render_config.sh" /usr/local/lib/mpquic/render_config.sh
install -m 0755 "$ROOT_DIR/scripts/generate_tls_certs.sh" /usr/local/lib/mpquic/generate_tls_certs.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-tunnel-watchdog.sh" /usr/local/lib/mpquic/mpquic-tunnel-watchdog.sh
install -m 0755 "$ROOT_DIR/scripts/mpquic-if-event.sh" /usr/local/lib/mpquic/mpquic-if-event.sh
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic@.service" /etc/systemd/system/mpquic@.service
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic-watchdog.service" /etc/systemd/system/mpquic-watchdog.service
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic-watchdog.timer" /etc/systemd/system/mpquic-watchdog.timer

install -d /etc/network/if-up.d /etc/network/if-post-down.d
install -m 0755 "$ROOT_DIR/deploy/hooks/mpquic-ifupdown-hook" /etc/network/if-up.d/mpquic-auto
install -m 0755 "$ROOT_DIR/deploy/hooks/mpquic-ifupdown-hook" /etc/network/if-post-down.d/mpquic-auto

for i in 1 2 3 4 5 6; do
  install -m 0644 "$ROOT_DIR/deploy/config/$ROLE/$i.yaml" "/etc/mpquic/instances/$i.yaml.tpl"
  install -m 0644 "$ROOT_DIR/deploy/config/$ROLE/$i.env" "/etc/mpquic/instances/$i.env"
done

if [[ ! -f /etc/mpquic/global.env ]]; then
  install -m 0644 "$ROOT_DIR/deploy/config/global.env" /etc/mpquic/global.env
fi

systemctl daemon-reload
for i in 1 2 3 4 5 6; do
  systemctl enable mpquic@"$i".service
done
systemctl enable --now mpquic-watchdog.timer

echo "Installed role=$ROLE. Set VPS_PUBLIC_IP in /etc/mpquic/global.env and edit /etc/mpquic/instances/*.yaml.tpl if needed."
