"""Wakeshift's plugin for Hermes Agent: lets Wakeshift messages wake your Hermes
conversations (API server, CLI and TUI ones), only once the user allows it
(plugins.entries.wakeshift.allow_gateway_injection: true). Installed by
``hermes plugins install wakeshift``, by Wakeshift's installer
(``/install/hermes.sh``) or by ``wakeshift install --harness hermes``; remove
it with ``hermes plugins remove wakeshift``. See plugin.py for what it does,
and plugin.trusted_issuers for which Wakeshifts it trusts.
"""
from __future__ import annotations

import json
import logging
import os
import re
from pathlib import Path

log = logging.getLogger("wakeshift")

MARKER = "Wakeshift's plugin for Hermes Agent"


def _hermes_home() -> Path:
    try:
        from hermes_constants import get_hermes_home  # Hermes's own resolution (profiles included)
        return Path(get_hermes_home())
    except Exception:
        return Path(os.environ.get("HERMES_HOME") or Path.home() / ".hermes")


def _pinned(d: Path = None) -> list:
    """The Wakeshifts Wakeshift's installer pinned in config.json (in the plugin's
    directory, d), if it wrote one."""
    try:
        cfg = json.loads(((d or Path(__file__).parent) / "config.json").read_text())
    except FileNotFoundError:
        return []
    except Exception:
        log.warning("wakeshift: config.json can't be read; ignoring it")
        return []
    issuers = cfg.get("issuers") if isinstance(cfg, dict) else None
    return [i for i in issuers if isinstance(i, str)] if isinstance(issuers, list) else []


def _setting(ctx):
    """The issuers plugin setting (plugins.entries.wakeshift.settings.issuers), or
    None when it isn't set (or this Hermes has no plugin settings)."""
    try:
        return ctx.get_config("issuers", None)
    except Exception:
        return None


def _spawn(target, name):
    try:  # carries the profile scope Hermes's secrets are read under
        from agent.memory_provider import spawn_context_thread
        return spawn_context_thread(target, name=name)
    except Exception:
        import threading
        return threading.Thread(target=target, name=name, daemon=True)


def _api_key() -> str:
    try:
        from agent.secret_scope import get_secret
        return get_secret("API_SERVER_KEY", "") or ""
    except Exception:
        return os.environ.get("API_SERVER_KEY", "")


def _gateway() -> bool:
    import sys
    return os.environ.get("_HERMES_GATEWAY") == "1" or "gateway" in sys.argv[1:3]


def _schema_of(tool_name: str):
    """A tool's schema from Hermes's registry (None if it has none)."""
    try:
        from tools.registry import registry
        return registry.get_schema(tool_name)
    except Exception:
        return None


def _one_gateway() -> bool:
    """Whether this Hermes runs every CLI chat's turns in one gateway, which
    loads the plugins (0.22.3 and later), rather than in each `hermes chat`."""
    import importlib.util
    try:
        return importlib.util.find_spec("hermes_cli.plugin_session") is not None
    except Exception:
        return False


def _route() -> str:
    """Hermes's route key for the session whose hook or tool is running here
    ("" outside one, or on a Hermes without it)."""
    try:
        from hermes_cli.plugin_session import session_key
        r = session_key()
    except Exception:
        return ""
    return r if isinstance(r, str) else ""


def _plugin_id(ctx) -> str:
    pid = getattr(ctx, "plugin_id", None)
    return pid if isinstance(pid, str) and pid else "wakeshift"


def _injection_allowed(ctx) -> bool:
    """Whether the user lets this plugin start turns in Hermes's sessions
    (plugins.entries.<plugin>.allow_gateway_injection: true): Hermes's own
    check where it has one, else its config read the same way. Fails closed."""
    check = getattr(ctx, "_gateway_injection_allowed", None)
    if callable(check):
        try:
            return check() is True
        except Exception:
            return False
    try:
        from hermes_cli.config import load_config_readonly as load
    except ImportError:
        try:
            from hermes_cli.config import load_config as load
        except ImportError:
            return False
    try:
        cfg = load() or {}
        entry = ((cfg.get("plugins") or {}).get("entries") or {}).get(_plugin_id(ctx))
    except Exception:
        return False
    return isinstance(entry, dict) and entry.get("allow_gateway_injection") is True


def allow_command(plugin_id: str) -> str:
    """The command that lets the plugin start turns (_injection_allowed)."""
    return f"hermes config set plugins.entries.{plugin_id}.allow_gateway_injection true"


def cant_wake_text(plugin_id: str) -> str:
    """What a chat is told when it connects while the user hasn't let the
    plugin start turns (allow_gateway_injection), with the fix."""
    return ("Wakeshift's plugin starts turns in Hermes's chats only once the user allows it (Hermes's "
            "allow_gateway_injection setting for the plugin), and it isn't allowed yet. "
            f"Ask the user to run this where Hermes runs: `{allow_command(plugin_id)}` "
            f"(in Docker: `docker exec -u hermes <container> {allow_command(plugin_id)}`). "
            "It lets the plugin start turns in this Hermes's chats; Wakeshift starts them only in conversations connected "
            "to it. It takes effect at once, without a restart, and messages that arrived meanwhile then reach this "
            "conversation, as long as Hermes is running.")


