import { z } from "zod";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { WakeshiftError } from "./errors.js";
import { parseKey, signAssertion, type AppKey } from "./key.js";

/** Wakeshift's production core, the default for live keys. Override with `core` or WAKESHIFT_CORE. */
export const DEFAULT_CORE = "https://app.wakeshift.ai";

/** The tool argument an agent passes its session key in (a result gives it one). */
export const SESSION_ARG = "session_key";

/** The tool argument an agent says which program it runs in with. */
export const HARNESS_ARG = "harness";

/**
 * The tool argument an agent passes its own ID for the conversation in,
 * when its program gives it one (Cursor and Grok Bot: CURSOR_CONVERSATION_ID).
 */
export const CONVERSATION_ARG = "conversation_id";

// What the agent is told each argument is (the core's own words:
// internal/harnesses, whose tests check these match).
const SESSION_DESCRIPTION = "The session key an earlier result gave you, if it did. It's a secret: keep it to this conversation; don't share or log it";
const HARNESS_DESCRIPTION =
  "The program you're running in: codex, claude-code, cursor (local CLI or IDE), cursor-cloud (Cursor cloud agent), grok-bot, copilot-cli, opencode, antigravity, openclaw (even when it runs your turns in Codex), hermes, chatgpt, or your program's name if it isn't listed";
const CONVERSATION_DESCRIPTION =
  "Your own ID for this conversation, if your program gives you one. Cursor and Grok Bot: the output of `printenv CURSOR_CONVERSATION_ID`. Codex and Antigravity send it automatically; others leave it empty";

export interface WakeshiftOptions {
  /** The app key from the console: wst_test_<app>_<seed>. */
  key: string;
  /**
   * Wakeshift's base URL: the console you got the key from. Default:
   * WAKESHIFT_CORE from the environment. A test key needs one of the two;
   * a live key falls back to https://app.wakeshift.ai.
   */
  core?: string;
  /** Default: the global fetch. */
  fetch?: typeof fetch;
  /** Per-request timeout in milliseconds. Default 20000. */
  timeoutMs?: number;
  /**
   * Called with a problem that doesn't fail the call but makes it work less
   * well, for example a tool call Wakeshift recognised no harness in. Each kind is
   * reported once. Default: console.warn.
   */
  onWarning?: (message: string) => void;
}

/** What the SDK says, once, when Wakeshift recognised no harness in a tool call. */
export const UNKNOWN_CLIENT_WARNING =
  "wakeshift: Wakeshift couldn't tell which agent made a tool call (no MCP client name, nothing it knows in the " +
  "call's _meta or User-Agent, and no harness argument), so it treats the agent as one it can't wake (its messages are held " +
  "for its next call to the tool). The name comes from the client's initialize request, which a server created per request " +
  "(stateless Streamable HTTP) never sees; Claude Code, Codex, Antigravity, Copilot CLI, opencode and Cursor are recognised " +
  "without it. Spread ...ws.args into the tool's input and pass args to ws.call, so the agent can say (harness); for " +
  "other clients, keep one McpServer per MCP session (a stateful transport), or pass the name yourself: " +
  "ws.call(extra, { client }) or ws.callOf({ clientName }).";

/**
 * Who called one of the app's tools, as Wakeshift needs it. Make one with
 * `ws.call(extra, …)` inside an MCP tool callback, or `ws.callOf(…)`.
 */
export interface Call {
  /** The app's own ID for the person (optional). */
  user?: string;
  /**
   * The MCP client's name (initialize's clientInfo.name). A stateless
   * server has none; Wakeshift then tells the harness from meta, else
   * user_agent.
   */
  client: string;
  /** The tool call's _meta (Codex sends its thread there). */
  meta?: Record<string, unknown>;
  /** The session handle an earlier result gave the agent. */
  session?: string;
  /** The MCP request's HTTP User-Agent. */
  user_agent?: string;
  /** The conversation's ID as the agent passed it (conversation_id: Cursor's CURSOR_CONVERSATION_ID). */
  conversation?: string;
  /** The program the agent says it runs in (harness): a hint, never a credential. */
  harness?: string;
}

/**
 * What Wakeshift recognised the calling conversation's harness as, and by
 * which signal ("claim": only the agent's harness argument named it); both
 * null when nothing named one. claimed is what the agent said (normalized:
 * "Grok Bot" is grok-bot), null if it didn't; a claimed that differs from
 * harness was ignored, since it contradicted what the call was recognised
 * as (the claim only refines it: Cursor's calls may be Grok Bot's).
 */
