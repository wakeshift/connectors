import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ACKED_TTL, Conversation, idempotencyKey, Ledger, MAX_AGE, type ConversationOptions, type Host } from "../src/deliveries.js";
import type { Delivery } from "../src/transport.js";
import type { WakeRequest, WakeResult } from "../src/wake.js";

const d = (id: string): Delivery => ({ delivery_id: id, attempt: 1, event: { id: "e-" + id, subject: "message " + id, from: { kind: "app", id: "try" } } });
const idsIn = (text: string) => [...text.matchAll(/&quot;id&quot;:&quot;e-([^&]+)&quot;|"id":"e-([^"]+)"/g)].map((m) => m[1] ?? m[2]);
const until = async (cond: () => boolean, ms = 5000) => {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error("timed out");
    await new Promise((r) => setTimeout(r, 2));
  }
};

// OpenClaw's /hooks/agent as a delivery sees it: a request runs a turn
// (taking the messages in it) or doesn't, and a request with a key it has
// admitted before is replayed, not run again, while the gateway process
// that admitted it lives (its replay cache is in memory).
class FakeOpenClaw {
  /** What the next request does: run (and say so), run but lose the answer, or not run. */
  next: () => "ran" | "lost" | WakeResult = () => "ran";
  taken = new Map<string, number>(); // delivery ID -> turns that took it, ever
  replay = new Map<string, true>(); // idempotency keys admitted, this process
  requests: WakeRequest[] = [];
  inFlight = new Map<string, number>(); // delivery ID -> requests in flight now
  overlaps: string[] = [];
  mayRepeat = new Set<string>(); // ran with its answer lost, then the process died
  lost = new Set<string>(); // keys that ran with their answer lost
  logs: string[] = [];
  delay = 0;
  host(): Host {
    return {
      wake: async (r) => {
        this.requests.push(r);
        const ids = idsIn(r.message);
        for (const id of ids) {
          const n = (this.inFlight.get(id) ?? 0) + 1;
          if (n > 1) this.overlaps.push(id);
          this.inFlight.set(id, n);
        }
        try {
          if (this.delay) await new Promise((res) => setTimeout(res, this.delay));
          if (this.replay.has(r.idempotencyKey)) return { kind: "ran" };
          const x = this.next();
          if (x === "ran" || x === "lost") {
            for (const id of ids) this.taken.set(id, (this.taken.get(id) ?? 0) + 1);
            this.replay.set(r.idempotencyKey, true);
            if (x === "lost") {
              this.lost.add(r.idempotencyKey);
              return { kind: "retry", fresh: false, why: "the answer was lost" };
            }
            return { kind: "ran" };
          }
          return x;
        } finally {
          for (const id of ids) this.inFlight.set(id, (this.inFlight.get(id) ?? 1) - 1);
        }
      },
      log: { info: (m) => this.logs.push(m), warn: (m) => this.logs.push(m) },
    };
  }
  /** The gateway process died: its replay cache with it. */
  restart() {
    for (const r of this.requests) if (this.lost.has(r.idempotencyKey)) for (const id of idsIn(r.message)) this.mayRepeat.add(id);
    this.replay.clear();
    this.lost.clear();
  }
}

const C = "openclaw/x/agent:main:main";

function conv(dir: string, oc: FakeOpenClaw, ack: (ids: string[]) => Promise<number>, extra: Partial<ConversationOptions> = {}) {
  return new Conversation({
    ledger: new Ledger(dir),
    conversation: C,
    sessionKey: "agent:main:main",
    agentId: "main",
    host: oc.host(),
    ack,
    redeliver: () => {},
    wakeRetry: 1,
    ackRetry: 1,
    redeliverAfter: 1,
    ...extra,
  });
}

test("a delivery is recorded (0600), handed to one wake request in the conversation's session, then acked", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  const acks: string[][] = [];
  const c = conv(dir, oc, async (ids) => (acks.push(ids), 200));
  assert.equal(await c.receive([d("a")]), 1);
  await c.settled();
  assert.deepEqual(acks, [["a"]]);
  assert.equal(new Ledger(dir).get(C, "a")!.state, "acked");
  assert.equal(oc.requests.length, 1);
  assert.equal(oc.requests[0].sessionKey, "agent:main:main");
  assert.equal(oc.requests[0].agentId, "main");
  assert.match(oc.requests[0].message, /wakeshift_message/);
  assert.equal(oc.requests[0].idempotencyKey, idempotencyKey(C, ["a"], 0));
  const [sub] = readdirSync(dir); // one directory per conversation
  assert.equal(statSync(join(dir, sub)).mode & 0o777, 0o700);
  for (const f of readdirSync(join(dir, sub))) assert.equal(statSync(join(dir, sub, f)).mode & 0o777, 0o600);
  assert.equal(statSync(dir).mode & 0o777, 0o700);
  // A redelivery of what's acked does nothing.
  assert.equal(await c.receive([d("a")]), 0);
  await c.settled();
  assert.equal(acks.length, 1);
  assert.equal(oc.requests.length, 1);
  c.stop();
});

