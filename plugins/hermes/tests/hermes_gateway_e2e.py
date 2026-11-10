"""Wakeshift's plugin in a real Hermes gateway, with Telegram and Discord chats.

Not part of `make hermes-test`: it runs inside Hermes's own Docker image
(0.22.5, 0.23.0), with Hermes's tests checked out at the image's tag, as one
of Hermes's gateway tests (copied to tests/gateway/). It uses the harness of
Hermes's own tests/gateway/test_plugin_origin_injection_authority.py: a real
GatewayRunner, the real Telegram and Discord adapters (sending stubbed: no
tokens, no accounts), a real SessionAuthority, and the plugin installed and
enabled in HERMES_HOME as `hermes plugins install` leaves it, loaded by
Hermes's own discovery. Only the model is stubbed: each turn calls the
plugin's hooks through Hermes's lifecycle, as the agent would, inside the
turn Hermes prepared (so the route key the plugin reads is Hermes's own). A
stand-in Wakeshift (tests/fake.py) mints the beacon and streams the events.

    WAKESHIFT_PLUGIN=/opt/wakeshift-plugin pytest tests/gateway/test_wakeshift_gateway_e2e.py

WAKESHIFT_PLUGIN is plugins/hermes of Wakeshift's repository.
"""
import asyncio
import json
import os
import shutil
import sys
import time
from pathlib import Path

import pytest
import hermes_yaml as yaml

from tests.waiting import async_wait_until

PLUGIN_SRC = Path(os.environ.get("WAKESHIFT_PLUGIN", "/opt/wakeshift-plugin"))
sys.path.insert(0, str(PLUGIN_SRC / "tests"))

from fake import Wakeshift as FakeWakeshift, event  # noqa: E402  (puts plugins/hermes on the path)
from wakeshift import wire  # noqa: E402

CONNECTED = "Wakeshift: connected."
NOT_DIRECT = "direct messages with its bot only"


def _install(home: Path, issuer: str, allow: bool) -> None:
    """The plugin as `hermes plugins install` + `hermes plugins enable` leave it."""
    dest = home / "plugins" / "wakeshift"
    if dest.exists():
        shutil.rmtree(dest)
    shutil.copytree(PLUGIN_SRC / "wakeshift", dest, ignore=shutil.ignore_patterns("__pycache__"))
    (home / "config.yaml").write_text(yaml.safe_dump({
        "plugins": {"enabled": ["wakeshift"], "entries": {"wakeshift": {
            "allow_gateway_injection": allow, "settings": {"issuers": [issuer]}}}},
    }), encoding="utf-8")


class Rig:
    def __init__(self, runner, adapter, sent, turns, fake):
        self.runner, self.adapter, self.sent, self.turns, self.fake = runner, adapter, sent, turns, fake

    async def human(self, text, *, chat_id="42", user_id="42", user_name="Alice", chat_type="dm",
                    thread_id=None, mid=None):
        from gateway.platforms.event import MessageEvent
        source = self.adapter.build_source(chat_id=chat_id, chat_type=chat_type, user_id=user_id,
                                           user_name=user_name, thread_id=thread_id)
        await self.adapter.handle_message(MessageEvent(text=text, source=source, message_id=mid or text))
        return self.runner.session_store._generate_session_key(source)

    def plugin(self):
        """The plugin's state (its Wakeshift object), as Hermes loaded it."""
        from hermes_cli.plugins import get_plugin_manager
        for fn in get_plugin_manager()._hooks.get("transform_tool_result", []):
            ws = getattr(fn, "__self__", None)
            if ws is not None and hasattr(ws, "wakers"):
                return ws
        raise AssertionError("the plugin isn't loaded")


@pytest.fixture
def fake():
    f = FakeWakeshift()
    yield f
    f.close()


