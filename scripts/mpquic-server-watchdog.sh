#!/usr/bin/env bash
set -euo pipefail

for i in 1 2 3 4 5 6; do
  svc="mpquic@${i}.service"
  if ! systemctl is-active --quiet "$svc"; then
    systemctl restart "$svc" || true
  fi
done

if systemctl list-unit-files | grep -q '^mpquic-vps-routes\.service'; then
  systemctl restart mpquic-vps-routes.service || true
fi

exit 0
