"""What the plugin does in Hermes, kept apart from Hermes's own modules so it can
be tested without them (``__init__.register`` wires it in).

1. transform_tool_result: an MCP result carrying a beacon has it taken out
   before the model sees it (Hermes shows ``_meta`` to the model). A beacon
   signed by the Wakeshift pinned at install (and for this conversation, or for
   none: Hermes tells MCP servers neither the session nor the call, so Hermes
   handing this session's result to this hook is what binds it) is redeemed for
   the conversation's stream link, and the result gets the note
   "Wakeshift: connected …".
2. A Listener holds the link (SSE, else long poll) and hands each batch to the
   session once it's idle (between turns: pre_llm_call / on_session_end), framed
   as outside data (wire.prompt), never as the user speaking:
   - API server sessions: a new run in the same session (POST /v1/runs with its
     session_id, an Idempotency-Key per batch, and author is_bot), on the
     loopback API server with its own key. Acknowledged once the run is accepted.
   - CLI sessions: ctx.inject_message (the REPL's input queue), framed.
     Acknowledged only once a turn starts after it (pre_llm_call), so a
     message still in the queue when the CLI quits comes back.
   Other platforms (Telegram, Discord…) aren't woken: Hermes's only way to start
   a turn there marks it internal (as if the user's own), which we never use.
3. Approvals stay as Hermes sets them: a woken run is unattended, so what needs
   approval is denied unless the user changed approvals.unattended_mode.
"""
from __future__ import annotations

import contextlib
import hashlib
import json
import logging
import os
import threading
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Callable, Optional

from . import wire
from .stream import Listener, opener

try:
    import fcntl
except ImportError:  # Windows: the in-process lock only
    fcntl = None

log = logging.getLogger("wakeshift")

WHEN = "as soon as they arrive, even while it's idle, as long as Hermes is running"
NOTE_CONNECTED = "Wakeshift: connected. New messages for this conversation will reach it " + WHEN + "."
NOTE_FAILED = ("Wakeshift: couldn't connect this conversation ({}), so new messages won't reach it yet. "
               "To retry, call the app's tool that started watching again.")
NOTE_OUTDATED = "Wakeshift: couldn't connect this conversation. {}"
NOTE_UNSUPPORTED = ("Wakeshift: Hermes can't wake {} conversations yet (only API server and CLI ones), "
                    "so new messages wait until you next use this app's tools.")
NOTE_PINNED = ("Wakeshift: didn't connect this conversation: {}. If that's the server you use for this app "
               "(you renamed or re-added it), remove its line from {} and call the app's tool again.")
NOTE_PIN_FILE = ("Wakeshift: didn't connect this conversation: {} can't be read, so Wakeshift can't check which MCP "
                 "server carries this app's messages. Fix the file, or remove it to start over, and call the app's tool again.")
NOTE_TAKEN = ("Wakeshift: this conversation stays connected where it was: a result asked to connect it to a session of "
              "another account or workspace, and Wakeshift doesn't move a connected conversation. To connect it there, stop "
              "it being woken where it is first (the app's unwatch tool), or start a new conversation.")
# _pin's answer when the pins exist but can't be read.
PINS_UNREADABLE = "the pins can't be read"
AUTHOR = {"id": "wakeshift", "name": "Wakeshift", "is_bot": True}

# A turn that never reports its end stops holding messages back after this.
BUSY_FOR = 35 * 60
# After a woken run is accepted, until its turn starts.
STARTING_FOR = 120
# How long a delivery that started a turn is remembered, so it's only
# acknowledged if it comes back (Hermes's own Idempotency-Key replay lasts a
# day, but only for the same batch and only on the API server).
STARTED_FOR = 7 * 24 * 3600
# Platforms whose waker only queues the message (the CLI's input queue): it's
# done once the next turn in that process starts, not when it's queued.
QUEUES = {"cli"}


