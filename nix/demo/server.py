# server.py -- the per-container HTTP server for the turnip demo. Answers every request with
# "hello from <name>", where <name> is the container name passed as argv[1] (set per container in
# homelab.nix). It's bind-mounted into each container and run as `python3 /srv/server.py <name>`, so
# sys.argv[1] is the name.
#
# Deliberately PLAIN HTTP on port 80 -- no TLS. The point of the demo is the L3 flow matrix, not the
# transport; serving on :80 (and curling http://) keeps it obvious there's no SSL in play. Only the
# stdlib is used, so any python3 image works. Container root holds CAP_NET_BIND_SERVICE for :80.
import http.server
import sys

name = sys.argv[1]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(f"hello from {name}\n".encode())


http.server.HTTPServer(("0.0.0.0", 80), Handler).serve_forever()
