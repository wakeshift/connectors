// Transport for one conversation's Wakeshift stream link: reading only. It
// owns no delivery state (deliveries.ts does): it hands each delivery it
// reads to `receive`, and sends acknowledgments when asked (`ack`).
//
// Server-Sent Events first (GET /v1/stream?format=sse), long polling (GET
// /v1/stream/poll) as the fallback when SSE doesn't get through (a proxy
// that buffers it), both with the link's token as Authorization: Bearer,
// never in a URL. The cursor (Last-Event-ID) moves past a delivery only
// once `receive` has resolved for it, i.e. once it's recorded. hub.rotate
// is followed to the same origin only; hub.closed and a 401 or 410 (or
// Wakeshift's link_revoked) end the stream for good; anything else,
// including another 403 or 404 (a router during a deploy, a firewall),
// reconnects with backoff. A connection that ends at once without a
// delivery counts as a failure for the backoff.

import { CLIENT_HEADER, clientHeader } from "./version.js";

export interface Delivery {
  delivery_id: string;
  attempt: number;
  subscription_id?: string;
  topic?: string;
  app?: string;
  event: Record<string, unknown>;
}

/** Why a stream stopped for good. */
export type Stopped =
  | { why: "gone"; reason: string } // the link no longer works, or the session has nothing more to receive
  | { why: "replaced" } // a newer connection or poll holds the stream
  | { why: "outdated" } // Wakeshift no longer serves this version (426 update_required); the link is kept for the update
  | { why: "stopped" }; // stop() was called

export interface TransportOptions {
  link: string;
  /** Records deliveries; resolves once they're recorded. Returns how many were new. */
  receive: (ds: Delivery[]) => Promise<number>;
  onLink?: (link: string) => void;
  onStop?: (s: Stopped) => void;
  log?: (msg: string) => void;
  fetch?: typeof fetch;
  /** Long-poll wait in ms (default 25s). */
  pollWait?: number;
  /** SSE failures in a row (Wakeshift answered, SSE didn't work) before long polling (default 3). */
  sseTries?: number;
  /** While polling, how often to try SSE again (default 10 minutes). */
  sseRetryEvery?: number;
  /** Silence after which SSE counts as not getting through (default 50s; the server writes every 15s). */
  silentFor?: number;
  minBackoff?: number;
  maxBackoff?: number;
  /** A connection that lasted this long (or brought a delivery) resets the backoff (default 10s). */
  healthyAfter?: number;
}

/** The longest SSE line, and the largest poll answer, read; a longer line is skipped. */
export const MAX_LINE = 1 << 20;
export const MAX_RESPONSE = 8 << 20;

/** A stream link's parts: its origin as https (or http) and its token. */
export function parseLink(link: string): { base: string; token: string } | undefined {
  let u: URL;
  try {
    u = new URL(link);
  } catch {
    return undefined;
  }
  const scheme =
    u.protocol === "wss:" || u.protocol === "https:" ? "https:" : u.protocol === "ws:" || u.protocol === "http:" ? "http:" : "";
  const token = u.searchParams.get("link") ?? "";
  if (!scheme || !u.host || !token) return undefined;
  return { base: `${scheme}//${u.host}`, token };
}

export function sameOrigin(a: string, b: string): boolean {
  const x = parseLink(a),
    y = parseLink(b);
  return !!x && !!y && x.base === y.base;
}

// hub.closed reasons after which the stream goes on (reconnect).
const RETRY_REASONS = new Set(["restarting", "reconnect", "error"]);

class Gone extends Error {
  constructor(readonly reason: string) {
    super("gone: " + reason);
  }
}
class Replaced extends Error {
  constructor() {
    super("replaced");
  }
}
class Outdated extends Error {
  constructor() {
    super("Wakeshift no longer serves this version of the plugin");
  }
}
// Wakeshift answered but the stream didn't work: what a proxy that won't
// pass SSE looks like.
class SSEBlocked extends Error {}
// A delivery couldn't be recorded: not the stream's fault.
class RecordFailed extends Error {}
class Unavailable extends Error {
  constructor(readonly retryAfter?: number) {
    super("Wakeshift is busy or restarting");
  }
}

export class Transport {
  private link: string;
  private readonly f: typeof fetch;
  private readonly abort = new AbortController();
  private stopped = false;
  private lastEventId = "";
  private mode: "sse" | "poll" = "sse";
  private sseFailures = 0;
  private pollingSince = 0;
  private conn?: AbortController;
  private reconnecting = false;
  private chain: Promise<void> = Promise.resolve();
  private progress = false; // a delivery (or a new link) came on the current connection
  readonly done: Promise<Stopped>;
  private resolveDone!: (s: Stopped) => void;

