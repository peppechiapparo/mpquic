#!/usr/bin/env bash
set -euo pipefail

for i in 1 2 3 4 5 6; do
  echo "=== instance $i ==="
  systemctl is-enabled mpquic@"$i".service
  systemctl is-active mpquic@"$i".service
  journalctl -u mpquic@"$i".service -n 10 --no-pager
  echo
done
