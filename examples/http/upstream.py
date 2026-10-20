"""Optional loopback API fixture for the HTTP capability example."""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/stock":
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0 or length > 65536:
            self.send_error(413)
            return
        try:
            params = json.loads(self.rfile.read(length))
            if not isinstance(params["item"], str) or params["channel"] != "onprest":
                raise ValueError("invalid fixture request")
        except (ValueError, KeyError, TypeError):
            self.send_error(400)
            return
        body = json.dumps({"item": params["item"], "available": 7,
                           "internal_note": "not published"}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8090), Handler).serve_forever()
