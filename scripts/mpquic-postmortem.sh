#!/usr/bin/env bash
set -euo pipefail

CLIENT_DIR="${1:-}"
SERVER_DIR="${2:-}"
OUT_FILE="${3:-}"

latest_dir() {
  local pattern="$1"
  ls -1dt ${pattern} 2>/dev/null | head -n 1 || true
}

if [[ -z "$CLIENT_DIR" ]]; then
  CLIENT_DIR="$(latest_dir /var/log/mpquic-diag-client-*)"
fi
if [[ -z "$SERVER_DIR" ]]; then
  SERVER_DIR="$(latest_dir /var/log/mpquic-diag-server-*)"
fi

if [[ -z "$CLIENT_DIR" || ! -d "$CLIENT_DIR" ]]; then
  echo "error: client diagnostic dir not found (arg1 or /var/log/mpquic-diag-client-*)" >&2
  exit 1
fi
if [[ -z "$SERVER_DIR" || ! -d "$SERVER_DIR" ]]; then
  echo "error: server diagnostic dir not found (arg2 or /var/log/mpquic-diag-server-*)" >&2
  exit 1
fi

CLIENT_J="$CLIENT_DIR/journal-follow.log"
SERVER_J="$SERVER_DIR/journal-follow.log"
CLIENT_S="$CLIENT_DIR/snapshots.log"
SERVER_S="$SERVER_DIR/snapshots.log"

for f in "$CLIENT_J" "$SERVER_J" "$CLIENT_S" "$SERVER_S"; do
  if [[ ! -f "$f" ]]; then
    echo "error: missing file $f" >&2
    exit 1
  fi
done

TMP_REPORT="$(mktemp)"
trap 'rm -f "$TMP_REPORT"' EXIT

count_matches() {
  local file="$1"
  local regex="$2"
  grep -E -c "$regex" "$file" 2>/dev/null || true
}

emit_counts() {
  local role="$1"
  local file="$2"
  echo "[$role] reconnect=$(count_matches "$file" 'reconnect in 3s')"
  echo "[$role] timeout_no_activity=$(count_matches "$file" 'timeout: no recent network activity')"
  echo "[$role] app_shutdown=$(count_matches "$file" 'Application error.*shutdown')"
  echo "[$role] app_superseded=$(count_matches "$file" 'Application error.*superseded')"
  echo "[$role] app_rx_error=$(count_matches "$file" 'Application error.*rx-error')"
  echo "[$role] app_tx_error=$(count_matches "$file" 'Application error.*tx-error')"
  echo "[$role] app_dial_error=$(count_matches "$file" 'Application error.*dial-error')"
  echo "[$role] connected=$(count_matches "$file" 'INFO connected')"
  echo "[$role] accepted=$(count_matches "$file" 'INFO accepted remote=')"
}

extract_window() {
  local role="$1"
  local file="$2"
  grep -E 'reconnect in 3s|timeout: no recent network activity|Application error|path down|path recovered|path telemetry|connected|accepted remote=' "$file" \
    | sed "s/^/[$role] /" || true
}

path_last_state() {
  local role="$1"
  local file="$2"
  grep 'path telemetry name=' "$file" \
    | awk -v role="$role" '
      {
        name=""; state="";
        for (i=1; i<=NF; i++) {
          if ($i ~ /^name=/) { split($i,a,"="); name=a[2] }
          if ($i ~ /^state=/) { split($i,b,"="); state=b[2] }
        }
        if (name != "") last[name]=state
      }
      END {
        if (length(last)==0) {
          print "[" role "] path_telemetry=none"
        } else {
          for (k in last) print "[" role "] path_last_state " k "=" last[k]
        }
      }
    '
}

first_ts() {
  local file="$1"
  sed -n '1p' "$file" | awk '{print $1" "$2}'
}

last_ts() {
  local file="$1"
  tail -n 1 "$file" | awk '{print $1" "$2}'
}

{
  echo "# MPQUIC post-mortem report"
  echo
  echo "client_dir=$CLIENT_DIR"
  echo "server_dir=$SERVER_DIR"
  echo
  echo "## Time range"
  echo "client_journal_first=$(first_ts "$CLIENT_J")"
  echo "client_journal_last=$(last_ts "$CLIENT_J")"
  echo "server_journal_first=$(first_ts "$SERVER_J")"
  echo "server_journal_last=$(last_ts "$SERVER_J")"
  echo
  echo "## Event counters"
  emit_counts "client" "$CLIENT_J"
  emit_counts "server" "$SERVER_J"
  echo
  echo "## Path last state"
  path_last_state "client" "$CLIENT_J"
  path_last_state "server" "$SERVER_J"
  echo
  echo "## Snapshot signals"
  echo "[client] packet_loss_100=$(count_matches "$CLIENT_S" '100% packet loss')"
  echo "[server] packet_loss_100=$(count_matches "$SERVER_S" '100% packet loss')"
  echo "[client] service_inactive=$(count_matches "$CLIENT_S" '@[1-6]=inactive')"
  echo "[server] service_inactive=$(count_matches "$SERVER_S" '@[1-6]=inactive')"
  echo
  echo "## Timeline (filtered)"
  {
    extract_window "client" "$CLIENT_J"
    extract_window "server" "$SERVER_J"
  } | sort
  echo
  echo "## Suggested next checks"
  echo "- Correlate 'reconnect in 3s' and 'Application error' timestamps across client/server timelines."
  echo "- If superseded spikes, check for overlapping tests (single-path instances + multipath smoke on same ports)."
  echo "- If timeout_no_activity increases without service restarts, inspect VM-to-VM bridge dataplane (ARP/FDB/offload)."
  echo "- Compare packet_loss_100 with path telemetry state transitions for impacted paths."
} > "$TMP_REPORT"

if [[ -n "$OUT_FILE" ]]; then
  install -D -m 0644 "$TMP_REPORT" "$OUT_FILE"
  cat "$OUT_FILE"
else
  cat "$TMP_REPORT"
fi
