// Delivery state: one durable record per delivery, and the one place that
// decides what happens to it.
//
// A record lives at <state dir>/deliveries/<sha256(conversation)>/<sha256(delivery ID)>.json
// (0600 in 0700 directories) and moves through
//
//   taken → turn-started → acked
//
// (or `refused`: Wakeshift answered 400 for that ID alone, so it's not
// sent again; or `superseded`: a turn had it, but no stream that carries it
// is left to acknowledge it on, below).
//
//   taken         recorded on arrival, with the delivery; not yet handled
//                 by a turn that finished.
//   turn-started  a woken turn that had it finished (OpenClaw's /hooks/agent
//                 answered completion ok): only now may it be acknowledged.
//   acked         Wakeshift answered 2xx to its ack.
//
// (`enqueued`, from the plugin's earlier heartbeat wake, is read as taken.)
//
// Streams: a conversation can watch several apps, each on its own link
// (store.ts), so several streams feed one conversation's records. A record
// names the stream it last arrived on (`via`). A stream that was replaced by
// a newer link of its app (a new Wakeshift session superseding the old one,
// which cancels what the old one held), or that ended for good, is lost:
// what it brought still runs here (nothing else would bring it), but isn't
// acknowledged on any link, as none that's left carries it; it's marked
// superseded once a turn had it. If it arrives again on a live stream, it's
// that stream's, and acknowledged there.
//
// Invariants (tested in test/deliveries.test.ts, including random
// interleavings of redelivery, failures and restarts):
//
//   1. A delivery is in at most one wake request at a time: requests are
//      made on the conversation's one chain, one after another, and a
//      redelivery only opens a new window for the next try. A try after an
//      outcome that isn't known (unreachable, busy, a timeout) sends the
//      same request again, the same records with the same idempotency key
//      (recorded as `unsure`), before anything newer, so OpenClaw replays a
//      run it admitted instead of running it again; a try after a known
//      one (a run that ended without ok, a refused request) uses a new key.
//   2. It's acknowledged only from `turn-started`, only on the stream it
//      last arrived on and only while that stream is live, and only a 2xx
//      moves it to `acked`; nothing is acknowledged after that.
//   3. A record isn't evicted while it isn't acked or refused (no capped
//      sets): records are pruned only once acked or refused for ACKED_TTL,
//      or after MAX_AGE whatever their state.
//   4. Every decision for a delivery reads and writes only its record (and
//      for a publish, the records of its other copies), on the
//      conversation's one chain, whichever of its streams brought it (the
//      gateway is one process, and owns its state dir), so no two decisions
//      for it interleave, and turns start one at a time per conversation.
//   5. A publish starts one turn: a copy of one that a turn already had (it
//      reached the conversation through two of its app's sessions, the
//      superseded one and its successor) is marked as had by that turn,
//      without one of its own, and one wake request carries it once.
//
// The gaps (at least once, never lost): OpenClaw keeps its replay keys in
// memory, so a request in flight when the gateway restarts is sent again
// and may run a second time; so may a turn that finished after its
// request's answer was lost, once its key has aged out.
//
// Turns: a turn is a wake request (wake.ts). One that doesn't run is tried
// again with backoff for `wakeFor` from when the delivery arrived (or was
// redelivered); past that, the transport reconnects so Wakeshift hands it
// out again, which opens a new window. Acks: sent in chunks of at most
// 100, retried with backoff on their own timer; a 400 splits the chunk to
// find the bad ID (logged and marked refused), and the rest go on.

import { createHash } from "node:crypto";
import { chmodSync, mkdirSync, readdirSync, readFileSync, rmdirSync, rmSync } from "node:fs";
import { join } from "node:path";
import { wakeText } from "./framing.js";
import { writeAtomic } from "./store.js";
import type { Delivery } from "./transport.js";
import type { Route, WakeRequest, WakeResult } from "./wake.js";

