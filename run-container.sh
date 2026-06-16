#!/usr/bin/env bash
set -euo pipefail

# run-container.sh -- start a podman container attached to one of turnip's
# persistent container netns (./netns/containers/<name>/netns), with the
# generated hosts file (./netns/containers/<name>/hosts) bind-mounted to
# /etc/hosts so it can resolve its reachable peers by name.
#
# The container JOINS the existing netns via `--network ns:<path>` instead of
# getting a fresh one, so it inherits that ns's interface + address
# (hass -> 10.0.0.12/32) and its default route via the gateway (10.0.0.1).
# Reachability to the other containers is then governed by the router's nft flow
# matrix: e.g. zwave -> hass and hass -> proxy on tcp/443 (directional).
#
# Usage:
#   ./run-container.sh [netns-name] [image] [-- cmd ...]
#
# Examples:
#   ./run-container.sh                       # hass netns, netshoot shell
#   ./run-container.sh zwave                 # zwave netns
#   ./run-container.sh hass alpine -- sh     # override image + run a shell
#
# Default image is nicolaka/netshoot (curl, nmap, dig, tcpdump, nc, iperf3, ...).
#
# Prereq: bring the network up first (creates ./netns/* + the routed network):
#   uv run turnip up

# Matches runtime.netns_dir's default (the login user's ~/netns); override here
# if your turnip.json sets a different netns_dir.
NETNS_DIR="$HOME/netns"

NS="${1:-hass}"
shift || true
IMAGE="${1:-docker.io/nicolaka/netshoot:latest}"
shift || true

# Anything after `--` is the command to run in the container.
CMD=()
if [[ "${1:-}" == "--" ]]; then
  shift
  CMD=("$@")
fi

NSPATH="$NETNS_DIR/containers/$NS/netns"
HOSTS="$NETNS_DIR/containers/$NS/hosts"

# The netns is a bind-mount living in podman's (pause-process) mount namespace,
# NOT the host mount namespace -- so check for it from inside `podman unshare`,
# and confirm it's a live mount (a leftover empty file is a dead namespace).
if ! podman unshare sh -c "[ -e '$NSPATH' ] && mountpoint -q '$NSPATH'"; then
  echo "error: '$NSPATH' is not a live netns." >&2
  echo "bring the network up first:" >&2
  echo "  uv run turnip up" >&2
  exit 1
fi

set -x
exec podman run --rm -it \
  --network "ns:$NSPATH" \
  -v "$HOSTS:/etc/hosts:ro" \
  --cap-add=net_raw \
  --name "iot-$NS" \
  --hostname "$NS" \
  "$IMAGE" \
  "${CMD[@]}"
