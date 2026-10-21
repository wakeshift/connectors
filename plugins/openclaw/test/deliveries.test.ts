import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ACKED_TTL, Conversation, Ledger, MAX_AGE, type ConversationOptions, type Host } from "../src/deliveries.js";
import type { Delivery } from "../src/transport.js";

const d = (id: string): Delivery => ({ delivery_id: id, attempt: 1, event: { id: "e-" + id, subject: "message " + id, from: { kind: "app", id: "try" } } });
const idsIn = (text: string) => [...text.matchAll(/&quot;id&quot;:&quot;e-([^&]+)&quot;|"id":"e-([^"]+)"/g)].map((m) => m[1] ?? m[2]);
const until = async (cond: () => boolean, ms = 5000) => {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error("timed out");
    await new Promise((r) => setTimeout(r, 2));
  }
};

// OpenClaw as a delivery sees it: a queue of system events per process
// (lost when the process dies), and heartbeat turns that take what's queued.
class FakeOpenClaw {
  queue: string[] = [];
  enqueues = new Map<string, number>(); // delivery ID -> times queued, this process
  taken = new Map<string, number>(); // delivery ID -> turns that took it, ever
  enqueue: () => boolean = () => true;
  beat: () => { status: string } = () => ({ status: "ran" });
  logs: string[] = [];
  host(): Host {
    return {
      enqueueSystemEvent: (text) => {
        if (!this.enqueue()) return false;
        for (const id of idsIn(text)) this.enqueues.set(id, (this.enqueues.get(id) ?? 0) + 1);
        this.queue.push(text);
        return true;
      },
      runHeartbeatOnce: async () => {
        const r = this.beat();
        if (r.status === "ran") {
          for (const t of this.queue.splice(0)) for (const id of idsIn(t)) this.taken.set(id, (this.taken.get(id) ?? 0) + 1);
        }
        return r;
      },
      run: (fn) => fn(),
      log: { info: (m) => this.logs.push(m), warn: (m) => this.logs.push(m) },
    };
  }
  restart() {
    this.queue = []; // the process died: its queue with it
    this.enqueues.clear();
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
    boot: "boot-1",
    wakeRetry: 1,
    ackRetry: 1,
    redeliverAfter: 1,
    ...extra,
  });
}

test("a delivery is recorded (0600), queued, taken by a turn, then acked", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  const acks: string[][] = [];
  const c = conv(dir, oc, async (ids) => (acks.push(ids), 200));
  assert.equal(await c.receive([d("a")]), 1);
  await c.settled();
  assert.deepEqual(acks, [["a"]]);
  assert.equal(new Ledger(dir).get(C, "a")!.state, "acked");
  const [sub] = readdirSync(dir); // one directory per conversation
  assert.equal(statSync(join(dir, sub)).mode & 0o777, 0o700);
  for (const f of readdirSync(join(dir, sub))) assert.equal(statSync(join(dir, sub, f)).mode & 0o777, 0o600);
  assert.equal(statSync(dir).mode & 0o777, 0o700);
  // A redelivery of what's acked does nothing.
  assert.equal(await c.receive([d("a")]), 0);
  await c.settled();
  assert.equal(acks.length, 1);
  assert.equal(oc.enqueues.get("a"), 1);
  c.stop();
});

test("acks go in chunks of at most 100; a 400 splits the chunk to find the bad ID, and the rest are acked", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  oc.beat = () => ({ status: "skipped" }); // queue all 250 before one turn takes them
  const sizes: number[] = [];
  const c = conv(dir, oc, async (ids) => (sizes.push(ids.length), ids.includes("bad") ? 400 : 200), { wakeRetry: 20 });
  const ds = Array.from({ length: 249 }, (_, i) => d("m" + i));
  ds.splice(130, 0, d("bad"));
  await c.receive(ds);
  oc.beat = () => ({ status: "ran" });
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

test("a turn that can't run is tried again within its window, then Wakeshift is asked to hand it out again", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  let beats = 0;
  oc.beat = () => (beats++, { status: "skipped" });
  let redelivered = 0;
  const acks: string[][] = [];
  const c = conv(dir, oc, async (ids) => (acks.push(ids), 200), { wakeRetry: 5, wakeFor: 60, redeliver: () => redelivered++ });
  await c.receive([d("a")]);
  await c.settled();
  await until(() => redelivered === 1);
  assert.ok(beats >= 3, `tried ${beats} times`);
  assert.equal(acks.length, 0);
  // The redelivery opens a new window: the turn is tried again, not the queue.
  oc.beat = () => ({ status: "ran" });
  await c.receive([d("a")]);
  await c.settled();
  await c.settled();
  assert.deepEqual(acks, [["a"]]);
  assert.equal(oc.enqueues.get("a"), 1);
  c.stop();
});

