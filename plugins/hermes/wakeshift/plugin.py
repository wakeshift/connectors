"""What the plugin does in Hermes, kept apart from Hermes's own modules so it can
be tested without them (``__init__.register`` wires it in).

1. transform_tool_result: an MCP result carrying a beacon has it taken out
   before the model sees it (Hermes shows ``_meta`` to the model). A beacon
   signed by a Wakeshift it trusts (trusted_issuers), and for this conversation
   or for none (Hermes tells MCP servers neither the session nor the call, so Hermes
   handing this session's result to this hook is what binds it) is redeemed for
   the conversation's stream link, and the result gets the note
   "Wakeshift: connected …".
2. A Listener holds the link (SSE, else long poll) and hands each batch to the
   session once it's idle (between turns: pre_llm_call / on_session_end), framed
   as outside data (wire.prompt), never as the user speaking. No turn starts,
   on any platform, until the user lets the plugin start them:
   plugins.entries.<plugin>.allow_gateway_injection: true (the user's setting,
   read on every try, so turning it on needs no restart; Wakeshift's installer
   sets it, saying so). Without it the chat is told when it connects, with the
   exact command (cant_wake), and its messages wait, then arrive once it's on.
   - API server sessions: a new run in the same session (POST /v1/runs with its
     session_id, an Idempotency-Key per batch, and author is_bot), on the
     loopback API server with its own key. Acknowledged once the run is accepted.
   - CLI sessions: ctx.inject_message, framed. Up to Hermes 0.22.2 each
     `hermes chat` loads its own plugins, and that goes to its REPL's input
     queue. From 0.22.3 one gateway runs every CLI chat's turns and loads the
     plugins, so it names the chat (its route key, read in the chat's own
     hooks) and is a plugin turn in that session, which Hermes itself also
     allows only with that setting. Acknowledged only once a turn starts after
     it (pre_llm_call), so a message still queued when the chat ends comes back.
   - TUI sessions (`hermes --tui`, platform "tui"), from 0.22.3: local sessions
     of the same gateway, woken as CLI ones are. The TUI shows the woken turn;
     the classic CLI runs it but doesn't show it.
   Other platforms (Telegram, Discord…) aren't woken: Hermes's only way to start
   a turn there marks it internal (as if the user's own), which we never use.
3. Approvals stay as Hermes sets them: a woken run is unattended, so what needs
   approval is denied unless the user changed approvals.unattended_mode.
4. One conversation can be connected to several Wakeshift apps at once: it
   keeps a link (and a Listener) per app (and issuer), and any of them wakes
   it. A beacon replaces only its own app's link, and hands back only that
   one (supersedes): another app's link is never sent anywhere but its own
   stream, and a new link never stops another app's listener. Turns start
   one at a time whichever app's message starts them (the session's busy
   state is shared), and a publish that reaches the conversation through two
   of one app's sessions (a superseded one and its successor) starts a turn
   once (_publish).
5. tool_request middleware (fill_args): every call to a Wakeshift app's
   tool (an MCP tool that takes Wakeshift's three arguments) says it comes from
   Hermes (harness "hermes"), so Wakeshift gives its result a beacon whether or
   not the model said so, even where the app never sees Hermes's client name.
   It names the program only: what a beacon is redeemed for, and for which
   conversation, is checked as before. A call that passes no session_key gets
   this conversation's own for that app (session_key_for): the one Wakeshift's
   redeem answered for this conversation when the beacon connected it, from
   the MCP server pinned to that app, never another conversation's or another
   app's. So the model never has to pass it: Hermes shows MCP results to the
   model as untrusted data, and models often won't pass a key from one. (From
   a Wakeshift whose redeem doesn't answer it: the one that result tells the
   model to pass back, if it names the session the redeem answered.)
6. A beacon it should act on but can't (Wakeshift unreachable for its keys or
   the redeem, after a few quick tries) is logged and the result says so, with
   how to retry: never dropped silently.
"""
from __future__ import annotations

import contextlib
import hashlib
import json
import logging
import os
import socket
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
MISSING_CRYPTOGRAPHY = ("Hermes's Python environment is missing the cryptography package, which Wakeshift's plugin "
                        "needs to check Wakeshift's signatures; ask the user to install it there")
# The chat connected, but this Hermes won't let the plugin start its turns
# (cant_wake says why, and the fix).
NOTE_CANT_WAKE = "Wakeshift: connected, but new messages can't wake this conversation yet. {}"
NOTE_UNSUPPORTED = ("Wakeshift: Hermes can't wake {} conversations yet (only API server, CLI and TUI ones), "
                    "so new messages wait until you next use this app's tools.")