def not_allowed(plugin_id: str) -> str:
    """Why a waker refused while the user hasn't allowed it, for the log."""
    return "Hermes doesn't allow the plugin to start turns; run: " + allow_command(plugin_id)


# Hermes reads a message as a command when, after leading whitespace and any
# leading @image:/@file:/@url: reference lines, it starts with "/"
# (gateway/platforms/event.py, MessageEvent._command_text; the same on 0.22.5
# and 0.23.0). Under its session authority a plugin turn can run one, so the
# plugin never hands Hermes such a text (its framing never starts that way).
_ATTACHMENT_REFS = re.compile(r"^(?:@(?:image|file|url):[^\n]+\n?)+", re.IGNORECASE)


def command_like(text: str) -> bool:
    """Whether Hermes could read text as a slash command."""
    if not isinstance(text, str):
        return True
    return _ATTACHMENT_REFS.sub("", text.lstrip()).lstrip().startswith("/")


def cli_waker(ctx, ws, one_gateway: bool, allowed):
    """The CLI's waker: (session, framed text, batch) -> accepted, only while
    the user lets the plugin start turns (allowed(), read on each try, so
    turning it on needs no restart). In a `hermes chat` that loads its own
    plugins (up to 0.22.2), its REPL's input queue. Where one gateway runs
    every CLI chat (0.22.3 and later), a plugin turn in the session named by
    its route key; never key-less there, which Hermes may hand to another
    chat."""
    told: dict = {}  # session -> why it was last refused, logged once (offered again every few seconds)

    def refuse(session: str, why: str) -> bool:
        if told.get(session) != why:
            told[session] = why
            log.warning("wakeshift: can't wake a CLI conversation yet: %s", why)
        return False

    def wake(session: str, text: str, batch: str) -> bool:
        if not allowed():
            return refuse(session, not_allowed(_plugin_id(ctx)))
        if command_like(text):
            return refuse(session, "its text would read as a Hermes command")
        if not one_gateway:
            told.pop(session, None)
            return bool(ctx.inject_message(text))
        route = ws.route_of(session)
        if not route:
            return refuse(session, "Hermes hasn't named its session to the plugin (it will on the conversation's next turn)")
        told.pop(session, None)
        return bool(ctx.inject_message(text, session_key=route))
    return wake


def register(ctx) -> None:
    from .plugin import ApiRuns, Wakeshift, fill_args, trusted_issuers

    try:
        port = int(os.environ.get("API_SERVER_PORT") or 8642)
    except ValueError:
        port = 8642
    one_gateway = _one_gateway()

    def allowed() -> bool:
        return _injection_allowed(ctx)

    # No plugin-started turn on any platform until the user allows it
    # (plugins.entries.<plugin>.allow_gateway_injection: true): a chat that
    # connects meanwhile is told so, with the command, and its messages wait.
    def cant_wake(platform: str) -> str:
        if platform in wakers and not allowed():
            return cant_wake_text(_plugin_id(ctx))
        return ""

    # By the session's platform: a gateway's API server sessions get a run on
    # its API server; CLI sessions, a turn through inject_message (cli_waker).
    # Where one gateway runs every local chat (0.22.3 and later), Ink TUI chats
    # (`hermes --tui`, platform "tui") are local sessions of it too, woken the
    # same way: the TUI shows a plugin turn, which the classic CLI doesn't yet.
    wakers = {"api_server": ApiRuns(port, _api_key, allowed=allowed, why=not_allowed(_plugin_id(ctx)))}
    ws = Wakeshift(state_dir=_hermes_home() / "wakeshift", issuers=trusted_issuers(_setting(ctx), _pinned()), wakers=wakers,
                   spawn=_spawn, route=_route, cant_wake=cant_wake)
    wakers["cli"] = cli_waker(ctx, ws, one_gateway, allowed)
    if one_gateway:
        wakers["tui"] = wakers["cli"]
    try:
        import cryptography  # noqa: F401  (Hermes's own dependency: checks beacons' signatures)
    except ImportError:
        log.warning("wakeshift: the cryptography package can't be imported here, so no conversation can be "
                    "connected; install it into the Python environment Hermes runs in")
    ctx.register_hook("transform_tool_result", ws.transform)
    ctx.register_hook("pre_llm_call", ws.turn_started)
    ctx.register_hook("on_session_end", ws.turn_ended)
    # Every Wakeshift app's tool is told it's Hermes, so its results carry a
    # beacon whether or not the model says so (a Hermes without middleware
    # still works, as before: the model passes it, or the app sees Hermes's
    # client name), and gets this conversation's session_key for that app
    # when the model leaves it out.
    middleware = getattr(ctx, "register_middleware", None)
    if callable(middleware):
        try:
            middleware("tool_request", fill_args(_schema_of, ws.session_key_for))
        except Exception:
            log.warning("wakeshift: couldn't register the tool_request middleware", exc_info=True)
    try:
        ctx.on_unload(ws.stop)
    except Exception:
        pass

    def resume():  # the gateway's API server sessions, connected before a restart
        import time
        time.sleep(5)  # the API server comes up after plugins load
        if _gateway():
            n = ws.resume(("api_server",))
            if n:
                log.info("wakeshift: listening again for %d conversation(s)", n)

    _spawn(resume, "wakeshift-resume").start()
