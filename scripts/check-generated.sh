#!/bin/sh
set -eu

before=$(mktemp)
after=$(mktemp)

cleanup() {
  rm -f "$before" "$after"
}
trap cleanup EXIT HUP INT TERM

snapshot() {
  git diff --binary HEAD
  git ls-files --others --exclude-standard | while IFS= read -r path; do
    printf 'untracked %s %s\n' "$(git hash-object -- "$path")" "$path"
  done
}

snapshot >"$before"
"${GO:-go}" generate ./...
snapshot >"$after"

if ! cmp -s "$before" "$after"; then
  printf '%s\n' 'go generate changed repository files:' >&2
  diff -u "$before" "$after" >&2 || true
  exit 1
fi
