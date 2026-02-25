#!/usr/bin/env bash
set -euo pipefail

for i in 1 2 3 4 5 6; do
  systemctl restart mpquic@"$i".service
  systemctl --no-pager --full status mpquic@"$i".service | sed -n '1,12p'
done
