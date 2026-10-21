// Delivery state: one durable record per delivery, and the one place that
// decides what happens to it.
//
// A record lives at <state dir>/deliveries/<sha256(conversation)>/<sha256(delivery ID)>.json
// (0600 in 0700 directories) and moves through
//
//   taken → enqueued → turn-started → acked
//
// (or `refused`: Wakeshift answered 400 for that ID alone, so it's not
// sent again).
//
//   taken         recorded on arrival, with the delivery; nothing done yet.
//   enqueued      queued as a system event in the session, in this gateway
//                 process (`boot`). OpenClaw's queue is in memory, so after
//                 a restart (another boot) a record that's enqueued but has
//                 no turn yet is queued again: the queue that held it is gone.
//   turn-started  a heartbeat turn ran in the session after it was queued:
//                 the turn took it. Only now may it be acknowledged.
//   acked         Wakeshift answered 2xx to its ack.
//
// Invariants (tested in test/deliveries.test.ts, including random
// interleavings of redelivery, failures and restarts):
//
//   1. Each delivery is in at most one live queue at a time: it's queued
//      only from `taken`, or from `enqueued` in an earlier boot. A
//      redelivery never queues it again; it only tries the turn again.
//      So every turn that takes it is the only one that does.
//   2. It's acknowledged only from `turn-started`, and only a 2xx moves it
//      to `acked`; nothing is acknowledged after that.
//   3. A record isn't evicted while it isn't acked or refused (no capped
//      sets): records are pruned only once acked or refused for ACKED_TTL,
//      or after MAX_AGE whatever their state.
//   4. Every decision for a delivery reads and writes only its record, on
//      the conversation's one chain (the gateway is one process, and owns
//      its state dir), so no two decisions for it interleave.
//
// The one gap: a process that dies between a turn running and its records
// becoming turn-started queues them again when it starts, so that turn and
// the next both see them (at least once, never lost).
//
// Turns: a turn is started with runHeartbeatOnce. One that can't run
// (skipped: busy, rate limits; failed; thrown) is tried again with backoff
// for `wakeFor` from when it was queued (or redelivered); past that, the
// transport reconnects so Wakeshift hands it out again, which opens a new
// window. Acks: sent in chunks of at most 100, retried with backoff on
// their own timer; a 400 splits the chunk to find the bad ID (logged and
// marked refused), and the rest go on.

import { createHash, randomBytes } from "node:crypto";
import { chmodSync, mkdirSync, readdirSync, readFileSync, rmdirSync, rmSync } from "node:fs";
import { join } from "node:path";
import { prompt, WAKE_PROMPT } from "./framing.js";
import { writeAtomic } from "./store.js";
import type { Delivery } from "./transport.js";

export type State = "taken" | "enqueued" | "turn-started" | "acked" | "refused";

export interface Record {
  id: string;
  conversation: string;
  state: State;
  delivery: Delivery;
  takenAt: number;
  boot?: string; // the process it was queued in
  windowFrom?: number; // when its current turn window opened
  turnAt?: number;
  doneAt?: number; // acked or refused
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
        const done = (r.state === "acked" || r.state === "refused") && now - (r.doneAt ?? r.takenAt) > ACKED_TTL;
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
  enqueueSystemEvent(text: string, opts: { sessionKey: string; agentId?: string }): boolean;
  runHeartbeatOnce(opts: { reason: string; agentId?: string; sessionKey: string; heartbeat?: { target: string; prompt?: string } }): Promise<{ status: string; reason?: string }>;
  /** Runs work inside the gateway's admission (the plugin service's scheduler). */
  run<T>(fn: () => Promise<T>): Promise<T>;
  log: { info(m: string): void; warn(m: string): void };
}

export interface ConversationOptions {
  ledger: Ledger;
  conversation: string;
  sessionKey: string;
  agentId?: string;
  host: Host;
  /** Sends one ack chunk (≤100 IDs): the HTTP status, 0 if unreachable. */
  ack: (ids: string[]) => Promise<number>;
  /** Makes Wakeshift hand out again what isn't acknowledged. */
  redeliver: () => void;
  /** This gateway process (default: random per process). */
  boot?: string;
  target?: string;
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

/**
 * This gateway process's boot ID. Kept on the process, not the module:
 * OpenClaw reloads a plugin in the same process (a config change), and
 * its queue, which this stands for, survives that.
 */
const g = globalThis as { __wakeshiftOpenClawBoot?: string };
export const BOOT = (g.__wakeshiftOpenClawBoot ??= randomBytes(8).toString("hex"));

/** One conversation's deliveries: every decision about them is made here, on one chain. */
export class Conversation {
  private chain: Promise<void> = Promise.resolve();
  private stopped = false;
  private turnTimer?: ReturnType<typeof setTimeout>;
  private ackTimer?: ReturnType<typeof setTimeout>;
  private redeliverTimer?: ReturnType<typeof setTimeout>;
  private turnDelay = 0;
  private ackDelay = 0;
  private readonly boot: string;

