// Wakeshift's plugin for OpenClaw (verified against 2026.10.4;
// docs/harnesses.md, "OpenClaw").
//
// Connecting: an app's tool result for an OpenClaw session carries a
// beacon (internal/beacon). OpenClaw drops a result's _meta in its own
// runtime, so there it's a line of the result's text, which the plugin's
// tool-result middleware takes out before the model sees it; when OpenClaw
// runs the turn in Codex's app-server, the beacon is in _meta, which the
// plugin's hooks see and Codex keeps from the model. The plugin trusts a
// beacon only if the Wakeshift pinned at install signed it
// (plugins.entries.wakeshift.config.issuer), redeems its one-time ticket
// for the conversation's own stream link, and keeps the link on disk.
//
// Waking: per connected conversation, a transport reads the stream
// (transport.ts: Server-Sent Events, falling back to long polling) and a
// Conversation (deliveries.ts) owns every delivery from there: a durable
// record each, framed as outside data (framing.ts), handed to a turn with
// OpenClaw's /hooks/agent (wake.ts: persistent, in the conversation's own
// session, which OpenClaw safety-wraps as outside content), and an ack only
// once that turn has finished. The hooks it needs are set up and scoped by
// control.ts (hooks.ts says what changes).

import { join } from "node:path";
import { beaconIn, BeaconError, jwkKey, mcpServer, verifyBound, type Claims } from "./beacon.js";
import { Conversation, Ledger, type Host } from "./deliveries.js";
import { agentOf } from "./hooks.js";
import { PINS_UNREADABLE, Store, type SavedLink } from "./store.js";
import { Transport, type Stopped, type TransportOptions } from "./transport.js";
import { CLIENT_HEADER, clientHeader } from "./version.js";
import type { Route } from "./wake.js";

/** The connected note's start (harnesses.Connected): agents look for it. */
export const CONNECTED = "Wakeshift: connected";
export const NOTE_CONNECTED =
  CONNECTED + ". New messages for this conversation will reach it as soon as they arrive, even while it's idle, as long as the OpenClaw gateway runs.";
export const noteFailed = (why: string) =>
  `Wakeshift: couldn't connect this conversation (${why}), so new messages won't reach it yet. To retry, call the app's tool that started watching again.`;
/** Connected, but OpenClaw's hooks can't wake it yet (why). */
export const noteNoWake = (why: string) => `${CONNECTED}, but new messages can't wake this conversation yet: ${why}. Until then they wait in Wakeshift.`;
/** A wake that OpenClaw's hooks refused (why), said on the conversation's next tool result. */
export const noteWakeFailed = (why: string) => `Wakeshift: couldn't wake this conversation: ${why}. New messages wait in Wakeshift until it can.`;
/** Wakeshift refused this version (426 update_required): its message asks for the update. */
export const noteOutdated = (message: string) => `Wakeshift: couldn't connect this conversation. ${message}`;

/** Wakeshift no longer serves this version of the plugin (426 update_required). */
class OutdatedError extends Error {}

// The text beacon line (beacon.Text), with the newlines before it, also as
// they appear inside JSON text, escaped once or more (OpenClaw's code mode
// shows an MCP result as JSON inside its exec result).
const BEACON_LINE = /(?:\\+n|\n)*\(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: [A-Za-z0-9_.-]+\)/g;

/** Whether a tool result has a beacon text line anywhere in it. */
export function hasBeaconLine(v: unknown): boolean {
  let s: string;
  try {
    s = typeof v === "string" ? v : (JSON.stringify(v) ?? "");
  } catch {
    return false;
  }
  return s.includes("wakeshift-beacon:");
}

/** A tool result with the beacon's text line taken out (anywhere in it). */
export function stripBeacon<T>(v: T): T {
  if (typeof v === "string") return v.replace(BEACON_LINE, "") as T;
  if (Array.isArray(v)) return v.map(stripBeacon) as T;
  if (v && typeof v === "object") {
    const o: Record<string, unknown> = {};
    for (const [k, x] of Object.entries(v)) o[k] = stripBeacon(x);
    return o as T;
  }
  return v;
}

/**
 * Whether a stream link is on the issuer's scheme and host (wss for https,
 * ws for http), as the plugin trusts only links from the pinned Wakeshift.
 */
export function linkOnIssuer(link: string, issuer: string): boolean {
  let l: URL, i: URL;
  try {
    l = new URL(link);
    i = new URL(issuer);
  } catch {
    return false;
  }
  const scheme: Record<string, string> = { "wss:": "https:", "https:": "https:", "ws:": "http:", "http:": "http:" };
  return !!l.host && scheme[l.protocol] !== undefined && scheme[l.protocol] === i.protocol && l.host.toLowerCase() === i.host.toLowerCase();
}

export type { Host };

