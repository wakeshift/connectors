"""One conversation's stream link, held from this machine (outbound only).

Server-Sent Events first (GET /v1/stream?format=sse), long poll as the fallback
(GET /v1/stream/poll), both with the link's token as ``Authorization: Bearer``
(never in a URL). Either way a delivery is acknowledged with
``POST /v1/stream/ack`` only once Hermes has accepted it (``deliver`` returned
True); what isn't acknowledged comes back on reconnect (at least once).
Deliveries already accepted are only acknowledged again, never handed over twice.
Rotation (hub.rotate, a poll's rotate) moves to the next link; hub.closed and a
401 end it. The token is never logged.
"""
from __future__ import annotations

import json
import logging
import threading
import time
import urllib.error
import urllib.request
from typing import Callable, Optional

from .wire import CLIENT_HEADER, client_header, link_parts

log = logging.getLogger("wakeshift")

# Reasons a stream ends for good.
TERMINAL = {"revoked", "expired", "forgotten", "removed", "unsubscribed", "closed", "ttl", "once"}


class Gone(Exception):
    """The link was revoked or expired."""


class Replaced(Exception):
    """A newer connection or poll took the stream."""


class Unavailable(Exception):
    """This transport didn't work here (e.g. a proxy or a server without it)."""


class Outdated(Exception):
    """Wakeshift no longer serves this version of the plugin (426
    update_required): it hands it nothing until the user updates it. The link
    is kept for the updated plugin."""


# The longest Retry-After honored.
RETRY_AFTER_MAX = 120.0

# The most delivery IDs one ack may carry (Wakeshift refuses more), and
# so the most handed to deliver at once.
MAX_ACKS = 100
MAX_BATCH = MAX_ACKS

# The longest SSE line and event read; a longer one is skipped.
MAX_LINE = 1 << 20
MAX_EVENT = 4 << 20

# A connection that ended this soon without a delivery counts as a failure
# for the backoff (a proxy that closes at once, a repeated hub.closed).
HEALTHY_AFTER = 10.0


class Busy(Exception):
    """Wakeshift refused for now (503 or 429): try again no sooner than after seconds."""

    def __init__(self, after: float):
        super().__init__(f"busy; retry after {after:g}s")
        self.after = after


def _retry_after(e: urllib.error.HTTPError) -> float:
    try:
        n = float(e.headers.get("Retry-After") or 0)
    except (TypeError, ValueError):
        return 0.0
    return min(max(n, 0.0), RETRY_AFTER_MAX)


