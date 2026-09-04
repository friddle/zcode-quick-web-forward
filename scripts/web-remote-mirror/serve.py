#!/usr/bin/env python3
"""Static server for the mirrored official web-remote bundle.
Serves local files; lazily fetches+ caches anything missing from the origin CDN."""
import http.server, os, socketserver, urllib.request, sys

DOCROOT = os.path.dirname(os.path.abspath(__file__))
ORIGIN = "https://zcode.z.ai"

class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **kw):
        super().__init__(*a, directory=DOCROOT, **kw)

    def send_head(self):
        path = self.translate_path(self.path)
        if os.path.isdir(path):
            path = os.path.join(path, "index.html")
        if not os.path.isfile(path) and self.path.startswith("/remote/v4/assets/"):
            rel = self.path.split("?")[0].lstrip("/")
            os.makedirs(os.path.dirname(os.path.join(DOCROOT, rel)), exist_ok=True)
            try:
                req = urllib.request.Request(ORIGIN + self.path, headers={"User-Agent": "mirror"})
                with urllib.request.urlopen(req, timeout=20) as r, open(os.path.join(DOCROOT, rel), "wb") as f:
                    f.write(r.read())
                print(f"[cache] fetched {rel}", flush=True)
            except Exception as e:
                print(f"[cache] MISS {rel}: {e}", flush=True)
                self.send_error(404, "missing")
                return None
        return super().send_head()

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8899
    with Server(("127.0.0.1", port), Handler) as httpd:
        print(f"serving {DOCROOT} on :{port}", flush=True)
        httpd.serve_forever()