/** How often delivery records are pruned while the gateway runs. */
export const PRUNE_EVERY = 3600 * 1000;

/** Keeps OpenClaw's hooks letting connected conversations be woken (control.ts). */
export interface Hooks {
  /** "" once hooks let this session be woken, else why not. */
  allow(sessionKey: string, agentId?: string): Promise<string>;
  release(inUse: { sessionKeys: string[]; agentIds: string[] }): Promise<void>;
}

export interface Options {
  issuer: string; // the pinned Wakeshift
  store: Store;
  host: Host;
  hooks: Hooks;
  fetch?: typeof fetch;
  /** Delivery timing (deliveries.ts). */
  wakeFor?: number;
  wakeRetry?: number;
  ackRetry?: number;
  redeliverAfter?: number;
  stream?: Partial<TransportOptions>;
}

interface Call {
  sessionKey?: string;
  agentId?: string;
  toolCallId?: string;
  toolName?: string;
}

export const notePinned = (why: string, file: string) =>
  why === PINS_UNREADABLE
    ? `Wakeshift: didn't connect this conversation: ${file} can't be read, so Wakeshift can't check which MCP server carries this app's messages. Fix the file, or remove it to start over, and call the app's tool again.`
    : `Wakeshift: didn't connect this conversation: ${why}. If that's the server you use for this app (you renamed or re-added it), remove its line from ${file} and call the app's tool again.`;

export class Wakeshift {
  private readonly f: typeof fetch;
  private readonly install: string;
  private readonly streams = new Map<string, { t: Transport; c: Conversation }>();
  private readonly ledger: Ledger;
  // Each session's last beacon and the note it produced: OpenClaw's own
  // runtime shows an MCP result twice (the call inside code mode, then the
  // exec result the model sees), and a ticket works once. One per session,
  // only while it's in flight or connected: dropped when connecting fails,
  // when the session's stream ends for good, and when the gateway starts.
  private readonly beacons = new Map<string, { token: string; note: Promise<string> }>();
  private keys?: { at: number; byKid: Map<string, ReturnType<typeof jwkKey>> };
  private keysFetchedAt = 0;
  private started = false;
  private pruneTimer?: ReturnType<typeof setInterval>;
  // Each session's last channel route (message_received), for the reply.
  private readonly routes = new Map<string, Route>();
  // A wake problem per session, and whether it was said, so it's said once.
  private readonly problems = new Map<string, { why: string; said: boolean }>();

  constructor(private readonly o: Options) {
    this.f = o.fetch ?? fetch;
    this.install = o.store.installId();
    this.ledger = new Ledger(join(o.store.dir, "deliveries"));
  }

  /** The conversation as Wakeshift knows it: unique to this install. */
  conversation(sessionKey: string) {
    return `openclaw/${this.install}/${sessionKey}`;
  }

  /** Starts the streams of conversations connected before (the gateway restarted). */
  start() {
    this.started = true;
    this.beacons.clear();
    this.prune();
    clearInterval(this.pruneTimer);
    this.pruneTimer = setInterval(() => this.prune(), PRUNE_EVERY);
    this.pruneTimer.unref?.();
    for (const l of this.o.store.links()) {
      // Only links from the Wakeshift pinned now: one saved under another
      // issuer (the pin changed since) or pointing elsewhere isn't used.
      if (l.issuer !== this.o.issuer || !linkOnIssuer(l.link, this.o.issuer)) {
        this.o.host.log.warn(`wakeshift (${l.sessionKey}): a saved link isn't from ${this.o.issuer}; not connecting it`);
        continue;
      }
      this.listen(l);
      void this.o.hooks.allow(l.sessionKey, l.agentId || agentOf(l.sessionKey)).then((why) => this.setProblem(l.sessionKey, why));
    }
  }

  /** The channel a session last spoke on (OpenClaw's message_received), for a woken turn's reply. */
  onMessage(sessionKey: string, route: Route | undefined) {
    // A message on OpenClaw's own surface (the TUI, the web chat) means the
    // reply belongs in the session, not on the channel it spoke on before.
    if (route) this.routes.set(sessionKey, route);
    else this.routes.delete(sessionKey);
  }

  private setProblem(sessionKey: string, why: string) {
    if (!why) {
      this.problems.delete(sessionKey);
      return;
    }
    if (this.problems.get(sessionKey)?.why === why) return;
    this.o.host.log.warn(`wakeshift (${sessionKey}): ${why}`);
    this.problems.set(sessionKey, { why, said: false });
  }

  /** A wake problem not yet told to this session's model, once ("" for none). */
  takeNote(sessionKey: string): string {
    const p = this.problems.get(sessionKey);
    if (!p || p.said) return "";
    p.said = true;
    return noteWakeFailed(p.why);
  }

  private prune() {
    try {
      this.ledger.prune();
    } catch (e) {
      this.o.host.log.warn(`wakeshift: pruning delivery records failed: ${(e as Error).message}`);
    }
  }