async def _rig(monkeypatch, fake, *, platform="telegram", allow=True):
    from gateway.config import GatewayConfig, Platform, PlatformConfig
    from gateway.platforms.base import SendResult
    from gateway.run import GatewayRunner
    from gateway.session_authority import initialize_session_authority
    from hermes_cli.lifecycle import invoke_hook
    from hermes_cli.plugins import discover_plugins
    from hermes_constants import get_hermes_home

    _install(Path(get_hermes_home()), fake.url, allow)
    discover_plugins(force=True)
    monkeypatch.setenv("TELEGRAM_ALLOWED_USERS", "42,43")
    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "42,43")
    runner = GatewayRunner(GatewayConfig())
    if platform == "telegram":
        from plugins.platforms.telegram.adapter import TelegramAdapter
        adapter = TelegramAdapter(PlatformConfig(enabled=True, token="1:fixture", typing_indicator=False))
        plat = Platform.TELEGRAM
    else:
        from plugins.platforms.discord.adapter import DiscordAdapter
        adapter = DiscordAdapter(PlatformConfig(enabled=True, token="fixture-token", typing_indicator=False))
        plat = Platform.DISCORD
    sent = []

    async def send(chat_id, content, reply_to=None, metadata=None):
        sent.append({"chat_id": chat_id, "content": content})
        return SendResult(success=True, message_id=f"sent-{len(sent)}")
    monkeypatch.setattr(adapter, "send", send)
    for name in ("send_typing", "stop_typing"):
        if hasattr(adapter, name):
            async def _noop(*a, **k):
                return None
            monkeypatch.setattr(adapter, name, _noop)
    adapter.gateway_runner = runner
    runner.adapters = {plat: adapter}
    await initialize_session_authority(runner, profile_id="default", instance_id="owner")
    runner._wire_adapter_handlers(adapter)
    turns = []

    async def run_agent(**kw):
        """The model, stubbed: the plugin's hooks through Hermes's lifecycle,
        in the turn Hermes prepared (its session context bound)."""
        from hermes_cli.plugin_session import session_key
        message, session_id, source = kw["message"], kw["session_id"], kw["source"]

        def agent():
            invoke_hook("pre_llm_call", session_id=session_id, platform=source.platform.value,
                        user_message=message, conversation_history=[], is_first_turn=False, model="stub")
            note = None
            if "WATCH" in message:  # the app's watch tool, called in this turn
                beacon = fake.beacon()
                res = json.dumps({"result": "Watching u/ada/hook.", "_meta": {wire.META_KEY: beacon}})
                out = invoke_hook("transform_tool_result", tool_name="mcp__try__watch", args={}, result=res,
                                  session_id=session_id, tool_call_id="call_1")
                note = next((r for r in out if isinstance(r, str)), res)
            invoke_hook("on_session_end", session_id=session_id, completed=True)
            return note
        note = await asyncio.to_thread(agent)
        turns.append({"message": message, "session_id": session_id, "route": session_key(),
                      "note": note, "user_id": source.user_id})
        return {"final_response": f"reply {len(turns)}", "messages": [], "api_calls": 1}
    monkeypatch.setattr(runner, "_run_agent", run_agent)
    runner._gateway_loop = asyncio.get_running_loop()
    runner._running = True
    runner._install_plugin_message_injector()
    return Rig(runner, adapter, sent, turns, fake)


def _framed(turn):
    return turn["message"].startswith("A message from outside this conversation arrived through Wakeshift")


async def _no_new_turn(rig, n, within=3.0):
    deadline = time.monotonic() + within
    while time.monotonic() < deadline:
        assert len(rig.turns) == n, rig.turns[n:]
        await asyncio.sleep(0.1)


