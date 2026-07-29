#!/usr/bin/env bash

set -euo pipefail

contract_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if node --version >/dev/null 2>&1 && npx --version >/dev/null 2>&1; then
  node_command=()
elif command -v mise >/dev/null 2>&1; then
  node_command=(mise x node@24.3.0 --)
else
  echo "contract validation requires Node.js 24 or mise with node@24.3.0" >&2
  exit 1
fi

cd "$contract_root"

"${node_command[@]}" npx --yes @redocly/cli@2.41.2 lint api/openapi.yaml --config api/redocly.yaml

"${node_command[@]}" npx --yes --package ajv-cli@5.0.0 --package ajv-formats@3.0.1 ajv validate \
  --spec=draft2020 \
  --strict=false \
  -c ajv-formats \
  -s api/terminal.schema.json \
  -d 'api/examples/terminal/*.json'

for invalid_fixture in api/fixtures/terminal-invalid/*.json; do
  if "${node_command[@]}" npx --yes --package ajv-cli@5.0.0 --package ajv-formats@3.0.1 ajv validate \
    --spec=draft2020 \
    --strict=false \
    -c ajv-formats \
    -s api/terminal.schema.json \
    -d "$invalid_fixture" >/dev/null 2>&1; then
    echo "invalid fixture unexpectedly passed: $invalid_fixture" >&2
    exit 1
  fi
done