export type State = "taken" | "enqueued" | "turn-started" | "acked" | "refused" | "superseded";

export interface Record {
  id: string;
  conversation: string;
  state: State;
  delivery: Delivery;
  takenAt: number;
  boot?: string; // (the heartbeat wake) the process it was queued in
  windowFrom?: number; // when its current turn window opened
  attempt?: number; // its wake requests so far that ended known (the idempotency key's part)
  unsure?: string; // the key of a request it was in whose outcome isn't known: sent again as is
  unsureRoute?: Route; // that request's route
  turnAt?: number;
  doneAt?: number; // acked, refused or superseded
  via?: string; // the stream it last arrived on ("" or absent: a link saved before streams were named)
}

export const ACKED_TTL = 24 * 3600 * 1000;
export const MAX_AGE = 7 * 24 * 3600 * 1000;

const hash = (s: string) => createHash("sha256").update(s).digest("hex").slice(0, 32);

/** The records on disk: one directory per conversation. */
export class Ledger {
  constructor(readonly dir: string) {}

  private convDir(conversation: string) {
    return join(this.dir, hash(conversation));
  }

  private path(conversation: string, id: string) {
    return join(this.convDir(conversation), hash(id) + ".json");
  }

  get(conversation: string, id: string): Record | undefined {
    try {
      const r = JSON.parse(readFileSync(this.path(conversation, id), "utf8")) as Record;
      return r.id === id && r.conversation === conversation ? r : undefined;
    } catch {
      return undefined;
    }
  }

  put(r: Record) {
    const d = this.convDir(r.conversation);
    mkdirSync(d, { recursive: true, mode: 0o700 });
    try {
      chmodSync(this.dir, 0o700);
      chmodSync(d, 0o700);
    } catch {
      /* best effort */
    }
    writeAtomic(this.path(r.conversation, r.id), JSON.stringify(r));
  }

  /** A conversation's records, oldest first (reads only its directory). */
  of(conversation: string): Record[] {
    return read(this.convDir(conversation))
      .filter((r) => r.conversation === conversation)
      .sort((a, b) => a.takenAt - b.takenAt);
  }

  all(): Record[] {
    return names(this.dir).flatMap((n) => (n.endsWith(".json") ? [] : read(join(this.dir, n))));
  }

  /**
   * Deletes records acked or refused over ACKED_TTL ago, and any over
   * MAX_AGE, and directories left empty. Records in the old flat layout
   * (<dir>/<hash>.json) are moved into their conversation's directory.
   */
  prune(now = Date.now()) {
    for (const n of names(this.dir)) {
      if (!n.endsWith(".json")) continue;
      const p = join(this.dir, n);
      try {
        const r = JSON.parse(readFileSync(p, "utf8")) as Record;
        if (r && typeof r.id === "string" && typeof r.conversation === "string" && !this.get(r.conversation, r.id)) this.put(r);
      } catch {
        /* unreadable: dropped */
      }
      rmSync(p, { force: true });
    }
    for (const n of names(this.dir)) {
      const d = join(this.dir, n);
      for (const r of read(d)) {
        const done = finished(r) && now - (r.doneAt ?? r.takenAt) > ACKED_TTL;
        if (done || now - r.takenAt > MAX_AGE) rmSync(this.path(r.conversation, r.id), { force: true });
      }
      try {
        rmdirSync(d); // only if empty
      } catch {
        /* not empty */
      }
    }
  }
}

function names(dir: string): string[] {
  try {
    return readdirSync(dir);
  } catch {
    return [];
  }
}

function read(dir: string): Record[] {
  const out: Record[] = [];
  for (const n of names(dir)) {
    if (!n.endsWith(".json")) continue;
    try {
      out.push(JSON.parse(readFileSync(join(dir, n), "utf8")) as Record);
    } catch {
      /* skip */
    }
  }
  return out;
}