# A beacon in a turn that started before the plugin loaded (it was installed
# or enabled during the turn, and Hermes made its hooks live at once): Hermes
# names a session's platform only when a turn starts (pre_llm_call), so the
# plugin can't tell yet whether it can wake this one. It can on the next turn.
NOTE_JUST_LOADED = ("Wakeshift: not connected yet. Wakeshift's plugin for Hermes was loaded during this turn (it was "
                    "just installed or enabled), so it can connect this conversation only from its next turn. Ask the "
                    "user to send any message (\"done\" is enough), then call this app's tool again: a note "
                    "\"Wakeshift: connected\" confirms it. Until then, new messages wait.")
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
# Platforms whose waker only queues the message (the CLI's input queue, or a
# plugin turn in a local session): it's done once the next turn in that
# session starts, not when it's queued.
QUEUES = {"cli", "tui"}


# A local chat's interfaces: one gateway's classic CLI and Ink TUI chats are
# the same local sessions, so a chat connected in one and resumed in the
# other (`hermes chat --tui --continue`) stays connected.
LOCAL = {"cli", "tui"}


def _resumed_as(saved: str, platform: str) -> str:
    """The platform a saved link listens as when its session's turn starts on
    platform ("" if it doesn't: another platform's session)."""
    if not platform or platform == saved or (saved in LOCAL and platform in LOCAL):
        return platform or saved
    return ""


# The Wakeshift the plugin trusts when nothing says otherwise: the public one.
DEFAULT_ISSUER = "https://app.wakeshift.ai"


def trusted_issuers(setting=None, pinned=()) -> list:
    """The Wakeshifts whose beacons the plugin acts on (it redeems their tickets
    and holds their stream links):

    - the ``issuers`` plugin setting (``plugins.entries.wakeshift.settings.issuers``
      in Hermes's config.yaml: a list, or one string of URLs separated by commas
      or spaces) when it's set, else the public Wakeshift (DEFAULT_ISSUER);
    - plus the ones Wakeshift's installer pinned in the plugin's config.json.

    Each must be an origin, https://host[:port], or http:// on this machine's
    loopback (a local Wakeshift); anything else is dropped. A setting that's
    neither a list nor a string trusts nothing from the setting (it never
    falls back to the default)."""
    if setting is None:
        chosen = [DEFAULT_ISSUER]
    elif isinstance(setting, str):
        chosen = setting.replace(",", " ").split()
    elif isinstance(setting, (list, tuple)):
        chosen = list(setting)
    else:
        log.warning("wakeshift: the issuers setting isn't a list of URLs; ignoring it")
        chosen = []
    out: list = []
    for raw in chosen + list(pinned or ()):
        o = _origin(raw)
        if o is None:
            log.warning("wakeshift: not trusting %r: a Wakeshift is an https:// origin (http:// only on loopback)",
                        raw if isinstance(raw, str) else type(raw).__name__)
        elif o not in out:
            out.append(o)
    return out


def _origin(raw) -> Optional[str]:
    """scheme://host[:port], lowercased, if raw is a trustworthy Wakeshift
    origin (see trusted_issuers), else None."""
    from urllib.parse import urlsplit
    if not isinstance(raw, str):
        return None
    u = raw.strip().rstrip("/")
    try:
        p = urlsplit(u)
        p.port  # a malformed port raises
    except ValueError:
        return None
    if (p.scheme not in ("https", "http") or not p.hostname or p.username is not None or p.password is not None
            or p.path or p.query or p.fragment or "@" in p.netloc or any(c.isspace() for c in u)):
        return None
    if p.scheme == "http" and not _loopback(p.hostname):
        return None
    return f"{p.scheme}://{p.netloc.lower()}"


# The program the plugin runs in, as Wakeshift's harness argument names it,
# and the arguments every Wakeshift app's tool takes (the SDKs' attach adds
# all three to a wrapped tool's input schema).
HARNESS = "hermes"
HARNESS_ARG = "harness"
WAKE_ARGS = ("session_key", HARNESS_ARG, "conversation_id")


def takes_wake_args(schema) -> bool:
    """Whether a tool's schema (Hermes's registry entry, its input schema under
    "parameters", or a bare input schema) has Wakeshift's three arguments."""
    if not isinstance(schema, dict):
        return False
    params = schema.get("parameters")
    props = (params if isinstance(params, dict) else schema).get("properties")
    return isinstance(props, dict) and all(isinstance(props.get(k), dict) for k in WAKE_ARGS)


SESSION_ARG = "session_key"


def fill_args(schema_of: Callable[[str], Optional[dict]], session_key_for: Callable[[str, str], str] = None) -> Callable:
    """Hermes's tool_request middleware: a call to an MCP tool that takes
    Wakeshift's arguments gets harness "hermes", whatever the model passed (the
    plugin knows where it runs; the model may not say, or say something else),
    and, when it passes no session_key, this conversation's own for that app
    (session_key_for(session, tool name), "" if there's none); a session_key
    the model passed is left as it is. Any other tool's arguments are left
    alone (None). schema_of(tool name) is the tool's schema from Hermes's
    registry."""
    def middleware(tool_name: str = "", args=None, session_id: str = "", **_):
        if not isinstance(args, dict) or not wire.mcp_server(tool_name or ""):
            return None
        missing_key = not (isinstance(args.get(SESSION_ARG), str) and args[SESSION_ARG].strip())
        if args.get(HARNESS_ARG) == HARNESS and not (missing_key and session_key_for and session_id):
            return None
        try:
            schema = schema_of(tool_name)
        except Exception:
            return None
        if not takes_wake_args(schema):
            return None
        out, why = dict(args), []
        if args.get(HARNESS_ARG) != HARNESS:
            out[HARNESS_ARG] = HARNESS
            why.append("names the program this conversation runs in, so Wakeshift can wake it")
        if missing_key and session_key_for and session_id:
            try:
                key = session_key_for(session_id, tool_name)
            except Exception:
                key = ""
            if key:
                out[SESSION_ARG] = key
                why.append("passes this conversation's own session_key for this app, which the call left out")
        if not why:
            return None
        return {"args": out, "source": "wakeshift", "reason": "; ".join(why)}
    return middleware


