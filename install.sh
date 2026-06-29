#!/usr/bin/env sh
# AgentCarto installer.
#
# Downloads the prebuilt binaries (host + all plugin executables) for this
# machine from the latest GitHub release and installs them into one directory
# (the host finds its plugins next to its own binary). No Go or git required.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/agentcarto/agentcarto/main/install.sh | sh
#
# Override defaults with env vars:
#   PREFIX  install directory   (default: ~/.local/bin)
#   REPO    owner/repo          (default: agentcarto/agentcarto)
#   VERSION release tag         (default: latest)
set -eu

REPO="${REPO:-agentcarto/agentcarto}"
PREFIX="${PREFIX:-$HOME/.local/bin}"
VERSION="${VERSION:-latest}"

err() { echo "error: $*" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || err "curl is required"
command -v tar  >/dev/null 2>&1 || err "tar is required"

os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $os (Windows users: download the .zip from the releases page)" ;;
esac
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) err "unsupported architecture: $arch" ;;
esac

name="agentcarto_${os}_${arch}"
if [ "$VERSION" = latest ]; then
  url="https://github.com/$REPO/releases/latest/download/${name}.tar.gz"
else
  url="https://github.com/$REPO/releases/download/${VERSION}/${name}.tar.gz"
fi

echo "AgentCarto installer"
echo "  target : $os/$arch"
echo "  source : $url"
echo "  install: $PREFIX"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "  downloading..."
curl -fSL --progress-bar "$url" -o "$tmp/$name.tar.gz" \
  || err "download failed ($url). Has a release been published yet?"
tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

mkdir -p "$PREFIX"
cp "$tmp/$name/"agentcarto "$tmp/$name/"agentcarto-plugin-* "$PREFIX/"
chmod +x "$PREFIX/agentcarto" "$PREFIX/"agentcarto-plugin-*

echo "Installed agentcarto and its plugins into $PREFIX"

case ":${PATH}:" in
  *":$PREFIX:"*) ;;
  *)
    echo
    echo "note: $PREFIX is not on your PATH. Add it, e.g.:"
    echo "  echo 'export PATH=\"$PREFIX:\$PATH\"' >> ~/.profile && . ~/.profile"
    ;;
esac

echo
echo "Done. Run:  agentcarto"