/** What a conversation needs from OpenClaw. */
export interface Host {
  /** One wake request (wake.ts). */
  wake(r: WakeRequest): Promise<WakeResult>;
  log: { info(m: string): void; warn(m: string): void };
}

export interface ConversationOptions {
  ledger: Ledger;
  conversation: string;
  sessionKey: string;
  agentId?: string;
  host: Host;
  /** Sends one ack chunk (≤100 IDs) on the stream via: the HTTP status, 0 if unreachable. */
  ack: (ids: string[], via: string) => Promise<number>;
  /**
   * Whether a stream reads now (live), has a link but isn't reading
   * (waiting: what's for it is acknowledged once it's back), or is lost
   * (replaced by a newer link, or ended for good). Default: live.
   */
  stream?: (via: string) => "live" | "waiting" | "lost";
  /** Makes Wakeshift hand out again what isn't acknowledged, on these streams. */
  redeliver: (vias: string[]) => void;
  /** Where the woken turn's reply goes, when the conversation has a channel. */
  route?: () => Route | undefined;
  /** The route was refused: it's dropped. */
  routeRefused?: () => void;
  /** OpenClaw's hooks can't take the wake (why), or can again (""). */
  problem?: (why: string) => void;
  wakeFor?: number; // default 5 minutes
  wakeRetry?: number; // first turn retry (default 5s, doubling to 60s)
  ackRetry?: number; // first ack retry (default 1s, doubling to 30s)
  redeliverAfter?: number; // after a turn window closes (default 30s)
  now?: () => number;
  /**
   * Nothing runs until this settles: the conversation's previous
   * Conversation (a re-listen), so the two never step on its records at once.
   */
  after?: Promise<void>;
}

/** Not yet handled by a turn that finished (enqueued: the earlier heartbeat wake's state). */
const pending = (r: Record) => r.state === "taken" || r.state === "enqueued";
/** Done with: nothing more is sent for it. */
const finished = (r: Record) => r.state === "acked" || r.state === "refused" || r.state === "superseded";
const viaOf = (r: Record) => r.via ?? "";

/**
 * Which of its app's publishes a delivery carries, the same in every
 * session it reached ("" if it doesn't say): a topic copy has the
 * publish's own ID as its event ID in every workspace, a direct copy names
 * it in $emit (internal/appapi/fanout.go). Scoped by the app it's from, so
 * two apps' IDs can't collide.
 */
export function publishKey(d: Delivery): string {
  const ev = d?.event;
  if (!ev || typeof ev !== "object") return "";
  const src = ev.source;
  if (typeof src !== "string" || !src.startsWith("app:")) return "";
  const attrs = ev.attrs && typeof ev.attrs === "object" ? (ev.attrs as { [k: string]: unknown }) : {};
  const id = attrs["$emit"] || ev.id;
  return typeof id === "string" && id ? `${src}|${id}` : "";
}

/** The wake request's idempotency key: the same deliveries and attempt, the same key. */
export function idempotencyKey(conversation: string, ids: string[], attempt: number, variant = ""): string {
  return "wakeshift-" + createHash("sha256").update(JSON.stringify([conversation, [...ids].sort(), attempt, variant])).digest("hex").slice(0, 40);
}