  constructor(private readonly o: TransportOptions) {
    this.link = o.link;
    this.f = o.fetch ?? fetch;
    this.done = new Promise((r) => (this.resolveDone = r));
  }

  get currentLink() {
    return this.link;
  }
  get transport() {
    return this.mode;
  }

  start(): this {
    void this.run();
    return this;
  }

  stop() {
    this.stopped = true;
    this.abort.abort();
  }

  /** Waits until what's been read so far is recorded. */
  async settled() {
    await this.chain;
  }

  /**
   * Drops the SSE connection and makes a new one, so Wakeshift hands out
   * again what isn't acknowledged (on SSE that comes back only on a new
   * connection; a poll gets it back by itself).
   */
  reconnect() {
    if (this.mode === "sse" && this.conn && !this.conn.signal.aborted) {
      this.reconnecting = true;
      this.conn.abort();
    }
  }

  /**
   * POST /v1/stream/ack for up to 100 IDs: the HTTP status, or 0 if
   * Wakeshift couldn't be reached.
   */
  async ack(ids: string[]): Promise<number> {
    if (ids.length === 0 || ids.length > 100) throw new Error("ack: 1 to 100 IDs");
    const p = parseLink(this.link);
    if (!p) return 401;
    try {
      const r = await this.f(`${p.base}/v1/stream/ack`, {
        method: "POST",
        headers: { Authorization: `Bearer ${p.token}`, "Content-Type": "application/json", [CLIENT_HEADER]: clientHeader() },
        body: JSON.stringify({ delivery_ids: ids }),
        signal: AbortSignal.any([this.abort.signal, AbortSignal.timeout(10000)]),
        redirect: "error", // never the token to wherever a 3xx points
      });
      if ((r.status === 403 || r.status === 404) && (await revoked(r))) return 401;
      await r.body?.cancel().catch(() => {});
      return r.status;
    } catch {
      return 0;
    }
  }

  private log(m: string) {
    this.o.log?.(m);
  }

  private async run() {
    const minBackoff = this.o.minBackoff ?? 1000;
    const maxBackoff = this.o.maxBackoff ?? 30000;
    const pollWait = this.o.pollWait ?? 25000;
    let backoff = minBackoff;
    let result: Stopped = { why: "stopped" };
    while (!this.stopped) {
      try {
        if (this.mode === "poll" && Date.now() - this.pollingSince > (this.o.sseRetryEvery ?? 600000)) this.mode = "sse";
        if (this.mode === "sse") {
          this.progress = false;
          const began = Date.now();
          await this.sse(); // returns when the stream says to reconnect
          if (this.progress || Date.now() - began >= (this.o.healthyAfter ?? 10000)) {
            backoff = minBackoff;
            continue;
          }
          // Ended at once with nothing new (a proxy that closes at once, a
          // repeated hub.closed "restarting"): don't reconnect in a tight loop.
          await sleep(backoff, this.abort.signal);
          backoff = Math.min(backoff * 2, maxBackoff);
          continue;
        }
        const started = Date.now();
        const fresh = await this.poll();
        // Nothing new (empty, or only what's recorded and waiting for its
        // turn, which a poll hands out again at once): don't spin.
        if (fresh === 0 && Date.now() - started < pollWait / 2) {
          await sleep(backoff, this.abort.signal);
          backoff = Math.min(backoff * 2, maxBackoff);
        } else backoff = minBackoff;
      } catch (e) {
        if (this.stopped) break;
        if (e instanceof Gone) {
          result = { why: "gone", reason: e.reason };
          break;
        }
        if (e instanceof Replaced) {
          result = { why: "replaced" };
          break;
        }
        if (e instanceof Outdated) {
          this.log("Wakeshift no longer serves this version of the plugin: update it to receive messages");
          result = { why: "outdated" };
          break;
        }
        // Only Wakeshift answering and SSE still not working counts (not
        // Wakeshift being unreachable, which polling wouldn't change).
        if (this.mode === "sse" && e instanceof SSEBlocked && ++this.sseFailures >= (this.o.sseTries ?? 3)) {
          this.log("Server-Sent Events aren't getting through; long polling instead");
          this.mode = "poll";
          this.pollingSince = Date.now();
          this.sseFailures = 0;
        }
        const wait = e instanceof Unavailable && e.retryAfter ? e.retryAfter * 1000 : backoff;
        this.log(`stream: ${(e as Error).message}; again in ${Math.round(wait / 1000)}s`);
        await sleep(wait, this.abort.signal);
        backoff = Math.min(backoff * 2, maxBackoff);
      }
    }
    await this.chain.catch(() => {});
    if (this.stopped) result = { why: "stopped" };
    this.o.onStop?.(result);
    this.resolveDone(result);
  }