  constructor(private readonly o: ConversationOptions) {
    this.boot = o.boot ?? BOOT;
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

  /** Picks up what an earlier process left (queues, turns, acks). */
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
   * many were new. A redelivery of one queued in this process opens a new
   * turn window; one already acked or refused is left alone.
   */
  receive(ds: Delivery[]): Promise<number> {
    return this.on(async () => {
      let fresh = 0;
      const now = this.now();
      for (const d of ds) {
        const r = this.o.ledger.get(this.o.conversation, d.delivery_id);
        if (!r) {
          this.o.ledger.put({ id: d.delivery_id, conversation: this.o.conversation, state: "taken", delivery: d, takenAt: now });
          fresh++;
        } else if (r.state === "taken" || (r.state === "enqueued" && r.boot === this.boot)) {
          this.o.ledger.put({ ...r, windowFrom: now });
          this.turnDelay = 0;
        }
      }
      void this.on(() => this.step()); // recorded: the rest happens after, on the chain
      return fresh;
    });
  }

  // Everything that can happen now, from the records alone.
  private async step() {
    const rs = this.o.ledger.of(this.o.conversation);
    const toQueue = rs.filter((r) => r.state === "taken" || (r.state === "enqueued" && r.boot !== this.boot));
    // After stop() nothing new starts (a step already past a host call
    // still records what that call did).
    if (toQueue.length > 0 && !this.stopped) await this.enqueue(toQueue);
    if (!this.stopped && this.o.ledger.of(this.o.conversation).some((r) => r.state === "enqueued" && r.boot === this.boot)) await this.turn();
    if (!this.stopped && this.o.ledger.of(this.o.conversation).some((r) => r.state === "turn-started")) await this.acks();
  }

  private opts() {
    return { sessionKey: this.o.sessionKey, ...(this.o.agentId ? { agentId: this.o.agentId } : {}) };
  }

  private async enqueue(rs: Record[]) {
    const { host } = this.o;
    let queued = false;
    try {
      queued = await host.run(async () => host.enqueueSystemEvent(prompt(rs.map((r) => r.delivery)), this.opts()));
    } catch (e) {
      host.log.warn(`wakeshift (${this.o.sessionKey}): queueing failed: ${(e as Error).message}`);
    }
    if (!queued) {
      // Still taken: tried again with the turn's backoff, within its window.
      host.log.warn(`wakeshift (${this.o.sessionKey}): OpenClaw didn't queue the message; trying again`);
      this.retryTurn(Math.min(...rs.map((r) => r.windowFrom ?? r.takenAt)));
      return;
    }
    const now = this.now();
    for (const r of rs) this.o.ledger.put({ ...r, state: "enqueued", boot: this.boot, windowFrom: now });
  }

  private async turn() {
    const { host } = this.o;
    const waiting = this.o.ledger.of(this.o.conversation).filter((r) => r.state === "enqueued" && r.boot === this.boot);
    let r: { status: string; reason?: string } | undefined;
    try {
      // The heartbeat's prompt is ours (OpenClaw spreads the requested
      // heartbeat over the configured one): its default tells the model to
      // leave earlier requests alone (framing.ts, WAKE_PROMPT).
      r = await host.run(() => host.runHeartbeatOnce({ reason: "hook:wakeshift", ...this.opts(), heartbeat: { target: this.o.target ?? "last", prompt: WAKE_PROMPT } }));
    } catch (e) {
      r = { status: "failed", reason: (e as Error).message };
    }
    if (r?.status === "ran") {
      // The turn took everything queued before it.
      const now = this.now();
      for (const w of waiting) this.o.ledger.put({ ...w, state: "turn-started", turnAt: now });
      this.turnDelay = 0;
      return;
    }
    host.log.info(`wakeshift (${this.o.sessionKey}): couldn't start a turn (${r?.status}: ${r?.reason ?? ""})`);
    this.retryTurn(Math.min(...waiting.map((w) => w.windowFrom ?? w.takenAt)));
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
      this.redeliverTimer = setTimeout(() => !this.stopped && this.o.redeliver(), this.o.redeliverAfter ?? 30000);
      return;
    }
    this.turnTimer = setTimeout(() => {
      this.turnTimer = undefined;
      void this.on(() => this.step());
    }, this.turnDelay);
  }

  // Acknowledges what a turn took, in chunks of 100.
  private async acks() {
    const ids = this.o.ledger
      .of(this.o.conversation)
      .filter((r) => r.state === "turn-started")
      .map((r) => r.id);
    let retry = false;
    for (let i = 0; i < ids.length && !this.stopped; i += 100) {
      const res = await this.ackChunk(ids.slice(i, i + 100));
      if (res === "gone") return; // the link no longer works: the transport ends it
      if (res === "retry") retry = true;
    }
    if (retry) this.retryAcks();
    else this.ackDelay = 0;
  }

  // One chunk: 2xx marks them acked; a 400 splits it to find the bad ID.
  private async ackChunk(ids: string[]): Promise<"ok" | "retry" | "gone"> {
    const status = await this.o.ack(ids);
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
      const a = await this.ackChunk(ids.slice(0, half));
      const b = await this.ackChunk(ids.slice(half));
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