class _NoRedirects(urllib.request.HTTPRedirectHandler):
    """Refuses every redirect: urllib's default handler sends the request's
    headers (the link's token as Authorization) to wherever a 3xx points, any
    host and even plain http. A redirect raises HTTPError instead."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


# What every request of the plugin's goes through (Wakeshift, the issuer's
# keys, Hermes's API server): Wakeshift never redirects these.
opener = urllib.request.build_opener(_NoRedirects)


def _gone(e: urllib.error.HTTPError) -> bool:
    """Whether a refusal means the link itself is finished: a 401 or 410, or
    Wakeshift's link_revoked. Any other 403 or 404 (a router during a deploy,
    a firewall) is only retried."""
    if e.code in (401, 410):
        return True
    if e.code not in (403, 404):
        return False
    try:
        body = json.loads(e.read(4096) or b"{}")
    except Exception:
        return False
    err = body.get("error") if isinstance(body, dict) else None
    return isinstance(err, dict) and err.get("code") == "link_revoked"


def _http(method: str, url: str, token: str, body: Optional[dict] = None, timeout: float = 30,
          headers: Optional[dict] = None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", "Bearer " + token)
    req.add_header("User-Agent", "wakeshift-hermes/1")
    req.add_header(CLIENT_HEADER, client_header())
    if data is not None:
        req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    return opener.open(req, timeout=timeout)


class Listener:
    """Holds one stream link and hands its deliveries to ``deliver``.

    deliver(deliveries) -> the IDs among them that are done (Hermes accepted
    them, now or before, and that's recorded durably): only those are
    acknowledged, and the rest are offered again later. on_link(url) saves a
    rotated link; on_gone() forgets it. After stop() nothing more is handed
    over or acknowledged."""

    def __init__(self, link: str, deliver: Callable[[list], list], *, on_link: Callable[[str], None] = lambda u: None,
                 on_gone: Callable[[], None] = lambda: None, spawn: Callable = None, poll_wait: float = 25,
                 sse_timeout: float = 90, retry_sse_after: float = 600, backoff_max: float = 30,
                 healthy_after: float = HEALTHY_AFTER):
        self.origin, self.token = link_parts(link)
        self.deliver = deliver
        self.on_link = on_link
        self.on_gone = on_gone
        self.poll_wait = poll_wait
        self.sse_timeout = sse_timeout
        self.retry_sse_after = retry_sse_after
        self.backoff_max = backoff_max
        self.healthy_after = healthy_after
        self._progress = False  # a delivery arrived on the current connection or poll
        self._spawn = spawn or (lambda target, name: threading.Thread(target=target, name=name, daemon=True))
        self._stop = threading.Event()
        self._lock = threading.Condition()
        self._pending: dict[str, dict] = {}  # delivery ID -> delivery, in arrival order
        self._threads: list = []
        self._resp = None
        self.last_event_id = ""
        self.mode = "sse"
        self.state = "starting"  # for tests and logs: sse, poll, stopped, gone, replaced

    # -- lifecycle --
    def start(self) -> "Listener":
        self._threads = [self._spawn(self._read, "wakeshift-stream"), self._spawn(self._work, "wakeshift-deliver")]
        for t in self._threads:
            t.start()
        return self

    def join(self, timeout: float) -> bool:
        """Waits for its threads to end (after stop); whether they did."""
        end = time.monotonic() + timeout
        for t in self._threads:
            if hasattr(t, "join"):
                t.join(max(0.0, end - time.monotonic()))
        return not any(getattr(t, "is_alive", lambda: False)() for t in self._threads)

    def stop(self) -> None:
        self._stop.set()
        with self._lock:
            self._lock.notify_all()
        r = self._resp
        if r is not None:
            try:
                r.close()
            except Exception:
                pass

    @property
    def stopped(self) -> bool:
        return self._stop.is_set()

    def _end(self, state: str) -> None:
        self.state = state
        if state == "gone":
            try:
                self.on_gone()
            except Exception:
                log.debug("wakeshift: forgetting the link failed", exc_info=True)
        self.stop()

    def _rotate(self, link: str) -> None:
        try:
            origin, token = link_parts(link)
        except ValueError:
            return
        if origin != self.origin:
            return  # only ever the same Wakeshift
        self.token = token
        try:
            self.on_link(link)
        except Exception:
            log.debug("wakeshift: saving the rotated link failed", exc_info=True)

    # -- reading --
    def _read(self) -> None:
        backoff = 1.0
        sse_again = 0.0
        while not self.stopped:
            try:
                if self.mode == "poll" and time.monotonic() >= sse_again:
                    self.mode = "sse"
                self._progress = False
                began = time.monotonic()
                if self.mode == "sse":
                    self._sse()
                    healthy = self.healthy_after
                else:
                    self._poll()
                    healthy = min(self.healthy_after, self.poll_wait / 2)
                if self._progress or time.monotonic() - began >= healthy:
                    backoff = 1.0
                    continue
                # Ended at once with nothing delivered: back off, or every
                # conversation reconnects in a tight loop.
            except Gone:
                return self._end("gone")
            except Replaced:
                return self._end("replaced")
            except Outdated:
                log.info("wakeshift: Wakeshift no longer serves this version of the plugin; update it to receive messages")
                return self._end("outdated")
            except Unavailable:
                if self.mode == "sse":
                    log.info("wakeshift: no event stream here, long polling instead")
                    self.mode = "poll"
                    sse_again = time.monotonic() + self.retry_sse_after
                    continue
            except Busy as e:  # at capacity: come back when it says
                self._stop.wait(max(backoff, e.after))
                backoff = min(backoff * 2, self.backoff_max)
                continue
            except Exception as e:  # unreachable, a dropped connection: try again
                if self.stopped:
                    return
                log.debug("wakeshift: stream error: %s", type(e).__name__)
            self._stop.wait(backoff)
            backoff = min(backoff * 2, self.backoff_max)

    def _status(self, e: urllib.error.HTTPError) -> Exception:
        if e.code == 426:
            return Outdated()
        if _gone(e):
            return Gone()
        if e.code == 409:
            return Replaced()
        if e.code in (429, 503):
            return Busy(_retry_after(e))
        return e

    def _sse(self) -> None:
        headers = {"Accept": "text/event-stream"}
        if self.last_event_id:
            headers["Last-Event-ID"] = self.last_event_id
        try:
            resp = _http("GET", self.origin + "/v1/stream?format=sse", self.token, timeout=self.sse_timeout, headers=headers)
        except urllib.error.HTTPError as e:
            if e.code in (401, 403, 404):
                # Revoked, or a stream that doesn't take the token in a header:
                # the poll (which always does) tells which. The server checks
                # a link the same way for both, so a poll that succeeds proves
                # the link is good and only SSE is in the way. Gone and Replaced
                # stand; anything else (a 5xx, unreachable) is the poll's to
                # retry, with its backoff.
                try:
                    self._poll(wait=0)
                except (Gone, Replaced, Outdated):
                    raise
                except Exception:
                    pass
                raise Unavailable()
            if e.code in (400, 405, 406, 415):
                raise Unavailable()
            raise self._status(e)
        if "text/event-stream" not in (resp.headers.get("Content-Type") or ""):
            resp.close()
            raise Unavailable()
        self._resp = resp
        self.state = "sse"
        ev: dict[str, str] = {}
        data: list[str] = []
        size, skip = 0, False
        try:
            while not self.stopped:
                raw = resp.readline(MAX_LINE)
                if not raw:
                    return  # ended: reconnect
                if len(raw) >= MAX_LINE and not raw.endswith(b"\n"):
                    while raw and not raw.endswith(b"\n"):  # too long: skip the rest of the line
                        raw = resp.readline(MAX_LINE)
                    log.warning("wakeshift: skipped an over-long stream line")
                    continue
                line = raw.decode("utf-8", "replace").rstrip("\r\n")
                if line == "":
                    skip, size = False, 0
                    if data:
                        self._event(ev.get("event", "message"), ev.get("id", ""), "\n".join(data))
                    ev, data = {}, []
                    continue
                if line.startswith(":"):
                    continue
                k, _, v = line.partition(":")
                v = v[1:] if v.startswith(" ") else v
                if k == "data":
                    size += len(v)
                    if size > MAX_EVENT:
                        log.warning("wakeshift: skipped an over-long stream event")
                        ev, data, size = {}, [], 0
                        skip = True
                        continue
                    if not skip:
                        data.append(v)
                else:
                    ev[k] = v
        finally:
            self._resp = None
            resp.close()

    def _event(self, typ: str, eid: str, raw: str) -> None:
        try:
            frame = json.loads(raw)
        except ValueError:
            return
        if not isinstance(frame, dict):
            return
        typ = frame.get("type") or typ
        if typ == "event" and isinstance(frame.get("delivery_id"), str):
            if eid:
                self.last_event_id = eid
            self._progress = True
            self._add([frame])
        elif typ == "hub.rotate":
            link = frame.get("link")
            if isinstance(link, str) and link:
                self._rotate(f"{self.origin.replace('https://', 'wss://').replace('http://', 'ws://')}/v1/stream?link={link}")
        elif typ == "hub.closed":
            reason = frame.get("reason")
            if reason == "replaced":
                raise Replaced()
            if reason == "update_required":  # the minimum was raised: kept for the updated plugin
                raise Outdated()
            if reason in TERMINAL:
                raise Gone()

    def _poll(self, wait: Optional[float] = None) -> None:
        wait = self.poll_wait if wait is None else wait
        if wait > 0:
            with self._lock:  # what the last poll returned is handed over first, or it comes back
                end = time.monotonic() + 600
                while self._pending and not self.stopped and time.monotonic() < end:
                    self._lock.wait(1)
            if self.stopped:
                return
        try:
            resp = _http("GET", f"{self.origin}/v1/stream/poll?wait={int(wait * 1000)}ms", self.token, timeout=wait + 10)
        except urllib.error.HTTPError as e:
            raise self._status(e)
        self.state = "poll" if wait > 0 else self.state
        with resp:
            out = json.loads(resp.read(8 << 20) or b"{}")
        rot = out.get("rotate") if isinstance(out, dict) else None
        if isinstance(rot, dict) and isinstance(rot.get("url"), str):
            self._rotate(rot["url"])
        ds = [d for d in (out.get("deliveries") or []) if isinstance(d, dict) and isinstance(d.get("delivery_id"), str)]
        if ds and not self.stopped:
            self._progress = True
            self._add(ds)

    # -- handing over --
    def _add(self, ds: list) -> None:
        with self._lock:
            for d in ds:
                self._pending.setdefault(d["delivery_id"], d)
            self._lock.notify_all()

    def _work(self) -> None:
        """Hands what's pending to deliver, and acknowledges what it says is
        done, retrying the ack with backoff; stop() ends it between any two
        steps."""
        backoff = 1.0
        while not self.stopped:
            with self._lock:
                while not self._pending and not self.stopped:
                    self._lock.wait(5)
                if self.stopped:
                    return
                batch = list(self._pending.values())[:MAX_BATCH]
            try:
                done = [i for i in self.deliver(batch) or [] if isinstance(i, str)]
            except Exception:
                log.warning("wakeshift: handing a message to Hermes failed", exc_info=True)
                done = []
            todo = done
            while todo and not self.stopped:
                todo = self._ack(todo)
                if todo:
                    self._stop.wait(backoff)  # done is recorded, so a redelivery is only acknowledged
                    backoff = min(backoff * 2, self.backoff_max)
            if self.stopped:
                return
            with self._lock:
                for i in done:
                    self._pending.pop(i, None)
                left = bool(self._pending)
                self._lock.notify_all()
            if done:
                backoff = 1.0
            if left and not done:
                self._stop.wait(backoff)  # not taken (busy, refused): offer it again later
                backoff = min(backoff * 2, self.backoff_max)

    def _ack(self, ids: list) -> list:
        """Acknowledges ids, at most MAX_ACKS a request; the ones to try again.
        A batch Wakeshift refuses as bad (400) is split until the ID it won't
        take is found, and that one is dropped rather than retried forever."""
        todo = [ids[i:i + MAX_ACKS] for i in range(0, len(ids), MAX_ACKS)]
        retry: list = []
        while todo:
            chunk = todo.pop(0)
            if self.stopped:
                return []
            got = self._ack_once(chunk)
            if got == "retry":
                retry += chunk
            elif got == "bad":
                if len(chunk) > 1:
                    h = len(chunk) // 2
                    todo[:0] = [chunk[:h], chunk[h:]]
                else:
                    log.warning("wakeshift: Wakeshift refused to acknowledge a delivery; leaving it")
        return retry

    def _ack_once(self, ids: list) -> str:
        try:
            with _http("POST", self.origin + "/v1/stream/ack", self.token, {"delivery_ids": ids}, timeout=5):
                return "ok"
        except urllib.error.HTTPError as e:
            if _gone(e):
                self._end("gone")
            elif e.code == 400:
                return "bad"
            return "retry"
        except Exception:
            return "retry"