def _loopback(host: str) -> bool:
    import ipaddress
    if host.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


class ApiRuns:
    """Starts a run in an API server session: POST /v1/runs on the loopback
    API server, with its key, only while the user lets the plugin start
    turns (allowed(), read on each try, so turning it on needs no restart;
    why is logged, once per session, while it's off). A refused message
    isn't taken: it's offered again, and runs once it's allowed."""

    def __init__(self, port: int, key: Callable[[], str], *, allowed: Callable[[], bool], why: str = ""):
        self.base = f"http://127.0.0.1:{port}"
        self.key = key
        self.allowed = allowed
        self.why = why or "Hermes doesn't allow the plugin to start turns"
        self.told: set = set()

    def __call__(self, session: str, text: str, batch: str) -> bool:
        try:
            ok = self.allowed() is True
        except Exception:
            ok = False
        if not ok:
            if session not in self.told:
                self.told.add(session)
                log.warning("wakeshift: can't wake an API server conversation yet: %s", self.why)
            return False
        self.told.discard(session)
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


# A link record's app, as a listener key: (issuer, app). A link saved before
# links named their app (one per conversation) is LEGACY: the first beacon
# connecting the conversation after the update replaces it, as before.
LEGACY = ("", "")


class Session:
    def __init__(self, sid: str):
        self.id = sid
        self.platform = ""
        # Whether a turn has started in this session since the plugin loaded
        # (pre_llm_call, which names the platform): a session the plugin first
        # sees in a tool result started its turn before the plugin loaded.
        self.started = False
        # Hermes's route key for the session (agent:main:local:dm:…), read in
        # its own hooks, where Hermes has one (0.22.3 and later): how a CLI
        # chat is named to start a turn in it.
        self.route = ""
        self.listeners: dict[tuple, Listener] = {}  # (issuer, app) -> its link's listener
        # Delivery ID -> its publish key (or ""), for what's in the input
        # queue with no turn started since.
        self.queued: dict = {}


