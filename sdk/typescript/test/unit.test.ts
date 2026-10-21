import { test } from "node:test";
import assert from "node:assert/strict";
import { createPublicKey, verify } from "node:crypto";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { z } from "zod";
import { CONVERSATION_ARG, HARNESS_ARG, SESSION_ARG, UNKNOWN_CLIENT_WARNING, Wakeshift, WakeshiftError, isCode, parseKey, signAssertion } from "../src/index.js";
import { publicKeyOf } from "../src/key.js";

// RFC 8032, test 1: this seed's public key.
const seedHex = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60";
const pubHex = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a";
const seed = Buffer.from(seedHex, "hex").toString("base64url");
const KEY = `wst_test_ci-bot_${seed}`;

test("parseKey reads the mode, app ID and key", () => {
  const k = parseKey(KEY);
  assert.equal(k.mode, "test");
  assert.equal(k.appId, "ci-bot");
  assert.equal(Buffer.from(publicKeyOf(k), "base64url").toString("hex"), pubHex);
  // A seed with "_" and "-" in it: split on the first two underscores only.
  const odd = parseKey("wst_live_my-app_3q2-7__7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA");
  assert.equal(odd.mode, "live");
  assert.equal(odd.appId, "my-app");
});

test("parseKey refuses anything else", () => {
  for (const bad of [
    "",
    seed,
    `wsk_test_ci-bot_${seed}`,
    `wst_prod_ci-bot_${seed}`,
    `wst_test__${seed}`,
    `wst_test_Ci_${seed}`,
    `wst_test_ci-bot_${seed.slice(0, 42)}`,
    `wst_test_ci-bot_${seed}A`,
    `wst_test_ci-bot_${seed.slice(0, 42)}=`,
    "wst_test_ci-bot",
  ]) {
    assert.throws(() => parseKey(bad), /wakeshift:/, bad);
  }
});

test("signAssertion signs a short-lived EdDSA assertion for the core", () => {
  const k = parseKey(KEY);
  const now = Date.UTC(2026, 9, 18, 12, 0, 0);
  const tok = signAssertion(k, "https://app.wakeshift.ai", now);
  const [h, c, s] = tok.split(".");
  assert.deepEqual(JSON.parse(Buffer.from(h, "base64url").toString()), { alg: "EdDSA", typ: "app+jwt" });
  const claims = JSON.parse(Buffer.from(c, "base64url").toString());
  assert.equal(claims.iss, "ci-bot");
  assert.equal(claims.sub, "ci-bot");
  assert.equal(claims.aud, "https://app.wakeshift.ai/api/apps");
  assert.equal(claims.iat, now / 1000);
  assert.equal(claims.exp, now / 1000 + 120);
  assert.match(claims.jti, /^[A-Za-z0-9_-]{22}$/);
  assert.ok(!tok.includes("="), "base64url without padding");
  const pub = createPublicKey(k.privateKey);
  assert.ok(verify(null, Buffer.from(h + "." + c), pub, Buffer.from(s, "base64url")));
  assert.notEqual(JSON.parse(Buffer.from(signAssertion(k, "x", now).split(".")[1], "base64url").toString()).jti, claims.jti);
});

interface Sent {
  method: string;
  url: string;
  auth: string;
  body: any;
}

