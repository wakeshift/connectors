import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { getEventListeners } from "node:events";
import { MAX_LINE, Transport, parseLink, sameOrigin, sleep, type Delivery } from "../src/transport.js";

// A stand-in for Wakeshift's stream endpoints: SSE, long poll and ack, with
// what each request asked for recorded.
interface Fake {
  url: string;
  link: (token?: string) => string;
  acks: string[][];
  sse: { token: string; lastEventId?: string }[];
  polls: { token: string; acks: string[] }[];
  /** Each request's X-Wakeshift-Connector. */
  clients: string[];
  onSSE?: (res: ServerResponse, n: number, req: IncomingMessage) => void;
  onPoll?: (res: ServerResponse, n: number) => void;
  close: () => Promise<void>;
}

async function fake(): Promise<Fake> {
  const f = { acks: [], sse: [], polls: [], clients: [] } as unknown as Fake;
  const srv = createServer((req, res) => {
    const u = new URL(req.url!, "http://x");
    f.clients.push(String(req.headers["x-wakeshift-connector"] ?? ""));
    const bearer = (req.headers.authorization ?? "").replace(/^Bearer /, "");
    if (u.pathname === "/v1/stream" && u.searchParams.get("format") === "sse") {
      // The token in the header (main's /v1/stream takes Bearer), never the URL.
      if (u.searchParams.has("link")) return void res.writeHead(400).end();
      f.sse.push({ token: bearer, lastEventId: req.headers["last-event-id"] as string | undefined });
      if (f.onSSE) return f.onSSE(res, f.sse.length, req);
      res.writeHead(404).end();
      return;
    }
    if (u.pathname === "/v1/stream/ack" && req.method === "POST") {
      let b = "";
      req.on("data", (c) => (b += c));
      req.on("end", () => {
        const ids = JSON.parse(b).delivery_ids as string[];
        f.acks.push(ids);
        res.writeHead(bearer ? 200 : 401, { "Content-Type": "application/json" }).end(JSON.stringify({ acked: ids }));
      });
      return;
    }
    if (u.pathname === "/v1/stream/poll") {
      f.polls.push({ token: bearer, acks: u.searchParams.getAll("ack") });
      if (f.onPoll) return f.onPoll(res, f.polls.length);
      res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ deliveries: [], acked: [] }));
      return;
    }
    res.writeHead(404).end();
  });
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
  const port = (srv.address() as AddressInfo).port;
  f.url = `http://127.0.0.1:${port}`;
  f.link = (token = "tok1") => `ws://127.0.0.1:${port}/v1/stream?link=${token}`;
  f.close = () =>
    new Promise((r) => {
      srv.closeAllConnections();
      srv.close(() => r());
    });
  return f;
}

const ev = (id: string, subject: string, attempt = 1) => ({
  type: "event",
  delivery_id: id,
  attempt,
  subscription_id: "s1",
  event: { id: "e-" + id, subject, from: { kind: "app", id: "try" } },
});
const sse = (res: ServerResponse, type: string, data: unknown, id?: string) =>
  res.write((id ? `id: ${id}\n` : "") + `event: ${type}\ndata: ${JSON.stringify(data)}\n\n`);
const open = (res: ServerResponse) => {
  res.writeHead(200, { "Content-Type": "text/event-stream" });
  res.write("retry: 3000\n\n");
};

const until = async (cond: () => boolean, ms = 3000) => {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error("timed out");
    await new Promise((r) => setTimeout(r, 10));
  }
};

// A receive that records IDs, as deliveries.ts does (new ones counted).
const recorder = () => {
  const seen: string[] = [];
  const receive = async (ds: Delivery[]) => {
    let n = 0;
    for (const d of ds) if (!seen.includes(d.delivery_id)) (seen.push(d.delivery_id), n++);
    return n;
  };
  return { seen, receive };
};

test("links: origin and token, same-origin only", () => {
  assert.deepEqual(parseLink("wss://app.example/v1/stream?link=abc"), { base: "https://app.example", token: "abc" });
  assert.equal(parseLink("ftp://x/v1/stream?link=a"), undefined);
  assert.equal(parseLink("wss://x/v1/stream"), undefined);
  assert.ok(sameOrigin("wss://a.example/v1/stream?link=1", "https://a.example/v1/stream?format=sse&link=2"));
  assert.ok(!sameOrigin("wss://a.example/v1/stream?link=1", "https://evil.example/v1/stream?link=2"));
});