  stop() {
    this.started = false;
    clearInterval(this.pruneTimer);
    this.pruneTimer = undefined;
    for (const s of this.streams.values()) {
      s.c.stop();
      s.t.stop();
    }
    this.streams.clear();
  }

  get listening() {
    return [...this.streams.keys()];
  }

  /**
   * A tool result in a session: connects the session on a beacon for it.
   * Resolves to the note for the model ("" for none: no beacon, or one
   * that isn't trusted, which it says nothing about).
   */
  onResult(result: unknown, call: Call): Promise<string> {
    const token = beaconIn(result);
    if (!token || !call.sessionKey) return Promise.resolve("");
    const seen = this.beacons.get(call.sessionKey);
    if (seen?.token === token) return seen.note;
    // A beacon proves Wakeshift minted it, not that this result is its
    // app's: OpenClaw tells servers nothing, so it's bound to no
    // conversation, and one an attacker minted could sit in a page a tool
    // fetched or in another server's result. It connects only from an MCP
    // server's own result (code mode's exec result shows the same beacon
    // again, after its inner call did), from the server pinned to its app.
    const server = mcpServer(call.toolName);
    if (!server) return Promise.resolve("");
    const sessionKey = call.sessionKey;
    const note = this.connect(token, call, server)
      .catch((e) => {
        this.o.host.log.warn(`wakeshift: connecting failed: ${(e as Error).message}`);
        return "";
      })
      .then((n) => {
        // Kept only while it connected: a failure is tried again when the
        // beacon shows up again.
        if (!n.startsWith(CONNECTED) && this.beacons.get(sessionKey)?.note === note) this.beacons.delete(sessionKey);
        return n;
      });
    this.beacons.set(sessionKey, { token, note });
    return note;
  }

  private async connect(token: string, call: Call, server: string): Promise<string> {
    const sessionKey = call.sessionKey!;
    const conversation = this.conversation(sessionKey);
    let claims: Claims;
    try {
      claims = await this.verify(token, conversation);
    } catch (e) {
      if (e instanceof BeaconError) return ""; // not for us: say nothing
      throw e;
    }
    const pinned = this.o.store.pin(server, claims.app);
    if (pinned) return notePinned(pinned, this.o.store.pinsPath());
    // The link this conversation had until now, if any: after a gateway
    // restart OpenClaw may run its turns in a new Codex thread, whose calls
    // start a new Wakeshift session. Handing the old link back ends the
    // session it belongs to, so one conversation stays one subscription.
    const prior = this.o.store.loadLink(conversation);
    const supersedes = prior && prior.issuer === this.o.issuer && linkOnIssuer(prior.link, this.o.issuer) ? prior.link : "";
    let link: string;
    try {
      link = await this.redeem(claims, conversation, call.toolCallId ?? "", supersedes);
    } catch (e) {
      if (e instanceof OutdatedError) return noteOutdated(e.message);
      return noteFailed((e as Error).message);
    }
    if (!linkOnIssuer(link, this.o.issuer)) return noteFailed("Wakeshift gave a stream link on another host");
    const saved = { conversation, sessionKey, agentId: call.agentId ?? "", link, issuer: this.o.issuer };
    this.o.store.saveLink(saved);
    this.listen({ ...saved, saved_at: new Date().toISOString() });
    const why = await this.o.hooks.allow(sessionKey, call.agentId || agentOf(sessionKey));
    if (why) {
      // Said now, in the note: not again on the next result.
      this.problems.set(sessionKey, { why, said: true });
      return noteNoWake(why);
    }
    this.problems.delete(sessionKey);
    return NOTE_CONNECTED;
  }

  private async verify(token: string, conversation: string): Promise<Claims> {
    const issuers = [this.o.issuer];
    const keyFor = (kid: string) => this.keys?.byKid.get(kid);
    await this.loadKeys(false);
    try {
      return verifyBound(token, { conversation, unbound: true }, issuers, keyFor);
    } catch (e) {
      // A key that changed under a kid we have, or a new one: fetch once (at most once a minute).
      if (e instanceof BeaconError && (await this.loadKeys(true))) return verifyBound(token, { conversation, unbound: true }, issuers, keyFor);
      throw e;
    }
  }

