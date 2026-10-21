import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { copyFileSync, existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { HooksControl } from "../src/control.js";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { addScope, admits, agentOf, bindMode, DEFAULT_KEY, listensBeyondLoopback, NETWORK, releaseScope, restorePatch, takeover, type HooksConfig } from "../src/hooks.js";
import { routeOf } from "../src/index.js";
import { hooksPath, wakeBody, wakeResult } from "../src/wake.js";

const local = { network: false, allowNetwork: false };
const T = "f".repeat(64);

test("hooks off: turned on with the plugin's token, allowing no agent and no session but its own", () => {
  const t = takeover(undefined, T, local);
  assert.ok(t.ok);
  assert.deepEqual(t.next, { enabled: true, token: T, allowRequestSessionKey: true, defaultSessionKey: DEFAULT_KEY, allowedSessionKeyPrefixes: [DEFAULT_KEY], allowedAgentIds: [] });
  assert.equal(t.changes.owned, true);
  assert.ok(!JSON.stringify(t.changes).includes(T), "the token isn't recorded");
  // Settings there already (hooks off) are kept, and recorded as they were.
  const t2 = takeover({ enabled: false, token: "theirs", path: "/in", mappings: [{ id: "m" }], allowedAgentIds: ["ops"] }, T, local);
  assert.ok(t2.ok);
  assert.equal(t2.next.token, "theirs", "their token stays");
  assert.equal(t2.next.path, "/in");
  assert.deepEqual(t2.next.mappings, [{ id: "m" }]);
  assert.deepEqual(t2.next.allowedAgentIds, ["ops"]);
  assert.equal(t2.changes.set.token, undefined);
  assert.deepEqual(t2.changes.set.enabled, { had: true, prior: false, ours: true });
  assert.ok(!JSON.stringify(t2.changes).includes("theirs"));
});

test("a gateway that listens beyond this computer: hooks aren't turned on without the user's yes", () => {
  assert.deepEqual(takeover(undefined, T, { network: true, allowNetwork: false }), { ok: false, why: NETWORK });
  assert.ok(takeover(undefined, T, { network: true, allowNetwork: true }).ok);
});

test("the user's hooks on already: used only if nothing would loosen; changed only by adding", () => {
  for (const cur of [
    { enabled: true, token: "x" }, // no caller-chosen sessions
    { enabled: true, token: "x", allowRequestSessionKey: true }, // any session allowed
    { enabled: true, token: "x", allowRequestSessionKey: true, allowedSessionKeyPrefixes: [" "] }, // all-blank: any session
    { enabled: true, allowRequestSessionKey: true, allowedSessionKeyPrefixes: ["agent:"] }, // no token
  ] as HooksConfig[]) {
    const t = takeover(cur, T, local);
    assert.equal(t.ok, false, JSON.stringify(cur));
  }
  const cur = { enabled: true, token: "x", allowRequestSessionKey: true, allowedSessionKeyPrefixes: ["hook:", "agent:ops:"], allowedAgentIds: ["ops"] };
  const t = takeover(cur, T, { network: true, allowNetwork: false }); // theirs: they chose the exposure
  assert.ok(t.ok);
  assert.deepEqual(t.next, cur);
  assert.equal(t.changes.owned, false);
  assert.deepEqual(t.changes.set, {});
  const s = addScope(t.next, t.changes, "agent:main:main", "main")!;
  assert.deepEqual(s.next.allowedSessionKeyPrefixes, ["hook:", "agent:ops:", "agent:main:main"]);
  assert.deepEqual(s.next.allowedAgentIds, ["ops", "main"]);
  // A session a prefix of theirs admits already, and every agent allowed: nothing to add.
  assert.equal(addScope({ ...cur, allowedAgentIds: ["*"] }, t.changes, "agent:ops:x"), null);
  assert.equal(addScope({ ...cur, allowedAgentIds: undefined }, t.changes, "AGENT:OPS:x"), null, "prefixes match case-insensitively");
});

test("scoping a conversation in and out: exact keys and agents, only those the plugin added", () => {
  const t = takeover(undefined, T, local);
  assert.ok(t.ok);
  let { next: h, changes: ch } = addScope(t.next, t.changes, "agent:main:main")!;
  assert.deepEqual(h.allowedAgentIds, ["main"], "the agent from the session key");
  ({ next: h, changes: ch } = addScope(h, ch, "agent:main:telegram:42", "main")!);
  assert.deepEqual(h.allowedSessionKeyPrefixes, [DEFAULT_KEY, "agent:main:main", "agent:main:telegram:42"]);
  assert.deepEqual(h.allowedAgentIds, ["main"]);
  assert.equal(addScope(h, ch, "agent:main:main", "main"), null, "already allowed");
  // One goes: its key goes; the agent stays while another conversation uses it.
  const r = releaseScope(h, ch, { sessionKeys: ["agent:main:telegram:42"], agentIds: ["main"] })!;
  assert.deepEqual(r.next.allowedSessionKeyPrefixes, [DEFAULT_KEY, "agent:main:telegram:42"]);
  assert.deepEqual(r.next.allowedAgentIds, ["main"]);
  const r2 = releaseScope(r.next, r.changes, { sessionKeys: [], agentIds: [] })!;
  assert.deepEqual(r2.next.allowedSessionKeyPrefixes, [DEFAULT_KEY], "the plugin's own default key stays");
  assert.deepEqual(r2.next.allowedAgentIds, []);
  assert.equal(releaseScope(r2.next, r2.changes, { sessionKeys: [], agentIds: [] }), null);
  // An entry the user added meanwhile is never removed.
  const user = { ...h, allowedSessionKeyPrefixes: [...h.allowedSessionKeyPrefixes!, "agent:ops:"] };
  assert.ok(releaseScope(user, ch, { sessionKeys: [], agentIds: [] })!.next.allowedSessionKeyPrefixes!.includes("agent:ops:"));
});

test("undoing: what the plugin added and set goes; what the user changed since stays", () => {
  const t = takeover(undefined, T, local);
  assert.ok(t.ok);
  const { next, changes } = addScope(t.next, t.changes, "agent:main:main", "main")!;
  // As `openclaw config get hooks --json` prints them: the token redacted.
  const shown = { ...next, token: "__OPENCLAW_REDACTED__" };
  assert.deepEqual(restorePatch(shown, changes), {
    allowedSessionKeyPrefixes: null,
    allowedAgentIds: null,
    enabled: null,
    allowRequestSessionKey: null,
    defaultSessionKey: null,
    token: null,
  });
  // The user added their own prefix and agent, and set their own default key since.
  const theirs = { ...shown, allowedSessionKeyPrefixes: [...next.allowedSessionKeyPrefixes!, "agent:ops:"], allowedAgentIds: ["main", "ops"], defaultSessionKey: "hook:mine" };
  const p = restorePatch(theirs, changes)!;
  assert.deepEqual(p.allowedSessionKeyPrefixes, ["agent:ops:"]);
  assert.deepEqual(p.allowedAgentIds, ["ops"]);
  assert.equal("defaultSessionKey" in p, false, "theirs now");
  // Hooks that were off with settings of the user's: put back as they were.
  const t2 = takeover({ enabled: false, token: "theirs", allowedAgentIds: ["ops"], allowedSessionKeyPrefixes: ["agent:ops:"] }, T, local);
  assert.ok(t2.ok);
  const p2 = restorePatch({ ...t2.next, token: "__OPENCLAW_REDACTED__" }, t2.changes)!;
  assert.deepEqual(p2, { allowedSessionKeyPrefixes: ["agent:ops:"], enabled: false, allowRequestSessionKey: null, defaultSessionKey: null });
  // Nothing of the plugin's left: nothing to do.
  assert.equal(restorePatch(undefined, changes), null);
});

test("restore.mjs: the uninstaller's script prints the patch for openclaw config patch", () => {
  const dir = mkdtempSync(join(tmpdir(), "wsh-"));
  const t = takeover(undefined, T, local);
  assert.ok(t.ok);
  const { next, changes } = addScope(t.next, t.changes, "agent:main:main", "main")!;
  writeFileSync(join(dir, "hooks.json"), JSON.stringify(changes));
  const script = join(dir, "restore.mjs");
  copyFileSync(fileURLToPath(new URL("../src/hooks.js", import.meta.url)), script);
  const out = execFileSync(process.execPath, [script, join(dir, "hooks.json")], { input: JSON.stringify({ ...next, token: "__OPENCLAW_REDACTED__" }) }).toString();
  assert.deepEqual(JSON.parse(out), { hooks: restorePatch(next, changes) });
  // Hooks not set at all (config get's failure envelope): nothing printed.
  const none = execFileSync(process.execPath, [script, join(dir, "hooks.json")], { input: JSON.stringify({ ok: false, error: { code: "unset" } }) }).toString();
  assert.equal(none, "");
});

test("where the gateway listens: its --bind, else gateway.bind, else OpenClaw's default", () => {
  assert.equal(bindMode(["node", "dist/index.js", "gateway", "--bind", "lan", "--port", "1"], { bind: "loopback" }, false), "lan");
  assert.equal(bindMode(["node", "x", "--bind=tailnet"], undefined, false), "tailnet");
  assert.equal(bindMode(["node", "x"], { bind: "custom" }, false), "custom");
  assert.equal(bindMode(["node", "x"], undefined, false), "loopback");
  assert.equal(bindMode(["node", "x"], undefined, true), "auto");
  assert.equal(listensBeyondLoopback("loopback"), false);
  assert.equal(listensBeyondLoopback("custom", "127.0.0.1"), false);
  for (const m of ["lan", "auto", "tailnet"]) assert.equal(listensBeyondLoopback(m), true, m);
  assert.equal(listensBeyondLoopback("custom", "10.0.0.2"), true);
});

test("small helpers: prefixes, agents, paths and routes", () => {
  assert.ok(admits(["Agent:Main:"], "agent:main:main"));
  assert.ok(!admits(["", " "], "agent:main:main"));
  assert.equal(agentOf("agent:ops:telegram:1"), "ops");
  assert.equal(agentOf("hook:x"), undefined);
  assert.equal(hooksPath(undefined), "/hooks");
  assert.equal(hooksPath("in/"), "/in");
  assert.equal(hooksPath("//a//"), "/a");
  assert.deepEqual(routeOf({ channelId: "telegram", conversationId: "42", accountId: "b" }), { channel: "telegram", to: "42", accountId: "b" });
  assert.equal(routeOf({ channelId: "webchat", conversationId: "x" }), undefined, "OpenClaw's own surface");
  assert.equal(routeOf({ channelId: "telegram" }), undefined);
});

test("wake requests and what OpenClaw's answers mean", () => {
  const base = { sessionKey: "agent:main:main", agentId: "main", message: "m", idempotencyKey: "k" };
  assert.deepEqual(wakeBody(base), { message: "m", name: "Wakeshift", sessionKey: "agent:main:main", agentId: "main", sessionMode: "persistent", waitForCompletion: true, deliver: false });
  const routed = wakeBody({ ...base, route: { channel: "telegram", to: "42" } });
  assert.equal(routed.deliver, undefined);
  assert.deepEqual([routed.channel, routed.to], ["telegram", "42"]);
  assert.deepEqual(wakeResult(200, { ok: true, completion: { status: "ok" } }, false), { kind: "ran" });
  assert.deepEqual(wakeResult(200, { ok: true, completion: { status: "error" } }, false), { kind: "retry", fresh: true, why: "the woken turn ended error" });
  assert.equal(wakeResult(401, "Unauthorized", false).kind, "broken");
  assert.equal(wakeResult(404, "Not Found", false).kind, "broken");
  assert.equal(wakeResult(0, undefined, false).kind, "broken");
  assert.equal(wakeResult(400, { error: "sessionKey prefix not allowed" }, false).kind, "broken");
  assert.equal(wakeResult(400, { error: "invalid delivery target" }, true).kind, "route");
  for (const s of [408, 409, 429, 502, 503, 500]) assert.deepEqual(wakeResult(s, {}, false).kind, "retry", String(s));
  assert.equal((wakeResult(503, {}, false) as { fresh: boolean }).fresh, false);
});

test("a change to hooks.* between reading and writing: nothing written, and the user's entry is never recorded as the plugin's", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsc-"));
  let cfg: HooksConfig | undefined;
  let meddle: ((h: HooksConfig | undefined) => HooksConfig | undefined) | undefined;
  const c = new HooksControl({
    dir,
    log: { info() {}, warn() {} },
    network: () => false,
    allowNetwork: false,
    config: {
      hooks: () => cfg,
      mutateHooks: async (fn) => {
        if (meddle) cfg = meddle(cfg); // the user's edit lands just before ours
        meddle = undefined;
        cfg = fn(cfg);
      },
    },
  });
  assert.equal(await c.allow("agent:main:main", "main"), "");
  // The user allows agent:main:other themselves, between our read and our write.
  meddle = (h) => ({ ...h!, allowedSessionKeyPrefixes: [...h!.allowedSessionKeyPrefixes!, "agent:main:other"] });
  assert.equal(await c.allow("agent:main:other", "main"), "", "allowed (by theirs)");
  const rec = JSON.parse(readFileSync(join(dir, "hooks.json"), "utf8"));
  assert.ok(!rec.added.allowedSessionKeyPrefixes.includes("agent:main:other"), "theirs, not recorded as ours");
  assert.equal(cfg!.allowedSessionKeyPrefixes!.filter((p) => p === "agent:main:other").length, 1, "not added twice");
  // Uninstalling keeps it.
  assert.deepEqual(restorePatch(cfg, JSON.parse(readFileSync(join(dir, "hooks.json"), "utf8")))!.allowedSessionKeyPrefixes, ["agent:main:other"]);
});

