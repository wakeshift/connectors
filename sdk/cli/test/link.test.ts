import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { canonical, eventsNamed, listedSubscriptions, norm, notifiesSentence, parseTrigger, sample } from "../src/inspect.js";
import { addKeyLink, newAppLink, openBrowser, PRODUCTION, resolveOrigin } from "../src/link.js";
import { messageFrom } from "../src/mcp.js";
import { clip, redact } from "../src/redact.js";
import { sessionKeyOf, waitUrlOf } from "../src/stream.js";
import { TestCtx } from "./fakes.js";

const PK = "O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik";

test("the confirm link carries the public key, ID and name in its fragment", () => {
  assert.equal(newAppLink(PRODUCTION, PK, "acme-ci-dev", "Acme CI"), `https://app.wakeshift.ai/new-app#pk=${PK}&id=acme-ci-dev&name=Acme%20CI`);
  assert.equal(newAppLink("http://localhost:8370", PK, "a-dev", "A & B (dev)"), `http://localhost:8370/new-app#pk=${PK}&id=a-dev&name=A%20%26%20B%20(dev)`);
  assert.equal(addKeyLink(PRODUCTION, "acme-ci-dev", PK), `https://app.wakeshift.ai/apps/acme-ci-dev/keys#pk=${PK}`);
  // Nothing before the fragment names the key: no server log or Referer sees it.
  const u = new URL(newAppLink(PRODUCTION, PK, "a-dev", "A"));
  assert.equal(u.search, "");
  assert.equal(new URLSearchParams(u.hash.slice(1)).get("pk"), PK);
  assert.equal(new URLSearchParams(u.hash.slice(1)).get("name"), "A");
});