@pytest.mark.asyncio
@pytest.mark.parametrize("platform", ["telegram", "discord"])
async def test_a_dm_gets_a_framed_turn_and_its_reply(monkeypatch, fake, platform):
    from tools.approval import is_session_yolo_enabled
    rig = await _rig(monkeypatch, fake, platform=platform)
    key = await rig.human("WATCH the hook")
    await async_wait_until(lambda: len(rig.turns) == 1, timeout=15, what="the human turn")
    assert rig.turns[0]["route"] == key == f"agent:main:{platform}:dm:42"
    assert CONNECTED in rig.turns[0]["note"], rig.turns[0]["note"]
    await async_wait_until(lambda: rig.fake.connected.is_set(), timeout=15, what="the stream")
    rig.fake.sse.put(event("d1", subject="/yolo"))  # an app's text that would be a command
    await async_wait_until(lambda: len(rig.turns) == 2, timeout=20, what="the woken turn")
    woken = rig.turns[1]
    assert _framed(woken), woken["message"][:120]
    assert '"text":"/yolo"' in woken["message"]  # inside the framing, as data
    assert woken["route"] == key and woken["session_id"] == rig.turns[0]["session_id"]
    await async_wait_until(lambda: any(m["content"] == "reply 2" for m in rig.sent), timeout=10, what="the reply")
    posted = [m["content"] for m in rig.sent]
    assert posted[-2:] == ["reply 1", "reply 2"], posted  # Hermes's own first-contact notice may come first
    assert not any("Wakeshift" in m or "/yolo" in m for m in posted)  # the woken text itself is never posted
    assert all(m["chat_id"] == "42" for m in rig.sent)
    await async_wait_until(lambda: rig.fake.acked == ["d1"], timeout=10, what="the acknowledgement")
    assert not is_session_yolo_enabled(key)


@pytest.mark.asyncio
@pytest.mark.parametrize("platform,chat", [
    ("telegram", {"chat_id": "-1001", "chat_type": "group"}),
    ("telegram", {"chat_id": "-1002", "chat_type": "group", "thread_id": "7"}),  # a forum topic
    ("discord", {"chat_id": "900", "chat_type": "group"}),
    ("discord", {"chat_id": "901", "chat_type": "thread", "thread_id": "901"}),
])
async def test_groups_and_threads_start_no_turn(monkeypatch, fake, platform, chat):
    rig = await _rig(monkeypatch, fake, platform=platform)
    key = await rig.human("WATCH the hook", **chat)
    await async_wait_until(lambda: len(rig.turns) == 1, timeout=15, what="the human turn")
    assert rig.turns[0]["route"] == key and ":dm:" not in key
    assert NOT_DIRECT in rig.turns[0]["note"] and CONNECTED not in rig.turns[0]["note"]
    assert rig.fake.redeems == []  # never connected
    rig.fake.sse.put(event("d1"))
    await _no_new_turn(rig, 1)
    # Even handed straight to the waker, a group's key starts nothing.
    ws = rig.plugin()
    assert ws.wakers[platform](rig.turns[0]["session_id"], "A message from outside", "b") is False
    await _no_new_turn(rig, 1, within=1.5)


@pytest.mark.asyncio
async def test_a_text_led_by_a_slash_starts_no_turn(monkeypatch, fake):
    from tools.approval import is_session_yolo_enabled
    rig = await _rig(monkeypatch, fake)
    key = await rig.human("WATCH the hook")
    await async_wait_until(lambda: len(rig.turns) == 1, timeout=15, what="the human turn")
    assert CONNECTED in rig.turns[0]["note"]
    ws = rig.plugin()
    for text in ("/yolo", "  /yolo", "@file:notes.md\n/yolo"):
        assert ws.wakers["telegram"](rig.turns[0]["session_id"], text, "b") is False
    await _no_new_turn(rig, 1)
    assert not is_session_yolo_enabled(key)


@pytest.mark.asyncio
async def test_with_the_opt_in_off_no_turn_starts(monkeypatch, fake):
    rig = await _rig(monkeypatch, fake, allow=False)
    await rig.human("WATCH the hook")
    await async_wait_until(lambda: len(rig.turns) == 1, timeout=15, what="the human turn")
    note = rig.turns[0]["note"]
    assert "can't wake this conversation yet" in note and "allow_gateway_injection true" in note
    await async_wait_until(lambda: rig.fake.connected.is_set(), timeout=15, what="the stream")
    rig.fake.sse.put(event("d1"))
    await _no_new_turn(rig, 1, within=4)
    assert rig.fake.acked == []  # held, not dropped
