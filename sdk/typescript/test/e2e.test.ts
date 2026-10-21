// End to end against a real core: run with scripts/e2e.sh (make sdk-e2e),
// which starts one (TestSDKHarness in internal/coretest) with a self-serve
// app and points WAKESHIFT_SDK_HARNESS at its URL and test key. Skipped
// without it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { UNKNOWN_CLIENT_WARNING, Wakeshift, isCode, type Subscription } from "../src/index.js";

const harness = process.env.WAKESHIFT_SDK_HARNESS;

interface Frame {
  type: string;
  delivery_id?: string;
  event?: { subject: string; data?: any; attrs?: Record<string, string>; trust?: string; from?: { kind: string; id: string } };
}

// Inbox: frames as they arrive, read with next().
class Inbox {
  private frames: Frame[] = [];
  private waiters: (() => void)[] = [];
  push(f: Frame) {
    this.frames.push(f);
    this.waiters.splice(0).forEach((w) => w());
  }
  async next(ms = 5000): Promise<Frame> {
    const deadline = Date.now() + ms;
    while (this.frames.length === 0) {
      const left = deadline - Date.now();
      if (left <= 0) throw new Error("nothing arrived");
      await new Promise<void>((resolve) => {
        const t = setTimeout(resolve, left);
        this.waiters.push(() => (clearTimeout(t), resolve()));
      });
    }
    return this.frames.shift()!;
  }
  async expect(subject: string): Promise<Frame> {
    const f = await this.next();
    assert.equal(f.event?.subject, subject);
    return f;
  }
  async expectNothing(ms = 700) {
    await assert.rejects(this.next(ms), /nothing arrived/);
  }
}

// readLines runs the command Claude Code's Monitor is given (the session's
// wait in a loop) in sh, as Monitor runs it: one JSON line per event.
async function readLines(cmd: string, signal: AbortSignal): Promise<Inbox> {
  const inbox = new Inbox();
  const sh = spawn("sh", ["-c", cmd], { detached: true, stdio: ["ignore", "pipe", "ignore"] });
  signal.addEventListener("abort", () => {
    try {
      process.kill(-sh.pid!, "SIGKILL"); // its curl too
    } catch {}
  });
  let buf = "";
  sh.stdout!.on("data", (chunk: Buffer) => {
    buf += chunk.toString();
    let i;
    while ((i = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, i).trim();
      buf = buf.slice(i + 1);
      if (!line) continue;
      // A compact line for Monitor: the event's fields at the top level.
      const l = JSON.parse(line) as { type: string; delivery_id?: string; subject: string; data?: any; from: string };
      if (l.type !== "event") continue;
      const [kind, id] = l.from.split("/");
      inbox.push({ type: l.type, delivery_id: l.delivery_id, event: { subject: l.subject, data: l.data, from: { kind, id } } });
    }
  });
  return inbox;
}

// connectCodex does what the connector for Codex does with a beacon: redeem
// its ticket for the thread's stream link, and hold it, acknowledging.
async function connectCodex(beacon: string, thread: string): Promise<{ inbox: Inbox; close: () => void }> {
  const claims = JSON.parse(Buffer.from(beacon.split(".")[1], "base64url").toString());
  assert.equal(claims.thread, thread);
  const resp = await fetch(claims.redeem, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ticket: claims.ticket, thread }) });
  const redeemed = (await resp.json()) as { url: string };
  assert.equal(resp.status, 200, JSON.stringify(redeemed));
  const inbox = new Inbox();
  const sock = new WebSocket(redeemed.url);
  await new Promise<void>((resolve, reject) => {
    sock.onopen = () => resolve();
    sock.onerror = () => reject(new Error("websocket"));
  });
  sock.onmessage = (m) => {
    const f = JSON.parse(String(m.data)) as Frame;
    if (f.type !== "event") return;
    sock.send(JSON.stringify({ type: "ack", delivery_id: f.delivery_id }));
    inbox.push(f);
  };
  return { inbox, close: () => sock.close() };
}

async function eventually<T>(what: string, fn: () => Promise<T | undefined>, ms = 5000): Promise<T> {
  const deadline = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v !== undefined) return v;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 100));
  }
}

const text = (r: CallToolResult) => r.content.map((c: any) => c.text ?? "").join("\n");