  // The pinned issuer's keys, from disk or <issuer>/oauth/jwks; true if fetched now.
  private async loadKeys(refresh: boolean): Promise<boolean> {
    const parse = (body: string) => {
      const byKid = new Map<string, ReturnType<typeof jwkKey>>();
      try {
        for (const k of (JSON.parse(body).keys ?? []) as { kid?: string; kty?: string; crv?: string; x?: string }[]) if (k.kid) byKid.set(k.kid, jwkKey(k));
      } catch {
        /* none */
      }
      return byKid;
    };
    if (!refresh && this.keys) return false;
    if (!refresh) {
      const disk = this.o.store.loadKeys(this.o.issuer);
      if (disk) {
        this.keys = { at: disk.at, byKid: parse(disk.body) };
        return false;
      }
    }
    if (Date.now() - this.keysFetchedAt < 60000) return false;
    this.keysFetchedAt = Date.now();
    try {
      const r = await this.f(this.o.issuer.replace(/\/$/, "") + "/oauth/jwks", { signal: AbortSignal.timeout(5000), redirect: "error" });
      if (!r.ok) return false;
      const body = await r.text();
      this.o.store.saveKeys(this.o.issuer, body);
      this.keys = { at: Date.now(), byKid: parse(body) };
      return true;
    } catch {
      return false;
    }
  }

  private async redeem(c: Claims, conversation: string, call: string, supersedes: string): Promise<string> {
    const body: Record<string, string> = { ticket: c.ticket, thread: conversation };
    if (call) body.call = call;
    if (supersedes) body.supersedes = supersedes;
    let r: Response;
    try {
      r = await this.f(c.redeem, {
        method: "POST",
        headers: { "Content-Type": "application/json", [CLIENT_HEADER]: clientHeader() },
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(10000),
        redirect: "error", // the ticket and the old link go only to the issuer
      });
    } catch {
      throw new Error("Wakeshift couldn't be reached");
    }
    let out: { url?: string; error?: { code?: string; message?: string } } = {};
    try {
      out = (await r.json()) as typeof out;
    } catch {
      /* not JSON */
    }
    if (r.ok && typeof out.url === "string" && out.url) return out.url;
    if (r.status === 426 || out.error?.code === "update_required") {
      throw new OutdatedError(
        typeof out.error?.message === "string" && out.error.message
          ? out.error.message
          : "Wakeshift's hooks for OpenClaw are out of date. Ask the user whether to update them; only if they agree, run the install command again.",
      );
    }
    switch (out.error?.code) {
      case "unknown_ticket":
        throw new Error("the ticket expired or was used");
      case "no_grant":
        throw new Error("the app isn't allowed for this agent any more");
      case "wrong_thread":
        throw new Error("the ticket was for another conversation");
    }
    throw new Error(`Wakeshift answered ${r.status}`);
  }

  // One connected conversation: its transport reads, its Conversation
  // decides (deliveries.ts).
  private listen(l: SavedLink) {
    if (!this.started) return;
    const old = this.streams.get(l.conversation);
    old?.c.stop();
    old?.t.stop();
    const log = (m: string) => this.o.host.log.info(`wakeshift (${l.sessionKey}): ${m}`);
    let t!: Transport;
    const c = new Conversation({
      ledger: this.ledger,
      conversation: l.conversation,
      sessionKey: l.sessionKey,
      agentId: l.agentId || undefined,
      host: this.o.host,
      ack: (ids) => t.ack(ids),
      redeliver: () => t.reconnect(),
      route: () => this.routes.get(l.sessionKey),
      routeRefused: () => this.routes.delete(l.sessionKey),
      problem: (why) => this.setProblem(l.sessionKey, why),
      wakeFor: this.o.wakeFor,
      wakeRetry: this.o.wakeRetry,
      ackRetry: this.o.ackRetry,
      redeliverAfter: this.o.redeliverAfter,
      // The old one may still be inside a host call for these records: the
      // new one starts once it's done, so nothing is queued twice.
      after: old?.c.settled(),
    });
    t = new Transport({
      ...this.o.stream,
      link: l.link,
      fetch: this.f,
      log,
      receive: (ds) => c.receive(ds),
      onLink: (link) => this.o.store.saveLink({ conversation: l.conversation, sessionKey: l.sessionKey, agentId: l.agentId, link, issuer: l.issuer }),
      onStop: (why: Stopped) => {
        const cur = this.streams.get(l.conversation);
        if (cur?.t === t) {
          this.streams.delete(l.conversation);
          c.stop();
        }
        if (why.why === "gone") {
          this.beacons.delete(l.sessionKey); // its next beacon connects again
          log(`the stream ended (${why.reason}); forgetting its link`);
          if (this.o.store.loadLink(l.conversation)?.link === t.currentLink) this.o.store.forgetLink(l.conversation);
          this.problems.delete(l.sessionKey);
          // Its session and agent are no longer allowed in OpenClaw's hooks
          // (unless another connected conversation still needs them).
          const rest = this.o.store.links();
          void this.o.hooks.release({ sessionKeys: rest.map((x) => x.sessionKey), agentIds: rest.map((x) => x.agentId || agentOf(x.sessionKey) || "").filter(Boolean) });
        }
      },
    });
    this.streams.set(l.conversation, { t, c });
    c.start();
    t.start();
  }
}