test("a config snapshot that lags the file (a hot reload not yet applied): the session is still allowed, and recorded as written", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsc-"));
  let file: HooksConfig | undefined;
  const c = new HooksControl({
    dir,
    log: { info() {}, warn() {} },
    network: () => false,
    allowNetwork: false,
    config: {
      hooks: () => undefined, // never reloaded
      mutateHooks: async (fn) => {
        file = fn(file);
      },
    },
  });
  assert.equal(await c.allow("agent:main:main", "main"), "");
  assert.deepEqual(file!.allowedSessionKeyPrefixes, [DEFAULT_KEY, "agent:main:main"]);
  assert.deepEqual(file!.allowedAgentIds, ["main"]);
  const rec = JSON.parse(readFileSync(join(dir, "hooks.json"), "utf8"));
  assert.deepEqual(rec.added, { allowedSessionKeyPrefixes: [DEFAULT_KEY, "agent:main:main"], allowedAgentIds: ["main"] });
});

test("releasing a gone conversation: worked out from the file, recorded only once written", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsc-"));
  let file: HooksConfig | undefined;
  let fail = false;
  const c = new HooksControl({
    dir,
    log: { info() {}, warn() {} },
    network: () => false,
    allowNetwork: false,
    config: {
      hooks: () => undefined, // a snapshot that never caught up
      mutateHooks: async (fn) => {
        if (fail) throw new Error("disk full");
        file = fn(file);
      },
    },
  });
  assert.equal(await c.allow("agent:main:main", "main"), "");
  const rec = () => JSON.parse(readFileSync(join(dir, "hooks.json"), "utf8"));
  fail = true;
  await c.release({ sessionKeys: [], agentIds: [] });
  assert.ok(rec().added.allowedSessionKeyPrefixes.includes("agent:main:main"), "a failed write: still recorded as the plugin's");
  fail = false;
  await c.release({ sessionKeys: [], agentIds: [] });
  assert.deepEqual(file!.allowedSessionKeyPrefixes, [DEFAULT_KEY]);
  assert.deepEqual(file!.allowedAgentIds, []);
  assert.deepEqual(rec().added, { allowedSessionKeyPrefixes: [DEFAULT_KEY], allowedAgentIds: [] });
});

