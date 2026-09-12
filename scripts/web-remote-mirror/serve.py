#!/usr/bin/env python3
"""Static server for the mirrored official web-remote bundle.
Serves local files; lazily fetches+ caches anything missing from the origin CDN.

回车提交版：对 /remote/v4 的 index.html 在「响应时」注入 enter_patch.js
（磁盘文件保持原样，注入发生在每次响应，官方更新后懒拉取的新 HTML 一样会被
注入）。手机浏览器打开时补丁生效；URL 加 &zqfp=0 可临时关掉补丁做 A/B 对照。

用法：
  python3 serve.py 8899                    # 本地验收用（仅 127.0.0.1）
  python3 serve.py 8899 --host 0.0.0.0     # 给手机访问（监听所有网卡）
"""
import http.server, os, socketserver, urllib.request, sys

DOCROOT = os.path.dirname(os.path.abspath(__file__))
ORIGIN = "https://zcode.z.ai"
INDEX_REL = "remote/v4/index.html"
PATCH_PATH = os.path.join(DOCROOT, "enter_patch.js")

with open(PATCH_PATH, encoding="utf-8") as f:
    PATCH = "<script>\n" + f.read() + "\n</script>"


class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **kw):
        super().__init__(*a, directory=DOCROOT, **kw)

    def _lazy_fetch(self, path_only):
        """path_only: URL path without query. Fetch from origin into the docroot."""
        rel = path_only.lstrip("/")
        dst = os.path.join(DOCROOT, rel)
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        try:
            req = urllib.request.Request(ORIGIN + path_only, headers={"User-Agent": "mirror"})
            with urllib.request.urlopen(req, timeout=20) as r, open(dst, "wb") as f:
                f.write(r.read())
            print(f"[cache] fetched {rel}", flush=True)
            return True
        except Exception as e:
            print(f"[cache] MISS {rel}: {e}", flush=True)
            return False

    def _serve_patched_index(self, query):
        dst = os.path.join(DOCROOT, INDEX_REL)
        if not os.path.isfile(dst):
            try:
                req = urllib.request.Request(ORIGIN + "/remote/v4", headers={"User-Agent": "mirror"})
                with urllib.request.urlopen(req, timeout=20) as r:
                    data = r.read()
                os.makedirs(os.path.dirname(dst), exist_ok=True)
                with open(dst, "wb") as f:
                    f.write(data)
                print("[cache] fetched index.html", flush=True)
            except Exception as e:
                print(f"[cache] MISS index: {e}", flush=True)
                self.send_error(404, "missing index")
                return
        with open(dst, "rb") as f:
            html = f.read().decode("utf-8", "replace")
        # &zqfp=0 → A/B 对照，返回原样页面
        if "zqfp=0" not in query and "<!--zqf-enter-patch-->" not in html:
            html = html.replace("<head>", "<head>\n<!--zqf-enter-patch-->\n" + PATCH, 1)
        body = html.encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-cache")  # 补丁更新要立刻生效
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path_only = self.path.split("?", 1)[0]
        if path_only in ("/remote/v4", "/remote/v4/", "/remote/v4/index.html"):
            query = self.path.split("?", 1)[1] if "?" in self.path else ""
            self._serve_patched_index(query)
            return
        return super().do_GET()

    def send_head(self):
        path = self.translate_path(self.path)
        if os.path.isdir(path):
            path = os.path.join(path, "index.html")
        if not os.path.isfile(path) and self.path.startswith("/remote/v4/"):
            if not self._lazy_fetch(self.path.split("?")[0]):
                self.send_error(404, "missing")
                return None
        return super().send_head()


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8899
    host = "127.0.0.1"
    argv = sys.argv[2:]
    if "--host" in argv:
        host = argv[argv.index("--host") + 1]
    with Server((host, port), Handler) as httpd:
        print(f"serving {DOCROOT} on {host}:{port}", flush=True)
        httpd.serve_forever()