// fakeCore answers the app API with reply(path, body) and records requests.
function fakeCore(reply: (path: string, body: any) => { status?: number; json: unknown }) {
  const sent: Sent[] = [];
  const fetchFn = (async (url: string, init: RequestInit) => {
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    const headers = init.headers as Record<string, string>;
    sent.push({ method: init.method!, url, auth: headers.Authorization, body });
    const r = reply(new URL(url).pathname, body);
    return new Response(JSON.stringify(r.json), { status: r.status ?? 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { sent, fetchFn };
}

const subscribeReply = {
  subscription: { id: "ts_AAAAAAAAAAAAAAAAAAAAAA", topic: "ci/acme/api", filter: { branch: "main" }, once: false, expires_at: "2026-11-17T12:00:00Z" },
  session: "claude-code#1",
  addition: { text: "You're claude-code#1. Start the Monitor tool now.", meta: { "wakeshift/x": "y" } },
};

test("the core URL: given, else WAKESHIFT_CORE; only a live key falls back to production", () => {
  assert.equal(new Wakeshift({ key: KEY, core: "http://127.0.0.1:9/" }).core, "http://127.0.0.1:9");
  const was = process.env.WAKESHIFT_CORE;
  process.env.WAKESHIFT_CORE = "https://app.staging.wakeshift.ai";
  try {
    assert.equal(new Wakeshift({ key: KEY }).core, "https://app.staging.wakeshift.ai");
    delete process.env.WAKESHIFT_CORE;
    assert.throws(() => new Wakeshift({ key: KEY }), /no core URL for this test key: set WAKESHIFT_CORE/);
    process.env.WAKESHIFT_CORE = "";
    assert.throws(() => new Wakeshift({ key: KEY }), /no core URL/);
    assert.equal(new Wakeshift({ key: KEY.replace("wst_test_", "wst_live_") }).core, "https://app.wakeshift.ai");
  } finally {
    if (was === undefined) delete process.env.WAKESHIFT_CORE;
    else process.env.WAKESHIFT_CORE = was;
  }
  assert.throws(() => new Wakeshift({ key: "" }), /no app key/);
});

test("subscribe sends the call and options, and builds the tool result", async () => {
  const core = fakeCore(() => ({ json: subscribeReply }));
  const ws = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  const call = ws.callOf({ clientName: "claude-code", user: "ada", meta: {} });
  const sub = await ws.subscribe(call, "ci/acme/api", { filter: { branch: "main" }, replay: "5m", ttl: 3600 });
  const r = core.sent[0];
  assert.equal(r.method, "POST");
  assert.equal(r.url, "http://core.test/api/apps/v1/topics/subscribe");
  assert.match(r.auth, /^Bearer [\w-]+\.[\w-]+\.[\w-]+$/);
  assert.ok(!r.auth.includes(seed), "the key never goes over the wire");
  assert.deepEqual(r.body, { call: { client: "claude-code", user: "ada" }, topic: "ci/acme/api", once: false, filter: { branch: "main" }, replay: "5m", ttl: 3600 });
  assert.equal(sub.id, "ts_AAAAAAAAAAAAAAAAAAAAAA");
  assert.deepEqual(sub.addition, subscribeReply.addition);
  assert.equal(sub.session, "claude-code#1");
  assert.equal(sub.expiresAt.toISOString(), "2026-11-17T12:00:00.000Z");

  assert.deepEqual(sub.result("Watching CI on acme/api."), {
    content: [{ type: "text", text: "Watching CI on acme/api.\n\nYou're claude-code#1. Start the Monitor tool now." }],
    _meta: { "wakeshift/x": "y" },
  });
  const base: CallToolResult = { content: [{ type: "text", text: "mine" }], _meta: { mine: 1 }, structuredContent: { ok: true } };
  assert.deepEqual(sub.result(base), {
    content: [{ type: "text", text: "mine" }, { type: "text", text: "You're claude-code#1. Start the Monitor tool now." }],
    _meta: { mine: 1, "wakeshift/x": "y" },
    structuredContent: { ok: true },
  });
  assert.equal(base.content.length, 1, "the app's result isn't changed");
});

test("errors carry the API's code", async () => {
  const core = fakeCore(() => ({ status: 403, json: { error: { code: "private_topic", message: "that topic is another user's" } } }));
  const ws = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  await assert.rejects(ws.subscribe(ws.callOf({ clientName: "x" }), "u/ada/inbox"), (err: unknown) => {
    assert.ok(err instanceof WakeshiftError);
    assert.equal(err.status, 403);
    assert.equal(err.code, "private_topic");
    assert.ok(isCode(err, "private_topic"));
    assert.ok(!isCode(err, "bad_request"));
    return true;
  });
  const bare = fakeCore(() => ({ status: 502, json: "bad gateway" }));
  const ws2 = new Wakeshift({ key: KEY, core: "http://core.test", fetch: bare.fetchFn });
  await assert.rejects(ws2.publish("a", { text: "x" }), (err: unknown) => err instanceof WakeshiftError && err.status === 502 && err.code === "");
});

test("a 2xx without a JSON object (a proxy's page) is a WakeshiftError, not a TypeError", async () => {
  for (const body of ["", "<html>ok</html>", "null", "42"]) {
    const page = (async () => new Response(body, { status: 200, headers: { "Content-Type": "text/html" } })) as typeof fetch;
    const ws = new Wakeshift({ key: KEY, core: "http://core.test", fetch: page });
    await assert.rejects(ws.subscribe(ws.callOf({ clientName: "claude-code" }), "ci/x"), (err: unknown) => {
      assert.ok(err instanceof WakeshiftError, String(err));
      assert.equal(err.code, "bad_response");
      assert.equal(err.status, 200);
      assert.match(err.message, /http:\/\/core\.test answered 200 without a JSON object/);
      return true;
    });
  }
});

test("a core that doesn't answer: the error names it", async () => {
  const refused = (async () => {
    throw new TypeError("fetch failed", { cause: Object.assign(new Error("connect ECONNREFUSED 127.0.0.1:9"), { code: "ECONNREFUSED" }) });
  }) as typeof fetch;
  const ws = new Wakeshift({ key: KEY, core: "http://127.0.0.1:9", fetch: refused });
  await assert.rejects(ws.publish("ci/x", { text: "x" }), (err: unknown) => {
    assert.ok(err instanceof WakeshiftError);
    assert.equal(err.code, "unreachable");
    assert.equal(err.status, 0);
    assert.match(err.message, /^wakeshift: can't reach Wakeshift at http:\/\/127\.0\.0\.1:9 \(fetch failed: ECONNREFUSED\)\. Check that WAKESHIFT_CORE/);
    assert.ok(err.cause instanceof TypeError);
    return true;
  });

  const slow = ((_url: string, init: RequestInit) =>
    new Promise((_, reject) => init.signal!.addEventListener("abort", () => reject(init.signal!.reason)))) as typeof fetch;
  const ws2 = new Wakeshift({ key: KEY, core: "http://core.test", fetch: slow, timeoutMs: 20 });
  // A real request's socket keeps the process alive while it waits; this
  // fake fetch has none, and AbortSignal.timeout's timer doesn't, so hold one.
  const alive = setTimeout(() => {}, 5000);
  try {
    await assert.rejects(ws2.topicStatus("ci/x"), /can't reach Wakeshift at http:\/\/core\.test \(no answer within 0\.02s\)/);
  } finally {
    clearTimeout(alive);
  }
});

test("the other calls: paths, bodies and results", async () => {
  const replies: Record<string, unknown> = {
    "/api/apps/v1/topics/unsubscribe": { unsubscribed: 2 },
    "/api/apps/v1/topics/publish": { matched: 3, duplicate: false },
    "/api/apps/v1/subscriptions/ts_a%2Fb/send": { status: "queued" },
    "/api/apps/v1/topics/publish-urls": { id: "u1", topic: "hooks/x", url: "http://core.test/v1/publish/wsp_x" },
    "/api/apps/v1/topics/publish-urls/u1": { revoked: true },
    "/api/apps/v1/topics/annotate": { text: "waiting: 1", meta: { "wakeshift/connector": "beacon" } },
    "/api/apps/v1/topics/close": { closed: 4 },
    "/api/apps/v1/users/forget": { forgotten: true },
    "/api/apps/v1/topics/status": { topic: "ci/x", subscriptions: 3, connected: 2, via: { stream: 1, hooks: 1 } },
    "/api/apps/v1/subscriptions/ts_a%2Fb": { id: "ts_a/b", topic: "ci/x", status: "active", connected: true, via: "hooks", expires_at: "2026-11-17T12:00:00Z" },
  };
  const core = fakeCore((path) => ({ json: replies[path] ?? { error: { code: "not_found" } }, status: replies[path] ? 200 : 404 }));
  const ws = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  const call = ws.callOf({ clientName: "codex-mcp-client", meta: { threadId: "t1" } });

  assert.equal(await ws.unsubscribe(call), 2);
  assert.deepEqual(core.sent.at(-1)!.body, { call: { client: "codex-mcp-client", meta: { threadId: "t1" } } });
  await ws.unsubscribe(call, "ci/x");
  assert.equal(core.sent.at(-1)!.body.topic, "ci/x");

  assert.deepEqual(await ws.publish("ci/x", { id: "d1", text: "CI failed", data: { branch: "main" } }), { matched: 3, duplicate: false });
  assert.deepEqual(core.sent.at(-1)!.body, { topics: ["ci/x"], event: { id: "d1", text: "CI failed", data: { branch: "main" } } });
  await ws.publish(["a", "b"], { text: "x" });
  assert.deepEqual(core.sent.at(-1)!.body.topics, ["a", "b"]);

  assert.equal(await ws.send("ts_a/b", { text: "just you" }), "queued");
  assert.deepEqual(core.sent.at(-1)!.body, { event: { text: "just you" } });

  assert.deepEqual(await ws.publishUrl("hooks/x"), { id: "u1", topic: "hooks/x", url: "http://core.test/v1/publish/wsp_x" });
  await ws.revokePublishUrl("u1");
  assert.equal(core.sent.at(-1)!.method, "DELETE");
  assert.equal(core.sent.at(-1)!.body, undefined);

  const annotated = await ws.annotate(call, { content: [{ type: "text", text: "3 builds" }] });
  assert.deepEqual(annotated, { content: [{ type: "text", text: "3 builds" }, { type: "text", text: "waiting: 1" }], _meta: { "wakeshift/connector": "beacon" } });

  assert.deepEqual(await ws.topicStatus("ci/x"), { topic: "ci/x", subscriptions: 3, connected: 2, via: { stream: 1, poll: 0, hooks: 1, webhook: 0 } });
  assert.equal(core.sent.at(-1)!.method, "GET");
  assert.equal(core.sent.at(-1)!.url, "http://core.test/api/apps/v1/topics/status?topic=ci%2Fx");
  assert.deepEqual(await ws.subscriptionStatus("ts_a/b"), {
    id: "ts_a/b",
    topic: "ci/x",
    status: "active",
    connected: true,
    via: "hooks",
    expiresAt: new Date("2026-11-17T12:00:00Z"),
  });

  assert.equal(await ws.closeTopic("ci/x"), 4);
  assert.equal(await ws.forgetUser("ada"), true);
  assert.deepEqual(core.sent.at(-1)!.body, { user: "ada" });
});

test("endSubscription and unsubscribeUser: paths, bodies and results", async () => {
  const replies: Record<string, unknown> = {
    "/api/apps/v1/subscriptions/ts_a%2Fb/end": { ended: true },
    "/api/apps/v1/topics/unsubscribe": { unsubscribed: 2 },
  };
  const core = fakeCore((path) => ({ json: replies[path] ?? { error: { code: "not_found" } }, status: replies[path] ? 200 : 404 }));
  const ws = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  assert.equal(await ws.endSubscription("ts_a/b"), true);
  assert.equal(core.sent.at(-1)!.method, "POST");
  assert.equal(core.sent.at(-1)!.url, "http://core.test/api/apps/v1/subscriptions/ts_a%2Fb/end");
  assert.equal(await ws.unsubscribeUser("bob", "org/org1/deploys"), 2);
  assert.deepEqual(core.sent.at(-1)!.body, { user: "bob", topic: "org/org1/deploys" });
  await ws.unsubscribeUser("bob");
  assert.deepEqual(core.sent.at(-1)!.body, { user: "bob" });
});

test("ws.call packs extra: _meta, the client's name, the user, and the arguments ws.args adds", () => {
  const ws = new Wakeshift({ key: KEY, core: "http://core.test" });
  const server = { getClientVersion: () => ({ name: "Cursor", version: "1.0.0" }) };
  const extra = { _meta: { "x-codex-turn-metadata": { thread_id: "t1" } } };
  const args = { repo: "x", session_key: "wsc_2", harness: " grok-bot ", conversation_id: " bc-1 " };
  assert.deepEqual(ws.call(extra, { server, user: "ada", args }), {
    client: "Cursor",
    user: "ada",
    meta: extra._meta,
    session: "wsc_2",
    harness: "grok-bot",
    conversation: "bc-1",
  });
  assert.deepEqual(ws.call({}, { server: { server } }), { client: "Cursor" });
  assert.deepEqual(ws.call(undefined, { client: "cursor-nightly", session: "wsc_x", harness: "cursor" }), { client: "cursor-nightly", session: "wsc_x", harness: "cursor" });
  assert.deepEqual(ws.call({}, { client: "x", args: { session_key: 7, harness: "", conversation_id: "  " } }), { client: "x" }, "only strings, and not empty ones");
  assert.throws(() => ws.call(extra), /pass \{ server \}/);
});

test("ws.args: session_key, harness and conversation_id, optional strings the agent is told how to fill", () => {
  const ws = new Wakeshift({ key: KEY, core: "http://core.test" });
  assert.deepEqual(Object.keys(ws.args), [SESSION_ARG, HARNESS_ARG, CONVERSATION_ARG]);
  assert.deepEqual([SESSION_ARG, HARNESS_ARG, CONVERSATION_ARG], ["session_key", "harness", "conversation_id"]);
  const s = z.object(ws.args);
  assert.deepEqual(s.parse({}), {});
  const d = (k: keyof typeof ws.args) => ws.args[k].description ?? "";
  assert.match(d(SESSION_ARG), /secret: keep it to this conversation; don't share or log it/);
  assert.match(d(HARNESS_ARG), /^The program you're running in: .*cursor \(local CLI or IDE\), cursor-cloud \(Cursor cloud agent\), grok-bot, .*or your program's name if it isn't listed$/);
  assert.match(d(CONVERSATION_ARG), /printenv CURSOR_CONVERSATION_ID/);
});

async function connect(server: McpServer, clientName: string) {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: clientName, version: "1.0" });
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

test("tools are the app's: only the arguments it spreads, and only results it hands over change", async () => {
  const core = fakeCore((path) => {
    if (path.endsWith("/topics/subscribe")) return { json: subscribeReply };
    return { json: { text: "Wakeshift: 1 message(s) waiting." } };
  });
  const ws = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  const server = new McpServer({ name: "ci", version: "1.0" });
  server.registerTool("watch_ci", { inputSchema: { repo: z.string(), ...ws.args } }, async ({ repo, ...args }, extra) => {
    const sub = await ws.subscribe(ws.call(extra, { server, user: "ada", args }), `ci/${repo}`);
    return sub.result(`Watching CI on ${repo}.`);
  });
  server.registerTool("list_builds", { inputSchema: { repo: z.string(), ...ws.args } }, async ({ repo, ...args }, extra) =>
    ws.annotate(ws.call(extra, { server, user: "ada", args }), { content: [{ type: "text", text: `No builds on ${repo}.` }] }),
  );
  server.registerTool("ping", { inputSchema: { repo: z.string() } }, async () => ({ content: [{ type: "text", text: "pong" }] }));
  server.registerTool("bare", {}, async () => ({ content: [{ type: "text", text: "bare" }] }));
  const client = await connect(server, "Cursor");

  const { tools } = await client.listTools();
  const props = Object.fromEntries(tools.map((t) => [t.name, Object.keys(t.inputSchema.properties ?? {}).sort()]));
  assert.deepEqual(props, {
    watch_ci: ["conversation_id", "harness", "repo", "session_key"],
    list_builds: ["conversation_id", "harness", "repo", "session_key"],
    ping: ["repo"],
    bare: [],
  });

  const watched = (await client.callTool({
    name: "watch_ci",
    arguments: { repo: "acme", session_key: "wsc_1", harness: "grok-bot", conversation_id: "bc-6f1d" },
    _meta: { k: "v" },
  })) as CallToolResult;
  assert.equal(core.sent.length, 1);
  assert.deepEqual(core.sent[0].body.call, { client: "Cursor", user: "ada", meta: { k: "v" }, session: "wsc_1", harness: "grok-bot", conversation: "bc-6f1d" });
  assert.match((watched.content[0] as any).text, /^Watching CI on acme\.\n\nYou're claude-code#1/);

  const listed = (await client.callTool({ name: "list_builds", arguments: { repo: "acme", session_key: "wsc_1" } })) as CallToolResult;
  assert.deepEqual(core.sent[1].body, { call: { client: "Cursor", user: "ada", session: "wsc_1" } });
  assert.deepEqual(
    listed.content.map((c: any) => c.text),
    ["No builds on acme.", "Wakeshift: 1 message(s) waiting."],
  );
  const pinged = (await client.callTool({ name: "ping", arguments: { repo: "acme" } })) as CallToolResult;
  const bare = (await client.callTool({ name: "bare", arguments: {} })) as CallToolResult;
  assert.deepEqual(pinged.content, [{ type: "text", text: "pong" }]);
  assert.deepEqual(bare.content, [{ type: "text", text: "bare" }]);
  assert.equal(pinged._meta, undefined);
  assert.equal(core.sent.length, 2, "tools that don't call Wakeshift don't reach it");
});

// statelessCalls runs two tool calls through the MCP SDK's stateless
// pattern (a new server and transport per request), with a fake core whose
// replies say agent (or nothing, for an older core).
async function statelessCalls(agent: unknown, userAgent?: string, harness?: string) {
  const reply = agent === undefined ? subscribeReply : { ...subscribeReply, agent };
  const core = fakeCore((path) => (path.endsWith("/topics/subscribe") ? { json: reply } : { json: agent === undefined ? {} : { agent } }));
  const warnings: string[] = [];
  const ws = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn, onWarning: (m) => warnings.push(m) });
  const subs: any[] = [];
  const http = createServer(async (req, res) => {
    const server = new McpServer({ name: "ci", version: "1.0" });
    server.registerTool("watch", { inputSchema: { repo: z.string(), ...ws.args } }, async ({ repo, ...args }, extra) => {
      const sub = await ws.subscribe(ws.call(extra, { server, args }), `ci/${repo}`);
      subs.push(sub);
      return sub.result("Watching.");
    });
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => void transport.close());
    await server.connect(transport);
    await transport.handleRequest(req, res);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  try {
    const port = (http.address() as AddressInfo).port;
    const client = new Client({ name: "claude-code", version: "2.1" });
    const requestInit = userAgent ? { headers: { "User-Agent": userAgent } } : undefined;
    await client.connect(new StreamableHTTPClientTransport(new URL(`http://127.0.0.1:${port}/mcp`), { requestInit }));
    const args = harness ? { repo: "acme", harness } : { repo: "acme" };
    await client.callTool({ name: "watch", arguments: args });
    await client.callTool({ name: "watch", arguments: args });
    await client.close();
  } finally {
    http.close();
  }
  return { core, warnings, subs };
}

test("a stateless server forwards the User-Agent, and warns only when the core recognised nothing and the agent didn't say", async () => {
  // The core recognised the harness (here by its User-Agent): no warning.
  const known = await statelessCalls({ harness: "claude-code", via: "user_agent", claimed: null }, "claude-code/2.1.280 (cli)");
  const call = known.core.sent.find((s) => s.url.endsWith("/topics/subscribe"))!.body.call;
  assert.equal(call.client, "", "a stateless server has no client name");
  assert.equal(call.user_agent, "claude-code/2.1.280 (cli)", "the request's User-Agent goes to the core");
  assert.deepEqual(known.subs[0].agent, { harness: "claude-code", via: "user_agent", claimed: null });
  assert.deepEqual(known.warnings, []);

  // Nothing recognised: warned once, for both calls.
  const unknown = await statelessCalls({ harness: null, via: null, claimed: null });
  assert.deepEqual(unknown.warnings, [UNKNOWN_CLIENT_WARNING], "warned once");
  assert.deepEqual(unknown.subs[0].agent, { harness: null, via: null, claimed: null });
  assert.match(UNKNOWN_CLIENT_WARNING, /stateless Streamable HTTP/);
  assert.match(UNKNOWN_CLIENT_WARNING, /ws\.call\(extra, \{ client \}\)/);

  // The agent named a program Wakeshift doesn't know: it said, so no warning.
  const named = await statelessCalls({ harness: null, via: null, claimed: "homegrown" }, undefined, "Homegrown");
  assert.equal(named.core.sent[0].body.call.harness, "Homegrown");
  assert.deepEqual(named.warnings, []);

  // A core that doesn't say: no warning (it can't be told apart from a recognised one).
  const older = await statelessCalls(undefined);
  assert.deepEqual(older.warnings, []);
  assert.deepEqual(older.subs[0].agent, { harness: null, via: null, claimed: null });

  // A missing client name alone doesn't warn: the core decides.
  const quiet: string[] = [];
  const ws2 = new Wakeshift({ key: KEY, core: "http://core.test", fetch: known.core.fetchFn, onWarning: (m) => quiet.push(m) });
  assert.deepEqual(ws2.callOf({ clientName: " ", userAgent: "opencode/1.18.7" }), { client: " ", user_agent: "opencode/1.18.7" });
  assert.deepEqual(ws2.call({ _meta: { "claudecode/toolUseId": "toolu_1" } }, { client: "" }), { client: "", meta: { "claudecode/toolUseId": "toolu_1" } });
  assert.deepEqual(quiet, []);
});

test("addition reports what the core recognised, and warns once when it was nothing", async () => {
  const warnings: string[] = [];
  const core = fakeCore(() => ({ json: { text: "waiting", agent: { harness: null, via: null, claimed: null } } }));
  const ws = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn, onWarning: (m) => warnings.push(m) });
  const a = await ws.addition(ws.callOf({ clientName: "" }));
  assert.deepEqual(a, { text: "waiting", agent: { harness: null, via: null, claimed: null } });
  await ws.addition(ws.callOf({ clientName: "" }));
  assert.deepEqual(warnings, [UNKNOWN_CLIENT_WARNING]);
  const r = await ws.annotate(ws.callOf({ clientName: "" }), { content: [] });
  assert.equal(JSON.stringify(r).includes("agent"), false, "agent isn't added to the tool result");
});

test("an ignored claim shows: claimed differs from harness", async () => {
  const core = fakeCore(() => ({ json: { ...subscribeReply, agent: { harness: "cursor", via: "client", claimed: "claude-code" } } }));
  const ws = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  const sub = await ws.subscribe(ws.callOf({ clientName: "Cursor", harness: "claude-code" }), "ci/x");
  assert.deepEqual(sub.agent, { harness: "cursor", via: "client", claimed: "claude-code" });
  assert.equal(core.sent[0].body.call.harness, "claude-code");
});