test("acks go in chunks of at most 100; a 400 splits the chunk to find the bad ID, and the rest are acked", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  oc.next = () => ({ kind: "retry", fresh: false, why: "busy" }); // all 250 arrive before one turn takes them
  const sizes: number[] = [];
  const c = conv(dir, oc, async (ids) => (sizes.push(ids.length), ids.includes("bad") ? 400 : 200), { wakeRetry: 20 });
  const ds = Array.from({ length: 249 }, (_, i) => d("m" + i));
  ds.splice(130, 0, d("bad"));
  await c.receive(ds);
  oc.next = () => "ran";
  const l = new Ledger(dir);
  await until(() => l.all().every((r) => r.state === "acked" || r.state === "refused"));
  assert.ok(sizes.every((n) => n <= 100), String(sizes));
  assert.equal(l.get(C, "bad")!.state, "refused");
  assert.equal(l.all().filter((r) => r.state === "acked").length, 249);
  assert.ok(oc.logs.some((m) => m.includes("refused the ack for bad")));
  c.stop();
});

test("only a 2xx acks; a failed ack is retried on its own timer, with backoff", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  const answers = [0, 500, 503, 200];
  const at: number[] = [];
  const c = conv(dir, oc, async () => (at.push(Date.now()), answers.shift() ?? 200), { ackRetry: 10 });
  await c.receive([d("a")]);
  await c.settled();
  assert.equal(new Ledger(dir).get(C, "a")!.state, "turn-started", "not acked on a failure");
  await until(() => new Ledger(dir).get(C, "a")!.state === "acked");
  assert.equal(at.length, 4);
  assert.ok(at[3] - at[2] >= at[2] - at[1] - 2, "backs off");
  c.stop();
});

test("a wake that doesn't run is tried again within its window, then Wakeshift is asked to hand it out again", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  oc.next = () => ({ kind: "retry", fresh: false, why: "OpenClaw answered 503" });
  let redelivered = 0;
  const acks: string[][] = [];
  const c = conv(dir, oc, async (ids) => (acks.push(ids), 200), { wakeRetry: 5, wakeFor: 200, redeliver: () => redelivered++ });
  await c.receive([d("a")]);
  await c.settled();
  await until(() => redelivered === 1);
  assert.ok(oc.requests.length >= 3, `tried ${oc.requests.length} times`);
  assert.equal(acks.length, 0);
  // Not known to have run: every try was the same request, the same key.
  assert.equal(new Set(oc.requests.map((r) => r.idempotencyKey)).size, 1);
  // The redelivery opens a new window.
  oc.next = () => "ran";
  await c.receive([d("a")]);
  await c.settled();
  await c.settled();
  assert.deepEqual(acks, [["a"]]);
  assert.equal(oc.taken.get("a"), 1);
  c.stop();
});

test("a run whose answer was lost is replayed, not run again: the same key", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  let first = true;
  oc.next = () => (first ? ((first = false), "lost") : "ran");
  const acks: string[][] = [];
  const c = conv(dir, oc, async (ids) => (acks.push(ids), 200), { wakeRetry: 5 });
  await c.receive([d("a")]);
  await until(() => acks.length === 1);
  assert.equal(oc.taken.get("a"), 1, "OpenClaw replayed the admitted run");
  assert.equal(new Set(oc.requests.map((r) => r.idempotencyKey)).size, 1);
  c.stop();
});

test("a delivery that arrives after a request whose answer was lost waits for that request again: the first run isn't repeated", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  let first = true;
  oc.next = () => (first ? ((first = false), "lost") : "ran");
  const acks: string[][] = [];
  const c = conv(dir, oc, async (ids) => (acks.push(ids), 200), { wakeRetry: 30 });
  await c.receive([d("a")]);
  await c.settled();
  assert.equal(new Ledger(dir).get(C, "a")!.unsure, oc.requests[0].idempotencyKey);
  await c.receive([d("b")]); // before the retry
  await until(() => acks.flat().length === 2);
  assert.deepEqual(oc.requests.map((r) => idsIn(r.message)), [["a"], ["a"], ["b"]]);
  assert.equal(oc.requests[1].idempotencyKey, oc.requests[0].idempotencyKey);
  assert.equal(oc.taken.get("a"), 1);
  assert.equal(oc.taken.get("b"), 1);
  c.stop();
});

