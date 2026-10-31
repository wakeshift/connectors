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

    def __init__(self, settings=None, get_config=True):
        self.hooks = {}
        self.settings = settings or {}
        if get_config:
            self.get_config = lambda key, default=None: self.settings.get(key, default)

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

    def test_settings(self):
        self.assertEqual(self.issuers(Ctx({"issuers": ["https://app.staging.wakeshift.ai"]})), ["https://app.staging.wakeshift.ai"])
        with self.assertLogs("wakeshift", "WARNING"):
            self.assertEqual(self.issuers(Ctx({"issuers": ["http://evil.example"]})), [])


if __name__ == "__main__":
    unittest.main()