class Wakeshift:
    """The plugin's state in one Hermes process.

    wakers maps a platform ("api_server", "cli", "tui") to a function
    (session, framed text, batch id) -> accepted. route() is Hermes's route
    key for the session whose hook is running ("" if it has none);
    cant_wake(platform) says why this Hermes won't let the plugin start that
    platform's turns, and the fix ("" when it will)."""

    def __init__(self, *, state_dir: Path, issuers: list[str], wakers: dict, spawn: Callable = None,
                 http_get: Callable = None, now: Callable[[], float] = time.time, listener_args: Optional[dict] = None,
                 sleep: Callable[[float], None] = time.sleep, route: Callable[[], str] = None,
                 cant_wake: Callable[[str], str] = None):
        self.dir = Path(state_dir)
        self.issuers = [i.rstrip("/") for i in issuers if isinstance(i, str)]
        self.wakers = wakers
        self.spawn = spawn or (lambda target, name: threading.Thread(target=target, name=name, daemon=True))
        self.http_get = http_get or _get
        self.now = now
        self.sleep = sleep
        self.told_missing: set = set()  # sessions told cryptography is missing
        self.listener_args = listener_args or {}
        self.lock = threading.Condition()
        self.sessions: dict[str, Session] = {}
        self.route = route or (lambda: "")
        self.cant_wake = cant_wake or (lambda platform: "")
        # (session, (issuer, app)) -> the session_key Wakeshift gave the
        # conversation for that app (its redeem's answer), kept after its link
        # ends (the key outlives a subscription).
        self.handles: dict[tuple, str] = {}

    # -- files --
    def _link_path(self, sid: str, key: tuple = LEGACY) -> Path:
        """links/<hash>.json: a conversation's link for one app of one issuer
        (key), named by a hash so odd IDs can't escape the directory."""
        name = sid if key == LEGACY else "\n".join((sid, key[0], key[1]))
        return self.dir / "links" / (hashlib.sha256(name.encode()).hexdigest()[:32] + ".json")

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

    def _save_link(self, sid: str, platform: str, link: str, key: tuple = LEGACY, handle: str = "") -> None:
        d = {"session": sid, "platform": platform, "link": link}
        if key != LEGACY:
            d.update(issuer=key[0], app=key[1])
            if handle:
                d["session_key"] = handle
        self._write(self._link_path(sid, key), json.dumps(d).encode())

    def _forget_link(self, sid: str, key: tuple = LEGACY) -> None:
        try:
            self._link_path(sid, key).unlink()
        except FileNotFoundError:
            pass

    @staticmethod
    def _key_of(d: dict) -> tuple:
        return (d.get("issuer") or "", d.get("app") or "") if d.get("app") else LEGACY

    def _stored(self) -> list[dict]:
        """Every saved link: only a file named for what it holds (a copy
        under another name, or a hand-edited one, isn't read)."""
        out = []
        for p in sorted((self.dir / "links").glob("*.json")):
            try:
                d = json.loads(p.read_text())
            except Exception:
                continue
            if not isinstance(d, dict) or not all(isinstance(d.get(k), str) and d.get(k) for k in ("session", "platform", "link")):
                continue
            if not all(isinstance(d.get(k, ""), str) for k in ("issuer", "app", "session_key")):
                continue
            if self._link_path(d["session"], self._key_of(d)).name == p.name:
                out.append(d)
        return out

    def _links_of(self, sid: str) -> list[dict]:
        """The conversation's saved links, one per app (and an old one that
        names none)."""
        return [d for d in self._stored() if d["session"] == sid]

    def _load_link(self, sid: str, key: tuple = LEGACY) -> Optional[dict]:
        try:
            d = json.loads(self._link_path(sid, key).read_text())
        except Exception:
            return None
        return d if isinstance(d, dict) and d.get("session") == sid and self._key_of(d) == key else None

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
        <issuer>/oauth/jwks when the kid isn't known (not again within
        KEYS_REFETCH_AFTER, so made-up kids can't make it fetch on every result).
        Raises KeysUnavailable when the issuer can't be reached for them, after
        a few quick tries (a Wakeshift mid-deploy answers again in seconds),
        and KeyUnknown when its keys, fetched again over those tries (a key
        being rotated in), don't have kid."""
        if issuer not in self.issuers:
            return None
        path = self.dir / "keys" / (hashlib.sha256(issuer.encode()).hexdigest()[:16] + ".json")
        k = _key_in(path, kid)
        if k is not None:
            return k
        try:
            if self.now() - path.stat().st_mtime < KEYS_REFETCH_AFTER:
                raise KeyUnknown(issuer, checked=False)  # fetched just now, without it: not checked again
        except FileNotFoundError:
            pass
        fetched = False
        for wait in (0,) + RETRY_WAITS:
            if wait:
                self.sleep(wait)
            body = self.http_get(issuer + "/oauth/jwks")
            if body is None:
                continue
            fetched = True
            self._write(path, body)
            k = _key_in(path, kid)
            if k is not None:
                return k
        raise KeyUnknown(issuer, checked=True) if fetched else KeysUnavailable(issuer)

    # -- sessions --
    def _session(self, sid: str) -> Session:
        s = self.sessions.get(sid)
        if s is None:
            s = self.sessions[sid] = Session(sid)
        return s

    def _note_route(self, s: Session) -> None:
        """Records Hermes's route key for s, read in its own hook (call under self.lock)."""
        try:
            r = self.route()
        except Exception:
            r = ""
        if isinstance(r, str) and r:
            s.route = r

    def route_of(self, sid: str) -> str:
        """Hermes's route key for the session, "" if none was seen."""
        with self.lock:
            s = self.sessions.get(sid)
            return s.route if s is not None else ""

    def session_key_for(self, sid: str, tool_name: str) -> str:
        """This conversation's session_key for the app whose MCP server tool_name
        is on: the one Wakeshift gave this conversation (sid) for the app pinned
        to that server; "" if there's none, or more than one (two Wakeshifts'
        apps of one name). Never another conversation's, nor another app's."""
        server = wire.mcp_server(tool_name or "")
        if not sid or not server:
            return ""
        try:
            pins = json.loads(self._pins_path().read_text()).get("servers")
        except Exception:
            return ""
        app = pins.get(server) if isinstance(pins, dict) else None
        if not isinstance(app, str) or not app:
            return ""
        with self.lock:
            found = {h for (s, k), h in self.handles.items() if s == sid and k[1] == app and h}
        found |= {d["session_key"] for d in self._links_of(sid) if d.get("app") == app and d.get("session_key")}
        return next(iter(found)) if len(found) == 1 else ""

    def turn_started(self, session_id: str = "", platform: str = "", **_) -> None:
        """pre_llm_call: the session is busy; resume its stream if it was connected."""
        if not session_id:
            return None
        with self.lock:
            s = self._session(session_id)
            s.started = True
            if platform:
                s.platform = platform
            self._note_route(s)
            listening = {k for k, l in s.listeners.items() if not l.stopped}
        self._set_busy(session_id, self.now() + BUSY_FOR, started=True)
        for d in self._links_of(session_id):
            key = self._key_of(d)
            now = _resumed_as(d["platform"], platform)
            if key not in listening and now in self.wakers:
                self._listen(session_id, now, d["link"], key)
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
            log.warning("wakeshift: a beacon in %s's result came with no session, so no conversation was connected", tool_name or "a tool")
            return stripped
        # A beacon proves Wakeshift minted it, not that this result is its
        # app's: one an attacker minted (Hermes tells servers nothing, so it's
        # bound to no conversation) could sit in a page a tool fetched, or in
        # another server's result. Acted on only in an MCP server's own result
        # (its _meta, not a document inside it), from the server pinned to the
        # beacon's app.
        server = wire.mcp_server(tool_name)
        if not server or wire.result_beacon(result) != token:
            log.info("wakeshift: ignored a beacon outside an MCP server's own result (%s)", tool_name or "a tool")
            return stripped
        try:
            claims = wire.verify_bound(token, conversation=session_id, call=tool_call_id or "", unbound=True,
                                       issuers=self.issuers, key_for=self._key_for, now=self.now())
        except ImportError:
            # Hermes's own environment has cryptography (a core dependency
            # since it pinned it), so this is a broken or unusual install:
            # logged every time, and each conversation is told once.
            log.warning("wakeshift: the cryptography package can't be imported, so beacons can't be checked; "
                        "install it into the Python environment Hermes runs in")
            with self.lock:
                told = session_id in self.told_missing
                self.told_missing.add(session_id)
            return stripped if told else wire.add_note(stripped, NOTE_FAILED.format(MISSING_CRYPTOGRAPHY))
        except KeysUnavailable as e:
            # Ours to act on (a trusted issuer), but its keys couldn't be
            # fetched: said, so the conversation isn't left unconnected unawares.
            log.warning("wakeshift: couldn't reach %s for its keys, so %s's beacon wasn't checked", e, tool_name)
            return wire.add_note(stripped, NOTE_FAILED.format("Wakeshift couldn't be reached to check it"))
        except KeyUnknown as e:
            # A beacon naming a trusted issuer, signed with a key it doesn't
            # list. Said only once its keys were fetched again to check (a key
            # being rotated in shows up within those tries), so the chat isn't
            # left unconnected unawares; a forged beacon gets that note and
            # nothing else. Against keys fetched moments ago, only logged.
            log.warning("wakeshift: %s doesn't list the key %s's beacon was signed with", e, tool_name)
            if not e.checked:
                return stripped
            return wire.add_note(stripped, NOTE_FAILED.format("its signing key wasn't one Wakeshift lists"))
        except (wire.Untrusted, wire.Invalid) as e:
            log.info("wakeshift: ignored a beacon in %s's result (%s)", tool_name, e)
            return stripped  # not ours to act on; say nothing to the model
        except Exception:
            log.warning("wakeshift: checking a beacon failed", exc_info=True)
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
            s = self._session(session_id)
            self._note_route(s)
            platform, started = s.platform, s.started
        if platform not in self.wakers:
            if not platform and not started:
                log.info("wakeshift: a beacon arrived in a turn that started before the plugin loaded; it connects from the next turn")
                return wire.add_note(stripped, NOTE_JUST_LOADED)
            return wire.add_note(stripped, NOTE_UNSUPPORTED.format(platform or "these"))
        call = "" if claims.get("thread") else (tool_call_id or "")
        key = (claims.get("iss", "").rstrip("/"), claims.get("app", ""))
        # The link this conversation had until now for this app, if any:
        # handed back, it ends the Wakeshift session it belonged to, so a
        # conversation whose calls started a new one stays one subscription
        # (and Wakeshift refuses to move it to another workspace's session
        # while that link is live). Another app's link isn't this app's to
        # hand back, and stays as it is. A link saved before links named
        # their app goes too, as it did then.
        prior = self._load_link(session_id, key)
        legacy = self._load_link(session_id) if prior is None else None
        held = prior or legacy
        supersedes = held["link"] if held and _same_origin(held["link"], claims.get("iss", "")) else ""
        try:
            link, session, redeemed_key = _redeem(claims, session_id, call, supersedes, sleep=self.sleep)
        except Outdated as e:
            log.warning("wakeshift: Wakeshift refused this plugin's version: %s", e)
            return wire.add_note(stripped, NOTE_OUTDATED.format(e))
        except Taken:
            log.warning("wakeshift: Wakeshift kept a conversation connected where it was (another workspace's session)")
            return wire.add_note(stripped, NOTE_TAKEN)  # the link it had stays
        except RedeemError as e:
            log.warning("wakeshift: couldn't connect a conversation: %s", e)
            return wire.add_note(stripped, NOTE_FAILED.format(e))
        if not _same_origin(link, claims.get("iss", "")):
            # A link on another host would get the token: refused, unused.
            return wire.add_note(stripped, NOTE_FAILED.format("its stream link wasn't on the Wakeshift this plugin trusts"))
        # The session_key Wakeshift gave the conversation for this app: the
        # one the redeem just answered for this conversation; from an older
        # Wakeshift, the one this result tells it to pass back that names
        # the session the redeem answered (wire.session_key_in). Never a
        # line the tool's own text may carry.
        handle = redeemed_key or wire.session_key_in(stripped, session)
        with self.lock:
            if handle:
                self.handles[(session_id, key)] = handle
            handle = self.handles.get((session_id, key), "")
        try:
            self._save_link(session_id, platform, link, key, handle)
        except OSError:
            return wire.add_note(stripped, NOTE_FAILED.format("its link couldn't be saved"))
        if legacy is not None:
            self._forget_link(session_id)
        self._set_busy(session_id, self.now() + BUSY_FOR)  # this turn is still running
        self._listen(session_id, platform, link, key, replaces=(LEGACY,) if legacy is not None else ())
        try:
            why = self.cant_wake(platform)
        except Exception:
            log.warning("wakeshift: couldn't check whether Hermes lets the plugin wake this conversation", exc_info=True)
            why = ""
        if why:
            # Connected (its messages wait, and arrive once Hermes allows it),
            # but never silently unwoken.
            log.warning("wakeshift: connected a %s conversation that can't be woken yet: %s", platform, why)
            return wire.add_note(stripped, NOTE_CANT_WAKE.format(why))
        return wire.add_note(stripped, NOTE_CONNECTED)

    def resume(self, platforms: tuple = ("api_server",)) -> int:
        """Listen again for every conversation connected before a restart, on
        these platforms (the gateway's; a CLI resumes its own on its next turn)."""
        n, idle = 0, set()
        for d in self._stored():
            if d["platform"] in platforms and d["platform"] in self.wakers:
                with self.lock:
                    self._session(d["session"]).platform = d["platform"]
                    if d.get("app") and d.get("session_key"):
                        self.handles.setdefault((d["session"], self._key_of(d)), d["session_key"])
                if d["session"] not in idle:
                    # A turn this process had running when it stopped ended with it.
                    self._set_busy(d["session"], 0.0)
                    idle.add(d["session"])
                self._listen(d["session"], d["platform"], d["link"], self._key_of(d))
                n += 1
        return n

    def _listen(self, sid: str, platform: str, link: str, key: tuple = LEGACY, replaces: tuple = ()) -> None:
        """Listens on the conversation's link for one app (key), in place of
        the listener it had for that app (and for any key in replaces). What
        a replaced listener took and never handed over goes to the new one:
        Wakeshift ends a superseded session's deliveries, so nothing else
        would bring them."""
        with self.lock:
            s = self._session(sid)
            olds = [s.listeners.pop(k) for k in (key,) + tuple(replaces) if k in s.listeners]
            l = Listener(link, lambda ds: self._deliver(s, l, ds),
                         on_link=lambda u: self._linked(s, key, l, platform, u),
                         on_gone=lambda: self._gone(s, key, l), spawn=self.spawn, **self.listener_args)
            s.listeners[key] = l
        for old in olds:
            old.stop()
            l.adopt(old.undelivered())
        l.start()

    def _current(self, s: Session, l) -> bool:
        """l is still one of the session's listeners, and running."""
        with self.lock:
            return not l.stopped and any(x is l for x in s.listeners.values())

    def _linked(self, s: Session, key: tuple, l: Listener, platform: str, link: str) -> None:
        """A rotated link, saved only while l is still the app's listener (a
        superseded one must not overwrite the new link)."""
        with self.lock:
            if s.listeners.get(key) is l:
                self._save_link(s.id, platform, link, key, self.handles.get((s.id, key), ""))

    def _gone(self, s: Session, key: tuple, l: Listener) -> None:
        with self.lock:
            if s.listeners.get(key) is l:
                del s.listeners[key]
                self._forget_link(s.id, key)

    # -- each session's state, owned by one lock --
    # started/<hash>.json holds a connected session's state, shared by every
    # Hermes process on this machine (a gateway, and a `hermes chat --resume`
    # of one of its sessions) and every app's listener: busy_until (a turn
    # runs until then), started (delivery ID -> when it started a turn) and
    # published (publish key -> when, _publish), each kept for STARTED_FOR.
    # It's read and written only under the session's flock (_locked), which
    # every thread and process takes for its own open of started/<hash>.lock;
    # that's also where a turn start decides, reserves, starts and records.
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

        def recent(name):
            m = d.get(name) if isinstance(d.get(name), dict) else {}
            return {k: v for k, v in m.items() if isinstance(v, (int, float)) and now - v < STARTED_FOR}
        busy = d.get("busy_until")
        return {"busy_until": float(busy) if isinstance(busy, (int, float)) else 0.0,
                "started": recent("started"), "published": recent("published")}

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
        if not (self._state_path(sid).exists() or self._links_of(sid)):
            return
        try:
            with self._locked(sid):
                st = self._state(sid)
                st["busy_until"] = until
                with self.lock:
                    s = self.sessions.get(sid)
                    queued = dict(s.queued) if started and s is not None else {}
                now = self.now()
                st["started"].update({i: now for i in queued})
                st["published"].update({k: now for k in queued.values() if k})
                self._save_state(sid, st)
                with self.lock:
                    if s is not None:
                        for i in queued:
                            s.queued.pop(i, None)
        except OSError:
            log.debug("wakeshift: couldn't record the session's state", exc_info=True)

    def _deliver(self, s: Session, l: Listener, ds: list) -> list:
        """Hands a batch to the session: returns the IDs that are done (they
        started a turn, now or before, or their publish did through another
        delivery, and that's recorded), which are the only ones the listener
        acknowledges. Under the session's lock: what already started is just
        done; the rest start one turn, if the session is idle (in any
        process, whichever app's listener started its last turn), recorded
        before it returns."""
        while True:
            if not self._current(s, l):
                return []
            with self._locked(s.id):
                st = self._state(s.id)
                with self.lock:
                    queued = dict(s.queued)
                seen = set(queued.values())
                done, fresh = [], []
                for d in ds:
                    i, k = d["delivery_id"], _publish(d)
                    if i in st["started"] or (k and k in st["published"]):
                        done.append(i)
                    elif i not in queued and not (k and k in seen):
                        fresh.append(d)
                # Another delivery of a publish that started a turn: done too.
                twice = {i: self.now() for i in done if i not in st["started"]}
                if twice:
                    st["started"].update(twice)
                    self._record(s.id, st)
                waker = self.wakers.get(s.platform)
                # What's done is acknowledged at once, busy or not; the rest
                # is offered again straight after.
                if done or not fresh or waker is None or not self._current(s, l):
                    return done
                if self.now() >= st["busy_until"]:
                    return self._start(s, st, ds, fresh, queued, waker)
            # Busy (a turn of the user's, or one another app's listener or
            # another process just started): wait for it to end.
            while self._busy_hint(s.id) and self._current(s, l):
                with self.lock:
                    self.lock.wait(1)

    def _start(self, s: Session, st: dict, ds: list, fresh: list, queued: dict, waker) -> list:
        """Starts one turn with fresh (under the session's lock, idle): the
        IDs it's done with, recorded before it returns."""
        # One per publish, in this batch too: the others are done with it.
        fresh = list({(_publish(d) or d["delivery_id"]): d for d in reversed(fresh)}.values())[::-1]
        ids = [d["delivery_id"] for d in fresh]
        keys = {k for k in map(_publish, fresh) if k}
        twins = {d["delivery_id"]: _publish(d) for d in ds
                 if d["delivery_id"] not in ids and d["delivery_id"] not in queued and _publish(d) in keys}
        st["busy_until"] = self.now() + STARTING_FOR  # until its turn starts (pre_llm_call)
        self._save_state(s.id, st)
        batch = hashlib.sha256(",".join(sorted(ids)).encode()).hexdigest()[:32]
        ok = False
        try:
            ok = bool(waker(s.id, wire.prompt(fresh), batch))
        except Exception:
            log.warning("wakeshift: starting a turn failed", exc_info=True)
        now = self.now()
        if ok and s.platform in QUEUES:
            # Only queued: done when the turn it's in starts.
            with self.lock:
                s.queued.update({d["delivery_id"]: _publish(d) for d in fresh})
                s.queued.update(twins)
            return []
        if ok:
            st["started"].update({i: now for i in ids + list(twins)})
            st["published"].update({k: now for k in keys})
        else:
            st["busy_until"] = 0.0
        try:
            self._save_state(s.id, st)
        except OSError:
            # Not recorded: not acknowledged either (its redelivery runs
            # again only if Hermes's Idempotency-Key doesn't stop it).
            log.warning("wakeshift: couldn't record a started message")
            return []
        return ids + list(twins) if ok else []

    def _record(self, sid: str, st: dict) -> None:
        try:
            self._save_state(sid, st)
        except OSError:
            log.debug("wakeshift: couldn't record the session's state", exc_info=True)

    def stop(self) -> None:
        with self.lock:
            ls = [l for s in self.sessions.values() for l in s.listeners.values()]
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