export interface Recognised {
  harness: string | null;
  via: "client" | "meta" | "user_agent" | "claim" | null;
  claimed: string | null;
}

/**
 * Seconds, or a string like "90s", "5m", "2h", "30d". Wakeshift checks the
 * value: days must be whole ("1.5d" is a bad_request; use "36h").
 */
export type Duration = number | `${number}${"s" | "m" | "h" | "d"}`;

export interface SubscribeOptions {
  /** Wake only for events whose data has these top-level fields equal to these values. */
  filter?: Record<string, string | number | boolean | null>;
  /** End after the first delivery. */
  once?: boolean;
  /** Deliver the latest event published within this long before subscribing (at most 24 hours). */
  replay?: Duration;
  /** End after this long without a delivery or a fresh subscribe (1 minute to 365 days; default 30 days). */
  ttl?: Duration;
}

/** What Wakeshift adds to a tool result: text for the agent, _meta for its harness. */
export interface Addition {
  text?: string;
  meta?: Record<string, unknown>;
  /** What Wakeshift recognised the caller's harness as (not added to the result). */
  agent?: Recognised;
}

export interface Subscription {
  /** For `send`. */
  id: string;
  topic: string;
  filter?: Record<string, unknown>;
  once: boolean;
  expiresAt: Date;
  /**
   * The conversation's session key (the agent passes it back as session_key;
   * the result says so). It's a secret that acts as the conversation: don't
   * show, store or log it. To name the conversation, use id, or an HMAC of it.
   */
  session: string;
  /** What the tool result must carry for the caller's harness. */
  addition: Addition;
  /** What Wakeshift recognised the caller's harness as. */
  agent: Recognised;
  /**
   * The tool result to return: your text (or result) followed by
   * Wakeshift's, with its _meta.
   */
  result(text: string | CallToolResult): CallToolResult;
}

export interface TopicEvent {
  /** What the agent reads first (at most 1000 bytes). */
  text: string;
  /** A JSON object, within the 64 KB request limit; filters match its top-level fields. */
  data?: Record<string, unknown>;
  /** Makes retries publish once for a day (at most 200 characters); after a day, the same id publishes again. */
  id?: string;
}

export interface Published {
  /** Conversations it woke (or will). */
  matched: number;
  /** An earlier publish had this event's id. */
  duplicate: boolean;
}

/** delivered: on its way to a connected conversation; queued: waits for it; closed: the subscription ended. */
export type SendStatus = "delivered" | "queued" | "closed";

/**
 * How a conversation is connected: stream (a stream held open, e.g.
 * Cursor's background command), poll (long polls or waits, e.g. Claude
 * Code's Monitor, in the last minute) or hooks (Wakeshift's hooks or
 * connector on the user's machine).
 */
export type Via = "stream" | "poll" | "hooks" | "webhook";

export interface TopicStatus {
  topic: string;
  /** Conversations subscribed (ended or expired subscriptions don't count). */
  subscriptions: number;
  /** Of those, connected now. */
  connected: number;
  /** How many are connected each way. */
  via: Record<Via, number>;
}

export interface SubscriptionStatus {
  id: string;
  topic: string;
  status: "active" | "closed";
  connected: boolean;
  via?: Via;
  expiresAt?: Date;
}

export interface PublishUrl {
  id: string;
  topic: string;
  url: string;
}

/** The parts of an MCP tool callback's `extra` that Wakeshift reads. */
export interface ToolExtra {
  _meta?: Record<string, unknown>;
  requestInfo?: { headers?: Record<string, string | string[] | undefined> };
}

// userAgentOf is the User-Agent of the HTTP request a tool call came in
// ("" if there's none, e.g. over stdio).
function userAgentOf(extra: ToolExtra | undefined): string {
  const headers = extra?.requestInfo?.headers;
  if (!headers) return "";
  const v = headers["user-agent"] ?? headers["User-Agent"];
  return (Array.isArray(v) ? v[0] : v) ?? "";
}

/** An McpServer or a low-level Server: anything that knows its client's name. */
export type ClientInfoSource =
  | { getClientVersion(): { name: string } | undefined }
  | { server: { getClientVersion(): { name: string } | undefined } };

export interface CallOptions {
  /** The MCP server the call came to, for the client's name (or pass client). */
  server?: ClientInfoSource;
  /** The client's name, if you know it (instead of server). */
  client?: string;
  /** The app's own ID for the person. */
  user?: string;
  /** The tool's arguments: session_key, harness and conversation_id (ws.args) are read from them. */
  args?: { [SESSION_ARG]?: string; [HARNESS_ARG]?: string; [CONVERSATION_ARG]?: string } | Record<string, unknown>;
  /** The session key, if you read it yourself. */
  session?: string;
  /** The program the agent says it runs in (harness), if you read it yourself. */
  harness?: string;
  /** The conversation's ID the agent passed (conversation_id), if you read it yourself. */
  conversation?: string;
}

