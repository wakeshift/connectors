// Stand-ins for the tests: a Wakeshift that checks assertions as the core
// does (kid, signature, audience), an MCP server whose tools subscribe
// through it, and a Ctx that records everything printed and never opens a
// browser.

import { createPublicKey, randomBytes, verify as edVerify } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Ctx, RunOptions, RunResult } from "../src/context.js";
import { realRun } from "../src/context.js";
import { keyCodeOf } from "../src/key.js";
import { KEY_PATTERN } from "../src/redact.js";

export interface App {
  id: string;
  name: string;
  /** Every key the app has, by kid: base64url public keys. */
  keys: Map<string, string>;
  manifest: Record<string, unknown>;
}

export interface Sub {
  id: string;
  event: string;
  args: Record<string, unknown>;
  link: string;
  user: string;
  ended: boolean;
  /** Ends after its first delivery (call.subscribe's once). */
  once?: boolean;
}

function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve) => {
    let b = "";
    req.on("data", (d) => (b += d));
    req.on("end", () => resolve(b));
  });
}

function send(res: ServerResponse, status: number, body: unknown, type = "application/json") {
  res.writeHead(status, { "content-type": type });
  res.end(typeof body === "string" ? body : JSON.stringify(body));
}

const canonical = (v: unknown): string =>
  Array.isArray(v)
    ? `[${v.map(canonical).join(",")}]`
    : v && typeof v === "object"
      ? `{${Object.keys(v as object)
          .sort()
          .map((k) => `${JSON.stringify(k)}:${canonical((v as Record<string, unknown>)[k])}`)
          .join(",")}}`
      : JSON.stringify(v);

export class FakeCore {
  server: Server;
  origin = "";
  apps: App[] = [];
  subs: Sub[] = [];
  /** Events waiting on each stream link. */
  queues = new Map<string, string[]>();
  emits: unknown[] = [];
  requests: { method: string; path: string; auth: string; body: string }[] = [];
  /** Answers GET /app with this instead, when set (an error, say). */
  appAnswer: ((req: IncomingMessage) => { status: number; body: unknown } | null) | null = null;
  /** Called on each unauthenticated GET /app (a poll before the console confirmed). */
  onUnknownPoll: ((n: number) => void) | null = null;
  private unknownPolls = 0;
  /** Extra subscriptions the status counts (other conversations). */
  others = 0;
  /** Confirms emits but doesn't deliver them (a lost wake). */
  dropEmits = false;
  /** The last occurrence delivered for each event and arguments (what replay delivers again). */
  private last = new Map<string, string>();

  constructor() {
    this.server = createServer((req, res) => void this.handle(req, res));
  }

  async start(): Promise<this> {
    await new Promise<void>((r) => this.server.listen(0, "127.0.0.1", r));
    const a = this.server.address();
    this.origin = `http://127.0.0.1:${typeof a === "object" && a ? a.port : 0}`;
    return this;
  }

  stop(): Promise<void> {
    this.server.closeAllConnections?.();
    return new Promise((r) => this.server.close(() => r()));
  }

  addApp(id: string, name: string, manifest: Record<string, unknown> = { description: "", events: [] }): App {
    const app = { id, name, keys: new Map<string, string>(), manifest };
    this.apps.push(app);
    return app;
  }

  /** What the console's confirm does: the app gets the public key. */
  register(appId: string, publicKey: string, kid: string): void {
    let app = this.apps.find((a) => a.id === appId);
    if (!app) app = this.addApp(appId, appId);
    app.keys.set(kid, publicKey);
  }

