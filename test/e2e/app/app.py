import os, socket, http.server

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            socket.create_connection(("cache", 6379), timeout=1).close()
            cache = "ok"
        except OSError as e:
            cache = "down (%s)" % e
        body = "%s %s | greeting=%s | cache=%s\n" % (
            os.environ.get("APP_VERSION", "?"), socket.gethostname(),
            os.environ.get("GREETING", "unset"), cache)
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(body.encode())
    def log_message(self, fmt, *args):
        print("request", self.path, flush=True)

print("listening on 8000", flush=True)
http.server.ThreadingHTTPServer(("", 8000), H).serve_forever()