# How the plugin is updated: Hermes's own update, for a plugin it installed
# from the repository or the catalog, else installing it again over itself.
UPDATE_COMMAND = "hermes plugins update wakeshift"
REINSTALL_COMMAND = "hermes plugins install wakeshift/hermes-plugin --enable --force"


# Hermes's record of what its plugin installer installed, next to the
# plugins: {directory name: {"source", "pinned", "catalog", …}}.
INSTALL_RECORDS = ".install-metadata.json"


def update_command(d: Path = None) -> str:
    """The one command that updates the plugin in its directory (d): `hermes
    plugins update wakeshift` where Hermes can (a catalog install, or one
    from the repository that isn't pinned to a commit: a git checkout, or a
    tree Hermes recorded the source of), else installing it again with
    --force (Wakeshift's installer writes the plugin itself, so Hermes has
    nothing to update it from). Read from the plugin's directory and
    Hermes's install record beside it (INSTALL_RECORDS), not Hermes's
    modules, which aren't a public plugin API."""
    d = Path(d or Path(__file__).parent)
    try:
        rec = json.loads((d.parent / INSTALL_RECORDS).read_text(encoding="utf-8-sig")).get(d.name)
    except Exception:
        rec = None
    rec = rec if isinstance(rec, dict) else {}
    if isinstance(rec.get("catalog"), dict) or rec.get("catalog_name"):
        return UPDATE_COMMAND  # Hermes re-pins a catalog install to its reviewed commit
    if rec.get("pinned") is True:
        return REINSTALL_COMMAND  # Hermes won't move a pinned install
    if (d / ".git").exists() or (isinstance(rec.get("source"), str) and rec["source"]):
        return UPDATE_COMMAND
    return REINSTALL_COMMAND