test("a run that ended without ok, or a refused request, is tried again with a new key", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  const answers: WakeResult[] = [
    { kind: "retry", fresh: true, why: "the woken turn ended error" },
    { kind: "broken", why: "OpenClaw refused the hooks token (401)" },
  ];
  oc.next = () => answers.shift() ?? "ran";
  const problems: string[] = [];
  const acks: string[][] = [];
  const c = conv(dir, oc, async (ids) => (acks.push(ids), 200), { wakeRetry: 5, problem: (w) => problems.push(w) });
  await c.receive([d("a")]);
  await until(() => acks.length === 1);
  const keys = oc.requests.map((r) => r.idempotencyKey);
  assert.equal(keys.length, 3);
  assert.equal(new Set(keys).size, 3, "each known failure gets a new key");
  assert.deepEqual(keys, [0, 1, 2].map((n) => idempotencyKey(C, ["a"], n)));
  // The broken hooks were said, and cleared once a wake ran.
  assert.deepEqual(problems, ["OpenClaw refused the hooks token (401)", ""]);
  c.stop();
});

test("the reply goes to the conversation's route; a refused route is dropped and the wake goes on without it", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  let route: { channel: string; to: string } | undefined = { channel: "telegram", to: "42" };
  oc.next = () => (oc.requests.length === 1 ? { kind: "route", why: "unknown channel" } : "ran");
  const acks: string[][] = [];
  const c = conv(dir, oc, async (ids) => (acks.push(ids), 200), { route: () => route, routeRefused: () => (route = undefined) });
  await c.receive([d("a")]);
  await until(() => acks.length === 1);
  assert.deepEqual(oc.requests[0].route, { channel: "telegram", to: "42" });
  assert.equal(oc.requests[1].route, undefined);
  assert.equal(route, undefined);
  c.stop();
});

test("after a restart: what no turn finished is woken again; what a turn finished is only acked", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  oc.next = () => ({ kind: "retry", fresh: false, why: "busy" });
  const c1 = conv(dir, oc, async () => 0, { wakeRetry: 1000 });
  await c1.receive([d("waiting")]);
  await c1.settled();
  c1.stop();
  const l = new Ledger(dir);
  l.put({ ...l.get(C, "waiting")!, id: "took", delivery: d("took"), state: "turn-started" });
  // A record from the earlier heartbeat wake, queued in a process now gone.
  l.put({ ...l.get(C, "waiting")!, id: "old", delivery: d("old"), state: "enqueued", boot: "gone" });
  oc.restart();
  oc.next = () => "ran";
  const acks: string[][] = [];
  const c2 = conv(dir, oc, async (ids) => (acks.push(ids), 200));
  c2.start();
  await until(() => l.all().every((r) => r.state === "acked"));
  assert.equal(oc.taken.get("waiting"), 1);
  assert.equal(oc.taken.get("old"), 1);
  assert.equal(oc.taken.get("took"), undefined, "a turn finished it: not woken again");
  assert.deepEqual(acks.flat().sort(), ["old", "took", "waiting"]);
  c2.stop();
});

test("records aren't evicted while not acked; acked ones are pruned after a TTL, anything after MAX_AGE", () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const l = new Ledger(dir);
  const now = Date.now();
  const base = { conversation: "c", delivery: d("x") };
  for (let i = 0; i < 1500; i++) l.put({ ...base, id: "p" + i, state: "taken", takenAt: now - 3600_000 });
  l.put({ ...base, id: "acked-old", state: "acked", takenAt: now - ACKED_TTL - 10, doneAt: now - ACKED_TTL - 5 });
  l.put({ ...base, id: "acked-new", state: "acked", takenAt: now - 10, doneAt: now - 5 });
  l.put({ ...base, id: "ancient", state: "turn-started", takenAt: now - MAX_AGE - 10 });
  l.prune(now);
  assert.equal(l.all().filter((r) => r.state === "taken").length, 1500);
  assert.ok(l.get("c", "acked-new"));
  assert.equal(l.get("c", "acked-old"), undefined);
  assert.equal(l.get("c", "ancient"), undefined);
});