  /** The app and key an assertion is signed by, as the core checks it; null if it isn't. */
  private authenticate(auth: string): { app: App; kid: string } | null {
    const token = auth.replace(/^Bearer /, "");
    const [h, c, s] = token.split(".");
    if (!h || !c || !s) return null;
    let header: { kid?: string; alg?: string };
    let claims: { aud?: string; exp?: number };
    try {
      header = JSON.parse(Buffer.from(h, "base64url").toString());
      claims = JSON.parse(Buffer.from(c, "base64url").toString());
    } catch {
      return null;
    }
    if (header.alg !== "EdDSA" || claims.aud !== this.origin + "/api/apps") return null;
    for (const app of this.apps) {
      const pub = app.keys.get(header.kid ?? "");
      if (!pub) continue;
      const key = createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: pub }, format: "jwk" });
      if (edVerify(null, Buffer.from(h + "." + c), key, Buffer.from(s, "base64url"))) return { app, kid: header.kid! };
    }
    return null;
  }

  /** A subscription, as a wrapped tool's call.subscribe makes one. */
  subscribe(event: string, args: Record<string, unknown>, user = "u1", once = false, replay = false, link?: string): Sub {
    const sub = { id: `00000000-0000-4000-8000-${String(this.subs.length + 1).padStart(12, "0")}`, event, args, link: link ?? `link${this.subs.length + 1}x`, user, ended: false, once };
    this.subs.push(sub);
    if (!this.queues.has(sub.link)) this.queues.set(sub.link, []);
    // Replay: the latest earlier occurrence, delivered to the new subscription at once.
    const past = this.last.get(`${event} ${canonical(args)}`);
    if (replay && past) this.queues.get(sub.link)!.push(past);
    return sub;
  }

  waitUrl(sub: Sub): string {
    return `${this.origin}/v1/stream/wait?link=${sub.link}`;
  }

  private live(event: string, args: unknown) {
    return this.subs.filter((s) => !s.ended && s.event === event && canonical(s.args) === canonical(args));
  }

  deliver(event: string, args: unknown, line: Record<string, unknown>): number {
    const subs = this.live(event, args);
    const l = JSON.stringify({ type: "event", topic: `app/${event}`, from: "app:x", trust: "outside data", ...line });
    this.last.set(`${event} ${canonical(args)}`, l);
    if (!this.dropEmits) for (const s of subs) this.queues.get(s.link)!.push(l);
    for (const s of subs) if (s.once) s.ended = true;
    return subs.length;
  }

  private async handle(req: IncomingMessage, res: ServerResponse) {
    const url = new URL(req.url ?? "/", this.origin);
    const body = await readBody(req);
    const auth = String(req.headers.authorization ?? "");
    this.requests.push({ method: req.method ?? "", path: url.pathname, auth, body });
    if (url.pathname === "/v1/stream/wait") {
      const q = this.queues.get(url.searchParams.get("link") ?? "");
      if (!q) return send(res, 200, `{"type":"hub.closed","reason":"expired","note":"the link expired"}\n`, "text/plain");
      if (url.searchParams.get("confirm")) return send(res, 200, `{"type":"hub.next","url":"${this.origin}/v1/stream/wait?link=${url.searchParams.get("link")}"}\n`, "text/plain");
      const deadline = Date.now() + Math.min(2000, (parseInt(url.searchParams.get("timeout") ?? "1", 10) || 1) * 1000);
      while (q.length === 0 && Date.now() < deadline) await new Promise((r) => setTimeout(r, 20));
      const lines = q.splice(0);
      const cursor = lines.length ? `{"type":"cursor","after":"00000000000000aa"}\n` : "";
      return send(res, 200, lines.map((l) => l + "\n").join("") + cursor + `{"type":"hub.next","url":"${this.origin}/v1/stream/wait?link=${url.searchParams.get("link")}&timeout=50s"}\n`, "text/plain");
    }
    if (!url.pathname.startsWith("/api/apps/v1/")) return send(res, 404, { error: { code: "not_found", message: "no such route" } });
    const path = url.pathname.slice("/api/apps/v1".length);
    if (path === "/app" && this.appAnswer) {
      const a = this.appAnswer(req);
      if (a) return send(res, a.status, a.body);
    }
    const who = this.authenticate(auth);
    if (!who) {
      if (path === "/app") this.onUnknownPoll?.(++this.unknownPolls);
      return send(res, 401, { error: { code: "unauthenticated", message: "the assertion's signature doesn't verify" } });
    }
    const b = body ? JSON.parse(body) : {};
    switch (`${req.method} ${path}`) {
      case "GET /app":
        return send(res, 200, { id: who.app.id, name: who.app.name, key: { key_id: who.kid.slice(0, 8), name: "Default" } });
      case "GET /manifest":
        return send(res, 200, who.app.manifest);
      case "POST /events/status":
        return send(res, 200, { subscriptions: this.live(b.name, b.arguments).length + this.others, connected: 0 });
      case "POST /events/emit": {
        this.emits.push(b);
        const matched = this.deliver(b.name, b.arguments, { subject: b.text, data: b.payload, id: b.id });
        return send(res, 200, { matched, duplicate: false, unmatched: [], ended: [] });
      }
      case "POST /event-subscriptions/end": {
        const s = this.subs.find((x) => x.id === b.id && x.user === b.user && !x.ended);
        if (s) s.ended = true;
        return send(res, 200, { ended: !!s });
      }
      default:
        return send(res, 404, { error: { code: "not_found", message: "no such route" } });
    }
  }
}

