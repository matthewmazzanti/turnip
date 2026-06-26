# server.py -- the per-container HTTP server for the turnip demo. Answers every request with
# "hello from <name>", where <name> is the container name passed as argv[1] (set per container in
# homelab.nix). It's bind-mounted into each container and run as `python3 /srv/server.py <name>`, so
# sys.argv[1] is the name. Binds :443 (container root holds CAP_NET_BIND_SERVICE). Only the stdlib is
# used, so any python3 image works.
import http.server
import sys

name = sys.argv[1]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(f"hello from {name}\n".encode())


http.server.HTTPServer(("0.0.0.0", 443), Handler).serve_forever()