test("records live in their conversation's directory; old flat records are moved there; empty directories go", () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const l = new Ledger(dir);
  const now = Date.now();
  l.put({ id: "a", conversation: "c1", state: "taken", delivery: d("a"), takenAt: now });
  l.put({ id: "b", conversation: "c2", state: "acked", delivery: d("b"), takenAt: now - ACKED_TTL - 10, doneAt: now - ACKED_TTL - 5 });
  assert.deepEqual(l.of("c1").map((r) => r.id), ["a"]);
  assert.equal(l.get("c2", "a"), undefined, "another conversation's record isn't found");
  // A record written by an older version, in the flat layout.
  writeFileSync(join(dir, "0123.json"), JSON.stringify({ id: "old", conversation: "c1", state: "enqueued", delivery: d("old"), takenAt: now }));
  l.prune(now);
  assert.deepEqual(l.of("c1").map((r) => r.id).sort(), ["a", "old"]);
  assert.equal(l.get("c2", "b"), undefined);
  assert.equal(readdirSync(dir).length, 1, "c2's empty directory is gone; no flat files left");
  assert.equal(readdirSync(join(dir, readdirSync(dir)[0])).filter((n) => n.endsWith(".tmp")).length, 0);
});

test("a new Conversation for the same conversation waits for the old one's wake request: nothing is woken twice", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  const host = oc.host();
  const slow: Host = { ...host, wake: async (r) => (await gate, host.wake(r)) };
  const c1 = conv(dir, oc, async () => 200, { host: slow });
  void c1.receive([d("a")]);
  await new Promise((r) => setTimeout(r, 20)); // c1 is inside its wake request
  c1.stop();
  const c2 = conv(dir, oc, async () => 200, { after: c1.settled() });
  c2.start();
  await new Promise((r) => setTimeout(r, 20));
  release();
  await until(() => new Ledger(dir).get(C, "a")?.state === "acked");
  assert.equal(oc.taken.get("a"), 1);
  assert.deepEqual(oc.overlaps, []);
  c2.stop();
});

// Random interleavings of new deliveries, redeliveries, wakes that don't
// run, runs that fail, runs whose answer is lost, failed acks and restarts:
// a delivery is never in two wake requests at once, is taken by a turn
// before it's acknowledged, is taken by exactly one turn (more only through
// the one gap: its answer lost, then the process died), and is acked (2xx)
// exactly once.
test("property: random interleavings keep every invariant", async () => {
  for (let seed = 1; seed <= 8; seed++) {
    let s = seed;
    const rand = () => ((s = (s * 1103515245 + 12345) & 0x7fffffff), s / 0x7fffffff);
    const dir = mkdtempSync(join(tmpdir(), "wsd-prop-"));
    const oc = new FakeOpenClaw();
    let flaky = true;
    oc.next = () => {
      if (!flaky) return "ran";
      const x = rand();
      if (x < 0.15) throw new Error("gateway busy");
      if (x < 0.3) return { kind: "retry", fresh: false, why: "503" };
      if (x < 0.4) return { kind: "retry", fresh: true, why: "the woken turn ended error" };
      if (x < 0.45) return { kind: "broken", why: "404" };
      if (x < 0.55) return "lost";
      return "ran";
    };
    const acked = new Map<string, number>();
    const violations: string[] = [];
    const ack = async (ids: string[]) => {
      if (ids.length > 100) violations.push("chunk over 100");
      const x = rand();
      if (flaky && x < 0.25) return 0;
      if (flaky && x < 0.35) return 500;
      for (const id of ids) {
        if (!oc.taken.get(id)) violations.push(`${id} acked before a turn took it`);
        acked.set(id, (acked.get(id) ?? 0) + 1);
      }
      return 200;
    };
    const all: string[] = [];
    const make = () => conv(dir, oc, ack, { wakeFor: 20, redeliver: () => void redeliver() });
    let c = make();
    const redeliver = () => c.receive(all.filter((id) => !acked.get(id)).map(d));
    for (let step = 0; step < 120; step++) {
      const x = rand();
      if (x < 0.4) {
        const id = `s${seed}-${all.length}`;
        all.push(id);
        void c.receive([d(id)]);
      } else if (x < 0.7 && all.length > 0) {
        void c.receive([d(all[Math.floor(rand() * all.length)])]);
      } else if (x < 0.78) {
        await c.settled();
        c.stop();
        oc.restart();
        c = make();
        c.start();
      } else {
        await new Promise((r) => setTimeout(r, Math.floor(rand() * 4)));
      }
    }
    // Then everything settles: no more failures, and what isn't acked is
    // handed out again until it is.
    flaky = false;
    await until(() => {
      void redeliver();
      return all.every((id) => acked.get(id));
    }, 10000);
    await c.settled();
    c.stop();
    for (const id of all) {
      const n = oc.taken.get(id) ?? 0;
      if (n < 1 || (n > 1 && !oc.mayRepeat.has(id))) violations.push(`${id} taken by ${n} turns`);
      if (acked.get(id) !== 1) violations.push(`${id} acked ${acked.get(id)} times`);
    }
    for (const id of new Set(oc.overlaps)) violations.push(`${id} in two wake requests at once`);
    assert.deepEqual(violations, [], `seed ${seed}`);
  }
});
