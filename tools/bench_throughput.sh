#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHAIN_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

cd "${CHAIN_DIR}"

if command -v go >/dev/null 2>&1; then
  GO_BIN="go"
elif [[ -x /usr/local/go/bin/go ]]; then
  GO_BIN="/usr/local/go/bin/go"
else
  echo "ERROR: go command not found. Install Go or re-login to refresh PATH." >&2
  exit 1
fi

exec "${GO_BIN}" run -buildvcs=false ./cmd/benchchain "$@"