def outdated_message(command: str = None) -> str:
    """What the chat is told when Wakeshift refuses this plugin's version
    and doesn't say what to do (its own message comes first)."""
    return ("Wakeshift's plugin for Hermes Agent is out of date. Ask the user whether to update it; only if they agree, "
            f"run `{command or update_command()}` where Hermes runs (inside its container, if it's in one), then resume "
            "this chat or start a new one, so Hermes loads the new plugin.")


def _publish(d: dict) -> str:
    """Which of its app's publishes a delivery carries, the same in every
    session it reached ("" if it doesn't say): a topic copy has the
    publish's own ID in every workspace, a direct one names it ($emit).
    Scoped by the app it's from, so two apps can't collide."""
    ev = d.get("event") if isinstance(d.get("event"), dict) else {}
    src = ev.get("source")
    attrs = ev.get("attrs") if isinstance(ev.get("attrs"), dict) else {}
    if not isinstance(src, str) or not src.startswith("app:"):
        return ""
    pid = attrs.get("$emit") or ev.get("id")
    return src + "|" + pid if isinstance(pid, str) and pid else ""


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


class KeysUnavailable(Exception):
    """A trusted Wakeshift couldn't be reached for its keys (its argument is
    the issuer)."""


class KeyUnknown(Exception):
    """A trusted Wakeshift's (issuer's) keys don't have the one a beacon names.
    checked: they were fetched again (over RETRY_WAITS) to check, rather than
    read from keys fetched moments ago."""

    def __init__(self, issuer: str, *, checked: bool):
        super().__init__(issuer)
        self.issuer = issuer
        self.checked = checked


