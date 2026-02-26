#!/usr/bin/env bash
set -euo pipefail

CLIENT_HOST="${1:-mpquic}"
SERVER_HOST="${2:-vps-it-mpquic}"
OUT_FILE="${3:-/tmp/mpquic-postmortem-remote.txt}"
WORK_DIR="${4:-/tmp/mpquic-postmortem-remote}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOCAL_POSTMORTEM="$SCRIPT_DIR/mpquic-postmortem.sh"

ssh_retry() {
  local host="$1"
  local cmd="$2"
  local attempts=3
  local i
  for i in $(seq 1 "$attempts"); do
    if ssh "$host" "$cmd"; then
      return 0
    fi
    sleep 3
  done
  return 1
}

if [[ ! -x "$LOCAL_POSTMORTEM" ]]; then
  echo "error: local parser not found: $LOCAL_POSTMORTEM" >&2
  exit 1
fi

mkdir -p "$WORK_DIR"
TS="$(date +%Y%m%d_%H%M%S)"
RUN_DIR="$WORK_DIR/$TS"
mkdir -p "$RUN_DIR/client" "$RUN_DIR/server"

client_remote_dir="$(ssh_retry "$CLIENT_HOST" 'ls -1dt /var/log/mpquic-diag-client-* 2>/dev/null | head -n 1' || true)"
server_remote_dir="$(ssh_retry "$SERVER_HOST" 'ls -1dt /var/log/mpquic-diag-server-* 2>/dev/null | head -n 1' || true)"

if [[ -z "$client_remote_dir" ]]; then
  echo "error: no client diagnostics found on $CLIENT_HOST" >&2
  exit 1
fi
if [[ -z "$server_remote_dir" ]]; then
  echo "error: no server diagnostics found on $SERVER_HOST" >&2
  exit 1
fi

ssh_retry "$CLIENT_HOST" "sudo tar -C / -czf - ${client_remote_dir#/}" > "$RUN_DIR/client.tgz"
ssh_retry "$SERVER_HOST" "sudo tar -C / -czf - ${server_remote_dir#/}" > "$RUN_DIR/server.tgz"

tar -xzf "$RUN_DIR/client.tgz" -C "$RUN_DIR/client"
tar -xzf "$RUN_DIR/server.tgz" -C "$RUN_DIR/server"

client_local_dir="$RUN_DIR/client/${client_remote_dir#/}"
server_local_dir="$RUN_DIR/server/${server_remote_dir#/}"

"$LOCAL_POSTMORTEM" "$client_local_dir" "$server_local_dir" "$OUT_FILE"

echo "[mpquic-postmortem-remote] client_src=$CLIENT_HOST:$client_remote_dir"
echo "[mpquic-postmortem-remote] server_src=$SERVER_HOST:$server_remote_dir"
echo "[mpquic-postmortem-remote] report=$OUT_FILE"
echo "[mpquic-postmortem-remote] artifacts=$RUN_DIR"
