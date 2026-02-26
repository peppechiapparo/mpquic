#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="${1:-/opt/mpquic}"

if [[ ! -d "$REPO_DIR/.git" ]]; then
  echo "error: git repository not found at $REPO_DIR" >&2
  exit 1
fi

cd "$REPO_DIR"

if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: repository has local changes, aborting update" >&2
  git status --short
  exit 2
fi

echo "[mpquic-update] repo=$REPO_DIR"
echo "[mpquic-update] remote=$(git remote get-url origin)"

git fetch origin
git pull --ff-only

echo "[mpquic-update] HEAD=$(git rev-parse --short HEAD)"
echo "[mpquic-update] done"