class ApiRuns:
    """Starts a run in an API server session: POST /v1/runs on the loopback
    API server, with its key."""

    def __init__(self, port: int, key: Callable[[], str]):
        self.base = f"http://127.0.0.1:{port}"
        self.key = key

    def __call__(self, session: str, text: str, batch: str) -> bool:
        body = json.dumps({"input": text, "session_id": session, "author": AUTHOR}).encode()
        req = urllib.request.Request(self.base + "/v1/runs", data=body, method="POST")
        req.add_header("Content-Type", "application/json")
        req.add_header("Authorization", "Bearer " + (self.key() or ""))
        req.add_header("Idempotency-Key", "wakeshift-" + batch)  # a redelivered batch doesn't run twice
        try:
            with opener.open(req, timeout=30) as r:
                return 200 <= r.status < 300
        except urllib.error.HTTPError as e:
            log.warning("wakeshift: Hermes's API server refused the run (%d)", e.code)
            return False
        except Exception:
            log.warning("wakeshift: Hermes's API server couldn't be reached")
            return False


class Session:
    def __init__(self, sid: str):
        self.id = sid
        self.platform = ""
        self.listener: Optional[Listener] = None
        self.queued: set = set()  # delivery IDs in the input queue, no turn started since


class Wakeshift:
    """The plugin's state in one Hermes process.

    wakers maps a platform ("api_server", "cli") to a function
    (session, framed text, batch id) -> accepted."""

    def __init__(self, *, state_dir: Path, issuers: list[str], wakers: dict, spawn: Callable = None,
                 http_get: Callable = None, now: Callable[[], float] = time.time, listener_args: Optional[dict] = None):
        self.dir = Path(state_dir)
        self.issuers = [i.rstrip("/") for i in issuers if isinstance(i, str)]
        self.wakers = wakers
        self.spawn = spawn or (lambda target, name: threading.Thread(target=target, name=name, daemon=True))
        self.http_get = http_get or _get
        self.now = now
        self.listener_args = listener_args or {}
        self.lock = threading.Condition()
        self.sessions: dict[str, Session] = {}

    # -- files --
    def _link_path(self, sid: str) -> Path:
        return self.dir / "links" / (hashlib.sha256(sid.encode()).hexdigest()[:32] + ".json")

    def _dir(self, d: Path) -> None:
        """Makes a directory of the plugin's state (self.dir or one inside it).
        Only what it creates is made 0700: never a directory that was already
        there, nor self.dir's parents ($HERMES_HOME), which it only creates if
        they're missing, with the default mode."""
        d = Path(d)
        if d != self.dir and self.dir not in d.parents:
            raise OSError("not in the plugin's state directory")
        self.dir.parent.mkdir(parents=True, exist_ok=True)
        for c in [self.dir] + [self.dir.joinpath(*d.relative_to(self.dir).parts[:i + 1])
                              for i in range(len(d.relative_to(self.dir).parts))]:
            try:
                os.mkdir(c, 0o700)
            except FileExistsError:
                continue
            os.chmod(c, 0o700)  # the umask may have taken bits off

    def _write(self, p: Path, data: bytes) -> None:
        """Writes a file only this user can read (it may hold a stream link's
        token): a fresh temp file (O_EXCL, its own name, 0600 before any byte)
        renamed over p, in a 0700 directory."""
        self._dir(p.parent)
        fd, tmp = tempfile.mkstemp(dir=p.parent, prefix="." + p.name + ".")
        try:
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, "wb") as f:
                f.write(data)
                f.flush()
                os.fsync(f.fileno())  # durable before anything relies on it (an ack)
            os.replace(tmp, p)
        except BaseException:
            try:
                os.unlink(tmp)
            except OSError:
                pass
            raise

    def _save_link(self, sid: str, platform: str, link: str) -> None:
        self._write(self._link_path(sid), json.dumps({"session": sid, "platform": platform, "link": link}).encode())

    def _forget_link(self, sid: str) -> None:
        try:
            self._link_path(sid).unlink()
        except FileNotFoundError:
            pass

    def _stored(self) -> list[dict]:
        out = []
        for p in sorted((self.dir / "links").glob("*.json")):
            try:
                d = json.loads(p.read_text())
            except Exception:
                continue
            if isinstance(d, dict) and all(isinstance(d.get(k), str) and d.get(k) for k in ("session", "platform", "link")):
                out.append(d)
        return out

    def _load_link(self, sid: str) -> Optional[dict]:
        try:
            d = json.loads(self._link_path(sid).read_text())
        except Exception:
            return None
        return d if isinstance(d, dict) and d.get("session") == sid else None

    # -- pins (connector.Pins): which app's beacons each MCP server carries --
    def _pins_path(self) -> Path:
        return self.dir / "servers.json"

    def _pin(self, server: str, app: str) -> str:
        """"" if server may carry app's beacons (pinning the pair the first time
        either is seen), else why not."""
        if not server or not app:
            return "the result named no app"
        with self.lock:
            # None yet is fine; pins that exist but can't be read fail closed
            # (read as none, any server could pin itself to any app).
            try:
                raw = self._pins_path().read_text()
            except FileNotFoundError:
                raw = None
            except Exception:
                return PINS_UNREADABLE
            pins: dict = {}
            if raw is not None:
                try:
                    pins = json.loads(raw).get("servers")
                except Exception:
                    return PINS_UNREADABLE
                if not isinstance(pins, dict):
                    return PINS_UNREADABLE
            got = pins.get(server)
            if got == app:
                return ""
            if got:
                return f'the MCP server "{server}" carries beacons for "{got}" here, not "{app}"'
            for s, a in pins.items():
                if a == app:
                    return f'beacons for "{app}" come from the MCP server "{s}" here, not "{server}"'
            pins[server] = app
            self._write(self._pins_path(), json.dumps({"servers": pins}, indent=2).encode())
        return ""

    # -- keys --
    def _key_for(self, issuer: str, kid: str) -> Optional[bytes]:
        """The issuer's Ed25519 key for kid, from a cache refreshed from
        <issuer>/oauth/jwks when the kid isn't known (at most once a minute)."""
        if issuer not in self.issuers:
            return None
        path = self.dir / "keys" / (hashlib.sha256(issuer.encode()).hexdigest()[:16] + ".json")
        k = _key_in(path, kid)
        if k is not None:
            return k
        try:
            if self.now() - path.stat().st_mtime < 60:
                return None
        except FileNotFoundError:
            pass
        body = self.http_get(issuer + "/oauth/jwks")
        if body is None:
            return None
        self._write(path, body)
        return _key_in(path, kid)

    # -- sessions --
    def _session(self, sid: str) -> Session:
        s = self.sessions.get(sid)
        if s is None:
            s = self.sessions[sid] = Session(sid)
        return s

    def turn_started(self, session_id: str = "", platform: str = "", **_) -> None:
        """pre_llm_call: the session is busy; resume its stream if it was connected."""
        if not session_id:
            return None
        with self.lock:
            s = self._session(session_id)
            if platform:
                s.platform = platform
            listening = s.listener is not None and not s.listener.stopped
        self._set_busy(session_id, self.now() + BUSY_FOR, started=True)
        if not listening:
            d = self._load_link(session_id)
            if d and d["platform"] == (platform or d["platform"]) and d["platform"] in self.wakers:
                self._listen(session_id, d["platform"], d["link"])
        return None  # pre_llm_call's return is added to the prompt: nothing

    def turn_ended(self, session_id: str = "", **_) -> None:
        """on_session_end (each turn's end): the session is idle."""
        if not session_id:
            return None
        self._set_busy(session_id, 0.0)
        with self.lock:
            self.lock.notify_all()
        return None

    def transform(self, tool_name: str = "", result=None, session_id: str = "", tool_call_id: str = "", **_) -> Optional[str]:
        """transform_tool_result: take a beacon out and connect the conversation."""
        if not isinstance(result, str) or wire.META_KEY not in result:
            return None
        stripped, token = wire.strip_beacon(result)
        if not token:
            return None
        if not session_id:
            return stripped
        # A beacon proves Wakeshift minted it, not that this result is its
        # app's: one an attacker minted (Hermes tells servers nothing, so it's
        # bound to no conversation) could sit in a page a tool fetched, or in
        # another server's result. Acted on only in an MCP server's own result
        # (its _meta, not a document inside it), from the server pinned to the
        # beacon's app.
        server = wire.mcp_server(tool_name)
        if not server or wire.result_beacon(result) != token:
            return stripped
        try:
            claims = wire.verify_bound(token, conversation=session_id, call=tool_call_id or "", unbound=True,
                                       issuers=self.issuers, key_for=self._key_for, now=self.now())
        except (wire.Untrusted, wire.Invalid):
            return stripped  # not ours to act on; say nothing
        except Exception:
            log.debug("wakeshift: checking a beacon failed", exc_info=True)
            return stripped
        # From the tool it names, where it names one (apps on SDKs from before
        # beacons did name none; this is the result's own _meta all the same).
        if claims.get("tool") and not wire.tool_matches(claims.get("tool"), wire.mcp_tool(tool_name)):
            return stripped
        pinned = self._pin(server, claims.get("app", ""))
        if pinned == PINS_UNREADABLE:
            return wire.add_note(stripped, NOTE_PIN_FILE.format(self._pins_path()))
        if pinned:
            return wire.add_note(stripped, NOTE_PINNED.format(pinned, self._pins_path()))
        with self.lock:
            platform = self._session(session_id).platform
        if platform not in self.wakers:
            return wire.add_note(stripped, NOTE_UNSUPPORTED.format(platform or "these"))
        call = "" if claims.get("thread") else (tool_call_id or "")
        # The link this conversation had until now, if any: handed back, it
        # ends the Wakeshift session it belonged to, so a conversation whose
        # calls started a new one stays one subscription.
        prior = self._load_link(session_id)
        supersedes = prior["link"] if prior and _same_origin(prior["link"], claims.get("iss", "")) else ""
        try:
            link = _redeem(claims, session_id, call, supersedes)
        except Outdated as e:
            return wire.add_note(stripped, NOTE_OUTDATED.format(e))
        except Taken:
            return wire.add_note(stripped, NOTE_TAKEN)  # the link it had stays
        except RedeemError as e:
            return wire.add_note(stripped, NOTE_FAILED.format(e))
        if not _same_origin(link, claims.get("iss", "")):
            # A link on another host would get the token: refused, unused.
            return wire.add_note(stripped, NOTE_FAILED.format("its stream link wasn't on the Wakeshift this plugin trusts"))
        try:
            self._save_link(session_id, platform, link)
        except OSError:
            return wire.add_note(stripped, NOTE_FAILED.format("its link couldn't be saved"))
        self._set_busy(session_id, self.now() + BUSY_FOR)  # this turn is still running
        self._listen(session_id, platform, link)
        return wire.add_note(stripped, NOTE_CONNECTED)

    def resume(self, platforms: tuple = ("api_server",)) -> int:
        """Listen again for every conversation connected before a restart, on
        these platforms (the gateway's; a CLI resumes its own on its next turn)."""
        n = 0
        for d in self._stored():
            if d["platform"] in platforms and d["platform"] in self.wakers:
                with self.lock:
                    self._session(d["session"]).platform = d["platform"]
                # A turn this process had running when it stopped ended with it.
                self._set_busy(d["session"], 0.0)
                self._listen(d["session"], d["platform"], d["link"])
                n += 1
        return n

    def _listen(self, sid: str, platform: str, link: str) -> None:
        with self.lock:
            s = self._session(sid)
            old = s.listener
            l = Listener(link, lambda ds: self._deliver(s, l, ds),
                         on_link=lambda u: self._linked(s, l, platform, u),
                         on_gone=lambda: self._gone(s, l), spawn=self.spawn, **self.listener_args)
            s.listener = l
        if old is not None:
            old.stop()
        l.start()

    def _linked(self, s: Session, l: Listener, platform: str, link: str) -> None:
        """A rotated link, saved only while l is still the session's listener
        (a superseded one must not overwrite the new link)."""
        with self.lock:
            if s.listener is l:
                self._save_link(s.id, platform, link)

    def _gone(self, s: Session, l: Listener) -> None:
        with self.lock:
            if s.listener is l:
                s.listener = None
                self._forget_link(s.id)

    # -- each session's state, owned by one lock --
    # started/<hash>.json holds a connected session's state, shared by every
    # Hermes process on this machine (a gateway, and a `hermes chat --resume`
    # of one of its sessions): busy_until (a turn runs until then) and started
    # (delivery ID -> when it started a turn, kept for STARTED_FOR). It's read
    # and written only under the session's flock (_locked), which every thread
    # and process takes for its own open of started/<hash>.lock; that's also
    # where a turn start decides, reserves, starts and records.
    def _state_path(self, sid: str) -> Path:
        return self.dir / "started" / (hashlib.sha256(sid.encode()).hexdigest()[:32] + ".json")

    @contextlib.contextmanager
    def _locked(self, sid: str):
        p = self._state_path(sid).with_suffix(".lock")
        self._dir(p.parent)
        fd = os.open(p, os.O_RDWR | os.O_CREAT, 0o600)
        try:
            if fcntl is not None:
                fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            os.close(fd)  # releases the flock

    def _state(self, sid: str) -> dict:
        """The session's state (call under _locked)."""
        try:
            d = json.loads(self._state_path(sid).read_text())
        except Exception:
            d = {}
        d = d if isinstance(d, dict) else {}
        now = self.now()
        started = d.get("started") if isinstance(d.get("started"), dict) else {}
        busy = d.get("busy_until")
        return {"busy_until": float(busy) if isinstance(busy, (int, float)) else 0.0,
                "started": {k: v for k, v in started.items() if isinstance(v, (int, float)) and now - v < STARTED_FOR}}

    def _save_state(self, sid: str, st: dict) -> None:
        self._write(self._state_path(sid), json.dumps(st).encode())

    def _busy_hint(self, sid: str) -> bool:
        """Whether the session looks busy, read without the lock: only to know
        when to try; the decision is _deliver's, under the lock."""
        try:
            return self.now() < float(json.loads(self._state_path(sid).read_text()).get("busy_until") or 0)
        except Exception:
            return False

    def _set_busy(self, sid: str, until: float, started: bool = False) -> None:
        """A turn started (or ended) in a connected session. A start also
        records what was waiting in its input queue as started."""
        if not (self._link_path(sid).exists() or self._state_path(sid).exists()):
            return
        try:
            with self._locked(sid):
                st = self._state(sid)
                st["busy_until"] = until
                with self.lock:
                    s = self.sessions.get(sid)
                    queued = set(s.queued) if started and s is not None else set()
                now = self.now()
                st["started"].update({i: now for i in queued})
                self._save_state(sid, st)
                with self.lock:
                    if s is not None:
                        s.queued -= queued
        except OSError:
            log.debug("wakeshift: couldn't record the session's state", exc_info=True)

    def _deliver(self, s: Session, l: Listener, ds: list) -> list:
        """Hands a batch to the session: returns the IDs that are done (they
        started a turn, now or before, and that's recorded), which are the only
        ones the listener acknowledges. Under the session's lock: what already
        started is just done; the rest start one turn, if the session is idle
        (in any process), recorded before it returns."""
        while self._busy_hint(s.id) and not l.stopped and s.listener is l:
            with self.lock:
                self.lock.wait(1)
        if l.stopped or s.listener is not l:
            return []
        with self._locked(s.id):
            st = self._state(s.id)
            done = [d["delivery_id"] for d in ds if d["delivery_id"] in st["started"]]
            with self.lock:
                queued = set(s.queued)
            fresh = [d for d in ds if d["delivery_id"] not in st["started"] and d["delivery_id"] not in queued]
            waker = self.wakers.get(s.platform)
            if not fresh or waker is None or self.now() < st["busy_until"] or l.stopped:
                return done
            st["busy_until"] = self.now() + STARTING_FOR  # until its turn starts (pre_llm_call)
            self._save_state(s.id, st)
            batch = hashlib.sha256(",".join(sorted(d["delivery_id"] for d in fresh)).encode()).hexdigest()[:32]
            ok = False
            try:
                ok = bool(waker(s.id, wire.prompt(fresh), batch))
            except Exception:
                log.warning("wakeshift: starting a turn failed", exc_info=True)
            now = self.now()
            if ok and s.platform in QUEUES:
                # Only queued: done when the turn it's in starts.
                with self.lock:
                    s.queued.update(d["delivery_id"] for d in fresh)
                ok = False
                fresh = []
            elif ok:
                st["started"].update({d["delivery_id"]: now for d in fresh})
            else:
                st["busy_until"] = 0.0
            try:
                self._save_state(s.id, st)
            except OSError:
                # Not recorded: not acknowledged either (its redelivery runs
                # again only if Hermes's Idempotency-Key doesn't stop it).
                log.warning("wakeshift: couldn't record a started message")
                return done
            return done + ([d["delivery_id"] for d in fresh] if ok else [])

    def stop(self) -> None:
        with self.lock:
            ls = [s.listener for s in self.sessions.values() if s.listener is not None]
            self.lock.notify_all()
        for l in ls:
            l.stop()


