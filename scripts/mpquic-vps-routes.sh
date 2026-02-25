#!/usr/bin/env bash
set -euo pipefail

safe() { "$@" 2>/dev/null || true; }

safe ip route replace 172.16.1.0/30 dev mpq1
safe ip route replace 172.16.2.0/30 dev mpq2
safe ip route replace 172.16.3.0/30 dev mpq3
safe ip route replace 172.16.4.0/30 dev mpq4
safe ip route replace 172.16.5.0/30 dev mpq5
safe ip route replace 172.16.6.0/30 dev mpq6

exit 0
