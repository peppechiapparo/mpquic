#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ ! -x "$ROOT_DIR/bin/mpquic" ]]; then
  echo "Errore: binario non trovato in $ROOT_DIR/bin/mpquic. Esegui prima: make build" >&2
  exit 1
fi

sudo "$ROOT_DIR/scripts/install_mpquic.sh" server

echo "Installazione server completata."
echo "Prossimo step: verifica/edita /etc/mpquic/instances/*.yaml e avvia mpquic@1.service"
