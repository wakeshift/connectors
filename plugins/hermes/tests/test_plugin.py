import json
import os
import stat
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
from pathlib import Path

from fake import HermesAPI, Wakeshift, event, until

from wakeshift import plugin, wire


def result(beacon, text="Watching u/ada/hook."):
    return json.dumps({"result": text, "_meta": {wire.META_KEY: beacon}})


class Plugin(unittest.TestCase):
    def setUp(self):
        self.ws = Wakeshift()
        self.api = HermesAPI()
        self.dir = Path(tempfile.mkdtemp())
        self.woken: list = []
        self.p = plugin.Wakeshift(state_dir=self.dir, issuers=[self.ws.url], listener_args={"poll_wait": 0.2, "backoff_max": 0.2},
                                  wakers={"api_server": plugin.ApiRuns(self.api.port, lambda: "api-key"),
                                          "cli": lambda s, text, batch: self.woken.append((s, text)) or True})

    def tearDown(self):
        self.p.stop()
        self.ws.close()
        self.api.close()

    def connect(self, sid="sess-1", platform="api_server"):
        self.p.turn_started(session_id=sid, platform=platform)
        return self.p.transform(tool_name="mcp__try__watch", result=result(self.ws.beacon()), session_id=sid, tool_call_id="call_1")

    def test_connects_from_a_beacon_and_wakes_with_a_run(self):
        out = self.connect()
        doc = json.loads(out)
        self.assertNotIn("_meta", doc)  # the model never sees the beacon
        self.assertTrue(doc["result"].endswith(plugin.NOTE_CONNECTED))
        self.assertTrue(plugin.NOTE_CONNECTED.startswith("Wakeshift: connected"))
        self.assertEqual(self.ws.redeems, [{"ticket": "t-1", "thread": "sess-1", "call": "call_1"}])
        links = list((self.dir / "links").glob("*.json"))
        self.assertEqual(len(links), 1)
        self.assertEqual(stat.S_IMODE(os.stat(links[0]).st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(os.stat(self.dir / "links").st_mode), 0o700)
        self.assertNotIn("sess-1", links[0].name)

        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put(event("d1", "Deploy finished"))
        # Busy (the turn that called watch hasn't ended): nothing yet.
        self.assertFalse(until(lambda: self.api.runs, within=0.5))
        self.p.turn_ended(session_id="sess-1")
        self.assertTrue(until(lambda: self.api.runs and self.ws.acked == ["d1"]))
        run = self.api.runs[0]
        h = {k.lower(): v for k, v in run["headers"].items()}
        self.assertEqual(run["path"], "/v1/runs")
        self.assertEqual(h["authorization"], "Bearer api-key")
        self.assertTrue(h["idempotency-key"].startswith("wakeshift-"))
        self.assertEqual(run["body"]["session_id"], "sess-1")
        self.assertEqual(run["body"]["author"], {"id": "wakeshift", "name": "Wakeshift", "is_bot": True})
        self.assertTrue(run["body"]["input"].startswith("A message from outside this conversation arrived through Wakeshift"))
        self.assertIn('<wakeshift_message from="app/try" via="Wakeshift" app="Try" trust="external">', run["body"]["input"])
        # The woken run is a turn too: the next message waits for it to end.
        self.p.turn_started(session_id="sess-1", platform="api_server")
        self.ws.sse.put(event("d2"))
        self.assertFalse(until(lambda: len(self.api.runs) > 1, within=0.5))
        self.p.turn_ended(session_id="sess-1")
        self.assertTrue(until(lambda: len(self.api.runs) == 2 and self.ws.acked == ["d1", "d2"]))

    def test_refused_run_is_not_acknowledged(self):
        self.api.status = 503
        self.connect()
        self.p.turn_ended(session_id="sess-1")
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put(event("d1"))
        self.assertTrue(until(lambda: len(self.api.runs) >= 2))  # tried again
        self.assertEqual(self.ws.acked, [])
        self.api.status = 202
        self.assertTrue(until(lambda: self.ws.acked == ["d1"]))
        keys = {r["headers"].get("Idempotency-Key") for r in self.api.runs}
        self.assertEqual(len(keys), 1)  # the same batch, the same key: it can't run twice

    def test_cli_sessions_are_woken_through_their_input(self):
        self.connect("cli-1", platform="cli")
        self.p.turn_ended(session_id="cli-1")
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put(event("d1"))
        self.assertTrue(until(lambda: self.woken))
        self.assertIn("The user didn't write it", self.woken[0][1])
        # Only in the input queue: not acknowledged (a CLI that quits now gets it again).
        self.assertFalse(until(lambda: self.ws.acked, within=0.8))
        self.assertEqual(len(self.woken), 1)  # and not queued twice
        self.p.turn_started(session_id="cli-1", platform="cli")  # its turn starts
        self.p.turn_ended(session_id="cli-1")
        self.assertTrue(until(lambda: self.ws.acked == ["d1"]))
        self.assertEqual(len(self.woken), 1)

    def test_a_superseded_listener_doesnt_save_its_rotated_link(self):
        self.connect()
        s = self.p._session("sess-1")
        old = s.listener
        had = self.p._load_link("sess-1")["link"]
        self.p._listen("sess-1", "api_server", had)  # a new listener takes over
        old.on_link(self.ws.link.replace("tok1", "tok-old"))
        self.assertEqual(self.p._load_link("sess-1")["link"], had)
        s.listener.on_link(self.ws.link.replace("tok1", "tok-new"))
        self.assertTrue(self.p._load_link("sess-1")["link"].endswith("link=tok-new"))

    def test_untrusted_beacons_are_stripped_silently(self):
        other = Wakeshift()
        try:
            self.p.turn_started(session_id="sess-1", platform="api_server")
            for tok in (other.beacon(), self.ws.beacon(thread="another-session"), self.ws.beacon(exp=1), "x.y.z"):
                out = self.p.transform(tool_name="mcp__try__watch", result=result(tok), session_id="sess-1", tool_call_id="c")
                self.assertEqual(json.loads(out), {"result": "Watching u/ada/hook."})
            self.assertEqual(self.ws.redeems + other.redeems, [])
        finally:
            other.close()
        self.assertIsNone(self.p.transform(result='{"result": "unrelated"}', session_id="sess-1"))

    def test_platforms_it_cant_wake(self):
        self.p.turn_started(session_id="tg-1", platform="telegram")
        out = json.loads(self.p.transform(tool_name="mcp__try__watch", result=result(self.ws.beacon()), session_id="tg-1", tool_call_id="c"))
        self.assertIn("can't wake telegram conversations", out["result"])
        self.assertFalse(out["result"].count("Wakeshift: connected"))
        self.assertEqual(self.ws.redeems, [])  # not redeemed: messages wait in the app's results

    def test_a_planted_beacon_is_never_redeemed(self):
        # Signed by the pinned Wakeshift and bound to nothing (Hermes tells
        # servers nothing), so only where it came from tells it apart from one
        # an attacker minted: never from a tool that isn't an MCP server's,
        # never from a document inside a result, never from another server
        # than the one pinned to its app.
        self.p.turn_started(session_id="sess-1", platform="api_server")
        page = json.dumps({"result": json.dumps({"_meta": {wire.META_KEY: self.ws.beacon(app="evil")}})})
        for tool, res in (("web_extract", result(self.ws.beacon(app="evil"))), ("terminal", result(self.ws.beacon(app="evil"))),
                          ("mcp__fetch__get", page)):
            with self.subTest(tool):
                out = self.p.transform(tool_name=tool, result=res, session_id="sess-1", tool_call_id="c")
                self.assertNotIn("connected", out or "")
        self.assertEqual(self.ws.redeems, [])
        self.connect()  # mcp__try__watch: pins try to "try"
        self.assertEqual(len(self.ws.redeems), 1)
        for tool, app in (("mcp__evil__lookup", "try"), ("mcp__try__watch", "evil")):
            with self.subTest(tool + " " + app):
                out = json.loads(self.p.transform(tool_name=tool, result=result(self.ws.beacon(app=app)), session_id="sess-1", tool_call_id="c"))
                self.assertIn("didn't connect this conversation", out["result"])
                self.assertNotIn("_meta", out)
        self.assertEqual(len(self.ws.redeems), 1)
        self.assertEqual(stat.S_IMODE(os.stat(self.dir / "servers.json").st_mode), 0o600)
        # Pins that can't be read fail closed: read as none, any server could
        # pin itself to any app.
        (self.dir / "servers.json").write_text("{not json")
        self.p.turn_started(session_id="sess-2", platform="api_server")
        out = json.loads(self.p.transform(tool_name="mcp__evil__lookup", result=result(self.ws.beacon()), session_id="sess-2", tool_call_id="c"))
        self.assertIn("servers.json can't be read", out["result"])
        self.assertEqual(len(self.ws.redeems), 1)
        self.assertEqual((self.dir / "servers.json").read_text(), "{not json")

    def test_redeem_refused(self):
        self.ws.redeem_answer = (403, {"error": {"code": "wrong_thread", "message": "x"}})
        out = json.loads(self.connect())
        self.assertIn("couldn't connect this conversation (the ticket was for another conversation)", out["result"])
        self.assertEqual(list((self.dir / "links").glob("*.json")) if (self.dir / "links").exists() else [], [])

    def test_redeem_outdated(self):
        msg = "Wakeshift's hooks for Hermes Agent are out of date. Ask the user whether to update them; only if they agree, run `curl -fsSL x/install/hermes.sh | sh`."
        self.ws.redeem_answer = (426, {"error": {"code": "update_required", "message": msg}})
        out = json.loads(self.connect())
        self.assertIn("Wakeshift: couldn't connect this conversation. " + msg, out["result"])
        redeem = [r for r in self.ws.requests if r[1] == "/v1/connector/redeem"][0]
        self.assertEqual(redeem[2].get("X-Wakeshift-Connector"), "wakeshift-hermes/dev hermes")

    def test_client_header(self):
        from wakeshift import wire
        was = wire.VERSION
        try:
            wire.VERSION = "2026.10.20.1435+abc1234"
            self.assertEqual(wire.client_header(), "wakeshift-hermes/2026.10.20.1435+abc1234 hermes")
            wire.VERSION = "not a version"
            self.assertEqual(wire.client_header(), "wakeshift-hermes/0 hermes")
        finally:
            wire.VERSION = was

    def test_connecting_again_hands_back_the_link_it_had(self):
        self.connect()
        first = self.ws.redeems[0]
        self.assertNotIn("supersedes", first)  # nothing to hand back the first time
        # A new Wakeshift session for the same conversation (its call didn't
        # pass its key): its beacon names the link this conversation had,
        # so Wakeshift ends the session that link belongs to.
        had = json.loads(next((self.dir / "links").glob("*.json")).read_text())["link"]
        self.connect()
        self.assertEqual(self.ws.redeems[1].get("supersedes"), had)

    def test_resumes_after_a_restart(self):
        self.connect()
        self.p.turn_started(session_id="sess-1", platform="api_server")  # stopped mid-turn
        self.p.stop()
        again = plugin.Wakeshift(state_dir=self.dir, issuers=[self.ws.url], listener_args={"poll_wait": 0.2},
                                 wakers={"api_server": plugin.ApiRuns(self.api.port, lambda: "k")})
        try:
            self.assertEqual(again.resume(("api_server",)), 1)
            self.assertTrue(until(lambda: again.sessions["sess-1"].listener and again.sessions["sess-1"].listener.state == "sse"))
            self.ws.sse.put(event("d9"))
            self.assertTrue(until(lambda: self.ws.acked == ["d9"]))  # idle since the restart
            self.assertEqual(again.resume(("cli",)), 0)
        finally:
            again.stop()

    def test_a_lost_ack_never_starts_a_second_run(self):
        # The run starts, every ack fails, the plugin restarts and the message
        # comes back, alone and then with a new one: it's acknowledged, and only
        # the new one starts a run (Hermes's Idempotency-Key replays only the
        # same batch, and only on the API server).
        self.ws.ack_fail = 1000
        self.connect()
        self.p.turn_ended(session_id="sess-1")
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put(event("d1"))
        self.assertTrue(until(lambda: len(self.api.runs) == 1))
        self.assertTrue(until(lambda: self.ws.ack_fail < 999))  # it tried to ack
        self.p.stop()
        self.ws.sse.put(None)
        self.ws.ack_fail = 0
        again = plugin.Wakeshift(state_dir=self.dir, issuers=[self.ws.url], listener_args={"poll_wait": 0.2, "backoff_max": 0.2},
                                 wakers={"api_server": plugin.ApiRuns(self.api.port, lambda: "k")})
        try:
            again.resume(("api_server",))
            self.assertTrue(until(self.ws.connected.is_set))
            self.ws.sse.put(event("d1"))
            self.assertTrue(until(lambda: "d1" in self.ws.acked))
            again.turn_ended(session_id="sess-1")  # the first run's turn ended
            self.ws.sse.put(event("d1"))
            self.ws.sse.put(event("d2", "new"))
            self.assertTrue(until(lambda: "d2" in self.ws.acked))
            self.assertEqual(len(self.api.runs), 2)
            self.assertIn("new", self.api.runs[1]["body"]["input"])
            self.assertNotIn("ev_d1", self.api.runs[1]["body"]["input"])
            started = list((self.dir / "started").glob("*.json"))
            self.assertEqual(len(started), 1)
            self.assertEqual(stat.S_IMODE(os.stat(started[0]).st_mode), 0o600)
        finally:
            again.stop()

    def test_one_turn_start_at_a_time(self):
        # Two listeners of one session (a replaced one still finishing) hand over
        # at once: one run starts at a time, and a message never starts two.
        active, most, woken = [0], [0], []
        lock = threading.Lock()

        def slow(sid, text, batch):
            with lock:
                active[0] += 1
                most[0] = max(most[0], active[0])
            time.sleep(0.3)
            with lock:
                active[0] -= 1
            woken.append(text)
            threading.Timer(0.05, lambda: (self.p.turn_started(session_id=sid, platform="cli"), self.p.turn_ended(session_id=sid))).start()
            return True

        self.p.wakers["cli"] = slow
        s = self.p._session("cli-2")
        s.platform = "cli"
        l = types.SimpleNamespace(stopped=False, stop=lambda: None)
        s.listener = l
        results = []

        def hand_over(ds):  # as a listener does: offered again until done
            end = time.time() + 8
            while time.time() < end:
                done = self.p._deliver(s, l, ds)
                if done:
                    return results.append(done)
                time.sleep(0.05)

        ts = [threading.Thread(target=hand_over, args=(ds,)) for ds in ([event("d1")], [event("d1")], [event("d2")])]
        for t in ts:
            t.start()
        for t in ts:
            t.join(10)
        self.assertEqual(sorted(sum(results, [])), ["d1", "d1", "d2"])  # each done, after starting once
        self.assertEqual(most[0], 1)
        self.assertEqual(len(woken), 2)  # d1 once, d2 once

    def test_a_turn_in_another_process_holds_messages_back(self):
        # The gateway holds the session's stream; `hermes chat --resume` runs a
        # turn in it from another process (same state dir): nothing starts
        # until that turn ends.
        self.p._save_link("shared-1", "api_server", self.ws.link)
        other = subprocess.Popen([sys.executable, "-c", (
            "import sys; sys.path.insert(0, %r)\n"
            "from wakeshift import plugin\n"
            "p = plugin.Wakeshift(state_dir=%r, issuers=[], wakers={})\n"
            "p.turn_started(session_id='shared-1', platform='cli'); print('started', flush=True)\n"
            "sys.stdin.readline(); p.turn_ended(session_id='shared-1'); print('ended', flush=True)\n"
        ) % (str(Path(__file__).resolve().parents[1]), str(self.dir))], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        try:
            self.assertEqual(other.stdout.readline().strip(), "started")
            woken = []
            self.p.wakers["api_server"] = lambda sid, text, batch: woken.append(text) or True
            s = self.p._session("shared-1")
            s.platform = "api_server"
            l = types.SimpleNamespace(stopped=False, stop=lambda: None)
            s.listener = l
            t = threading.Thread(target=lambda: self.p._deliver(s, l, [event("d1")]))
            t.start()
            self.assertFalse(until(lambda: woken, within=1.5))  # the other process's turn is running
            other.stdin.write("\n")
            other.stdin.flush()
            self.assertEqual(other.stdout.readline().strip(), "ended")
            self.assertTrue(until(lambda: woken, within=5))
            t.join(5)
        finally:
            other.kill()
            other.wait()

    def test_a_failed_start_is_never_acknowledged(self):
        # Two hand-overs of one message while its start fails: neither says it's
        # done (so it isn't acknowledged), and nothing is recorded as started.
        tries = []

        def failing(sid, text, batch):
            time.sleep(0.2)
            tries.append(batch)
            return False

        self.p.wakers["cli"] = failing
        s = self.p._session("cli-3")
        s.platform = "cli"
        l = types.SimpleNamespace(stopped=False, stop=lambda: None)
        s.listener = l
        results = []
        ts = [threading.Thread(target=lambda: results.append(self.p._deliver(s, l, [event("d1")]))) for _ in range(2)]
        for t in ts:
            t.start()
        for t in ts:
            t.join(10)
        self.assertEqual(results, [[], []])
        self.assertEqual(len(tries), 2)
        with self.p._locked("cli-3"):
            self.assertEqual(self.p._state("cli-3")["started"], {})

    def test_a_link_on_another_host_is_refused(self):
        other = Wakeshift()
        try:
            self.ws.redeem_answer = (200, {"url": other.link})  # not the pinned Wakeshift
            out = json.loads(self.connect())
            self.assertIn("its stream link wasn't on the Wakeshift this plugin trusts", out["result"])
            self.assertFalse(list((self.dir / "links").glob("*.json")) if (self.dir / "links").exists() else [])
            self.assertFalse(until(lambda: other.requests, within=0.5))  # the token went nowhere
        finally:
            other.close()

    def test_only_its_own_directories_are_made_private(self):
        home = Path(tempfile.mkdtemp())
        os.chmod(home, 0o755)
        state = home / "wakeshift"
        state.mkdir(mode=0o755)
        os.chmod(state, 0o755)  # there already, as the user left it
        p = plugin.Wakeshift(state_dir=state, issuers=[], wakers={})
        p._write(state / "links" / "x.json", b"{}")
        self.assertEqual(stat.S_IMODE(os.stat(home).st_mode), 0o755)
        self.assertEqual(stat.S_IMODE(os.stat(state).st_mode), 0o755)
        self.assertEqual(stat.S_IMODE(os.stat(state / "links").st_mode), 0o700)  # its own
        fresh = plugin.Wakeshift(state_dir=home / "new" / "wakeshift", issuers=[], wakers={})
        fresh._write(fresh.dir / "keys" / "k.json", b"{}")
        self.assertEqual(stat.S_IMODE(os.stat(fresh.dir).st_mode), 0o700)
        um = os.umask(0)
        os.umask(um)
        self.assertEqual(stat.S_IMODE(os.stat(home / "new").st_mode), 0o777 & ~um)  # a parent: the default mode
        with self.assertRaises(OSError):
            p._write(home / "elsewhere.json", b"{}")

    def test_private_files_are_written_fresh(self):
        p = self.dir / "links" / "x.json"
        p.parent.mkdir(parents=True)
        p.write_text("old")
        os.chmod(p, 0o644)  # left loose by something else
        self.p._write(p, b"secret")
        self.assertEqual(p.read_bytes(), b"secret")
        self.assertEqual(stat.S_IMODE(os.stat(p).st_mode), 0o600)
        self.assertEqual([f.name for f in p.parent.iterdir()], ["x.json"])  # no temp file left

    def test_a_revoked_link_is_forgotten(self):
        self.connect()
        self.assertTrue(until(self.ws.connected.is_set))
        self.ws.sse.put({"type": "hub.closed", "reason": "revoked", "note": "x"})
        self.assertTrue(until(lambda: not list((self.dir / "links").glob("*.json"))))

    def test_keys_are_cached(self):
        self.connect()
        self.connect()
        self.assertEqual(len([r for r in self.ws.requests if r[1] == "/oauth/jwks"]), 1)


if __name__ == "__main__":
    unittest.main()
