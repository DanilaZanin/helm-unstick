#!/usr/bin/env sh
# Helm plugin install/update hook: downloads the helm-unstick release binary that matches
# the version in plugin.yaml into $HELM_PLUGIN_DIR/bin and verifies its checksum.
#
# HELM_UNSTICK_DRY_RUN=1 prints the download URL and exits.
# HELM_UNSTICK_BINARY=/path/to/helm-unstick installs that binary instead of downloading a
# release, for installing the plugin from a local checkout (CI does this).
# HELM_UNSTICK_BASE_URL=URL downloads archive and checksums.txt from URL instead of the GitHub
# release (file:// works), so the download and checksum path can be tested without a release.
set -eu

repo="DanilaZanin/helm-unstick"
plugin_dir="${HELM_PLUGIN_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"

version=$(sed -n 's/^version:[[:space:]]*"\{0,1\}\([^"[:space:]]*\)"\{0,1\}[[:space:]]*$/\1/p' "$plugin_dir/plugin.yaml" | head -n 1)
if [ -z "$version" ]; then
  echo "helm-unstick: cannot read the version from $plugin_dir/plugin.yaml" >&2
  exit 1
fi

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "helm-unstick: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "helm-unstick: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

if [ -n "${HELM_UNSTICK_BINARY:-}" ]; then
  mkdir -p "$plugin_dir/bin"
  # a local install links the plugin directory to the checkout, so the file may be the same one
  [ "$HELM_UNSTICK_BINARY" -ef "$plugin_dir/bin/helm-unstick" ] || install -m 0755 "$HELM_UNSTICK_BINARY" "$plugin_dir/bin/helm-unstick"
  echo "helm-unstick: installed $HELM_UNSTICK_BINARY to $plugin_dir/bin/helm-unstick" >&2
  exit 0
fi

archive="helm-unstick_v${version}_${os}_${arch}.tar.gz"
base="${HELM_UNSTICK_BASE_URL:-https://github.com/${repo}/releases/download/v${version}}"

if [ "${HELM_UNSTICK_DRY_RUN:-0}" = 1 ]; then
  echo "$base/$archive"
  exit 0
fi

fetch() { # URL DEST
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$1" -O "$2"
  else
    echo "helm-unstick: curl or wget is required" >&2
    exit 1
  fi
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/helm-unstick.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

echo "helm-unstick: downloading $archive" >&2
fetch "$base/$archive" "$tmp/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt"

expected=$(grep "  $archive\$" "$tmp/checksums.txt" | cut -d ' ' -f 1)
if [ -z "$expected" ]; then
  echo "helm-unstick: $archive is not listed in checksums.txt" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$archive" | cut -d ' ' -f 1)
else
  actual=$(shasum -a 256 "$tmp/$archive" | cut -d ' ' -f 1)
fi
if [ "$expected" != "$actual" ]; then
  echo "helm-unstick: checksum mismatch for $archive" >&2
  exit 1
fi

tar -xzf "$tmp/$archive" -C "$tmp" helm-unstick
mkdir -p "$plugin_dir/bin"
install -m 0755 "$tmp/helm-unstick" "$plugin_dir/bin/helm-unstick"
echo "helm-unstick: installed $version to $plugin_dir/bin/helm-unstick" >&2
