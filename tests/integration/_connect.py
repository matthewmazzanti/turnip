"""Standalone TCP connect probe: exit 0 if <ip>:<port> accepts, 1 otherwise.

Usage: python3 _connect.py <ip> <port> [timeout]

Used by the testScript for connects originating ON a peer node (e.g. the `world` node
hitting the host's published port to test ingress DNAT) -- where probe.connects (which
enters a turnip container netns) doesn't apply.
"""

import socket
import sys

timeout = float(sys.argv[3]) if len(sys.argv) > 3 else 3.0
try:
    socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=timeout).close()
except OSError:
    sys.exit(1)
