#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
STATE_DIR="${STATE_DIR:-$HOME/Library/Application Support/codex-remote}"
REMOVE_CLOUDFLARED="${REMOVE_CLOUDFLARED:-0}"

if command -v codex-remote >/dev/null 2>&1; then
  codex-remote daemon uninstall --purge >/dev/null 2>&1 || true
fi

rm -f "$INSTALL_DIR/codex-remote"
rm -rf "$STATE_DIR"

if [[ "$REMOVE_CLOUDFLARED" == "1" ]]; then
  rm -f "$INSTALL_DIR/cloudflared"
  if command -v brew >/dev/null 2>&1; then
    brew uninstall cloudflared >/dev/null 2>&1 || true
  fi
fi

echo "Uninstalled codex-remote"
if [[ "$REMOVE_CLOUDFLARED" == "1" ]]; then
  echo "Also attempted to uninstall cloudflared"
fi
