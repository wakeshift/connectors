import json
import os
import tempfile
import unittest
from pathlib import Path

import wakeshift
from wakeshift import plugin

DEFAULT = "https://app.wakeshift.ai"


class TrustedIssuers(unittest.TestCase):
    """Which Wakeshifts the plugin acts for (plugin.trusted_issuers): the
    issuers setting, else the public Wakeshift, plus config.json's."""

    def test_nothing_set_trusts_the_public_wakeshift_only(self):
        self.assertEqual(plugin.trusted_issuers(None, []), [DEFAULT])

    def test_the_setting_replaces_the_default(self):
        self.assertEqual(plugin.trusted_issuers(["https://app.staging.wakeshift.ai"], []), ["https://app.staging.wakeshift.ai"])
        self.assertEqual(plugin.trusted_issuers("https://a.example//, https://b.example:8443/", []),
                         ["https://a.example", "https://b.example:8443"])

    def test_an_empty_setting_trusts_nothing_from_it(self):
        self.assertEqual(plugin.trusted_issuers([], []), [])
        self.assertEqual(plugin.trusted_issuers("", []), [])

    def test_a_setting_of_the_wrong_type_never_falls_back_to_the_default(self):
        with self.assertLogs("wakeshift", "WARNING"):
            for bad in ({"issuers": DEFAULT}, 1, True):
                self.assertEqual(plugin.trusted_issuers(bad, []), [], bad)
            self.assertEqual(plugin.trusted_issuers({"x": 1}, ["https://pinned.example"]), ["https://pinned.example"])

    def test_config_json_adds_to_the_setting_or_the_default(self):
        self.assertEqual(plugin.trusted_issuers(None, ["https://app.staging.wakeshift.ai"]),
                         [DEFAULT, "https://app.staging.wakeshift.ai"])
        self.assertEqual(plugin.trusted_issuers(["https://a.example"], [DEFAULT, "https://a.example/"]),
                         ["https://a.example", DEFAULT])

    def test_only_origins_over_https_or_loopback_http(self):
        ok = {"https://app.wakeshift.ai": "https://app.wakeshift.ai",
              "HTTPS://App.Wakeshift.AI/": "https://app.wakeshift.ai",
              "http://localhost:8370": "http://localhost:8370",
              "http://127.0.0.1:54321": "http://127.0.0.1:54321",
              "http://[::1]:8370": "http://[::1]:8370",
              "https://localhost:8371": "https://localhost:8371"}
        for raw, want in ok.items():
            self.assertEqual(plugin.trusted_issuers([raw], []), [want], raw)
        bad = ["http://app.wakeshift.ai", "http://192.168.1.10:8370", "http://localhost.evil.example",
               "https://app.wakeshift.ai/path", "https://app.wakeshift.ai?x=1", "https://app.wakeshift.ai#f",
               "https://user@app.wakeshift.ai", "https://user:pw@app.wakeshift.ai", "https://app.wakeshift.ai:notaport",
               "ftp://app.wakeshift.ai", "app.wakeshift.ai", "//app.wakeshift.ai", "https://", "https://a b.example",
               "javascript:alert(1)", 5, None]
        for raw in bad:
            with self.assertLogs("wakeshift", "WARNING"):  # and says why
                self.assertEqual(plugin.trusted_issuers([raw], []), [], raw)
                self.assertEqual(plugin.trusted_issuers(None, [raw]), [DEFAULT], raw)

    def test_config_json_is_read_strictly(self):
        d = Path(tempfile.mkdtemp())
        self.assertEqual(wakeshift._pinned(d), [])  # none
        (d / "config.json").write_text(json.dumps({"issuers": ["https://a.example", 3], "mine": 1}))
        self.assertEqual(wakeshift._pinned(d), ["https://a.example"])
        for bad in (json.dumps({"issuers": "https://a.example"}), json.dumps(["https://a.example"])):
            (d / "config.json").write_text(bad)
            self.assertEqual(wakeshift._pinned(d), [], bad)
        (d / "config.json").write_text("{not json")
        with self.assertLogs("wakeshift", "WARNING"):
            self.assertEqual(wakeshift._pinned(d), [])


