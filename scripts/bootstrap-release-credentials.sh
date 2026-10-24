#!/bin/sh
set -eu
umask 077
context=${KUBE_CONTEXT:?Set KUBE_CONTEXT explicitly}
namespace=${SANDHERD_NAMESPACE:?Set SANDHERD_NAMESPACE to a dedicated namespace}
token_output=${SANDHERD_TOKEN_FILE:?Set SANDHERD_TOKEN_FILE to a new private file}
principal=${SANDHERD_PRINCIPAL_ID:-operator}
kubectl_bin=${KUBECTL:-kubectl}
if [ -e "$token_output" ]; then
  printf '%s\n' 'Refusing to overwrite an existing client token.' >&2
  exit 2
fi
for object in secret/sandherd-api-principals secret/sandherd-runner-capability-signing-key configmap/sandherd-runner-capability-key; do
  found=$("$kubectl_bin" --context "$context" -n "$namespace" get "$object" --ignore-not-found -o name)
  if [ -n "$found" ]; then
    printf 'Refusing to rotate existing credential resource: %s\n' "$object" >&2
    exit 2
  fi
done
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
python3 - "$scratch" "$principal" <<'PY'
import json,pathlib,secrets,sys
folder=pathlib.Path(sys.argv[1])
token=secrets.token_urlsafe(48)
(folder/'api-token').write_text(token+'\n')
(folder/'principals.json').write_text(json.dumps({'version':1,'principals':[{'id':sys.argv[2],'token':token,'permissions':['control'],'credentialProfiles':['chatgpt-subscription']}]}))
PY
openssl genpkey -algorithm ED25519 -out "$scratch/private-key.pem"
openssl pkey -in "$scratch/private-key.pem" -pubout -out "$scratch/public-key.pem"
"$kubectl_bin" --context "$context" -n "$namespace" create secret generic sandherd-api-principals --from-file="principals.json=$scratch/principals.json"
"$kubectl_bin" --context "$context" -n "$namespace" create secret generic sandherd-runner-capability-signing-key --from-file="private-key.pem=$scratch/private-key.pem"
"$kubectl_bin" --context "$context" -n "$namespace" create configmap sandherd-runner-capability-key --from-file="public-key.pem=$scratch/public-key.pem"
# Exclusive creation prevents an accidental overwrite even if another process
# creates the destination after the initial check. Never print the token.
python3 - "$scratch/api-token" "$token_output" <<'PY'
import os,pathlib,sys
contents=pathlib.Path(sys.argv[1]).read_bytes()
fd=os.open(sys.argv[2],os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
with os.fdopen(fd,'wb') as output:output.write(contents)
PY
printf '%s\n' 'Credentials provisioned; the client token was written with mode 0600.'