# Waits before each further try of a request to Wakeshift that couldn't
# reach it (its keys, a redeem): a deploy's brief unavailability passes.
RETRY_WAITS = (0.5, 1.5)
# Keys fetched this recently aren't fetched again for an unknown kid: short
# enough that a retry after a key rotation finds it, long enough that a
# made-up kid can't make every result a fetch.
KEYS_REFETCH_AFTER = 10
# Answers that mean the request never reached a Wakeshift that could act on it.
UNAVAILABLE = {502, 503, 504}


def _redeem(claims: dict, conversation: str, call: str, supersedes: str = "", sleep: Callable[[float], None] = time.sleep) -> tuple:
    """POST <redeem> {"ticket", "thread"[, "call", "supersedes"]} -> the conversation's
    stream link, its session's handle (<agent>@<label>; "" if the answer
    has none) and its session_key ("" if the answer has none), tried again (RETRY_WAITS) when Wakeshift couldn't be reached or
    answered that it's unavailable."""
    for wait in RETRY_WAITS + (None,):
        try:
            return _redeem_once(claims, conversation, call, supersedes)
        except Unavailable:
            if wait is None:
                raise
            sleep(wait)
    raise Unavailable("Wakeshift couldn't be reached")  # not reached


class Unavailable(RedeemError):
    """Wakeshift couldn't be reached, or answered that it's unavailable."""