export function clientNameOf(s: ClientInfoSource): string {
  const info = "getClientVersion" in s ? s.getClientVersion() : s.server.getClientVersion();
  return info?.name ?? "";
}

/** Adds Wakeshift's text and _meta to a tool result (a copy). */
export function applyAddition(a: Addition, base: string | CallToolResult): CallToolResult {
  let out: CallToolResult;
  if (typeof base === "string") {
    const text = (base.trim() + "\n\n" + (a.text ?? "")).trim();
    out = { content: text ? [{ type: "text", text }] : [] };
  } else {
    out = { ...base, content: [...(base.content ?? [])] };
    if (a.text) out.content.push({ type: "text", text: a.text });
  }
  if (a.meta && Object.keys(a.meta).length > 0) out._meta = { ...(out._meta ?? {}), ...a.meta };
  return out;
}

const argsShape = {
  [SESSION_ARG]: z.string().optional().describe(SESSION_DESCRIPTION),
  [HARNESS_ARG]: z.string().optional().describe(HARNESS_DESCRIPTION),
  [CONVERSATION_ARG]: z.string().optional().describe(CONVERSATION_DESCRIPTION),
};

// str is a tool argument's value if it's a string.
function str(args: Record<string, unknown> | undefined, name: string): string | undefined {
  return typeof args?.[name] === "string" ? (args[name] as string) : undefined;
}

/**
 * The Wakeshift client for an app: subscribe the conversations that call
 * your tools to topics, and publish to them.
 */
export class Wakeshift {
  readonly appId: string;
  readonly mode: "test" | "live";
  readonly core: string;
  private readonly key: AppKey;
  private readonly fetchFn: typeof fetch;
  private readonly timeoutMs: number;
  private readonly warnFn: (message: string) => void;
  private readonly warned = new Set<string>();

  constructor(opts: WakeshiftOptions) {
    if (!opts?.key) throw new Error("wakeshift: no app key (WAKESHIFT_KEY?)");
    this.key = parseKey(opts.key);
    this.appId = this.key.appId;
    this.mode = this.key.mode;
    // Test keys come from staging and local consoles, and one sent to
    // production fails in a way that's hard to read: ask for the core.
    const core = opts.core || process.env.WAKESHIFT_CORE || (this.mode === "live" ? DEFAULT_CORE : "");
    if (!core) {
      throw new Error(
        "wakeshift: no core URL for this test key: set WAKESHIFT_CORE, or pass { core }, to the address of the console the key came from (for example https://app.staging.wakeshift.ai)",
      );
    }
    this.core = core.replace(/\/+$/, "");
    this.fetchFn = opts.fetch ?? globalThis.fetch;
    this.timeoutMs = opts.timeoutMs ?? 20_000;
    this.warnFn = opts.onWarning ?? ((m) => console.warn(m));
  }

  /** Reports a warning, once per kind. */
  private warnOnce(kind: string, message: string): void {
    if (this.warned.has(kind)) return;
    this.warned.add(kind);
    this.warnFn(message);
  }

  /**
   * The arguments the agent passes Wakeshift's part of the call in, as a
   * zod shape for your subscribe tool's input (and any other tool whose
   * results you annotate): `inputSchema: { repo: z.string(), ...ws.args }`.
   * session_key (the key a result gives the conversation), harness (the
   * program the agent runs in) and conversation_id (its own ID for the
   * conversation, where its program gives one). Wakeshift adds nothing to
   * tools you don't spread it into.
   */
  get args(): typeof argsShape {
    return argsShape;
  }

  /**
   * Packs the tool call from an MCP tool callback's `extra`: its _meta and
   * User-Agent, the client's name (from `server`, or pass `client`), the
   * user, and session_key, harness and conversation_id from `args`.
   */
  call(extra: ToolExtra | undefined, opts: CallOptions = {}): Call {
    let client = opts.client;
    if (client === undefined) {
      if (!opts.server) throw new Error("wakeshift: ws.call needs the MCP client's name: pass { server } (your McpServer), or { client }");
      client = clientNameOf(opts.server);
    }
    const args = opts.args as Record<string, unknown> | undefined;
    return this.callOf({
      meta: extra?._meta,
      clientName: client,
      user: opts.user,
      session: opts.session ?? str(args, SESSION_ARG),
      userAgent: userAgentOf(extra),
      harness: opts.harness ?? str(args, HARNESS_ARG),
      conversation: opts.conversation ?? str(args, CONVERSATION_ARG),
    });
  }

