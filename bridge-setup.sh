#!/usr/bin/env bash
set -xeuo pipefail

IFACE=enp1s0
BRIDGE=br0
GW=172.18.0.1
MASK=20

# Get current IP/gateway from the main interface
SYS_IP=172.18.2.10
BRIDGE_IP=172.18.2.14
VETH_IP=172.18.2.13

# Create the bridge
ip link add "$BRIDGE" type bridge
ip link set "$BRIDGE" up

# Move the physical interface into the bridge
ip addr flush dev "$IFACE"
ip link set "$IFACE" master "$BRIDGE"

# Assign the original IP to the bridge
ip addr add "$ADDR/$MASK" dev "$BRIDGE"
ip route add default via "$GW" dev "$BRIDGE"

# Allocate a second IP on the bridge
ip addr add "$IP/$MASK" dev "$BRIDGE"

# Create a veth pair and attach one end to the bridge
ip link add veth0 type veth peer name veth1
ip link set veth0 master "$BRIDGE"
ip link set veth0 up
ip link set veth1 up

# Assign an IP to the non-bridge end of the veth pair
ip addr add "$VETH_IP/$MASK" dev veth1