  private parts() {
    const p = parseLink(this.link);
    if (!p) throw new Gone("not a stream link");
    return p;
  }

  private async status(r: Response) {
    if (r.status === 426) throw new Outdated();
    if (r.status === 401 || r.status === 410) throw new Gone("link_revoked");
    if ((r.status === 403 || r.status === 404) && (await revoked(r))) throw new Gone("link_revoked");
    if (r.status === 409) throw new Replaced();
    if (r.status === 503 || r.status === 429) {
      const ra = Number(r.headers.get("retry-after"));
      throw new Unavailable(Number.isFinite(ra) && ra > 0 ? Math.min(ra, 120) : undefined);
    }
    if (!r.ok) throw new Error(`Wakeshift answered ${r.status}`);
  }

  private rotate(next: unknown) {
    if (typeof next !== "string" || !next) return;
    const p = parseLink(next);
    if (!p || !sameOrigin(next, this.link)) return;
    const cur = new URL(this.link);
    const link = `${cur.protocol}//${cur.host}/v1/stream?link=${encodeURIComponent(p.token)}`;
    if (link === this.link) return;
    this.link = link;
    this.progress = true;
    this.o.onLink?.(link);
  }

  // Records deliveries in order, one batch at a time; resolves with how
  // many were new. The cursor moves only once they're recorded.
  // Throws if recording fails: the caller stops reading this connection
  // (or poll) there and reconnects, with the cursor still before it.
  private receive(ds: Delivery[]): Promise<number> {
    const valid = ds.filter((d) => d && typeof d.delivery_id === "string" && d.event && typeof d.event === "object");
    const p = this.chain.then(async () => {
      if (valid.length === 0 || this.stopped) return 0;
      const n = await this.o.receive(valid);
      this.progress = true;
      this.lastEventId = valid[valid.length - 1].delivery_id; // SSE ids are delivery IDs
      return n;
    });
    this.chain = p.then(
      () => {},
      () => {},
    );
    return p.catch((e) => {
      throw new RecordFailed(`recording failed: ${(e as Error).message}`);
    });
  }

  // One SSE connection, until the stream says to reconnect (returns) or
  // ends any other way (throws).
  private async sse(): Promise<void> {
    const { base, token } = this.parts();
    const conn = new AbortController();
    this.conn = conn;
    this.reconnecting = false;
    const onStop = () => conn.abort();
    this.abort.signal.addEventListener("abort", onStop, { once: true });
    let last = Date.now();
    const silentFor = this.o.silentFor ?? 50000;
    let silent = false;
    let answered = false;
    const watchdog = setInterval(
      () => {
        if (Date.now() - last > silentFor) {
          silent = true;
          conn.abort();
        }
      },
      Math.min(5000, silentFor / 2),
    );
    try {
      const headers: Record<string, string> = { Accept: "text/event-stream", Authorization: `Bearer ${token}`, [CLIENT_HEADER]: clientHeader() };
      if (this.lastEventId) headers["Last-Event-ID"] = this.lastEventId;
      const r = await this.f(`${base}/v1/stream?format=sse`, { headers, signal: conn.signal, redirect: "error" });
      answered = true;
      await this.status(r);
      if (!r.body) throw new Error("no body");
      const reader = r.body.pipeThrough(new TextDecoderStream()).getReader();
      try {
        let buf = "";
        let ev: { event?: string; data: string[]; size: number; skip?: boolean } = { data: [], size: 0 };
        let longLine = false; // the rest of an over-long line is being skipped
        for (;;) {
          const { value, done } = await reader.read();
          if (done) throw new Error("the stream ended");
          last = Date.now();
          this.sseFailures = 0;
          buf += value;
          let i: number;
          while ((i = buf.search(/\r\n|\r|\n/)) >= 0) {
            const line = buf.slice(0, i);
            buf = buf.slice(i + (buf.startsWith("\r\n", i) ? 2 : 1));
            if (longLine || line.length > MAX_LINE) {
              if (!longLine) this.log("skipped an over-long stream line");
              longLine = false; // its end: the event it was in is dropped
              ev = { data: [], size: 0, skip: true };
              continue;
            }
            if (line === "") {
              // Each frame is recorded before the next is read.
              if (!ev.skip && ev.data.length > 0 && (await this.onFrame(ev.event ?? "message", ev.data.join("\n"))) === "reconnect") return;
              ev = { data: [], size: 0 };
              continue;
            }
            if (line.startsWith(":")) continue;
            const c = line.indexOf(":");
            const k = c < 0 ? line : line.slice(0, c);
            const v = c < 0 ? "" : line.slice(c + 1).replace(/^ /, "");
            if (k === "data") {
              ev.size += v.length;
              if (ev.size > MAX_LINE * 4) ev = { data: [], size: 0, skip: true };
              if (!ev.skip) ev.data.push(v);
            } else if (k === "event") ev.event = v;
          }
          if (buf.length > MAX_LINE) {
            this.log("skipped an over-long stream line");
            buf = "";
            longLine = true;
            ev = { data: [], size: 0, skip: true };
          }
        }
      } finally {
        reader.cancel().catch(() => {});
      }
    } catch (e) {
      if (this.reconnecting && !this.stopped) return; // asked to reconnect
      if (silent && !this.stopped) throw new SSEBlocked(`nothing came on the stream for ${Math.round(silentFor / 1000)}s`);
      if (answered && !(e instanceof Gone || e instanceof Replaced || e instanceof Outdated || e instanceof Unavailable || e instanceof RecordFailed)) throw new SSEBlocked((e as Error).message);
      throw e;
    } finally {
      clearInterval(watchdog);
      this.abort.signal.removeEventListener("abort", onStop);
      conn.abort();
    }
  }