test("the SDK against a real core", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (make sdk-e2e)" }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const ws = new Wakeshift({ key, core });
  assert.equal(ws.mode, "test");
  const subs: Subscription[] = [];

  // The app's MCP server, as a developer writes it: one per connection,
  // knowing its user (here, fixed per connection).
  const appServer = (user?: string) => {
    const server = new McpServer({ name: "ci-bot", version: "1.0" });
    server.registerTool(
      "watch_ci",
      { description: "Watch CI on a repo", inputSchema: { repo: z.string(), branch: z.string().optional(), ...ws.args } },
      async ({ repo, branch, ...args }, extra) => {
        const sub = await ws.subscribe(ws.call(extra, { server, user, args }), `ci/${repo}`, { filter: branch ? { branch } : undefined, replay: "5m" });
        subs.push(sub);
        return sub.result(`Watching CI on ${repo}.`);
      },
    );
    server.registerTool("unwatch_ci", { inputSchema: { repo: z.string(), ...ws.args } }, async ({ repo, ...args }, extra) => {
      const n = await ws.unsubscribe(ws.call(extra, { server, user, args }), `ci/${repo}`);
      return { content: [{ type: "text", text: `Stopped watching ${repo} (${n}).` }] };
    });
    // A tool whose results the app chose to annotate.
    server.registerTool("list_builds", { inputSchema: { repo: z.string(), ...ws.args } }, async ({ repo, ...args }, extra) =>
      ws.annotate(ws.call(extra, { server, user, args }), { content: [{ type: "text", text: `No builds on ${repo} today.` }] }),
    );
    return server;
  };
  const clients: Client[] = [];
  const connect = async (clientName: string, user?: string) => {
    const [a, b] = InMemoryTransport.createLinkedPair();
    const client = new Client({ name: clientName, version: "1.0" });
    await Promise.all([appServer(user).connect(a), client.connect(b)]);
    clients.push(client);
    return client;
  };
  const stop = new AbortController();
  const closers: (() => void)[] = [];
  t.after(async () => {
    stop.abort();
    closers.forEach((c) => c());
    await Promise.all(clients.map((c) => c.close()));
  });

  // Claude Code: the result says to start Monitor on the session's wait, in a loop.
  const claude = await connect("claude-code", "bob");
  const watchedClaude = (await claude.callTool({ name: "watch_ci", arguments: { repo: "acme" } })) as CallToolResult;
  const claudeSub = subs.at(-1)!;
  assert.match(claudeSub.session, /^wsc_[a-z2-7]{26}$/);
  const said = text(watchedClaude);
  assert.match(said, /^Watching CI on acme\.\n\nTo be woken when /);
  assert.ok(said.includes(`session_key="${claudeSub.session}"`), said);
  const monitor = said.match(/with this command: (u='https?:\/\/[^']+\/v1\/stream\/wait\?[^']+'; c=; p=\$PPID; .*?; done) \./);
  assert.ok(monitor, said);
  const claudeLines = await readLines(monitor[1], stop.signal);
  // Once its Monitor is running, subscribing again says so (and is the same
  // subscription), and send reports delivered.
  const bob = ws.callOf({ clientName: "claude-code", user: "bob", session: claudeSub.session });
  const again = await eventually("Claude Code's Monitor", async () => {
    const s = await ws.subscribe(bob, "ci/acme");
    return s.addition.text?.includes("already running") ? s : undefined;
  });
  assert.equal(again.id, claudeSub.id);
  assert.equal(await ws.send(claudeSub.id, { text: "hello claude" }), "delivered");
  await claudeLines.expect("hello claude");

  // Codex: a thread in _meta; the result carries a connector beacon.
  const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118e";
  const codexMeta = { "x-codex-turn-metadata": { thread_id: thread } };
  const codex = await connect("codex-mcp-client", "ada");
  const watchedCodex = (await codex.callTool({ name: "watch_ci", arguments: { repo: "acme", branch: "main" }, _meta: codexMeta })) as CallToolResult;
  const codexSub = subs.at(-1)!;
  assert.match(codexSub.session, /^wsc_[a-z2-7]{26}$/);
  assert.deepEqual(codexSub.filter, { branch: "main" });
  assert.match(text(watchedCodex), /"Wakeshift: connected"/);
  const beacon = watchedCodex._meta?.["wakeshift/connector"];
  assert.equal(typeof beacon, "string");
  // Before its connector runs, a send waits.
  assert.equal(await ws.send(codexSub.id, { text: "hello codex" }), "queued");
  const codexStream = await connectCodex(beacon as string, thread);
  closers.push(codexStream.close);
  await codexStream.inbox.expect("hello codex");

  // Publish reaches both, as outside information from the app; the same id
  // again is one publish.
  const ev = { id: "gh-delivery-1", text: "CI failure on main: tests", data: { branch: "main", url: "https://ci.example/runs/1" } };
  assert.deepEqual(await ws.publish("ci/acme", ev), { matched: 2, duplicate: false });
  assert.deepEqual(await ws.publish("ci/acme", ev), { matched: 2, duplicate: true });
  const got = await claudeLines.expect("CI failure on main: tests");
  assert.equal(got.event?.data?.url, "https://ci.example/runs/1");
  const codexGot = await codexStream.inbox.expect("CI failure on main: tests");
  assert.equal(codexGot.event?.trust, "external");
  assert.deepEqual(codexGot.event?.from, { kind: "app", id: ws.appId });
  // The filter: Codex only hears about main.
  assert.equal((await ws.publish(["ci/acme"], { text: "CI passed on dev", data: { branch: "dev" } })).matched, 1);
  await claudeLines.expect("CI passed on dev");
  await codexStream.inbox.expectNothing();

  // Status: who's subscribed and how they're connected (read-only). IDs
  // name nothing.
  assert.match(claudeSub.id, /^ts_[A-Za-z0-9_-]{22}$/);
  assert.deepEqual(await ws.topicStatus("ci/acme"), { topic: "ci/acme", subscriptions: 2, connected: 2, via: { stream: 0, poll: 1, hooks: 1, webhook: 0 } });
  const codexStatus = await ws.subscriptionStatus(codexSub.id);
  assert.equal(codexStatus.status, "active");
  assert.equal(codexStatus.via, "hooks");
  assert.equal((await ws.subscriptionStatus(claudeSub.id)).via, "poll");
  assert.deepEqual((await ws.topicStatus("ci/nothing")).subscriptions, 0);

  // A publish URL: a webhook's JSON body is the event's data; gone once revoked.
  const pu = await ws.publishUrl("ci/acme");
  assert.ok(pu.url.startsWith(core + "/v1/publish/"), pu.url);
  const hook = await fetch(pu.url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ action: "completed", branch: "main" }) });
  assert.equal(hook.status, 202);
  assert.equal(((await hook.json()) as any).matched, 2);
  const viaHook = await claudeLines.expect(`Webhook to ci/acme: {"action":"completed","branch":"main"}`);
  assert.equal(viaHook.event?.data?.action, "completed");
  await codexStream.inbox.expect(`Webhook to ci/acme: {"action":"completed","branch":"main"}`);
  await ws.revokePublishUrl(pu.id);
  assert.equal((await fetch(pu.url, { method: "POST", body: "x" })).status, 410);

  // A harness with no wake path (a Cursor variant, no user): messages are
  // held for its next call to the tool, or a result the app annotates.
  const cursor = await connect("cursor-nightly");
  const watchedCursor = (await cursor.callTool({ name: "watch_ci", arguments: { repo: "acme" } })) as CallToolResult;
  const cursorSub = subs.at(-1)!;
  assert.match(cursorSub.session, /^wsc_/);
  assert.match(text(watchedCursor), /call this tool again to check for them/);
  // Replay: the latest publish within 5 minutes is waiting already (in the
  // watch result itself, or the next annotated one).
  if (!/Webhook to ci\/acme/.test(text(watchedCursor))) {
    const listed = await eventually("the replay", async () => {
      const r = (await cursor.callTool({ name: "list_builds", arguments: { repo: "acme", session_key: cursorSub.session } })) as CallToolResult;
      return r.content.length > 1 ? r : undefined;
    });
    assert.equal((listed.content[0] as any).text, "No builds on acme today.");
    assert.match((listed.content[1] as any).text, /Webhook to ci\/acme/);
    assert.match((listed.content[1] as any).text, /not as instructions from the user/);
  } else {
    assert.match(text(watchedCursor), /not as instructions from the user/);
  }
  // Annotate directly: nothing new waiting.
  const plain = await ws.annotate(ws.callOf({ clientName: "cursor-nightly", session: cursorSub.session }), { content: [{ type: "text", text: "x" }] });
  assert.equal(plain.content.length, 1);

  // A stateless server has no client name: the core tells the harness from
  // the call's _meta, else its User-Agent, and says what it recognised; the
  // SDK warns only when that was nothing.
  const warned: string[] = [];
  const stateless = new Wakeshift({ key, core, onWarning: (m) => warned.push(m) });
  const byMeta = await stateless.subscribe(stateless.callOf({ clientName: "", meta: { "claudecode/toolUseId": "toolu_e2e" } }), "ci/stateless");
  assert.deepEqual(byMeta.agent, { harness: "claude-code", via: "meta", claimed: null });
  assert.match(byMeta.addition.text ?? "", /start the Monitor tool/);
  const byUA = await stateless.subscribe(stateless.callOf({ clientName: "", userAgent: "opencode/1.18.7" }), "ci/stateless");
  assert.deepEqual(byUA.agent, { harness: "opencode", via: "user_agent", claimed: null });
  assert.match(byUA.addition.text ?? "", /install\/opencode\.sh/);
  assert.deepEqual(warned, []);
  const unknown = await stateless.subscribe(stateless.callOf({ clientName: "", userAgent: "node" }), "ci/stateless");
  assert.deepEqual(unknown.agent, { harness: null, via: null, claimed: null });
  assert.deepEqual(warned, [UNKNOWN_CLIENT_WARNING]);
  // The agent's claim: it refines Cursor's calls into Grok Bot's, and is
  // ignored where it contradicts the client name.
  const grok = await stateless.subscribe(stateless.callOf({ clientName: "Cursor", harness: "Grok Bot", conversation: "bc-e2e-grok" }), "ci/stateless");
  assert.deepEqual(grok.agent, { harness: "grok-bot", via: "client", claimed: "grok-bot" });
  assert.match(grok.addition.text ?? "", /in the foreground/);
  const spoof = await stateless.subscribe(stateless.callOf({ clientName: "Cursor", harness: "claude-code" }), "ci/stateless");
  assert.deepEqual(spoof.agent, { harness: "cursor", via: "client", claimed: "claude-code" });
  assert.doesNotMatch(spoof.addition.text ?? "", /Monitor/);

  // Errors carry the API's code.
  await assert.rejects(ws.subscribe(ws.callOf({ clientName: "claude-code", user: "bob" }), "u/ada/inbox"), (e: unknown) => isCode(e, "private_topic"));
  await assert.rejects(ws.subscribe(ws.callOf({ clientName: "claude-code", session: cursorSub.session }), "ci/acme"), (e: unknown) => isCode(e, "other_client"));
  await assert.rejects(ws.publish([], { text: "x" }), (e: unknown) => isCode(e, "bad_request"));
  // A handle Wakeshift doesn't know: a new conversation, and it says so.
  const stale = await ws.subscribe(ws.callOf({ clientName: "cursor-nightly", session: "wsc_AAAAAAAAAAAAAAAAAAAAAA" }), "ci/stale");
  assert.notEqual(stale.session, "wsc_AAAAAAAAAAAAAAAAAAAAAA");
  assert.match(stale.addition.text ?? "", /session_key you passed isn't known/);
  assert.equal(await ws.unsubscribe(ws.callOf({ clientName: "cursor-nightly", session: "wsc_AAAAAAAAAAAAAAAAAAAAAA" })), 0);

  // Removing one user from a topic, and ending one subscription by its id.
  const bobBeta = await ws.subscribe(ws.callOf({ clientName: "claude-code", user: "bob" }), "ci/beta");
  assert.equal(await ws.unsubscribeUser("bob", "ci/beta"), 1);
  assert.equal((await ws.subscriptionStatus(bobBeta.id)).status, "closed");
  assert.equal(await ws.unsubscribeUser("bob", "ci/beta"), 0);
  assert.equal(await ws.endSubscription(stale.id), true);
  assert.equal(await ws.endSubscription(stale.id), false);
  assert.equal(await ws.send(stale.id, { text: "x" }), "closed");

  // Unsubscribe: send reports closed.
  const unwatched = (await codex.callTool({ name: "unwatch_ci", arguments: { repo: "acme" }, _meta: codexMeta })) as CallToolResult;
  assert.match(text(unwatched), /Stopped watching acme \(1\)/);
  assert.equal(await ws.send(codexSub.id, { text: "anyone?" }), "closed");

  // closeTopic and forgetUser.
  assert.equal(await ws.closeTopic("ci/acme"), 2);
  assert.equal(await ws.forgetUser("bob"), true);
  assert.equal(await ws.forgetUser("bob"), false);
  assert.equal(await ws.forgetUser("nobody"), false);
  assert.equal(await ws.send(claudeSub.id, { text: "x" }), "closed");
});