test("a delivery that can't be recorded ends the connection there: nothing after it is read, the cursor stays before it", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res, n) => {
    open(res);
    sse(res, "event", ev("d0", "zero"), "d0");
    sse(res, "event", ev("d1", "one", n), "d1");
    sse(res, "event", ev("d2", "two", n), "d2");
  };
  const seen: string[] = [];
  let failed = false;
  const s = new Transport({
    link: f.link(),
    minBackoff: 10,
    receive: async (ds) => {
      const id = ds[0].delivery_id;
      if (id === "d1" && !failed) {
        failed = true;
        throw new Error("disk full");
      }
      seen.push(id);
      return 1;
    },
  }).start();
  t.after(() => s.stop());
  await until(() => seen.includes("d2"));
  assert.deepEqual(seen, ["d0", "d0", "d1", "d2"], "d2 isn't read on the connection where d1 failed");
  assert.equal(f.sse.length, 2);
  assert.equal(f.sse[1].lastEventId, "d0", "the cursor stayed before d1");
  assert.equal(s.transport, "sse", "a recording failure isn't SSE failing");
});

test("every request names the plugin and its version; a 426 update_required stops the stream, keeping the link", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res) => {
    open(res);
    sse(res, "event", ev("d1", "one"), "d1");
  };
  const r = recorder();
  const s = new Transport({ link: f.link(), minBackoff: 10, receive: r.receive }).start();
  t.after(() => s.stop());
  await until(() => r.seen.includes("d1"));
  assert.equal(await s.ack(["d1"]), 200);
  assert.ok(f.clients.length >= 2);
  for (const c of f.clients) assert.equal(c, "wakeshift-openclaw/dev openclaw");

  const g = await fake();
  t.after(g.close);
  g.onSSE = (res) => void res.writeHead(426, { "Content-Type": "application/json" }).end(JSON.stringify({ error: { code: "update_required", message: "out of date" } }));
  const old = new Transport({ link: g.link(), minBackoff: 10, receive: recorder().receive }).start();
  t.after(() => old.stop());
  assert.deepEqual(await old.done, { why: "outdated" });
  assert.equal(g.sse.length, 1, "not tried again");

  // A live stream the server ends once the minimum is raised.
  const h = await fake();
  t.after(h.close);
  h.onSSE = (res) => {
    open(res);
    sse(res, "hub.closed", { type: "hub.closed", reason: "update_required", note: "out of date" });
  };
  const live = new Transport({ link: h.link(), minBackoff: 10, receive: recorder().receive }).start();
  t.after(() => live.stop());
  assert.deepEqual(await live.done, { why: "outdated" });
});

test("SSE: hands deliveries over in order, with the token in a header", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res) => {
    open(res);
    sse(res, "event", ev("d1", "one"), "d1");
    sse(res, "event", ev("d2", "two"), "d2");
  };
  const r = recorder();
  const s = new Transport({ link: f.link(), receive: r.receive }).start();
  t.after(() => s.stop());
  await until(() => r.seen.length === 2);
  assert.deepEqual(r.seen, ["d1", "d2"]);
  assert.equal(f.sse[0].token, "tok1");
  assert.deepEqual(f.acks, [], "the transport acknowledges nothing by itself");
});

test("the cursor moves only past what's recorded", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res, n) => {
    open(res);
    if (n <= 2) sse(res, "event", ev("d1", "one", n), "d1");
    if (n <= 2) setTimeout(() => res.destroy(), 60);
  };
  let n = 0;
  const s = new Transport({
    link: f.link(),
    minBackoff: 10,
    receive: async () => {
      if (++n === 1) throw new Error("disk full");
      return 1;
    },
  }).start();
  t.after(() => s.stop());
  await until(() => f.sse.length === 3);
  assert.equal(f.sse[1].lastEventId, undefined, "not recorded: not past it");
  assert.equal(f.sse[2].lastEventId, "d1", "recorded: past it");
});