/** One record per publish (the first), for a wake request's message. */
function once(rs: Record[]): Record[] {
  const seen = new Set<string>();
  return rs.filter((r) => {
    const k = publishKey(r.delivery);
    if (!k) return true;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
}

/** One conversation's deliveries, from every stream it has: every decision about them is made here, on one chain. */
export class Conversation {
  private chain: Promise<void> = Promise.resolve();
  private stopped = false;
  private turnTimer?: ReturnType<typeof setTimeout>;
  private ackTimer?: ReturnType<typeof setTimeout>;
  private redeliverTimer?: ReturnType<typeof setTimeout>;
  private turnDelay = 0;
  private ackDelay = 0;

  constructor(private readonly o: ConversationOptions) {
    if (o.after) this.chain = o.after.catch(() => {});
  }

  private now() {
    return this.o.now ? this.o.now() : Date.now();
  }

  private on<T>(fn: () => Promise<T> | T): Promise<T> {
    const p = this.chain.then(() => (this.stopped ? (undefined as T) : fn()));
    this.chain = p.then(
      () => {},
      (e) => this.o.host.log.warn(`wakeshift (${this.o.sessionKey}): ${(e as Error).message}`),
    );
    return p;
  }

  /** Picks up what an earlier process left (turns, acks). */
  start() {
    void this.on(() => this.step());
  }

  stop() {
    this.stopped = true;
    clearTimeout(this.turnTimer);
    clearTimeout(this.ackTimer);
    clearTimeout(this.redeliverTimer);
  }

  /** Waits until everything queued on the chain so far is done. */
  async settled() {
    for (let c = this.chain; ; c = this.chain) {
      await c; // and whatever was queued on it meanwhile
      if (c === this.chain) return;
    }
  }

  /**
   * Records deliveries from the stream; resolves once recorded, with how
   * many were new. A redelivery of one not yet handled opens a new turn
   * window; one a turn handled, acked or refused is left alone.
   */
  receive(ds: Delivery[], via = ""): Promise<number> {
    return this.on(async () => {
      let fresh = 0;
      const now = this.now();
      // Arriving on a live stream makes it that stream's to acknowledge (a
      // stream that was replaced may still hand over what it read).
      const live = this.streamOf(via) === "live";
      for (const d of ds) {
        const r = this.o.ledger.get(this.o.conversation, d.delivery_id);
        if (!r) {
          this.o.ledger.put({ id: d.delivery_id, conversation: this.o.conversation, state: "taken", delivery: d, takenAt: now, via });
          fresh++;
        } else if (pending(r)) {
          this.o.ledger.put({ ...r, ...(live ? { via } : {}), windowFrom: now });
          this.turnDelay = 0;
        } else if (r.state === "turn-started" && live) {
          this.o.ledger.put({ ...r, via });
        } else if (r.state === "superseded" && live) {
          // Had by a turn, and on a stream that can acknowledge it after all.
          this.o.ledger.put({ ...r, via, state: "turn-started", doneAt: undefined });
        }
      }
      void this.on(() => this.step()); // recorded: the rest happens after, on the chain
      return fresh;
    });
  }

  private streamOf(via: string) {
    return this.o.stream ? this.o.stream(via) : "live";
  }

  // streamOf, asked once per stream for one pass over the records.
  private streams() {
    const m = new Map<string, "live" | "waiting" | "lost">();
    return (via: string) => {
      let s = m.get(via);
      if (!s) m.set(via, (s = this.streamOf(via)));
      return s;
    };
  }

  // Everything that can happen now, from the records alone.
  private async step() {
    // After stop() nothing new starts (a step already past a wake request
    // still records what came of it).
    if (!this.stopped && this.o.ledger.of(this.o.conversation).some(pending)) await this.turn();
    if (!this.stopped && this.o.ledger.of(this.o.conversation).some((r) => r.state === "turn-started")) await this.acks();
  }

  // One wake request for everything not yet handled; or, after a request
  // whose outcome isn't known, that same request again (the same records,
  // the same key), so OpenClaw replays it rather than running it twice.
  private async turn() {
    const { host } = this.o;
    this.twins();
    const all = this.o.ledger.of(this.o.conversation).filter(pending);
    const unsure = all.find((r) => r.unsure)?.unsure;
    const waiting = unsure ? all.filter((r) => r.unsure === unsure) : all;
    if (waiting.length === 0) return;
    const attempt = Math.max(...waiting.map((w) => w.attempt ?? 0));
    let req = {
      sessionKey: this.o.sessionKey,
      ...(this.o.agentId ? { agentId: this.o.agentId } : {}),
      message: wakeText(once(waiting).map((w) => w.delivery)),
      idempotencyKey: unsure ?? idempotencyKey(this.o.conversation, waiting.map((w) => w.id), attempt),
      // A request sent again as is (unsure) keeps the route it had.
      route: unsure ? waiting[0].unsureRoute : this.o.route?.(),
    };
    let r: WakeResult;
    try {
      r = await host.wake(req);
      if (r.kind === "route") {
        // The channel it last spoke on was refused (nothing ran): the reply
        // stays in the session instead, a new request without the route.
        host.log.info(`wakeshift (${this.o.sessionKey}): the reply's route was refused (${r.why}); waking without it`);
        this.o.routeRefused?.();
        req = { ...req, route: undefined, idempotencyKey: idempotencyKey(this.o.conversation, waiting.map((w) => w.id), attempt, "noroute") };
        r = await host.wake(req);
        if (r.kind === "route") r = { kind: "broken", why: `OpenClaw refused the wake (400: ${r.why})` };
      }
    } catch (e) {
      r = { kind: "retry", fresh: false, why: (e as Error).message };
    }
    if (r.kind === "ran") {
      const now = this.now();
      for (const w of waiting) this.o.ledger.put({ ...w, state: "turn-started", turnAt: now, unsure: undefined, unsureRoute: undefined });
      this.turnDelay = 0;
      this.o.problem?.("");
      // What arrived while a request was sent again as is (unsure) wasn't
      // in it: its own turn next (no timer or delivery would start one).
      if (this.o.ledger.of(this.o.conversation).some(pending)) void this.on(() => this.step());
      return;
    }
    if (r.kind === "broken") this.o.problem?.(r.why);
    // A known outcome (a run that ended without ok, a refused request) needs
    // a new key; an unknown one keeps it, with these records, so a run
    // OpenClaw admitted is replayed, not run again.
    const known = r.kind !== "retry" || r.fresh;
    for (const w of waiting) {
      this.o.ledger.put(
        known ? { ...w, attempt: attempt + 1, unsure: undefined, unsureRoute: undefined } : { ...w, unsure: req.idempotencyKey, unsureRoute: req.route },
      );
    }
    host.log.info(`wakeshift (${this.o.sessionKey}): the wake didn't run (${r.why})`);
    this.retryTurn(Math.min(...waiting.map((w) => w.windowFrom ?? w.takenAt)));
  }

  // A copy of a publish that a turn already had (the same publish through
  // another session) is had by that turn: no turn of its own. Every record
  // that isn't pending had a turn: refused and superseded are reached only
  // from turn-started.
  private twins() {
    const recs = this.o.ledger.of(this.o.conversation);
    const had = new Set(recs.filter((r) => !pending(r)).map((r) => publishKey(r.delivery)).filter(Boolean));
    if (had.size === 0) return;
    const now = this.now();
    for (const r of recs) {
      if (pending(r) && !r.unsure && had.has(publishKey(r.delivery))) {
        this.o.ledger.put({ ...r, state: "turn-started", turnAt: now });
      }
    }
  }

  // Tries the turn again with backoff while the window is open; once it
  // closes, has Wakeshift hand the delivery out again (a new window).
  private retryTurn(windowFrom: number) {
    if (this.stopped || this.turnTimer) return;
    const base = this.o.wakeRetry ?? 5000;
    this.turnDelay = this.turnDelay ? Math.min(this.turnDelay * 2, 60000) : base;
    if (this.now() + this.turnDelay > windowFrom + (this.o.wakeFor ?? 300000)) {
      this.o.host.log.warn(`wakeshift (${this.o.sessionKey}): no turn could run in time; Wakeshift will hand it out again`);
      this.turnDelay = 0;
      clearTimeout(this.redeliverTimer);
      this.redeliverTimer = setTimeout(() => !this.stopped && void this.on(() => this.reopen()), this.o.redeliverAfter ?? 30000);
      return;
    }
    this.turnTimer = setTimeout(() => {
      this.turnTimer = undefined;
      void this.on(() => this.step());
    }, this.turnDelay);
  }

  // A window closed: Wakeshift hands out again what a live stream carries;
  // what no live stream carries (a replaced stream's) has its window opened
  // here, as nothing else would bring it.
  private async reopen() {
    const waiting = this.o.ledger.of(this.o.conversation).filter(pending);
    const vias = new Set<string>();
    const now = this.now();
    let local = false;
    const streamOf = this.streams();
    for (const r of waiting) {
      if (streamOf(viaOf(r)) === "live") vias.add(viaOf(r));
      else {
        this.o.ledger.put({ ...r, windowFrom: now });
        local = true;
      }
    }
    if (vias.size > 0) this.o.redeliver([...vias]);
    if (local) {
      this.turnDelay = 0;
      await this.step();
    }
  }

  // Acknowledges what a turn took, in chunks of 100, each on the stream it
  // came from; what came from a lost stream is done with, unacknowledged.
  private async acks() {
    const by = new Map<string, string[]>();
    const now = this.now();
    const streamOf = this.streams();
    for (const r of this.o.ledger.of(this.o.conversation)) {
      if (r.state !== "turn-started") continue;
      const s = streamOf(viaOf(r));
      if (s === "lost") this.o.ledger.put({ ...r, state: "superseded", doneAt: now });
      else if (s === "live") by.set(viaOf(r), [...(by.get(viaOf(r)) ?? []), r.id]);
      // waiting: acknowledged once its stream is back
    }
    let retry = false;
    for (const [via, ids] of by) {
      for (let i = 0; i < ids.length && !this.stopped; i += 100) {
        const res = await this.ackChunk(ids.slice(i, i + 100), via);
        if (res === "gone") break; // the link no longer works: the transport ends it
        if (res === "retry") retry = true;
      }
    }
    if (retry) this.retryAcks();
    else this.ackDelay = 0;
  }

  // One chunk: 2xx marks them acked; a 400 splits it to find the bad ID.
  private async ackChunk(ids: string[], via: string): Promise<"ok" | "retry" | "gone"> {
    const status = await this.o.ack(ids, via);
    if (status >= 200 && status < 300) {
      const now = this.now();
      for (const id of ids) {
        const r = this.o.ledger.get(this.o.conversation, id);
        if (r && r.state === "turn-started") this.o.ledger.put({ ...r, state: "acked", doneAt: now });
      }
      return "ok";
    }
    if (status === 400) {
      if (ids.length === 1) {
        this.o.host.log.warn(`wakeshift (${this.o.sessionKey}): Wakeshift refused the ack for ${ids[0]}; not sending it again`);
        const r = this.o.ledger.get(this.o.conversation, ids[0]);
        if (r) this.o.ledger.put({ ...r, state: "refused", doneAt: this.now() });
        return "ok";
      }
      const half = Math.ceil(ids.length / 2);
      const a = await this.ackChunk(ids.slice(0, half), via);
      const b = await this.ackChunk(ids.slice(half), via);
      return a === "gone" || b === "gone" ? "gone" : a === "retry" || b === "retry" ? "retry" : "ok";
    }
    // Gone only as the transport counts it (401, 410; a 403 or 404 that's
    // Wakeshift's link_revoked comes back as 401): another 403 or 404 is retried.
    if (status === 401 || status === 410) return "gone";
    this.o.host.log.info(`wakeshift (${this.o.sessionKey}): ack: ${status ? `Wakeshift answered ${status}` : "Wakeshift couldn't be reached"}; trying again`);
    return "retry";
  }

  private retryAcks() {
    if (this.stopped || this.ackTimer) return;
    const base = this.o.ackRetry ?? 1000;
    this.ackDelay = this.ackDelay ? Math.min(this.ackDelay * 2, 30000) : base;
    this.ackTimer = setTimeout(() => {
      this.ackTimer = undefined;
      void this.on(() => this.acks());
    }, this.ackDelay);
  }
}