class Ctx:
    """Enough of Hermes's PluginContext for register()."""

    def __init__(self, settings=None, get_config=True, middleware=False):
        self.hooks = {}
        self.middleware = {}
        self.settings = settings or {}
        if get_config:
            self.get_config = lambda key, default=None: self.settings.get(key, default)
        if middleware:  # Hermes with middleware (0.22.5)
            self.register_middleware = lambda kind, fn: self.middleware.setdefault(kind, []).append(fn)

    def register_hook(self, name, fn):
        self.hooks[name] = fn

    def on_unload(self, fn):
        self.unload = fn


class Register(unittest.TestCase):
    """register() trusts what the settings say, else the public Wakeshift (the
    plugin's directory here has no config.json)."""

    def setUp(self):
        self.home = tempfile.mkdtemp()
        self.old = os.environ.get("HERMES_HOME")
        os.environ["HERMES_HOME"] = self.home

    def tearDown(self):
        if self.old is None:
            os.environ.pop("HERMES_HOME", None)
        else:
            os.environ["HERMES_HOME"] = self.old

    def issuers(self, ctx):
        self.assertFalse((Path(wakeshift.__file__).parent / "config.json").exists())
        wakeshift.register(ctx)
        ws = ctx.hooks["transform_tool_result"].__self__
        ctx.unload()
        return ws.issuers

    def test_no_settings(self):
        self.assertEqual(self.issuers(Ctx()), [DEFAULT])

    def test_a_hermes_without_plugin_settings(self):
        self.assertEqual(self.issuers(Ctx(get_config=False)), [DEFAULT])

    def test_the_harness_middleware(self):
        ctx = Ctx(middleware=True)
        wakeshift.register(ctx)
        ctx.unload()
        self.assertEqual(list(ctx.middleware), ["tool_request"])
        (mw,) = ctx.middleware["tool_request"]
        # Outside Hermes there's no registry: no schema, so nothing changes.
        self.assertIsNone(mw(tool_name="mcp__try__watch", args={}))
        # A Hermes without middleware still loads the plugin.
        self.assertEqual(self.issuers(Ctx()), [DEFAULT])

    def test_settings(self):
        self.assertEqual(self.issuers(Ctx({"issuers": ["https://app.staging.wakeshift.ai"]})), ["https://app.staging.wakeshift.ai"])
        with self.assertLogs("wakeshift", "WARNING"):
            self.assertEqual(self.issuers(Ctx({"issuers": ["http://evil.example"]})), [])


class InjectCtx:
    """PluginContext.inject_message, recorded."""

    def __init__(self, allowed=None, ok=True):
        self.plugin_id = "wakeshift"
        self.calls: list = []
        self.ok = ok
        if allowed is not None:
            self._gateway_injection_allowed = lambda: allowed

    def inject_message(self, content, role="user", *, session_key=None, origin=None):
        self.calls.append((content, session_key))
        return self.ok


class Routes:
    def __init__(self, routes):
        self.routes = routes

    def route_of(self, sid):
        return self.routes.get(sid, "")


