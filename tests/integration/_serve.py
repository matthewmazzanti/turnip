"""Standalone TCP listener for multi-node / cross-machine orchestration in the NixOS
tests (the world node's egress target, the ingress-DNAT listener in a container netns).

Usage: python3 _serve.py <port> [seconds]   (default: effectively forever)

(probe.py keeps its own inlined listener for single-process container reachability so
it stays path-independent across the dev VM and the tests; this file is for the cases
where the testScript starts a listener as a separate, addressable process.)
"""

import socket
import sys
import time

port = int(sys.argv[1])
deadline = time.time() + (float(sys.argv[2]) if len(sys.argv) > 2 else 1e9)
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", port))
s.listen()
s.settimeout(0.5)
while time.time() < deadline:
    try:
        conn, _ = s.accept()
        conn.close()
    except OSError:
        pass