  /** Packs a tool call without an MCP framework. */
  callOf(c: {
    meta?: Record<string, unknown>;
    clientName: string;
    user?: string;
    session?: string;
    userAgent?: string;
    harness?: string;
    conversation?: string;
  }): Call {
    const out: Call = { client: c.clientName ?? "" };
    if (c.user) out.user = c.user;
    if (c.meta && Object.keys(c.meta).length > 0) out.meta = c.meta;
    if (c.session) out.session = c.session;
    if (c.userAgent) out.user_agent = c.userAgent;
    if (c.harness?.trim()) out.harness = c.harness.trim();
    if (c.conversation?.trim()) out.conversation = c.conversation.trim();
    return out;
  }

  // recognised reads what the core recognised the caller as, and warns
  // once if it was nothing and the agent didn't say (an older core that
  // doesn't say: no warning).
  private recognised(agent: Partial<Recognised> | undefined): Recognised {
    if (agent && !agent.harness && !agent.claimed) this.warnOnce("unknown_client", UNKNOWN_CLIENT_WARNING);
    return { harness: agent?.harness ?? null, via: agent?.via ?? null, claimed: agent?.claimed ?? null };
  }

  /**
   * Subscribes the calling conversation to topic (again: refreshes it).
   * Check the caller may see the topic first: Wakeshift only refuses another
   * user's u/<user>/… topics.
   */
  async subscribe(call: Call, topic: string, opts: SubscribeOptions = {}): Promise<Subscription> {
    const body: Record<string, unknown> = { call, topic, once: !!opts.once };
    if (opts.filter && Object.keys(opts.filter).length > 0) body.filter = opts.filter;
    if (opts.replay !== undefined) body.replay = opts.replay;
    if (opts.ttl !== undefined) body.ttl = opts.ttl;
    const out = await this.request<{
      subscription: { id: string; topic: string; filter?: Record<string, unknown>; once: boolean; expires_at: string };
      session: string;
      addition: Addition;
      agent?: Recognised;
    }>("POST", "/topics/subscribe", body);
    const agent = this.recognised(out.agent);
    const addition = out.addition ?? {};
    const s = out.subscription;
    return {
      id: s.id,
      topic: s.topic,
      filter: s.filter,
      once: s.once,
      expiresAt: new Date(s.expires_at),
      session: out.session,
      addition,
      agent,
      result: (text) => applyAddition(addition, text),
    };
  }

  /** Ends the conversation's subscription to topic, or to all the app's topics. Returns how many ended. */
  async unsubscribe(call: Call, topic?: string): Promise<number> {
    const out = await this.request<{ unsubscribed: number }>("POST", "/topics/unsubscribe", topic ? { call, topic } : { call });
    return out.unsubscribed;
  }

  /** Wakes every conversation subscribed to one of topics whose filter the event passes (each once). */
  async publish(topics: string | string[], event: TopicEvent): Promise<Published> {
    const out = await this.request<Published>("POST", "/topics/publish", { topics: typeof topics === "string" ? [topics] : topics, event });
    return { matched: out.matched, duplicate: !!out.duplicate };
  }

  /** Wakes exactly one subscription's conversation, whatever its filter. */
  async send(subscriptionId: string, event: TopicEvent): Promise<SendStatus> {
    const out = await this.request<{ status: SendStatus }>("POST", `/subscriptions/${encodeURIComponent(subscriptionId)}/send`, { event });
    return out.status;
  }

  /**
   * How many conversations are subscribed to topic, and how many are
   * connected now, and how. Only reads: nothing is delivered, minted or ended.
   */
  async topicStatus(topic: string): Promise<TopicStatus> {
    const out = await this.request<Omit<TopicStatus, "via"> & { via?: Partial<Record<Via, number>> }>("GET", `/topics/status?topic=${encodeURIComponent(topic)}`);
    return { topic: out.topic, subscriptions: out.subscriptions, connected: out.connected, via: { stream: 0, poll: 0, hooks: 0, webhook: 0, ...out.via } };
  }

  /** Whether a subscription is active and its conversation connected now, and how. Only reads. */
  async subscriptionStatus(subscriptionId: string): Promise<SubscriptionStatus> {
    const out = await this.request<{ id: string; topic: string; status: "active" | "closed"; connected: boolean; via?: Via; expires_at?: string }>(
      "GET",
      `/subscriptions/${encodeURIComponent(subscriptionId)}`,
    );
    const s: SubscriptionStatus = { id: out.id, topic: out.topic, status: out.status, connected: out.connected };
    if (out.via) s.via = out.via;
    if (out.expires_at) s.expiresAt = new Date(out.expires_at);
    return s;
  }

