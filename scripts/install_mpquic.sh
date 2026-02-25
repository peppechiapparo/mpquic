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

install -m 0755 "$ROOT_DIR/bin/mpquic" /usr/local/bin/mpquic
install -m 0755 "$ROOT_DIR/scripts/ensure_tun.sh" /usr/local/lib/mpquic/ensure_tun.sh
install -m 0644 "$ROOT_DIR/deploy/systemd/mpquic@.service" /etc/systemd/system/mpquic@.service

for i in 1 2 3 4 5 6; do
  install -m 0644 "$ROOT_DIR/deploy/config/$ROLE/$i.yaml" "/etc/mpquic/instances/$i.yaml"
  install -m 0644 "$ROOT_DIR/deploy/config/$ROLE/$i.env" "/etc/mpquic/instances/$i.env"
done

systemctl daemon-reload
for i in 1 2 3 4 5 6; do
  systemctl enable mpquic@"$i".service
done

echo "Installed role=$ROLE. Edit /etc/mpquic/instances/*.yaml before first start if needed."
