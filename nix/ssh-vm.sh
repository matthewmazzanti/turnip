#!/usr/bin/env bash
# Connect to a running turnip dev VM over its host-forwarded ssh port.
#
# Self-contained: resolves the key relative to this script, so it works from any cwd or terminal
# window (no dev shell needed). The VM must be booted (`just host` / `just world`).
#
# An optional leading `host` or `world` token selects the VM (host :2222, world :2223); without
# it, host. Then an optional user (host defaults to `homelab`, world to `dev`), then a command.
#
# Usage:
#   nix/ssh-vm.sh                     # host, log in as `homelab` (the rootless run user)
#   nix/ssh-vm.sh dev                 # host, log in as `dev` (admin / passwordless sudo)
#   nix/ssh-vm.sh homelab turnip up   # host, run a command instead of an interactive shell
#   nix/ssh-vm.sh world               # world peer, log in as `dev`
#   nix/ssh-vm.sh world dev ip -br a  # world peer, run a command
#
# Host-key checking is disabled because a VM regenerates its key on every fresh disk
# (`just host-fresh` / `world-fresh`), which would otherwise trip known_hosts.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

port=2222
default_user=homelab
if [ "${1:-}" = host ] || [ "${1:-}" = world ]; then
  if [ "$1" = world ]; then port=2223; default_user=dev; fi
  shift
fi

user=${1:-$default_user}
exec ssh -i "$here/vm/testvm_key" -p "$port" \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "$user@localhost" "${@:2}"
