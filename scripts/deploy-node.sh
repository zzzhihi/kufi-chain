#!/usr/bin/env bash
set -Eeuo pipefail

CHAIN_DIR="${CHAIN_DIR:-/srv/kufi/dacn_datn/chain}"
DEPLOY_BRANCH="${DEPLOY_BRANCH:-main}"

log() {
  printf '[deploy-chain-node] %s\n' "$*"
}

cd "$CHAIN_DIR"

log "Updating chain repository (branch: ${DEPLOY_BRANCH})"
git fetch origin "$DEPLOY_BRANCH"

current_branch="$(git branch --show-current || true)"
if [[ -z "$current_branch" ]]; then
  git checkout -B "$DEPLOY_BRANCH" "origin/$DEPLOY_BRANCH"
elif [[ "$current_branch" != "$DEPLOY_BRANCH" ]]; then
  git checkout "$DEPLOY_BRANCH" || git checkout -B "$DEPLOY_BRANCH" "origin/$DEPLOY_BRANCH"
fi

git pull --ff-only origin "$DEPLOY_BRANCH"

log "Applying runtime config patch for existing gateway.yaml files"
while IFS= read -r gw; do
  sed -i -E 's/^([[:space:]]*rate_limit:[[:space:]]*).*/\120000/' "$gw"
done < <(find "$CHAIN_DIR" -maxdepth 2 -type f -name 'gateway.yaml' 2>/dev/null || true)

log "Building binaries"
go mod download
go build -o kufichain ./cmd/kufichain
go build -o gateway ./cmd/gateway

log "Restarting systemd service"
sudo systemctl restart kufichain
sudo systemctl is-active --quiet kufichain
sudo systemctl status kufichain --no-pager -n 20

log "Deployment completed successfully"