class RedeemError(Exception):
    pass


class Outdated(RedeemError):
    """Wakeshift no longer serves this version (426 update_required); the
    message says what to tell the agent (ask the user to update)."""


class Taken(RedeemError):
    """Wakeshift refused to move the conversation to another workspace's session
    (409 conversation_taken): the link it has stays."""


def _same_origin(link: str, issuer: str) -> bool:
    """A stream link (wss://host/…) is on the issuer's own scheme and host
    (wss for https, ws for http)."""
    from urllib.parse import urlparse
    try:
        origin, _ = wire.link_parts(link)
        a, b = urlparse(origin), urlparse(issuer)
    except ValueError:
        return False
    return bool(b.scheme and b.netloc) and (a.scheme, a.netloc.lower()) == (b.scheme, b.netloc.lower())


def _redeem(claims: dict, conversation: str, call: str, supersedes: str = "") -> str:
    """POST <redeem> {"ticket", "thread"[, "call", "supersedes"]} -> the conversation's stream link."""
    body = {"ticket": claims["ticket"], "thread": conversation}
    if call:
        body["call"] = call
    if supersedes:
        body["supersedes"] = supersedes
    req = urllib.request.Request(claims["redeem"], data=json.dumps(body).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header(wire.CLIENT_HEADER, wire.client_header())
    try:
        with opener.open(req, timeout=10) as r:
            out = json.loads(r.read(1 << 20) or b"{}")
    except urllib.error.HTTPError as e:
        try:
            err = json.loads(e.read(1 << 20) or b"{}").get("error") or {}
            code, message = err.get("code"), err.get("message")
        except Exception:
            code, message = None, None
        if e.code == 426 or code == "update_required":
            raise Outdated(message if isinstance(message, str) and message else
                           "Wakeshift's hooks for Hermes Agent are out of date. Ask the user whether to update them; only if they agree, run the install command again.")
        if code == "conversation_taken":
            raise Taken("this conversation is connected to another workspace's session")
        raise RedeemError({"unknown_ticket": "the ticket expired or was used",
                           "no_grant": "the app isn't allowed for this agent any more",
                           "wrong_thread": "the ticket was for another conversation"}.get(code, f"Wakeshift answered {e.code}"))
    except Exception:
        raise RedeemError("Wakeshift couldn't be reached")
    url = out.get("url") if isinstance(out, dict) else None
    if not isinstance(url, str) or not url:
        raise RedeemError("Wakeshift's answer had no link")
    try:
        wire.link_parts(url)
    except ValueError:
        raise RedeemError("Wakeshift's answer had no link")
    return url


def _get(url: str) -> Optional[bytes]:
    try:
        with opener.open(url, timeout=5) as r:
            return r.read(1 << 20) if r.status == 200 else None
    except Exception:
        return None


def _key_in(path: Path, kid: str) -> Optional[bytes]:
    try:
        jwks = json.loads(path.read_text())
    except Exception:
        return None
    for k in jwks.get("keys") or [] if isinstance(jwks, dict) else []:
        if isinstance(k, dict) and k.get("kid") == kid and k.get("kty") == "OKP" and k.get("crv") == "Ed25519":
            try:
                x = wire._b64(k.get("x") or "")
            except ValueError:
                continue
            if len(x) == 32:
                return x
    return None
