#!/usr/bin/env bash
set -euo pipefail

REPO="${REPO:-davidmilleronline85-eng/codex-remote}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${VERSION:-latest}"
CLOUDFLARED_REPO="${CLOUDFLARED_REPO:-cloudflare/cloudflared}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

need curl
need tar

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *)
    echo "unsupported architecture: $arch" >&2
    exit 1
    ;;
esac

fetch_latest_tag() {
  local repo="$1"
  curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1
}

install_codex_remote_release() {
  local version="$1"
  local version_no_v="${version#v}"
  local asset="codex-remote_${version_no_v}_${os}_${arch}.tar.gz"
  local url="https://github.com/$REPO/releases/download/$version/$asset"

  echo "Downloading $url"
  curl -fsSL "$url" -o "$tmpdir/$asset"
  tar -xzf "$tmpdir/$asset" -C "$tmpdir"
  install "$tmpdir/codex-remote" "$INSTALL_DIR/codex-remote"
}

install_codex_remote_go() {
  if ! command -v go >/dev/null 2>&1; then
    echo "could not download a release and Go is not installed for fallback" >&2
    exit 1
  fi
  echo "Falling back to go install"
  GOBIN="$INSTALL_DIR" go install "github.com/$REPO/cmd/codex-remote@latest"
}

install_cloudflared_direct() {
  local version asset url
  version="$(fetch_latest_tag "$CLOUDFLARED_REPO")"
  if [[ -z "$version" ]]; then
    echo "could not resolve latest cloudflared release" >&2
    exit 1
  fi

  case "$os" in
    darwin)
      asset="cloudflared-${os}-${arch}.tgz"
      url="https://github.com/$CLOUDFLARED_REPO/releases/download/$version/$asset"
      echo "Downloading $url"
      curl -fsSL "$url" -o "$tmpdir/$asset"
      tar -xzf "$tmpdir/$asset" -C "$tmpdir"
      install "$tmpdir/cloudflared" "$INSTALL_DIR/cloudflared"
      ;;
    linux)
      asset="cloudflared-${os}-${arch}"
      url="https://github.com/$CLOUDFLARED_REPO/releases/download/$version/$asset"
      echo "Downloading $url"
      curl -fsSL "$url" -o "$tmpdir/cloudflared"
      chmod +x "$tmpdir/cloudflared"
      install "$tmpdir/cloudflared" "$INSTALL_DIR/cloudflared"
      ;;
    *)
      echo "automatic cloudflared install is not supported on $os" >&2
      exit 1
      ;;
  esac
}

ensure_cloudflared() {
  if command -v cloudflared >/dev/null 2>&1; then
    return
  fi
  echo "Installing cloudflared"
  install_cloudflared_direct
}

if [[ "$VERSION" == "latest" ]]; then
  VERSION="$(fetch_latest_tag "$REPO")"
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

mkdir -p "$INSTALL_DIR"

if [[ -n "$VERSION" ]]; then
  if ! install_codex_remote_release "$VERSION"; then
    install_codex_remote_go
  fi
else
  install_codex_remote_go
fi

ensure_cloudflared

echo "Installed codex-remote to $INSTALL_DIR/codex-remote"
echo "Installed cloudflared to $(command -v cloudflared || echo "$INSTALL_DIR/cloudflared")"
echo
if ! command -v codex >/dev/null 2>&1; then
  echo "warning: codex CLI was not found on PATH." >&2
  echo "Install Codex and log in before running `codex-remote start`." >&2
  echo
fi
echo "Next steps:"
echo "  codex-remote start"
echo
echo "If '$INSTALL_DIR' is not on your PATH, add it first."
