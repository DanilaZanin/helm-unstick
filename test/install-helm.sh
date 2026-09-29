#!/usr/bin/env bash
# Install the newest stable Helm of one major version, for the e2e matrix.
# Usage: test/install-helm.sh MAJOR DEST_DIR      (for example: 3 /tmp/helm3)
# Prints the path of the installed binary.
set -euo pipefail

major=${1:?usage: install-helm.sh MAJOR DEST_DIR}
dest=${2:?usage: install-helm.sh MAJOR DEST_DIR}

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

auth=()
if [[ -n "${GITHUB_TOKEN:-}" ]]; then auth=(-H "Authorization: Bearer $GITHUB_TOKEN"); fi

# Releases come newest first, so the first stable tag with this major is the newest one.
tag=$(curl -fsSL ${auth[@]+"${auth[@]}"} "https://api.github.com/repos/helm/helm/releases?per_page=100" |
  jq -r --arg prefix "v${major}." '[.[] | select(.prerelease == false and .draft == false) | .tag_name | select(startswith($prefix))][0] // empty')
if [[ -z "$tag" ]]; then
  echo "no stable Helm release found for major version $major" >&2
  exit 1
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/install-helm.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
archive="helm-${tag}-${os}-${arch}.tar.gz"
curl -fsSL "https://get.helm.sh/${archive}" -o "$tmp/$archive"
curl -fsSL "https://get.helm.sh/${archive}.sha256sum" -o "$tmp/$archive.sha256sum"
(
  cd "$tmp"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c "$archive.sha256sum" >&2
  else
    shasum -a 256 -c "$archive.sha256sum" >&2
  fi
)
tar -xzf "$tmp/$archive" -C "$tmp"
mkdir -p "$dest"
install -m 0755 "$tmp/${os}-${arch}/helm" "$dest/helm"
echo "installed Helm ${tag} to $dest/helm" >&2
echo "$dest/helm"
