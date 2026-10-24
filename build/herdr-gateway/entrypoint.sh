#!/bin/bash
set -euo pipefail
umask 077

# Only runtime sockets and the mode-restricted host key live outside the PVC.
mkdir -p "$XDG_RUNTIME_DIR" "$HOME/.ssh" "$HOME/.config" "$HOME/.local/state" "$HERDR_PLUGIN_STATE_DIR"
chmod 0700 "$HOME" "$XDG_RUNTIME_DIR" "$HOME/.ssh" "$HOME/.config" "$HOME/.local/state" "$HERDR_PLUGIN_STATE_DIR"
cp /var/run/secrets/sandherd-herdr/ssh_host_ed25519_key "$XDG_RUNTIME_DIR/ssh_host_ed25519_key"
chmod 0600 "$XDG_RUNTIME_DIR/ssh_host_ed25519_key"
# Secret projection directories fail OpenSSH StrictModes; use the private home.
cp /var/run/secrets/sandherd-herdr/authorized_keys "$HOME/.ssh/authorized_keys"
chmod 0600 "$HOME/.ssh/authorized_keys"
cd "$HOME"
# Refresh registration after image upgrades while retaining plugin state.
herdr plugin link /opt/sandherd >/dev/null

herdr_pid=
sshd_pid=
shutdown() {
    trap - EXIT
    trap '' TERM INT HUP
    # tini reaps descendants and forwards container signals to this process group.
    # Stop both services also when either one exits independently.
    if [[ -n "$sshd_pid" ]]; then kill -TERM "$sshd_pid" 2>/dev/null || true; fi
    if [[ -n "$herdr_pid" ]]; then kill -TERM "$herdr_pid" 2>/dev/null || true; fi
    wait || true
}
trap shutdown EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
trap 'exit 129' HUP

herdr --session default server &
herdr_pid=$!
/usr/sbin/sshd -D -e -f /etc/ssh/sshd_config &
sshd_pid=$!

status=0
wait -n "$herdr_pid" "$sshd_pid" || status=$?
# Neither foreground service should exit during normal operation.
if [[ "$status" -eq 0 ]]; then status=1; fi
exit "$status"