test("SSE: follows hub.rotate to the same origin only", async (t) => {
  const f = await fake();
  t.after(f.close);
  const links: string[] = [];
  f.onSSE = (res, n) => {
    open(res);
    if (n === 1) sse(res, "hub.rotate", { type: "hub.rotate", url: "https://evil.example/v1/stream?format=sse&link=bad" });
    else if (n === 2) sse(res, "hub.rotate", { type: "hub.rotate", url: f.url + "/v1/stream?format=sse&link=tok2" });
    else sse(res, "event", ev("d1", "one"), "d1");
  };
  const r = recorder();
  const s = new Transport({ link: f.link(), onLink: (l) => links.push(l), receive: r.receive }).start();
  t.after(() => s.stop());
  await until(() => r.seen.length === 1);
  assert.deepEqual(
    f.sse.map((x) => x.token),
    ["tok1", "tok1", "tok2"],
  );
  assert.deepEqual(links, [f.link("tok2")]);
  // Acks use the rotated link's token.
  assert.equal(await s.ack(["d1"]), 200);
});

test("hub.closed: replaced stops quietly; unsubscribed and revoked are gone; restarting reconnects", async (t) => {
  for (const [reason, want] of [
    ["replaced", "replaced"],
    ["unsubscribed", "gone"],
    ["revoked", "gone"],
  ] as const) {
    const f = await fake();
    f.onSSE = (res) => {
      open(res);
      sse(res, "closed", { type: "hub.closed", reason, note: "…" });
      res.end();
    };
    const s = new Transport({ link: f.link(), receive: async () => 1 }).start();
    assert.equal((await s.done).why, want, reason);
    assert.equal(f.sse.length, 1);
    await f.close();
  }
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res, n) => {
    open(res);
    if (n === 1) {
      sse(res, "closed", { type: "hub.closed", reason: "restarting" });
      res.end();
    } else sse(res, "event", ev("d1", "one"), "d1");
  };
  const r = recorder();
  const s = new Transport({ link: f.link(), receive: r.receive }).start();
  t.after(() => s.stop());
  await until(() => r.seen.length === 1);
  assert.equal(f.sse.length, 2);
});

test("a revoked link (401) stops for good", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res) => res.writeHead(401).end();
  const s = new Transport({ link: f.link(), receive: async () => 1 }).start();
  assert.deepEqual(await s.done, { why: "gone", reason: "link_revoked" });
});

test("at capacity (503): waits out Retry-After, not its backoff, and keeps to SSE", async (t) => {
  const f = await fake();
  t.after(f.close);
  const at: number[] = [];
  f.onSSE = (res, n) => {
    at.push(Date.now());
    if (n === 1) return void res.writeHead(503, { "Retry-After": "1" }).end();
    open(res);
  };
  const s = new Transport({ link: f.link(), minBackoff: 10, maxBackoff: 20, receive: async () => 1 }).start();
  t.after(() => s.stop());
  await until(() => at.length >= 2);
  assert.ok(at[1] - at[0] >= 950, `reconnected after ${at[1] - at[0]}ms`);
  assert.equal(s.transport, "sse");
});

test("falls back to long polling when SSE doesn't get through; a poll of nothing new backs off", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res) => res.writeHead(502).end(); // a proxy that won't pass it
  // Every poll hands out the same unacknowledged delivery at once.
  f.onPoll = (res) => res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ deliveries: [ev("p1", "polled")], acked: [] }));
  const r = recorder();
  const s = new Transport({ link: f.link(), minBackoff: 50, sseTries: 2, pollWait: 300, receive: r.receive }).start();
  t.after(() => s.stop());
  await until(() => f.polls.length >= 2);
  const at = f.polls.length;
  await new Promise((res) => setTimeout(res, 120));
  assert.ok(f.polls.length - at <= 2, `polled ${f.polls.length - at} times in 120ms`);
  assert.equal(s.transport, "poll");
  assert.deepEqual(r.seen, ["p1"]);
  assert.equal(f.polls[0].token, "tok1");
});

test("Wakeshift unreachable (restarting) isn't SSE failing: it keeps to SSE", async (t) => {
  const f = await fake();
  const link = f.link();
  await f.close();
  const logs: string[] = [];
  const s = new Transport({ link, minBackoff: 10, maxBackoff: 20, sseTries: 2, receive: async () => 1, log: (m) => logs.push(m) }).start();
  t.after(() => s.stop());
  await until(() => logs.length >= 4);
  assert.equal(s.transport, "sse");
});

test("silent SSE (a buffering proxy) counts as not getting through", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res) => res.writeHead(200, { "Content-Type": "text/event-stream" });
  const s = new Transport({ link: f.link(), minBackoff: 10, sseTries: 2, silentFor: 100, pollWait: 300, receive: async () => 1 }).start();
  t.after(() => s.stop());
  await until(() => f.polls.length >= 1);
  assert.equal(s.transport, "poll");
});