test("the origin is production's unless something says otherwise, and https", () => {
  assert.equal(resolveOrigin(undefined, undefined, undefined), "https://app.wakeshift.ai");
  assert.equal(resolveOrigin("https://example.com/x/", undefined, undefined), "https://example.com");
  assert.equal(resolveOrigin(undefined, "http://localhost:8370", "https://ignored.example"), "http://localhost:8370");
  assert.equal(resolveOrigin(undefined, undefined, "http://127.0.0.1:9"), "http://127.0.0.1:9");
  assert.throws(() => resolveOrigin("http://example.com", undefined, undefined), /https/);
  assert.throws(() => resolveOrigin("not a url", undefined, undefined), /isn't a URL/);
  assert.throws(() => resolveOrigin("https://u:p@example.com", undefined, undefined), /password/);
});

test("the browser: $BROWSER, the platform's opener with a screen, never in CI or over SSH", () => {
  const url = newAppLink(PRODUCTION, PK, "a-dev", "A & B");
  const with_ = (env: Record<string, string>, platform: NodeJS.Platform) => {
    const c = new TestCtx({ WAKESHIFT_NO_BROWSER: undefined, ...env });
    c.platform = platform;
    const r = openBrowser(c, url);
    c.cleanup();
    return { r, opened: c.opened };
  };
  assert.deepEqual(with_({ BROWSER: "/usr/bin/firefox:/usr/bin/chromium" }, "linux").opened, [{ cmd: "/usr/bin/firefox", args: [url] }]);
  assert.deepEqual(with_({}, "darwin").opened, [{ cmd: "open", args: [url] }]);
  assert.deepEqual(with_({ SSH_CONNECTION: "1 2 3 4" }, "darwin"), { r: "skipped", opened: [] });
  assert.deepEqual(with_({}, "linux"), { r: "skipped", opened: [] });
  assert.deepEqual(with_({ DISPLAY: ":0" }, "linux").opened, [{ cmd: "xdg-open", args: [url] }]);
  assert.deepEqual(with_({ WAYLAND_DISPLAY: "wayland-0" }, "freebsd").opened, [{ cmd: "xdg-open", args: [url] }]);
  // Not cmd /c start, whose parser would split the link at &.
  assert.deepEqual(with_({}, "win32").opened, [{ cmd: "rundll32", args: ["url.dll,FileProtocolHandler", url] }]);
  assert.deepEqual(with_({ CI: "true" }, "darwin"), { r: "skipped", opened: [] });
  assert.deepEqual(with_({ WAKESHIFT_NO_BROWSER: "1" }, "darwin"), { r: "skipped", opened: [] });
  const c = new TestCtx({ WAKESHIFT_NO_BROWSER: undefined });
  c.platform = "darwin";
  assert.equal(openBrowser(c, url, true), "skipped");
  assert.deepEqual(c.opened, []);
  c.cleanup();
});

test("redact removes keys, links, session keys, assertions and bearer tokens", () => {
  const s = [
    "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV",
    "wst_test_ci-bot_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
    "https://core/v1/stream/wait?link=abc.def&timeout=50s",
    'session_key="wsc_abcdef123"',
    "eyJhbGciOiJFZERTQSJ9.eyJpc3MiOiJ4In0.c2ln",
    "Authorization: Bearer sk_live_abcdefghij",
  ].join("\n");
  const r = redact(s, ["sk_live_abcdefghij"]);
  for (const bad of ["1116qJ", "AAAAAAAAAAAAA", "abc.def", "wsc_abcdef123", "eyJpc3MiOiJ4In0", "sk_live_abcdefghij"]) assert.ok(!r.includes(bad), `${bad} in\n${r}`);
  assert.match(r, /timeout=50s/);
  assert.equal(clip("x".repeat(10), 4), "xxxx…");
});

test("the notifies sentence, as every SDK writes it (subscribes.json)", (t) => {
  const golden = join(dirname(fileURLToPath(import.meta.url)), "../../../typescript/test/subscribes.json");
  assert.deepEqual(notifiesSentence("Watch a build."), null);
  const s = notifiesSentence(
    "Watch. This tool is how Shipyard notifies you in this conversation of these events: A deploy started; A deploy finished: e.g. rolled back. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it.",
  );
  assert.deepEqual(s?.descriptions, ["A deploy started", "A deploy finished: e.g. rolled back"]);
  assert.deepEqual(eventsNamed(s!, [{ name: "deploy.started", description: "A deploy started." }, { name: "other", description: "Other" }]).map((e) => e.name), ["deploy.started"]);
  if (!existsSync(golden)) return t.skip("not in Wakeshift's repository");
  const cases = JSON.parse(readFileSync(golden, "utf8")).cases as { name: string; description: string; events: string[]; want: string }[];
  for (const c of cases) {
    const got = notifiesSentence(c.want);
    const added = c.want !== c.description;
    const own = notifiesSentence(c.description);
    if (!added && !own) {
      assert.equal(got, null, c.name);
      continue;
    }
    assert.ok(got, c.name);
    if (added) assert.deepEqual(got.descriptions, [...new Set(c.events.map(norm))], c.name);
    else assert.deepEqual(got.descriptions, [], c.name);
  }
});

test("list_subscriptions' lines, and payloads that fit a schema", () => {
  const listed = listedSubscriptions(
    "Your subscriptions to this app's events:\n- 0199-a: build.finished {\"number\":412,\"pipeline\":\"p\"}, wakes a conversation, since 2026-11-03\n- 0199-b: x.y {\"a\":{\"b\":1}}, delivered to example.com, since 2026\n\nStop one with unsubscribe_events and its id.",
  );
  assert.deepEqual(listed, [
    { id: "0199-a", event: "build.finished", arguments: { number: 412, pipeline: "p" } },
    { id: "0199-b", event: "x.y", arguments: { a: { b: 1 } } },
  ]);
  assert.deepEqual(
    sample(
      {
        type: "object",
        properties: { s: { type: "string" }, n: { type: "integer", minimum: 3 }, e: { enum: ["a", "b"] }, o: { type: "string" }, u: { type: "string", format: "uri" }, l: { type: "array", items: { type: "string" }, minItems: 1 }, ok: { type: ["null", "boolean"] } },
        required: ["s", "n", "e", "u", "l", "ok"],
      },
      "wsverify-1",
    ),
    { s: "wsverify-1", n: 3, e: "a", u: "https://example.com/wsverify-1", l: ["wsverify-1"], ok: false },
  );
  assert.equal(canonical({ b: 1, a: [2, { d: 1, c: 2 }] }), '{"a":[2,{"c":2,"d":1}],"b":1}');
  assert.equal(sample({ type: "integer", maximum: -3 }, "m"), -3);
  assert.equal(sample({ type: "integer", exclusiveMaximum: 0 }, "m"), -1);
  assert.equal(sample({ type: "integer", exclusiveMinimum: 0 }, "m"), 1);
  assert.equal(sample({ type: "number", minimum: 1.5, maximum: 9 }, "m"), 1.5);
  assert.equal(sample({ type: "integer", minimum: -5, maximum: 5 }, "m"), 0);
});

test("--trigger", () => {
  assert.deepEqual(parseTrigger(undefined), { kind: "synthetic" });
  assert.deepEqual(parseTrigger("tool:start_build"), { kind: "tool", name: "start_build", args: {} });
  assert.deepEqual(parseTrigger('tool:start_build:{"branch":"main:x"}'), { kind: "tool", name: "start_build", args: { branch: "main:x" } });
  assert.deepEqual(parseTrigger("cmd:curl -X POST localhost:3000/hook"), { kind: "cmd", command: "curl -X POST localhost:3000/hook" });
  for (const bad of ["tool:", "tool:x:[1]", "tool:x:{", "cmd:", "start_build"]) assert.equal(typeof parseTrigger(bad), "string", bad);
});

test("the wait URL and session key in Wakeshift's block, and JSON-RPC answers", () => {
  const block = `start this command in the background now: u='https://app.wakeshift.ai/v1/stream/wait?link=a.b'; c=; p=$PPID; … Pass session_key="wsc_Ab-9" next time.`;
  assert.equal(waitUrlOf(block), "https://app.wakeshift.ai/v1/stream/wait?link=a.b");
  assert.equal(sessionKeyOf(block), "wsc_Ab-9");
  assert.equal(waitUrlOf("nothing here"), null);
  assert.deepEqual(messageFrom("text/event-stream", 'event: message\ndata: {"jsonrpc":"2.0","id":3,"result":{"ok":1}}\n\n', 3)?.result, { ok: 1 });
  assert.deepEqual(messageFrom("application/json", '{"jsonrpc":"2.0","id":3,"result":{"ok":2}}', 3)?.result, { ok: 2 });
});
