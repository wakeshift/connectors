"""A stand-in Wakeshift (stream link endpoints, redeem, JWKS) and a stand-in
Hermes API server, for the plugin's tests."""
from __future__ import annotations

import base64
import json
import queue
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))


def b64(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


class Keys:
    def __init__(self, kid="k1"):
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        self.kid = kid
        self.key = Ed25519PrivateKey.generate()
        self.pub = self.key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)

    def jwks(self) -> bytes:
        return json.dumps({"keys": [{"kid": self.kid, "kty": "OKP", "crv": "Ed25519", "x": b64(self.pub)}]}).encode()

    def sign(self, claims: dict, typ="wakeshift-connector+jwt", alg="EdDSA") -> str:
        h = b64(json.dumps({"alg": alg, "typ": typ, "kid": self.kid}).encode())
        c = b64(json.dumps(claims).encode())
        return h + "." + c + "." + b64(self.key.sign((h + "." + c).encode()))


class Wakeshift:
    """Stream link endpoints for one token at a time, plus redeem and JWKS.

    Queue frames for the SSE stream with sse.put(frame) (None ends the
    connection); the poll returns what's in poll_batches."""

    def __init__(self, sse_takes_bearer=True):
        self.keys = Keys()
        self.tokens = {"tok1"}
        self.sse_takes_bearer = sse_takes_bearer
        self.sse: "queue.Queue" = queue.Queue()
        self.poll_batches: list = []
        self.acked: list = []
        self.ack_fail = 0  # this many acks answer 503
        self.ack_sizes: list = []  # how many IDs each ack carried
        self.ack_bad: set = set()  # an ack carrying any of these answers 400
        self.refuse = 0  # this many stream and poll requests answer refuse_code (a router, a firewall)
        self.refuse_code = 404
        self.sse_end_at_once = False  # each SSE connection ends right after it opens
        self.sse_busy = 0  # this many SSE connections answer 503 with Retry-After: retry_after
        self.retry_after = "1"
        self.sse_times: list = []  # when each SSE connection came (time.monotonic)
        self.requests: list = []
        self.outdated = False  # stream and poll requests answer 426 update_required
        self.redeems: list = []
        self.redeem_answer = (200, {"url": "", "expires_at": "x", "session": "mcp#1"})
        self.connected = threading.Event()
        outer = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def _auth(self):
                a = self.headers.get("Authorization", "")
                return a[len("Bearer "):] if a.startswith("Bearer ") else ""

            def _json(self, code, body):
                b = json.dumps(body).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(b)))
                self.end_headers()
                self.wfile.write(b)

            def do_GET(self):
                outer.requests.append(("GET", self.path, dict(self.headers)))
                if self.path == "/oauth/jwks":
                    b = outer.keys.jwks()
                    self.send_response(200)
                    self.send_header("Content-Length", str(len(b)))
                    self.end_headers()
                    self.wfile.write(b)
                    return
                if self.path.startswith("/v1/stream") and outer.outdated:
                    return self._json(426, {"error": {"code": "update_required", "message": "Wakeshift's hooks for Hermes Agent are out of date."}})
                if self.path.startswith("/v1/stream") and outer.refuse > 0:
                    outer.refuse -= 1
                    return self._json(outer.refuse_code, {"error": {"code": "not_found"}})
                if self.path.startswith("/v1/stream/poll"):
                    if self._auth() not in outer.tokens:
                        return self._json(401, {"error": {"code": "link_revoked"}})
                    batch = outer.poll_batches.pop(0) if outer.poll_batches else {"deliveries": []}
                    if batch in (409, 503):
                        return self._json(batch, {"error": {"code": "replaced" if batch == 409 else "unavailable"}})
                    if not batch.get("deliveries"):
                        time.sleep(0.05)
                    return self._json(200, batch)
                if self.path.startswith("/v1/stream"):
                    if "link=" in self.path or not outer.sse_takes_bearer or self._auth() not in outer.tokens:
                        return self._json(401, {"error": {"code": "invalid_link"}})
                    outer.sse_times.append(time.monotonic())
                    if outer.sse_busy > 0:
                        outer.sse_busy -= 1
                        b = b'{"error":{"code":"at_capacity"}}'
                        self.send_response(503)
                        self.send_header("Retry-After", outer.retry_after)
                        self.send_header("Content-Length", str(len(b)))
                        self.end_headers()
                        self.wfile.write(b)
                        return
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream; charset=utf-8")
                    self.end_headers()
                    self.wfile.write(b"retry: 3000\n\n")
                    self.wfile.flush()
                    outer.connected.set()
                    if outer.sse_end_at_once:
                        self.close_connection = True
                        return
                    while True:
                        try:
                            f = outer.sse.get(timeout=0.2)
                        except queue.Empty:
                            try:
                                self.wfile.write(b": keepalive\n\n")
                                self.wfile.flush()
                            except OSError:
                                return
                            continue
                        if f is None:
                            outer.connected.clear()
                            self.close_connection = True
                            return
                        typ = f.get("type", "message")
                        head = f"id: {f['delivery_id']}\n" if f.get("delivery_id") else ""
                        try:
                            self.wfile.write(b": keepalive\n\n")  # finds a client that's gone first
                            self.wfile.flush()
                            time.sleep(0.05)
                            self.wfile.write(b": keepalive\n\n")
                            self.wfile.flush()
                            self.wfile.write((head + f"event: {typ}\ndata: {json.dumps(f)}\n\n").encode())
                            self.wfile.flush()
                        except OSError:
                            outer.sse.put(f)  # for the next connection, as the server releases it
                            return
                return self._json(404, {})

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n) or b"{}")
                outer.requests.append(("POST", self.path, dict(self.headers)))
                if self.path == "/v1/stream/ack":
                    if self._auth() not in outer.tokens:
                        return self._json(401, {"error": {"code": "link_revoked"}})
                    if outer.ack_fail > 0:
                        outer.ack_fail -= 1
                        return self._json(503, {"error": {"code": "unavailable"}})
                    ids = body.get("delivery_ids") or []
                    outer.ack_sizes.append(len(ids))
                    if not ids or len(ids) > 100 or outer.ack_bad & set(ids):
                        return self._json(400, {"error": {"code": "bad_request"}})
                    outer.acked.extend(ids)
                    return self._json(200, {"acked": body.get("delivery_ids")})
                if self.path == "/v1/connector/redeem":
                    outer.redeems.append(body)
                    code, out = outer.redeem_answer
                    return self._json(code, out)
                return self._json(404, {})

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.srv.daemon_threads = True
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"
        self.link = f"ws://127.0.0.1:{self.srv.server_address[1]}/v1/stream?link=tok1"
        self.redeem_answer = (200, {"url": self.link})
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def beacon(self, **over) -> str:
        now = int(time.time())
        c = {"iss": self.url, "aud": "wakeshift-connector", "thread": "", "app": "try", "tool": "watch",
             "redeem": self.url + "/v1/connector/redeem", "ticket": "t-1", "iat": now, "exp": now + 60}
        c.update(over)
        return self.keys.sign(c)

    def close(self):
        self.sse.put(None)
        self.srv.shutdown()
        self.srv.server_close()


class HermesAPI:
    """Hermes's API server's POST /v1/runs."""

    def __init__(self, status=202):
        self.runs: list = []
        self.status = status
        outer = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                outer.runs.append({"path": self.path, "headers": dict(self.headers), "body": json.loads(self.rfile.read(n))})
                b = json.dumps({"run_id": "run_1", "status": "started"}).encode()
                self.send_response(outer.status)
                self.send_header("Content-Length", str(len(b)))
                self.end_headers()
                self.wfile.write(b)

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


def until(cond, within=5.0):
    end = time.time() + within
    while time.time() < end:
        if cond():
            return True
        time.sleep(0.02)
    return False


def event(i: str, subject="hello") -> dict:
    return {"type": "event", "delivery_id": i, "attempt": 1, "subscription_id": "s1", "app": "Try",
            "event": {"id": "ev_" + i, "subject": subject, "attrs": {"topic": "u/ada/hook"}, "from": {"kind": "app", "id": "try"}}}
