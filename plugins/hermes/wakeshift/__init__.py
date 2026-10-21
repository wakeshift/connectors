"""Wakeshift's plugin for Hermes Agent: lets Wakeshift messages wake your Hermes
conversations (API server and CLI ones). Written by
``wakeshift install --harness hermes``; remove it with
``wakeshift uninstall --harness hermes``. See plugin.py for what it does.
"""
from __future__ import annotations

import json
import logging
import os
from pathlib import Path

log = logging.getLogger("wakeshift")

MARKER = "Wakeshift's plugin for Hermes Agent"


def _hermes_home() -> Path:
    try:
        from hermes_constants import get_hermes_home  # Hermes's own resolution (profiles included)
        return Path(get_hermes_home())
    except Exception:
        return Path(os.environ.get("HERMES_HOME") or Path.home() / ".hermes")


def _issuers() -> list:
    try:
        cfg = json.loads((Path(__file__).parent / "config.json").read_text())
    except Exception:
        return []
    return [i for i in cfg.get("issuers") or [] if isinstance(i, str)]


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


def register(ctx) -> None:
    from .plugin import ApiRuns, Wakeshift

    try:
        port = int(os.environ.get("API_SERVER_PORT") or 8642)
    except ValueError:
        port = 8642
    # By the session's platform: a gateway's API server sessions get a run on
    # its API server; a CLI's sessions, its input queue.
    wakers = {"api_server": ApiRuns(port, _api_key),
              "cli": lambda session, text, batch: bool(ctx.inject_message(text))}
    ws = Wakeshift(state_dir=_hermes_home() / "wakeshift", issuers=_issuers(), wakers=wakers, spawn=_spawn)
    ctx.register_hook("transform_tool_result", ws.transform)
    ctx.register_hook("pre_llm_call", ws.turn_started)
    ctx.register_hook("on_session_end", ws.turn_ended)
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