def _redeem_once(claims: dict, conversation: str, call: str, supersedes: str = "") -> tuple:
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
            raise Outdated(message if isinstance(message, str) and message else outdated_message())
        if code == "conversation_taken":
            raise Taken("this conversation is connected to another workspace's session")
        if e.code in UNAVAILABLE and not code:
            raise Unavailable(f"Wakeshift answered {e.code}")
        raise RedeemError({"unknown_ticket": "the ticket expired or was used",
                           "no_grant": "the app isn't allowed for this agent any more",
                           "wrong_thread": "the ticket was for another conversation"}.get(code, f"Wakeshift answered {e.code}"))
    except ValueError:  # an answer that isn't JSON: it was redeemed (or refused) all the same
        raise RedeemError("Wakeshift's answer had no link")
    except urllib.error.URLError as e:
        # Tried again only when the request can't have reached Wakeshift (no
        # connection, no address): after a timeout or a dropped answer the
        # ticket may be used up already.
        if isinstance(e.reason, (ConnectionRefusedError, socket.gaierror)):
            raise Unavailable("Wakeshift couldn't be reached")
        raise RedeemError("Wakeshift couldn't be reached")
    except Exception:
        raise RedeemError("Wakeshift couldn't be reached")
    url = out.get("url") if isinstance(out, dict) else None
    if not isinstance(url, str) or not url:
        raise RedeemError("Wakeshift's answer had no link")
    try:
        wire.link_parts(url)
    except ValueError:
        raise RedeemError("Wakeshift's answer had no link")
    session = out.get("session")
    return url, (session if isinstance(session, str) and len(session) <= 512 else ""), wire.redeemed_key(out.get("session_key"))


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
