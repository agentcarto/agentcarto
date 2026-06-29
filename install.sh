#!/usr/bin/env sh
# AgentCarto installer / updater.
#
# Downloads the prebuilt host binary plus the selected plugin executables for
# this machine and installs them into one directory (the host finds its plugins
# next to its own binary). The host and each plugin are released from their own
# GitHub repository, so the installer fetches one archive per component. No Go or
# git required.
#
# Re-run this same command to UPDATE. On an existing install the installer:
#   * updates only the plugins that are already installed (unless PLUGINS is set),
#   * resolves each component's latest release tag and skips the download when the
#     installed version already matches (recorded in $PREFIX/.agentcarto-versions).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/agentcarto/agentcarto/main/install.sh | sh
#
# Choose which plugins to install (default: all four on a fresh install):
#   curl -fsSL .../install.sh | PLUGINS="claude codex" sh
#   curl -fsSL .../install.sh | PLUGINS=none sh        # host only
#
# Override defaults with env vars:
#   PREFIX   install directory       (default: ~/.local/bin)
#   REPO     host owner/repo          (default: agentcarto/agentcarto)
#   VERSION  host release tag         (default: latest)
#   PLUGINS  space-separated plugin names, or "all" / "none"
#            (default: all on a fresh install; the already-installed set on update)
#            available: claude codex grok copilot
#
# Note: the host's VERSION can be pinned, but plugins are always installed from
# each plugin repo's latest release (their versions are managed independently).
set -eu

REPO="${REPO:-agentcarto/agentcarto}"
PREFIX="${PREFIX:-$HOME/.local/bin}"
VERSION="${VERSION:-latest}"

ALL_PLUGINS="claude codex grok copilot"
MARKER="$PREFIX/.agentcarto-versions"

# Distinguish "PLUGINS unset" (auto-select) from an explicit value (incl. empty).
if [ "${PLUGINS+x}" = x ]; then PLUGINS_SET=1; else PLUGINS_SET=0; PLUGINS=all; fi

err() { echo "error: $*" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || err "curl is required"
command -v tar  >/dev/null 2>&1 || err "tar is required"

os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $os (Windows users: download the .zip files from the releases pages)" ;;
esac
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) err "unsupported architecture: $arch" ;;
esac

# Map a plugin short name to "owner/repo". The archive prefix is always
# "agentcarto-plugin-<name>" and the contained executables are agentcarto-plugin-*.
plugin_repo() {
  case "$1" in
    claude)  echo "agentcarto/plugin-claude" ;;
    codex)   echo "agentcarto/plugin-codex" ;;
    grok)    echo "agentcarto/plugin-grok" ;;
    copilot) echo "agentcarto/plugin-copilot" ;;
    *) return 1 ;;
  esac
}

# Is plugin <name> already present in PREFIX? (copilot ships -vc/-jb; probe -vc.)
plugin_installed() {
  case "$1" in
    copilot) [ -e "$PREFIX/agentcarto-plugin-copilot-vc" ] ;;
    *)       [ -e "$PREFIX/agentcarto-plugin-$1" ] ;;
  esac
}

updating=0
[ -e "$PREFIX/agentcarto" ] && updating=1

# Resolve the plugin selection into a concrete list.
if [ "$PLUGINS_SET" = 1 ]; then
  case "$PLUGINS" in
    all)  plugins="$ALL_PLUGINS" ;;
    none) plugins="" ;;
    *)    plugins="$PLUGINS" ;;
  esac
elif [ "$updating" = 1 ]; then
  # Update mode: touch only the plugins that are already installed.
  plugins=""
  for p in $ALL_PLUGINS; do plugin_installed "$p" && plugins="$plugins $p"; done
  plugins="${plugins# }"
else
  plugins="$ALL_PLUGINS"
fi

# Validate the selection up front so a typo fails before any download.
for p in $plugins; do
  plugin_repo "$p" >/dev/null 2>&1 || err "unknown plugin: $p (available: $ALL_PLUGINS)"
done

echo "AgentCarto installer"
echo "  mode   : $([ "$updating" = 1 ] && echo update || echo install)"
echo "  target : $os/$arch"
echo "  install: $PREFIX"
echo "  plugins: ${plugins:-<none>}"

mkdir -p "$PREFIX"

# Resolve a repo's latest release tag from the redirect of /releases/latest
# (no GitHub API token or jq needed). Echoes the tag, or fails if none exists.
latest_tag() {
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/$1/releases/latest" 2>/dev/null) \
    || return 1
  tag="${url##*/tag/}"
  [ -n "$tag" ] && [ "$tag" != "$url" ] && echo "$tag"
}

# resolve_tag <owner/repo> <version-spec>  ->  concrete tag
resolve_tag() {
  if [ "$2" = latest ]; then latest_tag "$1"; else echo "$2"; fi
}

# Marker file helpers: one "<archive-prefix> <tag>" line per component.
recorded_tag() {
  [ -f "$MARKER" ] || return 0
  while IFS=' ' read -r k v; do [ "$k" = "$1" ] && { echo "$v"; return 0; }; done < "$MARKER"
}
record_tag() {
  tmpm=$(mktemp)
  [ -f "$MARKER" ] && grep -v "^$1 " "$MARKER" > "$tmpm" 2>/dev/null || true
  echo "$1 $2" >> "$tmpm"
  mv "$tmpm" "$MARKER"
}

# download_extract <owner/repo> <archive-prefix> <tag>
# Fetches <prefix>_<os>_<arch>.tar.gz for the given tag into a fresh tmp dir,
# extracts it, and copies every executable it contains into PREFIX.
download_extract() {
  repo="$1"; prefix="$2"; tag="$3"
  name="${prefix}_${os}_${arch}"
  url="https://github.com/$repo/releases/download/${tag}/${name}.tar.gz"

  tmp=$(mktemp -d)
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" EXIT
  echo "  downloading $repo@$tag ..."
  curl -fSL --progress-bar "$url" -o "$tmp/$name.tar.gz" \
    || err "download failed ($url)."
  tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
  cp "$tmp/$name/"* "$PREFIX/"
  rm -rf "$tmp"
  trap - EXIT
}

# install_component <owner/repo> <archive-prefix> <version-spec>
# Resolves the target tag, skips when already current, otherwise updates and
# records the new version in the marker file.
install_component() {
  repo="$1"; prefix="$2"; spec="$3"
  tag=$(resolve_tag "$repo" "$spec") || err "could not resolve a release for $repo (has one been published?)"
  if [ "$(recorded_tag "$prefix")" = "$tag" ]; then
    echo "  $prefix $tag (up to date, skipping)"
    return 0
  fi
  download_extract "$repo" "$prefix" "$tag"
  record_tag "$prefix" "$tag"
}

# Host first.
install_component "$REPO" "agentcarto" "$VERSION"
chmod +x "$PREFIX/agentcarto"

# Then each selected plugin, in order. Plugins are always pulled from latest.
for p in $plugins; do
  install_component "$(plugin_repo "$p")" "agentcarto-plugin-$p" latest
done
# Make every installed plugin executable.
chmod +x "$PREFIX/"agentcarto-plugin-* 2>/dev/null || true

echo "$([ "$updating" = 1 ] && echo Updated || echo Installed) agentcarto${plugins:+ and plugins:$plugins} in $PREFIX"

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
