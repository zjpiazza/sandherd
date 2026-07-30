#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
version=${CODEX_VERSION:-0.146.0}
release_url="https://github.com/openai/codex/releases/download/rust-v${version}"
identity="https://github.com/openai/codex/.github/workflows/rust-release.yml@refs/tags/rust-v${version}"
issuer=https://token.actions.githubusercontent.com
verification_dir=$(mktemp -d)

cleanup() {
  rm -rf "$verification_dir"
}
trap cleanup EXIT HUP INT TERM

for target in x86_64-unknown-linux-musl aarch64-unknown-linux-musl; do
  compressed="codex-${target}.zst"
  binary="codex-${target}"
  bundle="${binary}.sigstore"
  curl --fail --location --silent --show-error "$release_url/$compressed" --output "$verification_dir/$compressed"
  curl --fail --location --silent --show-error "$release_url/$bundle" --output "$verification_dir/$bundle"
  (
    cd "$verification_dir"
    grep "  ${compressed}$" "$repo_root/build/codex/checksums.txt" | sha256sum --check -
    zstd --decompress --quiet "$compressed" -o "$binary"
    cosign verify-blob \
      --bundle "$bundle" \
      --certificate-identity "$identity" \
      --certificate-oidc-issuer "$issuer" \
      "$binary"
  )
done

printf 'Codex %s release provenance is valid for amd64 and arm64.\n' "$version"