/** The event the fake MCP server's tools subscribe to. */
export const BUILD_FINISHED = {
  name: "build.finished",
  description: "A build finished: it passed, failed or was canceled",
  arguments: { type: "object", properties: { pipeline: { type: "string" }, number: { type: "integer" } }, required: ["pipeline", "number"] },
  payload: { type: "object", properties: { status: { type: "string" }, number: { type: "integer" } }, required: ["status", "number"] },
  topic: "builds/{pipeline}/{number}",
};

export interface FakeMcpOptions {
  /** watch_build's description. Default: with the sentence subscribes adds. */
  description?: string;
  /** Leave Wakeshift's inputs out of watch_build's schema. */
  noInputs?: boolean;
  /** watch_build replies without Wakeshift's block. */
  noBlock?: boolean;
  /** watch_build leaves Wakeshift's harness argument out of what it subscribes with. */
  dropHarness?: boolean;
  /** watch_build subscribes with once: the subscription ends with its first delivery. */
  once?: boolean;
  /** watch_build answers with this error text. */
  errorText?: string;
  /** attach's tools aren't served. */
  noAttachTools?: boolean;
  /** The server refuses initialize (a stateless 2026-07-28 server). */
  stateless?: boolean;
  /** Requires this bearer token. */
  token?: string;
  /** watch_build subscribes to build.started too (and declares both). */
  twoEvents?: boolean;
  /** watch_build subscribes with replay: the latest earlier build.finished arrives at once. */
  replay?: boolean;
  /** watch_build replies with JSON text and structuredContent ({number, pipeline, run}). */
  json?: boolean;
}

/** The second event watch_build subscribes to with twoEvents. */
export const BUILD_STARTED = {
  name: "build.started",
  description: "A build started",
  arguments: BUILD_FINISHED.arguments,
  payload: { type: "object", properties: { number: { type: "integer" } } },
  topic: "builds/{pipeline}/{number}/started",
};

const SENTENCE =
  "This tool is how Shipyard notifies you in this conversation of this event: A build finished: it passed, failed or was canceled. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it.";

/** An MCP server like Shipyard's: watch_build subscribes build.finished {pipeline, number} through the fake core. */
export class FakeMcp {
  server: Server;
  url = "";
  calls: { name: string; args: Record<string, unknown>; client?: unknown }[] = [];
  initialized = 0;
  private session = "";

  constructor(
    private core: FakeCore,
    private o: FakeMcpOptions = {},
    readonly pipeline = "acme-web-1a2b",
  ) {
    this.server = createServer((req, res) => void this.handle(req, res));
  }

  async start(): Promise<this> {
    await new Promise<void>((r) => this.server.listen(0, "127.0.0.1", r));
    const a = this.server.address();
    this.url = `http://127.0.0.1:${typeof a === "object" && a ? a.port : 0}/mcp`;
    return this;
  }

  stop(): Promise<void> {
    this.server.closeAllConnections?.();
    return new Promise((r) => this.server.close(() => r()));
  }

