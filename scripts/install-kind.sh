#!/bin/sh
set -eu
destination=${1:?usage: install-kind.sh /path/to/tools}
version=v0.33.0
case "$(uname -m)" in
  x86_64) arch=amd64; checksum=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d ;;
  *) printf '%s\n' 'This pinned CI installer supports Linux amd64; install kind 0.33.0 from its official release for other architectures.' >&2; exit 1 ;;
esac
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
curl --fail --location --silent --show-error "https://github.com/kubernetes-sigs/kind/releases/download/$version/kind-linux-$arch" -o "$scratch/kind"
printf '%s  %s\n' "$checksum" "$scratch/kind" | sha256sum -c -
mkdir -p "$destination"
install -m 0755 "$scratch/kind" "$destination/kind"
"$destination/kind" version
