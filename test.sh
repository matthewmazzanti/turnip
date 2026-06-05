#!/usr/bin/env bash
set -xeuo pipefail

export NETNS_DIR="$HOME/netns"
export BRIDGE="br-iot"
export BRIDGE_IP="10.88.0.1/24"

# --- Nix-friendly tool resolution --------------------------------------------
# podman unshare resets PATH, and on Nix there's no /usr/sbin etc -- ip/nsenter
# live in the Nix store (surfaced via /run/current-system/sw/bin or
# ~/.nix-profile/bin). Resolve absolute paths HERE (PATH still intact) and call
# the binaries by absolute path inside the unshare, so PATH is irrelevant there.
export IP="$(readlink -f $(command -v ip))"
export NSENTER="$(readlink -f $(command -v nsenter))"
export UNSHARE="$(readlink -f $(command -v unshare))"
export SLEEP="$(readlink $(command -v sleep))"
export BASH="$(readlink -f $(command -v bash))"
: "${IP:?ip not found in PATH}"
: "${NSENTER:?nsenter not found in PATH}"
: "${UNSHARE:?unshare not found in PATH}"
: "${SLEEP:?sleep not found in PATH}"
: "${BASH:?bash not found in PATH}"

mkdir -p "$NETNS_DIR"

# Create the hass netns with loopback up.
# (plain `podman unshare` is fine here: this just bind-mounts a fresh netns
#  onto a file -- no host-netns interface manipulation involved.)
nsfile="$NETNS_DIR/hass"
touch "$nsfile"
podman unshare "$UNSHARE" --net="$nsfile" "$IP" link set lo up
echo "created $nsfile"

# Bridge + veth must run in podman's ROOTLESS NETNS, not the host netns.
# In the host netns, `ip link add ... type bridge` returns "Operation not
# permitted" because bridge creation needs CAP_NET_ADMIN in a net namespace
# you actually own. --rootless-netns drops us into that owned namespace.
podman unshare --rootless-netns "$BASH" -s << 'EOF'
set -xeuo pipefail
# No PATH reliance: use the absolute paths resolved outside and exported in.

# create + bring up the bridge
"$IP" link add "$BRIDGE" type bridge
"$IP" addr add "$BRIDGE_IP" dev "$BRIDGE"
"$IP" link set "$BRIDGE" up

# veth pair for hass: v-hass -> bridge, c-hass -> hass netns
"$IP" link add v-hass type veth peer name c-hass
"$IP" link set v-hass master "$BRIDGE"
"$IP" link set v-hass up

# move c-hass into the hass netns via a transient holder pid
"$NSENTER" --net="$NETNS_DIR/hass" "$SLEEP" 60 &
pid=$!
"$IP" link set c-hass netns "$pid"

"$NSENTER" --net="$NETNS_DIR/hass" "$IP" addr add 10.88.0.11/24 dev c-hass
"$NSENTER" --net="$NETNS_DIR/hass" "$IP" link set c-hass up

kill "$pid"
EOF
echo "created bridge $BRIDGE and veth pair for hass (10.88.0.11)"
