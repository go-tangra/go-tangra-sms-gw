"""Mock Voicecom/LinkMobility carrier and callback receiver for local sms-gw runs.

Never forwards anything to a real carrier. One process, port 8080 inside the
container (published on host loopback by deploy/compose.yaml):

  POST <any other path>   carrier submission: recorded, answered return_code 0
                          (the legacy cmd/test-linkmobility behaviour); the
                          path /fail answers HTTP 500, /reject return_code 2001
  GET  /_submissions      recorded submissions
  POST /_dlr?request_id=..&status=1
                          deliver a receipt to the submission's callback_url
                          like the carrier (GET, receipt merged into the query,
                          dlr_token kept)
  POST /_cb/<any>         record a client callback (headers incl. signature)
  GET  /_callbacks        recorded callbacks
  GET  /healthz           liveness
"""
import json
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

subs, cbs, lock = [], [], threading.Lock()


class Handler(BaseHTTPRequestHandler):
    def reply(self, code, obj):
        body = (json.dumps(obj) + "\n").encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = urllib.parse.urlparse(self.path).path
        with lock:
            if path == "/_submissions":
                return self.reply(200, subs)
            if path == "/_callbacks":
                return self.reply(200, cbs)
        if path == "/healthz":
            return self.reply(200, {"status": "ok"})
        self.reply(404, {"error": "not found"})

    def do_POST(self):
        u = urllib.parse.urlparse(self.path)
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if u.path.startswith("/_cb"):
            with lock:
                cbs.append({"path": u.path, "headers": {k.lower(): v for k, v in self.headers.items()}, "body": raw.decode(errors="replace")})
            self.send_response(204)
            self.end_headers()
            return
        if u.path == "/_dlr":
            return self.receipt(dict(urllib.parse.parse_qsl(u.query)))
        if u.path.endswith("/fail"):
            return self.reply(500, {"error": "mock carrier failure"})
        try:
            s = json.loads(raw)
        except ValueError:
            return self.reply(200, {"return_code": 3000, "return_message": "Invalid JSON input data"})
        with lock:
            subs.append(s)
        if u.path.endswith("/reject"):
            return self.reply(200, {"return_code": 2001, "return_message": "Invalid SID"})
        self.reply(200, {"return_code": 0, "return_message": "Message accepted",
                         "channels": {"sms": {"send_order": 1, "message_parts": 1}}})

    def receipt(self, q):
        with lock:
            sub = next((s for s in subs if s.get("request_id") == q.get("request_id")), None)
        if not sub or not sub.get("callback_url"):
            return self.reply(404, {"error": "unknown request_id or no callback_url"})
        cb = urllib.parse.urlparse(sub["callback_url"])
        cq = dict(urllib.parse.parse_qsl(cb.query))
        cq.update({"request_id": sub["request_id"], "channel": "sms", "sid": str(sub.get("sid", "")),
                   "message_status": q.get("status", "1"), "to": str(sub.get("to", "")),
                   "from": sub.get("sms", {}).get("from", ""), "timestamp": str(int(time.time()))})
        url = urllib.parse.urlunparse(cb._replace(query=urllib.parse.urlencode(cq)))
        try:
            with urllib.request.urlopen(url, timeout=10) as r:
                self.reply(200, {"status": r.status, "body": r.read().decode()})
        except urllib.error.HTTPError as e:
            self.reply(200, {"status": e.code, "body": e.read().decode()})
        except OSError as e:
            self.reply(502, {"error": str(e)})

    def log_message(self, *a):
        pass


ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