  private tools() {
    const wsInputs = this.o.noInputs ? {} : { session_key: { type: "string" }, harness: { type: "string" }, conversation_id: { type: "string" } };
    const t = [
      { name: "watch_build", description: this.o.description ?? `Watch a build. ${this.o.twoEvents ? SENTENCE.replace("of this event: A build finished: it passed, failed or was canceled", "of these events: A build finished: it passed, failed or was canceled; A build started") : SENTENCE}`, inputSchema: { type: "object", properties: { number: { type: "integer" }, ...wsInputs } } },
      { name: "unwatch_build", description: "Stop watching a build.", inputSchema: { type: "object", properties: { number: { type: "integer" }, ...wsInputs } } },
      { name: "cancel_build", description: "Cancel a build.", inputSchema: { type: "object", properties: { number: { type: "integer" } } } },
      { name: "leak", description: "Fails, quoting a key.", inputSchema: { type: "object", properties: {} } },
    ];
    if (!this.o.noAttachTools) {
      t.push({ name: "list_subscriptions", description: "List.", inputSchema: { type: "object", properties: { ...wsInputs } } });
      t.push({ name: "unsubscribe_events", description: "Stop.", inputSchema: { type: "object", properties: { ...wsInputs } } });
    }
    return t;
  }

  private call(name: string, args: Record<string, unknown>): { content: { type: string; text: string }[]; structuredContent?: unknown; isError?: boolean } {
    const text = (s: string, isError = false) => ({ content: [{ type: "text", text: s }], ...(isError ? { isError } : {}) });
    const n = Number(args.number ?? 412);
    switch (name) {
      case "watch_build": {
        if (this.o.errorText) return text(this.o.errorText, true);
        if (this.o.noBlock) return text(`Watching build #${n}.`);
        const sub = this.core.subscribe(BUILD_FINISHED.name, { pipeline: this.pipeline, number: n }, "u1", this.o.once, this.o.replay);
        if (this.o.twoEvents) this.core.subscribe(BUILD_STARTED.name, { pipeline: this.pipeline, number: n }, "u1", false, false, sub.link);
        const key = `Pass session_key="wsc_secretsessionkey${sub.id.slice(-2)}" to this app's tools from now on.`;
        // As the core answers: the Monitor's command for Claude Code, and
        // for a program it doesn't recognize only that it can't wake it.
        const block =
          args.harness === "claude-code" && !this.o.dropHarness
            ? `To be woken when Shipyard sends something, start the Monitor tool now (timeout_ms 1800000) with this command: u='${this.core.waitUrl(sub)}&until=hooks&timeout=50s'; c=; p=$PPID; while :; do …; done . ${key}`
            : `Wakeshift can't wake this agent, so nothing will notify you here when something arrives. ${key}`;
        if (this.o.json) {
          const own = { number: n, pipeline: this.pipeline, run: `run-${n}` };
          return { content: [{ type: "text", text: JSON.stringify(own) }, { type: "text", text: block }], structuredContent: { ...own, wakeshift: block } };
        }
        return { content: [{ type: "text", text: `Watching build #${n}.` }, { type: "text", text: block }] };
      }
      case "unsubscribe_events":
        if (typeof args.id === "string") {
          const s = this.core.subs.find((x) => x.id === args.id && !x.ended);
          if (s) s.ended = true;
          return text(s ? `Stopped subscription ${args.id}.` : `No subscription of yours has the id ${args.id}: it may have ended already.`);
        }
      // falls through
      case "unwatch_build": {
        const live = this.core.subs.filter((s) => !s.ended);
        const last = live[live.length - 1];
        if (last) last.ended = true;
        return text(last ? "Stopped build.finished." : "This conversation wasn't subscribed.");
      }
      case "list_subscriptions": {
        const live = this.core.subs.filter((s) => !s.ended);
        if (!live.length) return text("You have no subscriptions to this app's events.");
        return text(`Your subscriptions to this app's events:\n${live.map((s) => `- ${s.id}: ${s.event} ${canonical(s.args)}, wakes a conversation, since 2026-11-03T10:00:00Z`).join("\n")}\n\nStop one with unsubscribe_events and its id.`);
      }
      case "cancel_build": {
        this.core.deliver(BUILD_FINISHED.name, { pipeline: this.pipeline, number: n }, { subject: `Build #${n} was canceled.`, data: { status: "canceled", number: n } });
        return text(`Canceled build #${n}.`);
      }
      case "leak":
        return text("error: the key wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV was refused", true);
      default:
        return text(`no tool ${name}`, true);
    }
  }

