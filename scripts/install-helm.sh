#!/bin/sh
set -eu
# Install only into the caller-selected tool directory; never change system tools.
destination=${1:?usage: install-helm.sh /path/to/tools}
version=v3.20.2
case "$(uname -m)" in
  x86_64) arch=amd64; checksum=258e830a9e613c8a7a302d6059b4bb3b9758f2f3e1bb8ea0d707ce10a9a72fea ;;
  aarch64|arm64) arch=arm64; checksum=5ea2d6bc2cda3f8edf985e028809f5a9278f404fb8ab24044de9b7cb9b79a691 ;;
  *) printf '%s\n' 'Unsupported Helm architecture' >&2; exit 1 ;;
esac
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
asset="helm-$version-linux-$arch.tar.gz"
curl --fail --location --silent --show-error "https://get.helm.sh/$asset" -o "$scratch/$asset"
printf '%s  %s\n' "$checksum" "$scratch/$asset" | sha256sum -c -
tar -xzf "$scratch/$asset" -C "$scratch" "linux-$arch/helm"
mkdir -p "$destination"
install -m 0755 "$scratch/linux-$arch/helm" "$destination/helm"
"$destination/helm" version --short