  // A frame: "reconnect" when the stream ends here and goes on; throws
  // when it ends for good.
  private async onFrame(type: string, data: string): Promise<"reconnect" | undefined> {
    let m: Record<string, unknown>;
    try {
      m = JSON.parse(data);
    } catch {
      return undefined;
    }
    const t = typeof m.type === "string" ? m.type : type;
    if (t === "event") {
      await this.receive([m as unknown as Delivery]); // a failure ends this connection here
      return undefined;
    }
    if (t === "hub.rotate") {
      this.rotate(m.url);
      return "reconnect";
    }
    if (t === "hub.closed") {
      const reason = typeof m.reason === "string" ? m.reason : "";
      if (reason === "replaced") throw new Replaced();
      if (reason === "update_required") throw new Outdated(); // the minimum was raised; the link stays for the update
      if (RETRY_REASONS.has(reason)) return "reconnect";
      throw new Gone(reason || "closed");
    }
    return undefined;
  }

  // One long poll; how many new deliveries it brought.
  private async poll(): Promise<number> {
    const { base, token } = this.parts();
    await this.chain;
    const wait = this.o.pollWait ?? 25000;
    const r = await this.f(`${base}/v1/stream/poll?wait=${wait}ms`, {
      headers: { Authorization: `Bearer ${token}`, [CLIENT_HEADER]: clientHeader() },
      signal: AbortSignal.any([this.abort.signal, AbortSignal.timeout(wait + 15000)]),
      redirect: "error",
    });
    await this.status(r);
    const b = JSON.parse(await bounded(r, MAX_RESPONSE)) as { deliveries?: Delivery[]; rotate?: { url?: string } };
    if (b.rotate?.url) this.rotate(b.rotate.url);
    const ds = b.deliveries ?? [];
    return ds.length > 0 ? this.receive(ds) : 0;
  }
}

// Whether a 403 or 404 is Wakeshift's own link_revoked (not a router's or a firewall's).
async function revoked(r: Response): Promise<boolean> {
  try {
    const b = JSON.parse(await bounded(r, 1 << 16)) as { error?: { code?: string } };
    return b?.error?.code === "link_revoked";
  } catch {
    return false;
  }
}

// A response's body as text, refusing one over max bytes.
async function bounded(r: Response, max: number): Promise<string> {
  if (!r.body) return "";
  const reader = r.body.getReader();
  const parts: Uint8Array[] = [];
  let n = 0;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      n += value.byteLength;
      if (n > max) throw new Error(`Wakeshift's answer was over ${max} bytes`);
      parts.push(value);
    }
  } finally {
    reader.cancel().catch(() => {});
  }
  return Buffer.concat(parts).toString("utf8");
}

export function sleep(ms: number, signal: AbortSignal) {
  return new Promise<void>((resolve) => {
    if (signal.aborted) return resolve();
    const onAbort = () => {
      clearTimeout(t);
      resolve();
    };
    const t = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}