test("a hooks write that fails leaves no record of it: the record matches OpenClaw's config", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsc-"));
  let cfg: HooksConfig | undefined;
  let fail = true;
  const c = new HooksControl({
    dir,
    log: { info() {}, warn() {} },
    network: () => false,
    allowNetwork: false,
    config: {
      hooks: () => cfg,
      mutateHooks: async (fn) => {
        if (fail) throw new Error("disk full");
        cfg = fn(cfg);
      },
    },
  });
  // The first write (turning hooks on) fails: no record, nothing on.
  assert.match(await c.allow("agent:main:main", "main"), /didn't save.*disk full/);
  assert.equal(existsSync(join(dir, "hooks.json")), false);
  assert.equal(cfg, undefined);
  // It works; then a scope write fails: the record is as before it.
  fail = false;
  assert.equal(await c.allow("agent:main:main", "main"), "");
  const before = readFileSync(join(dir, "hooks.json"), "utf8");
  fail = true;
  assert.match(await c.allow("agent:main:other", "main"), /didn't save/);
  assert.equal(readFileSync(join(dir, "hooks.json"), "utf8"), before);
  assert.ok(!cfg!.allowedSessionKeyPrefixes!.includes("agent:main:other"));
  // Retried once it can: allowed, and recorded.
  fail = false;
  assert.equal(await c.allow("agent:main:other", "main"), "");
  assert.ok(cfg!.allowedSessionKeyPrefixes!.includes("agent:main:other"));
  assert.ok(JSON.parse(readFileSync(join(dir, "hooks.json"), "utf8")).added.allowedSessionKeyPrefixes.includes("agent:main:other"));
});