test("a message OpenClaw won't queue isn't run or acked, and is tried again", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  let n = 0;
  oc.enqueue = () => ++n > 2;
  let beats = 0;
  const beat = oc.beat;
  oc.beat = () => (beats++, beat());
  const acks: string[][] = [];
  const c = conv(dir, oc, async (ids) => (acks.push(ids), 200), { wakeRetry: 5 });
  await c.receive([d("a")]);
  await c.settled();
  assert.equal(beats, 0);
  assert.equal(new Ledger(dir).get(C, "a")!.state, "taken");
  await until(() => acks.length === 1);
  assert.equal(oc.enqueues.get("a"), 1);
  c.stop();
});

test("after a restart: what was queued in the dead process is queued again; what a turn took is only acked", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  oc.beat = () => ({ status: "skipped" });
  const c1 = conv(dir, oc, async () => 0, { wakeRetry: 1000 });
  await c1.receive([d("queued")]);
  await c1.settled();
  c1.stop();
  const l = new Ledger(dir);
  l.put({ ...l.get(C, "queued")!, id: "took", delivery: d("took"), state: "turn-started" });
  oc.restart();
  oc.beat = () => ({ status: "ran" });
  const acks: string[][] = [];
  const c2 = conv(dir, oc, async (ids) => (acks.push(ids), 200), { boot: "boot-2" });
  c2.start();
  await until(() => l.all().every((r) => r.state === "acked"));
  assert.equal(oc.enqueues.get("queued"), 1);
  assert.equal(oc.enqueues.get("took"), undefined, "a turn took it: not queued again");
  assert.deepEqual(acks.flat().sort(), ["queued", "took"]);
  c2.stop();
});

test("records aren't evicted while not acked; acked ones are pruned after a TTL, anything after MAX_AGE", () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const l = new Ledger(dir);
  const now = Date.now();
  const base = { conversation: "c", delivery: d("x") };
  for (let i = 0; i < 1500; i++) l.put({ ...base, id: "p" + i, state: "enqueued", takenAt: now - 3600_000 });
  l.put({ ...base, id: "acked-old", state: "acked", takenAt: now - ACKED_TTL - 10, doneAt: now - ACKED_TTL - 5 });
  l.put({ ...base, id: "acked-new", state: "acked", takenAt: now - 10, doneAt: now - 5 });
  l.put({ ...base, id: "ancient", state: "turn-started", takenAt: now - MAX_AGE - 10 });
  l.prune(now);
  assert.equal(l.all().filter((r) => r.state === "enqueued").length, 1500);
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

test("a new Conversation for the same conversation waits for the old one's host call: nothing is queued twice", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wsd-"));
  const oc = new FakeOpenClaw();
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  const host = oc.host();
  const slow: Host = { ...host, run: async (fn) => (await gate, fn()) };
  const c1 = conv(dir, oc, async () => 200, { host: slow });
  void c1.receive([d("a")]);
  await new Promise((r) => setTimeout(r, 20)); // c1 is inside its host call
  c1.stop();
  const c2 = conv(dir, oc, async () => 200, { after: c1.settled() });
  c2.start();
  await new Promise((r) => setTimeout(r, 20));
  release();
  await until(() => new Ledger(dir).get(C, "a")?.state === "acked");
  assert.equal(oc.enqueues.get("a"), 1);
  c2.stop();
});

// Random interleavings of new deliveries, redeliveries, refused queues,
// turns that can't run, failed acks and restarts: each delivery is taken
// by exactly one turn, queued at most once per process (so never twice in
// a live queue), and acked (2xx) exactly once, only after its turn.
test("property: random interleavings keep every invariant", async () => {
  for (let seed = 1; seed <= 8; seed++) {
    let s = seed;
    const rand = () => ((s = (s * 1103515245 + 12345) & 0x7fffffff), s / 0x7fffffff);
    const dir = mkdtempSync(join(tmpdir(), "wsd-prop-"));
    const oc = new FakeOpenClaw();
    let flaky = true;
    oc.enqueue = () => !flaky || rand() > 0.2;
    oc.beat = () => {
      if (flaky && rand() < 0.2) throw new Error("gateway busy");
      return !flaky || rand() < 0.6 ? { status: "ran" } : { status: "skipped" };
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
    let boot = 1;
    const make = () => conv(dir, oc, ack, { boot: "boot-" + boot, wakeFor: 20, redeliver: () => void redeliver() });
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
        boot++;
        c = make();
        c.start();
      } else {
        await new Promise((r) => setTimeout(r, Math.floor(rand() * 4)));
      }
      for (const [id, n] of oc.enqueues) if (n > 1) violations.push(`${id} queued ${n} times in one process`);
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
      if (oc.taken.get(id) !== 1) violations.push(`${id} taken by ${oc.taken.get(id) ?? 0} turns`);
      if (acked.get(id) !== 1) violations.push(`${id} acked ${acked.get(id)} times`);
    }
    assert.deepEqual(violations, [], `seed ${seed}`);
  }
});
