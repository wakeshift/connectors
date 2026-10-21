import threading
import time
import unittest

from fake import Wakeshift, event, until

from wakeshift.stream import Listener


class Stream(unittest.TestCase):
    def setUp(self):
        self.ws = Wakeshift()
        self.handed: list = []
        self.accept = threading.Event()
        self.accept.set()
        self.links: list = []
        self.gone = threading.Event()
        self.done: set = set()

    def tearDown(self):
        self.l.stop()
        self.ws.close()

    def deliver(self, ds):
        # As the plugin does: what's recorded as started is done; the rest
        # start (and are recorded) only if accepted.
        done = [d["delivery_id"] for d in ds if d["delivery_id"] in self.done]
        fresh = [d["delivery_id"] for d in ds if d["delivery_id"] not in self.done]
        if fresh and self.accept.is_set():
            self.handed.append(fresh)
            self.done.update(fresh)
            done += fresh
        return done

    def listen(self, **kw):
        self.l = Listener(self.ws.link, self.deliver, on_link=self.links.append, on_gone=self.gone.set,
                          poll_wait=0.2, backoff_max=0.2, **kw).start()
        return self.l

    def test_sse_acks_only_what_hermes_accepted(self):
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set))
        get = [r for r in self.ws.requests if r[0] == "GET"][0]
        self.assertNotIn("link=", get[1])  # the token goes in a header, never the URL
        self.assertEqual(get[2].get("Authorization"), "Bearer tok1")
        self.accept.clear()
        self.ws.sse.put(event("d1"))
        self.assertTrue(until(lambda: self.l._pending))
        self.assertEqual(self.ws.acked, [])  # not accepted: not acknowledged
        self.accept.set()
        self.assertTrue(until(lambda: self.ws.acked == ["d1"]))
        self.assertEqual(self.handed, [["d1"]])

    def test_reconnect_resumes_and_never_hands_over_twice(self):
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put(event("d1"))
        self.assertTrue(until(lambda: self.ws.acked == ["d1"]))
        self.ws.sse.put(None)  # the connection drops
        self.assertTrue(until(lambda: len([r for r in self.ws.requests if r[1].startswith("/v1/stream?")]) == 2))
        self.assertTrue(until(self.ws.connected.is_set))
        last = [r for r in self.ws.requests if r[1].startswith("/v1/stream?")][-1]
        self.assertEqual({k.lower(): v for k, v in last[2].items()}.get("last-event-id"), "d1")
        # Redelivered (its ack was lost, say): acknowledged again, not handed over again.
        self.done.add("d2")
        self.ws.sse.put(event("d2"))
        self.assertTrue(until(lambda: "d2" in self.ws.acked))
        self.assertEqual(self.handed, [["d1"]])

    def test_rotate_and_close(self):
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.tokens.add("tok2")
        self.ws.sse.put({"type": "hub.rotate", "url": "http://x/v1/stream?format=sse&link=tok2", "link": "tok2"})
        self.ws.sse.put(None)
        self.assertTrue(until(lambda: self.links and self.links[-1].endswith("link=tok2")))
        self.assertTrue(until(lambda: [r for r in self.ws.requests if r[1].startswith("/v1/stream?")][-1][2].get("Authorization") == "Bearer tok2"))
        self.ws.sse.put({"type": "hub.closed", "reason": "revoked", "note": "x"})
        self.assertTrue(until(self.gone.is_set))
        self.assertEqual(self.l.state, "gone")

    def test_requests_name_the_plugin(self):
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put(event("d1"))
        self.assertTrue(until(lambda: self.ws.acked == ["d1"]))
        for method, path, headers in self.ws.requests:
            self.assertEqual(headers.get("X-Wakeshift-Connector"), "wakeshift-hermes/dev hermes", (method, path))
        self.assertEqual({r[1].split("?")[0] for r in self.ws.requests}, {"/v1/stream", "/v1/stream/ack"})

    def test_outdated_stops_without_forgetting(self):
        self.ws.outdated = True
        self.listen()
        self.assertTrue(until(lambda: self.l.state == "outdated"))
        self.assertTrue(self.l.stopped)
        self.assertFalse(self.gone.is_set())  # the updated plugin uses the same link

    def test_closed_as_outdated_stops_without_forgetting(self):
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put({"type": "hub.closed", "reason": "update_required", "note": "out of date"})
        self.assertTrue(until(lambda: self.l.state == "outdated"))
        self.assertFalse(self.gone.is_set())

    def test_replaced_stops_without_forgetting(self):
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put({"type": "hub.closed", "reason": "replaced", "note": "x"})
        self.assertTrue(until(lambda: self.l.state == "replaced"))
        self.assertFalse(self.gone.is_set())

    def test_at_capacity_waits_out_retry_after(self):
        # backoff_max is 0.2s; the 503's Retry-After (1s) is what it waits.
        self.ws.sse_busy, self.ws.retry_after = 1, "1"
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set, 5))
        self.assertEqual(len(self.ws.sse_times), 2)
        self.assertGreaterEqual(self.ws.sse_times[1] - self.ws.sse_times[0], 0.95)
        self.assertEqual(self.l.mode, "sse")  # still SSE: busy isn't a transport that doesn't work

    def test_long_poll_when_sse_doesnt_work(self):
        self.ws.sse_takes_bearer = False  # a Wakeshift whose stream wants ?link=, or a proxy that eats SSE
        self.ws.poll_batches = [{"deliveries": [event("p1")]}, {"deliveries": [], "rotate": {"url": "wss://127.0.0.1:1/v1/stream?link=tok9"}},
                                {"deliveries": [event("p2")]}]
        self.listen()
        self.assertTrue(until(lambda: self.ws.acked == ["p1", "p2"]))
        self.assertEqual(self.l.mode, "poll")
        self.assertEqual(self.handed, [["p1"], ["p2"]])
        self.assertEqual(self.links, [])  # a rotation to another host is ignored
        for r in self.ws.requests:
            self.assertNotIn("link=", r[1])

    def test_a_failing_probe_still_falls_back(self):
        # SSE refuses the header and the poll that would tell why answers 503:
        # long poll anyway (with its backoff), rather than retrying SSE.
        self.ws.sse_takes_bearer = False
        self.ws.poll_batches = [503, {"deliveries": [event("p1")]}]
        self.listen()
        self.assertTrue(until(lambda: self.ws.acked == ["p1"]))
        self.assertEqual(self.l.mode, "poll")
        self.assertEqual(len([r for r in self.ws.requests if r[1].startswith("/v1/stream?")]), 1)

    def test_stop_ends_ack_retries(self):
        # Every ack fails; after stop() the worker ends between retries and
        # nothing more is acknowledged or handed over.
        self.ws.ack_fail = 10**6
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put(event("d1"))
        self.assertTrue(until(lambda: self.ws.ack_fail < 10**6))
        self.l.stop()
        self.assertTrue(self.l.join(3))
        tried = self.ws.ack_fail
        self.ws.sse.put(event("d2"))
        self.assertFalse(until(lambda: self.ws.ack_fail != tried, within=1))
        self.assertEqual(self.handed, [["d1"]])

    def test_revoked_link_is_forgotten(self):
        self.ws.tokens.clear()
        self.listen()
        self.assertTrue(until(self.gone.is_set))
        self.assertEqual(self.handed, [])

    def test_more_than_100_are_acked_in_chunks(self):
        self.accept.clear()
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set))
        self.l._add([event(f"d{i}") for i in range(250)])
        self.accept.set()
        self.assertTrue(until(lambda: len(self.ws.acked) == 250, 10))
        self.assertTrue(all(n <= 100 for n in self.ws.ack_sizes))
        self.assertTrue(all(len(h) <= 100 for h in self.handed))  # deliver gets at most 100 at once

    def test_an_ack_refused_as_bad_is_split_not_retried_forever(self):
        self.ws.ack_bad = {"d3"}
        self.accept.clear()
        self.listen()
        self.assertTrue(until(self.ws.connected.is_set))
        self.l._add([event(f"d{i}") for i in range(6)])
        self.accept.set()
        self.assertTrue(until(lambda: sorted(self.ws.acked) == ["d0", "d1", "d2", "d4", "d5"]))
        self.assertTrue(until(lambda: not self.l._pending))  # d3 dropped, the loop moves on
        self.ws.sse.put(event("d9"))
        self.assertTrue(until(lambda: "d9" in self.ws.acked))

    def test_a_404_or_403_is_retried_not_forgotten(self):
        for code in (404, 403):
            self.ws.refuse, self.ws.refuse_code = 3, code
            self.ws.connected.clear()
            self.listen(retry_sse_after=0)
            self.assertTrue(until(self.ws.connected.is_set))
            self.assertFalse(self.gone.is_set())
            self.l.stop()
            self.l.join(3)

    def test_a_link_revoked_404_is_gone(self):
        import io
        import urllib.error

        from wakeshift.stream import _gone

        def err(code, body):
            return urllib.error.HTTPError("http://x", code, "x", {}, io.BytesIO(body))
        self.assertTrue(_gone(err(401, b"")))
        self.assertTrue(_gone(err(410, b"")))
        self.assertTrue(_gone(err(404, b'{"error": {"code": "link_revoked"}}')))
        self.assertFalse(_gone(err(404, b"<html>not found</html>")))
        self.assertFalse(_gone(err(403, b'{"error": {"code": "forbidden"}}')))
        self.l = Listener(self.ws.link, self.deliver)  # for tearDown

    def test_connections_that_end_at_once_back_off(self):
        self.ws.sse_end_at_once = True
        self.l = Listener(self.ws.link, self.deliver, poll_wait=0.2, backoff_max=0.4).start()
        time.sleep(2)
        self.l.stop()
        n = len(self.ws.sse_times)
        self.assertGreaterEqual(n, 2)
        self.assertLessEqual(n, 8)  # 1s, then 0.4s apart: not a tight loop
        gaps = [b - a for a, b in zip(self.ws.sse_times, self.ws.sse_times[1:])]
        self.assertTrue(all(g >= 0.35 for g in gaps), gaps)

    def test_an_over_long_line_is_skipped(self):
        import wakeshift.stream as st
        old = st.MAX_LINE
        st.MAX_LINE = 4096
        try:
            self.listen()
            self.assertTrue(until(self.ws.connected.is_set))
            self.ws.sse.put(event("big", "x" * 10000))
            self.ws.sse.put(event("d1"))
            self.assertTrue(until(lambda: "d1" in self.ws.acked))
            self.assertNotIn("big", self.ws.acked)
        finally:
            st.MAX_LINE = old


if __name__ == "__main__":
    unittest.main()


class Redirects(unittest.TestCase):
    def test_the_token_never_follows_a_redirect(self):
        # urllib's default handler copies Authorization to wherever a 3xx
        # points (another host, even plain http); the plugin's opener refuses.
        import http.server
        import urllib.error

        from wakeshift import stream

        seen = []

        class Elsewhere(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append(self.headers.get("Authorization"))
                self.send_response(200)
                self.end_headers()

            def log_message(self, *a):
                pass

        other = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Elsewhere)
        target = f"http://127.0.0.1:{other.server_port}/steal"

        class Redirecting(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(307)
                self.send_header("Location", target)
                self.end_headers()

            def log_message(self, *a):
                pass

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Redirecting)
        for s in (other, srv):
            threading.Thread(target=s.serve_forever, daemon=True).start()
        try:
            with self.assertRaises(urllib.error.HTTPError) as e:
                stream._http("GET", f"http://127.0.0.1:{srv.server_port}/v1/stream/poll", "SECRET")
            self.assertEqual(e.exception.code, 307)
            self.assertEqual(seen, [])
        finally:
            for s in (other, srv):
                s.shutdown()
                s.server_close()
