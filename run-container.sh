#!/usr/bin/env bash
set -euo pipefail

# run-container.sh -- start a podman container attached to one of the
# persistent network namespaces created by main.py (./netns/<name>).
#
# The container JOINS the existing netns via `--network ns:<path>` instead of
# getting a fresh one, so it inherits that ns's interface + address
# (hass -> 10.88.0.12) and can reach the bridge (10.88.0.1) and the other
# containers on br-iot.
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
# Prereq: bring the network up first (creates ./netns/* + br-iot):
#   podman unshare ./.venv/bin/python main.py up

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

NSPATH="$NETNS_DIR/$NS"

# The netns is a bind-mount living in podman's (pause-process) mount namespace,
# NOT the host mount namespace -- so check for it from inside `podman unshare`,
# and confirm it's a live mount (a leftover empty file is a dead namespace).
if ! podman unshare sh -c "[ -e '$NSPATH' ] && mountpoint -q '$NSPATH'"; then
  echo "error: '$NSPATH' is not a live netns." >&2
  echo "bring the network up first:" >&2
  echo "  podman unshare ./.venv/bin/python main.py up" >&2
  exit 1
fi

set -x
exec podman run --rm -it \
  --network "ns:$NSPATH" \
  --cap-add=net_raw \
  --name "iot-$NS" \
  --hostname "$NS" \
  "$IMAGE" \
  "${CMD[@]}"