  private async handle(req: IncomingMessage, res: ServerResponse) {
    const raw = await readBody(req);
    if (req.method === "DELETE") return send(res, 200, "");
    if (this.o.token && req.headers.authorization !== `Bearer ${this.o.token}`) return send(res, 401, { error: "unauthorized" });
    let m: { id?: number; method: string; params?: any };
    try {
      m = JSON.parse(raw);
    } catch {
      return send(res, 400, "bad json");
    }
    if (m.id === undefined) return send(res, 202, "");
    const reply = (result: unknown) => {
      // Answers as SSE, as many servers do.
      res.writeHead(200, { "content-type": "text/event-stream", ...(this.session ? { "mcp-session-id": this.session } : {}) });
      res.end(`event: message\ndata: ${JSON.stringify({ jsonrpc: "2.0", id: m.id, result })}\n\n`);
    };
    switch (m.method) {
      case "initialize":
        if (this.o.stateless) return send(res, 200, { jsonrpc: "2.0", id: m.id, error: { code: -32601, message: "initialize isn't supported on 2026-07-28" } });
        this.initialized++;
        this.session = "sess-1";
        return reply({ protocolVersion: "2025-11-25", capabilities: { tools: {} }, serverInfo: { name: "shipyard", version: "1" } });
      case "tools/list":
        return reply({ tools: this.tools() });
      case "tools/call": {
        const args = { ...(m.params?.arguments ?? {}) };
        this.calls.push({ name: m.params?.name, args, client: m.params?._meta?.["io.modelcontextprotocol/clientInfo"] });
        return reply(this.call(m.params?.name, args));
      }
      default:
        return send(res, 200, { jsonrpc: "2.0", id: m.id, error: { code: -32601, message: "method not found" } });
    }
  }
}

/** A Ctx for a test: a temp directory, output recorded, a browser that's never real. */
export class TestCtx implements Ctx {
  cwd: string;
  env: Record<string, string | undefined>;
  platform: NodeJS.Platform = "linux";
  out = "";
  errOut = "";
  opened: { cmd: string; args: string[] }[] = [];
  ran: { cmd: string; args: string[]; opts?: RunOptions }[] = [];
  fetch: typeof fetch = globalThis.fetch;
  private t = 1_800_000_000_000;
  random: (n: number) => Buffer = randomBytes;

  constructor(env: Record<string, string | undefined> = {}, dir?: string) {
    this.cwd = dir ?? mkdtempSync(join(tmpdir(), "wscli-"));
    // Never a real browser: a test that wants one says so with BROWSER.
    this.env = { WAKESHIFT_NO_BROWSER: "1", WAKESHIFT_CLI_POLL_INTERVAL_MS: "5", WAKESHIFT_CLI_POLL_TIMEOUT_MS: "2000", ...env };
  }
  stdout = (s: string) => {
    this.out += s;
  };
  stderr = (s: string) => {
    this.errOut += s;
  };
  sleep = async (ms: number) => {
    this.t += ms;
    await new Promise((r) => setTimeout(r, 1));
  };
  now = () => this.t;
  spawnDetached = (cmd: string, args: string[]) => {
    this.opened.push({ cmd, args });
    return true;
  };
  run = async (cmd: string, args: string[], opts?: RunOptions): Promise<RunResult> => {
    this.ran.push({ cmd, args, opts });
    return realRun(cmd, args, opts);
  };
  all(): string {
    return this.out + this.errOut;
  }
  cleanup(): void {
    rmSync(this.cwd, { recursive: true, force: true });
  }
}

/** Fails if s shows a key: the given ones, or anything shaped like a current key. */
export function assertNoKey(s: string, keys: (string | undefined)[], what = "output"): void {
  for (const k of keys) {
    if (k && s.includes(k)) throw new Error(`${what} shows a key:\n${s}`);
  }
  if (KEY_PATTERN.test(s)) throw new Error(`${what} shows a key-shaped string:\n${s}`);
  if (/wst_(test|live)_[a-z]/.test(s)) throw new Error(`${what} shows an older key:\n${s}`);
}

export { keyCodeOf };
