#!/usr/bin/env python3
"""Outage switch for the tests: the emulator cannot run docker, so it asks
the host. GET /relay/down stops the relay container, /relay/up starts it.
GET /socks/log is what socks.py saw."""
import http.server, subprocess, sys, os

HERE = os.path.dirname(os.path.abspath(__file__))

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/socks/log":
            self.send_response(200); self.end_headers()
            with open(os.path.join(HERE, "data", "socks.log"), "rb") as f:
                self.wfile.write(f.read())
            return
        cmd = {"/relay/down": "stop", "/relay/up": "start"}.get(self.path)
        if cmd is None:
            self.send_response(404); self.end_headers(); return
        r = subprocess.run(["docker", "compose", cmd, "relay"], cwd=HERE, capture_output=True, text=True)
        self.send_response(200 if r.returncode == 0 else 500)
        self.end_headers()
        self.wfile.write((r.stdout + r.stderr).encode())
    def log_message(self, *a): pass

http.server.HTTPServer(("127.0.0.1", int(sys.argv[1]) if len(sys.argv) > 1 else 9099), H).serve_forever()