class CliWaker(unittest.TestCase):
    """How a CLI chat is woken: its own REPL's queue where each `hermes chat`
    loads its plugins (0.22.2), a turn in the session its route key names
    where one gateway runs them all (0.22.3+), only while the user allows it,
    and never key-less there."""

    def test_a_chat_that_loads_its_own_plugins(self):
        ctx = InjectCtx()
        allowed = [False]
        wake = wakeshift.cli_waker(ctx, Routes({}), one_gateway=False, allowed=lambda: allowed[0])
        with self.assertLogs("wakeshift", "WARNING") as logs:
            self.assertFalse(wake("s1", "framed", "b"))  # not allowed: no turn on any surface
            self.assertFalse(wake("s1", "framed", "b"))  # said once
        self.assertEqual(len(logs.output), 1)
        self.assertIn(wakeshift.allow_command("wakeshift"), logs.output[0])
        self.assertEqual(ctx.calls, [])
        allowed[0] = True  # the user ran the command: no restart needed
        self.assertTrue(wake("s1", "framed", "b"))
        self.assertEqual(ctx.calls, [("framed", None)])

    def test_one_gateway_names_the_chat(self):
        ctx = InjectCtx()
        wake = wakeshift.cli_waker(ctx, Routes({"s1": "agent:main:local:dm:s1"}), one_gateway=True, allowed=lambda: True)
        self.assertTrue(wake("s1", "framed", "b"))
        self.assertEqual(ctx.calls, [("framed", "agent:main:local:dm:s1")])
        ctx.ok = False
        self.assertFalse(wake("s1", "framed", "b"))  # Hermes refused: not taken, offered again later

    def test_one_gateway_never_injects_without_a_key_or_the_users_leave(self):
        ctx = InjectCtx()
        allowed = [False]
        wake = wakeshift.cli_waker(ctx, Routes({"s1": "agent:main:local:dm:s1"}), one_gateway=True, allowed=lambda: allowed[0])
        with self.assertLogs("wakeshift", "WARNING") as logs:
            self.assertFalse(wake("s1", "framed", "b"))  # not allowed
            self.assertFalse(wake("s1", "framed", "b"))  # said once
        self.assertEqual(ctx.calls, [])
        self.assertEqual(len(logs.output), 1)
        self.assertIn(wakeshift.allow_command("wakeshift"), logs.output[0])
        allowed[0] = True  # the user ran the command: no restart needed
        with self.assertLogs("wakeshift", "WARNING"):
            self.assertFalse(wake("s2", "framed", "b"))  # no route: never key-less, which may reach another chat
        self.assertTrue(wake("s1", "framed", "b"))
        self.assertEqual(ctx.calls, [("framed", "agent:main:local:dm:s1")])

    def test_what_the_chat_is_told(self):
        why = wakeshift.cant_wake_text("wakeshift")
        self.assertIn("`hermes config set plugins.entries.wakeshift.allow_gateway_injection true`", why)
        self.assertIn("`docker exec -u hermes <container> hermes config set plugins.entries.wakeshift.allow_gateway_injection true`", why)
        self.assertNotIn("0.22.3", why)  # every chat, whatever the platform or version
        self.assertIn("without a restart", why)

    def test_allowed_is_hermess_own_check_and_fails_closed(self):
        self.assertTrue(wakeshift._injection_allowed(InjectCtx(allowed=True)))
        self.assertFalse(wakeshift._injection_allowed(InjectCtx(allowed=False)))
        self.assertFalse(wakeshift._injection_allowed(InjectCtx(allowed="yes")))
        ctx = InjectCtx()

        def boom():
            raise RuntimeError("config")
        ctx._gateway_injection_allowed = boom
        self.assertFalse(wakeshift._injection_allowed(ctx))
        self.assertFalse(wakeshift._injection_allowed(InjectCtx()))  # outside Hermes: no config to read

    def test_register_tells_every_chat_it_cant_be_woken_until_allowed(self):
        home = tempfile.mkdtemp()
        old = os.environ.get("HERMES_HOME")
        os.environ["HERMES_HOME"] = home
        try:
            for one_gateway in (False, True):
                allowed = [False]
                ctx = Ctx()
                ctx.plugin_id = "wakeshift"
                ctx._gateway_injection_allowed = lambda: allowed[0]
                real = wakeshift._one_gateway
                wakeshift._one_gateway = lambda one_gateway=one_gateway: one_gateway
                try:
                    wakeshift.register(ctx)
                finally:
                    wakeshift._one_gateway = real
                ws = ctx.hooks["transform_tool_result"].__self__
                ctx.unload()
                for platform in ("cli", "api_server"):
                    self.assertEqual(ws.cant_wake(platform), wakeshift.cant_wake_text("wakeshift"), (one_gateway, platform))
                # Telegram and Discord DMs are woken (and gated) only where one gateway names their key.
                for platform in ("telegram", "discord"):
                    self.assertEqual(platform in ws.wakers, one_gateway)
                    self.assertEqual(ws.cant_wake(platform), wakeshift.cant_wake_text("wakeshift") if one_gateway else "")
                self.assertEqual(ws.cant_wake("slack"), "")  # not woken at all: NOTE_UNSUPPORTED says so
                # Ink TUI chats are woken (and gated) only where one gateway runs them.
                self.assertEqual("tui" in ws.wakers, one_gateway)
                self.assertEqual(ws.cant_wake("tui"), wakeshift.cant_wake_text("wakeshift") if one_gateway else "")
                with self.assertLogs("wakeshift", "WARNING"):
                    self.assertFalse(ws.wakers["api_server"]("s1", "framed", "b"))
                allowed[0] = True  # read again on each check: no restart
                for platform in ("cli", "tui", "api_server"):
                    self.assertEqual(ws.cant_wake(platform), "", (one_gateway, platform))
        finally:
            if old is None:
                os.environ.pop("HERMES_HOME", None)
            else:
                os.environ["HERMES_HOME"] = old


if __name__ == "__main__":
    unittest.main()
