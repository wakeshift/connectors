import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { checkStreamLink, Transport } from "../src/transport.js";

const PROD = "https://app.wakeshift.ai";
const STAGING = "https://app.staging.wakeshift.ai";

// A stream link is dialed only on the pinned issuer's own origin: wss or
// https on its host exactly; ws or http only to an http issuer on loopback.
test("checkStreamLink: the issuer's own origin", () => {
  for (const [link, issuer] of [
    ["wss://app.wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["https://app.wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["wss://APP.Wakeshift.AI/v1/stream?link=SECRET", PROD],
    ["wss://app.wakeshift.ai:443/v1/stream?link=SECRET", PROD], // the default port: the same origin, as URL reads it
    ["wss://app.staging.wakeshift.ai/v1/stream?link=SECRET", STAGING],
    ["https://app.staging.wakeshift.ai/v1/stream?link=SECRET", STAGING],
    ["ws://127.0.0.1:8370/v1/stream?link=SECRET", "http://127.0.0.1:8370"],
    ["http://localhost:8370/v1/stream?link=SECRET", "http://localhost:8370"],
  ]) {
    assert.equal(checkStreamLink(link, issuer), undefined, link);
  }
});

test("checkStreamLink: everything else is refused, naming only scheme and host", () => {
  for (const [link, issuer] of [
    ["wss://evil.example/v1/stream?link=SECRET", PROD],
    ["wss://app.wakeshift.ai.evil.example/v1/stream?link=SECRET", PROD],
    ["wss://evil.app.wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["wss://wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["wss://app.wakeshift.ai./v1/stream?link=SECRET", PROD],
    ["wss://app.wakeshift.ai@evil.example/v1/stream?link=SECRET", PROD],
    ["wss://alice:hunter2@app.wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["wss://evil.example#@app.wakeshift.ai", PROD],
    ["wss://evil.example\\@app.wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["wss://app.wakeshift.ai:8443/v1/stream?link=SECRET", PROD],
    ["wss://34.149.10.20/v1/stream?link=SECRET", PROD],
    ["wss://[2600:1901::1]/v1/stream?link=SECRET", PROD],
    ["ws://127.0.0.1:8370/v1/stream?link=SECRET", PROD],
    ["ws://app.wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["http://app.wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["ftp://app.wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["", PROD],
    ["wss://app.staging.wakeshift.ai/v1/stream?link=SECRET", PROD],
    ["wss://app.wakeshift.ai/v1/stream?link=SECRET", STAGING],
    ["wss://app.wakeshift.ai/v1/stream?link=SECRET", ""],
    ["ws://127.0.0.1:8371/v1/stream?link=SECRET", "http://127.0.0.1:8370"],
    ["ws://localhost:8370/v1/stream?link=SECRET", "http://127.0.0.1:8370"],
    ["ws://example.com/v1/stream?link=SECRET", "http://example.com"],
  ]) {
    const why = checkStreamLink(link, issuer);
    assert.ok(why?.startsWith("refused a stream link"), `${link}: ${why}`);
    for (const secret of ["SECRET", "alice", "hunter2", "app.wakeshift.ai@"]) assert.ok(!why!.includes(secret), `${link}: ${why}`);
  }
});

async function serve(handler: Parameters<typeof createServer>[1]): Promise<{ s: Server; url: string; hits: () => number }> {
  let n = 0;
  const s = createServer((req, res) => {
    n++;
    handler!(req, res);
  });
  await new Promise<void>((r) => s.listen(0, "127.0.0.1", r));
  return { s, url: `http://127.0.0.1:${(s.address() as AddressInfo).port}`, hits: () => n };
}

// The transport never sends the token off the pinned origin: a link it's
// handed elsewhere (a saved file someone changed) is never dialed, and the
// issuer's server redirecting elsewhere is never followed.
test("the transport dials only the pinned issuer's origin, and follows no redirect", async (t) => {
  const evil = await serve((_req, res) => res.end(JSON.stringify({ deliveries: [], acked: [] })));
  t.after(() => evil.s.close());
  const issuer = await serve((req, res) => {
    res.writeHead(307, { Location: evil.url + req.url });
    res.end();
  });
  t.after(() => issuer.s.close());
  const logs: string[] = [];

  for (const link of [evil.url.replace("http", "ws") + "/v1/stream?link=SECRET", issuer.url.replace("http://", "ws://x:y@") + "/v1/stream?link=SECRET"]) {
    const s = new Transport({ link, issuer: issuer.url, minBackoff: 10, receive: async () => 1, log: (m) => logs.push(m) });
    assert.equal(await s.ack(["d1"]), 401);
    s.start();
    assert.deepEqual(await s.done, { why: "refused" });
  }
  assert.ok(logs.some((l) => l.startsWith("refused a stream link to ws://127.0.0.1:")), logs.join("\n"));
  assert.ok(!logs.some((l) => l.includes("SECRET") || l.includes("x:y")), logs.join("\n"));
  assert.equal(issuer.hits(), 0);

  // On its origin, the issuer's 3xx isn't followed (SSE, poll and ack).
  const s = new Transport({ link: issuer.url.replace("http", "ws") + "/v1/stream?link=SECRET", issuer: issuer.url, minBackoff: 10, maxBackoff: 20, sseTries: 1, pollWait: 50, receive: async () => 1 }).start();
  t.after(() => s.stop());
  await s.ack(["d1"]);
  const until = Date.now() + 1500;
  while (Date.now() < until && issuer.hits() < 4) await new Promise((r) => setTimeout(r, 20));
  assert.ok(issuer.hits() >= 3, `issuer reached ${issuer.hits()} times`);
  assert.equal(evil.hits(), 0);
});