  /** A capability URL that publishes to topic: a POST's body becomes an event. */
  async publishUrl(topic: string): Promise<PublishUrl> {
    return this.request<PublishUrl>("POST", "/topics/publish-urls", { topic });
  }

  /** Stops a publish URL working (410 from then on). */
  async revokePublishUrl(id: string): Promise<void> {
    await this.request("DELETE", `/topics/publish-urls/${encodeURIComponent(id)}`);
  }

  /** What Wakeshift would add to any other tool result for this conversation (creates nothing). */
  async addition(call: Call): Promise<Addition> {
    const out = await this.request<Addition>("POST", "/topics/annotate", { call });
    return { ...out, agent: this.recognised(out.agent) };
  }

  /**
   * Adds what Wakeshift has for the calling conversation to a tool result
   * of yours (a copy): what's waiting for a harness that can't be woken, a
   * fresh connector beacon while a Codex thread's connector isn't running.
   * Only results you pass here (and a subscription's result()) change.
   */
  async annotate(call: Call, result: CallToolResult): Promise<CallToolResult> {
    return applyAddition(await this.addition(call), result);
  }

  /** Ends every subscription to topic; each conversation is told. Returns how many ended. */
  async closeTopic(topic: string): Promise<number> {
    return (await this.request<{ closed: number }>("POST", "/topics/close", { topic })).closed;
  }

  /**
   * Ends one subscription by its id, telling its conversation that the app
   * ended it. Returns false if it had already ended (or there's no such
   * subscription of the app's).
   */
  async endSubscription(subscriptionId: string): Promise<boolean> {
    return (await this.request<{ ended: boolean }>("POST", `/subscriptions/${encodeURIComponent(subscriptionId)}/end`, {})).ended;
  }

  /**
   * Ends the subscriptions of every conversation of one of the app's users
   * to topic, or to all of the app's topics (they lost access to something),
   * telling each conversation. Returns how many ended.
   */
  async unsubscribeUser(user: string, topic?: string): Promise<number> {
    return (await this.request<{ unsubscribed: number }>("POST", "/topics/unsubscribe", topic ? { user, topic } : { user })).unsubscribed;
  }

  /**
   * Deletes everything Wakeshift keeps for one of the app's users: their
   * conversations, subscriptions, events and deliveries, recent publishes to
   * their u/<user>/… topics and publish URLs for them, and their ID. Open
   * streams close. Returns false if there was nothing to forget.
   */
  async forgetUser(user: string): Promise<boolean> {
    return (await this.request<{ forgotten: boolean }>("POST", "/users/forget", { user })).forgotten;
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Authorization: "Bearer " + signAssertion(this.key, this.core) };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let resp: Response;
    let text: string;
    try {
      resp = await this.fetchFn(this.core + "/api/apps/v1" + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(this.timeoutMs),
      });
      text = await resp.text();
    } catch (err) {
      throw this.unreachable(err);
    }
    let json: any;
    try {
      json = text ? JSON.parse(text) : undefined;
    } catch {
      json = undefined;
    }
    if (!resp.ok) {
      const e = json?.error ?? {};
      throw new WakeshiftError(resp.status, e.code ?? "", e.message ?? (text.slice(0, 200) || resp.statusText));
    }
    // A 2xx that isn't Wakeshift's JSON: a proxy's page, or a wrong core.
    if (json === null || typeof json !== "object") {
      throw new WakeshiftError(
        resp.status,
        "bad_response",
        `Wakeshift at ${this.core} answered ${resp.status} without a JSON object. Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from`,
      );
    }
    return json as T;
  }

  // A request that got no answer. Name the core it went to: a wrong or
  // missing WAKESHIFT_CORE is the usual cause.
  private unreachable(err: unknown): WakeshiftError {
    const e = err as { name?: string; message?: string; cause?: { code?: string; message?: string } } | undefined;
    const why =
      e?.name === "TimeoutError" || e?.name === "AbortError"
        ? `no answer within ${this.timeoutMs / 1000}s`
        : [e?.message, e?.cause?.code ?? e?.cause?.message].filter(Boolean).join(": ") || String(err);
    return new WakeshiftError(
      0,
      "unreachable",
      `can't reach Wakeshift at ${this.core} (${why}). Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from`,
      { cause: err },
    );
  }
}