test("reconnect() makes a new connection, so Wakeshift hands out again what isn't acked", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res) => {
    open(res);
    sse(res, "event", ev("d1", "one"), "d1");
  };
  const r = recorder();
  const s = new Transport({ link: f.link(), receive: r.receive }).start();
  t.after(() => s.stop());
  await until(() => r.seen.length === 1);
  s.reconnect();
  await until(() => f.sse.length === 2);
});

test("ack: one POST of up to 100 IDs, its status (0 when unreachable)", async (t) => {
  const f = await fake();
  const s = new Transport({ link: f.link(), receive: async () => 1 });
  assert.equal(await s.ack(["a", "b"]), 200);
  assert.deepEqual(f.acks, [["a", "b"]]);
  await assert.rejects(s.ack(Array.from({ length: 101 }, (_, i) => String(i))));
  await f.close();
  assert.equal(await s.ack(["a"]), 0);
  t.after(() => s.stop());
});

test("a 403 or 404 that isn't Wakeshift's link_revoked is retried, not gone; link_revoked is gone", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res, n) => {
    if (n === 1) return void res.writeHead(404, { "Content-Type": "text/html" }).end("<h1>Not Found</h1>");
    if (n === 2) return void res.writeHead(403).end();
    open(res);
    sse(res, "event", ev("d1", "one"), "d1");
  };
  const r = recorder();
  const s = new Transport({ link: f.link(), minBackoff: 10, maxBackoff: 20, receive: r.receive }).start();
  t.after(() => s.stop());
  await until(() => r.seen.length === 1);

  const g = await fake();
  t.after(g.close);
  g.onSSE = (res) => res.writeHead(404, { "Content-Type": "application/json" }).end(JSON.stringify({ error: { code: "link_revoked" } }));
  const s2 = new Transport({ link: g.link(), minBackoff: 10, receive: async () => 1 }).start();
  assert.deepEqual(await s2.done, { why: "gone", reason: "link_revoked" });
});

test("an ack refused with a 404 that's link_revoked reads as 401; a plain 404 stays 404", async (t) => {
  let revoked = true;
  const f = (async () =>
    new Response(revoked ? JSON.stringify({ error: { code: "link_revoked" } }) : "nope", { status: 404 })) as unknown as typeof fetch;
  const s = new Transport({ link: "ws://x/v1/stream?link=t", fetch: f, receive: async () => 1 });
  t.after(() => s.stop());
  assert.equal(await s.ack(["a"]), 401);
  revoked = false;
  assert.equal(await s.ack(["a"]), 404);
});

test("connections that end at once with nothing new back off instead of reconnecting in a tight loop", async (t) => {
  const f = await fake();
  t.after(f.close);
  const at: number[] = [];
  f.onSSE = (res) => {
    at.push(Date.now());
    open(res);
    sse(res, "closed", { type: "hub.closed", reason: "restarting" });
    res.end();
  };
  const s = new Transport({ link: f.link(), minBackoff: 50, maxBackoff: 400, receive: async () => 1 }).start();
  t.after(() => s.stop());
  await new Promise((r) => setTimeout(r, 800));
  s.stop();
  assert.ok(at.length >= 3 && at.length <= 6, `${at.length} connections`);
  for (let i = 2; i < at.length; i++) assert.ok(at[i] - at[i - 1] >= at[i - 1] - at[i - 2] - 5, "backs off");
});

test("an over-long SSE line is skipped, and the stream goes on", async (t) => {
  const f = await fake();
  t.after(f.close);
  f.onSSE = (res) => {
    open(res);
    sse(res, "event", ev("big", "x".repeat(MAX_LINE + 10)), "big");
    sse(res, "event", ev("d1", "one"), "d1");
  };
  const r = recorder();
  const s = new Transport({ link: f.link(), receive: r.receive }).start();
  t.after(() => s.stop());
  await until(() => r.seen.length === 1);
  assert.deepEqual(r.seen, ["d1"]);
});

test("sleep leaves no abort listener behind once its timer fires", async () => {
  const ac = new AbortController();
  for (let i = 0; i < 20; i++) await sleep(1, ac.signal);
  assert.equal(getEventListeners(ac.signal, "abort").length, 0);
});
