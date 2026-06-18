#!/usr/bin/env bash
# Connect to the running turnip dev VM over the host-forwarded ssh port (2222).
#
# Self-contained: resolves the key relative to this script, so it works from any
# cwd or terminal window (no dev shell needed). The VM must be booted (`just vm`).
#
# Usage:
#   nix/ssh-vm.sh                 # log in as `homelab` (the rootless run user)
#   nix/ssh-vm.sh dev             # log in as `dev` (admin / passwordless sudo)
#   nix/ssh-vm.sh homelab turnip up   # run a command instead of an interactive shell
#
# Host-key checking is disabled because the VM regenerates its key on every fresh
# disk (`just vm-fresh`), which would otherwise trip known_hosts.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
user=${1:-homelab}
exec ssh -i "$here/testvm_key" -p 2222 \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "$user@localhost" "${@:2}"
