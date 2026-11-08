// The CLI against a real core (Go's TestSDKHarness, started by
// scripts/e2e.sh) and wakebench's Shipyard (bench/app/ci.ts), as an agent
// would run it: init, with a scripted person confirming the link in the
// console; keys new the same way; status; then verify, with a synthetic
// emit, with a tool that emits, and refusing to emit while another
// conversation is subscribed. Nothing any command prints may show a key.
//
// Skipped unless WAKESHIFT_SDK_HARNESS names the harness's file.

import assert from "node:assert/strict";
import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { after, before, test } from "node:test";
import { fileURLToPath } from "node:url";
import { getVar, readEnvFile } from "../src/envfile.js";
import { McpClient, textOf } from "../src/mcp.js";
import { assertNoKey } from "./fakes.js";

const harnessFile = process.env.WAKESHIFT_SDK_HARNESS;
const here = dirname(fileURLToPath(import.meta.url));
const pkg = join(here, "../..");
const repo = join(pkg, "../..");
const bin = join(pkg, "build-test/src/cli.js");
const shipyardMain = join(repo, "bench/dist/app/main.js");

// With a harness (scripts/e2e.sh, CI) every test runs: nothing skips.
const skip = !harnessFile ? "set WAKESHIFT_SDK_HARNESS (scripts/e2e.sh)" : false;
if (harnessFile && !existsSync(shipyardMain)) throw new Error("build the bench first (scripts/e2e.sh does)");

let h: { core: string; key: string; new_key: string; waitlist_cookie: string };
let work: string;
let project: string;
let browserLog: string;
let shipyard: ChildProcess | null = null;
let base = "";

before(() => {
  if (skip) return;
  h = JSON.parse(readFileSync(harnessFile!, "utf8"));
  work = mkdtempSync(join(tmpdir(), "wscli-e2e-"));
  project = join(work, "shop");
  execFileSync("mkdir", ["-p", project]);
  execFileSync("git", ["init", "-q"], { cwd: project });
  writeFileSync(join(project, "package.json"), '{"name":"e2e-shop"}');
  // $BROWSER records the link instead of opening anything.
  browserLog = join(work, "browser.log");
  const rec = join(work, "browser.sh");
  writeFileSync(rec, `#!/bin/sh\nprintf '%s\\n' "$1" >> '${browserLog}'\n`);
  chmodSync(rec, 0o755);
});

after(async () => {
  if (shipyard && shipyard.exitCode === null) {
    shipyard.kill("SIGTERM");
    await new Promise((r) => setTimeout(r, 300));
  }
  if (work) rmSync(work, { recursive: true, force: true });
});

interface Ran {
  code: number;
  out: string;
  err: string;
}

/** Starts the CLI as npx would, in the project (or cwd), with a temp HOME and the recording browser. */
function start(args: string[], cwd = project): { proc: ChildProcess; done: Promise<Ran> } {
  const proc = spawn(process.execPath, [bin, ...args], {
    cwd,
    env: { PATH: process.env.PATH ?? "", HOME: work, BROWSER: join(work, "browser.sh"), WAKESHIFT_CLI_POLL_INTERVAL_MS: "200" },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let out = "";
  let err = "";
  proc.stdout!.on("data", (d) => (out += d));
  proc.stderr!.on("data", (d) => (err += d));
  const done = new Promise<Ran>((r) => proc.on("close", (code) => r({ code: code ?? -1, out, err })));
  return { proc, done };
}

const envKey = (name = "WAKESHIFT_KEY") => getVar(readEnvFile(join(project, ".env")), name);

async function cli(args: string[]): Promise<Ran> {
  const r = await start(args).done;
  assertNoKey(r.out + r.err, [envKey(), envKey("WAKESHIFT_KEY_NEW"), h.new_key, h.key], args.join(" "));
  return r;
}

/** The next link the CLI opened. */
async function nextLink(seen: number): Promise<URL> {
  for (let i = 0; i < 200; i++) {
    const lines = existsSync(browserLog) ? readFileSync(browserLog, "utf8").split("\n").filter(Boolean) : [];
    if (lines.length > seen) return new URL(lines[seen]);
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error("the CLI didn't open a link");
}

/** The person, signed in with dev sign-in: a console session. */
async function console_(): Promise<(method: string, path: string, body: unknown) => Promise<{ status: number; json: any }>> {
  const r = await fetch(h.core + "/api/v1/session/dev", { method: "POST", headers: { origin: h.core, "content-type": "application/json" }, body: JSON.stringify({ email: "cli-e2e@example.com" }) });
  assert.equal(r.status, 200, "dev sign-in");
  const cookie = (r.headers.getSetCookie?.() ?? [r.headers.get("set-cookie") ?? ""]).map((c) => c.split(";")[0]).join("; ");
  return async (method, path, body) => {
    const res = await fetch(h.core + path, { method, headers: { origin: h.core, "content-type": "application/json", cookie }, body: JSON.stringify(body) });
    const text = await res.text();
    let json: any;
    try {
      json = JSON.parse(text);
    } catch {
      json = text;
    }
    return { status: res.status, json };
  };
}

let appId = "";

test("init: the person confirms the link in the console, and the app exists", { skip }, async () => {
  const run = start(["init", "--origin", h.core]);
  const link = await nextLink(0);
  assert.equal(link.pathname, "/new-app");
  const frag = new URLSearchParams(link.hash.slice(1));
  assert.equal(frag.get("id"), "e2e-shop-dev");
  assert.equal(frag.get("name"), "E2e Shop (dev)");
  const pk = frag.get("pk")!;
  assert.match(pk, /^[A-Za-z0-9_-]{43}$/);
  const as = await console_();
  // The confirm page's call (docs/agentic-onboarding.md, "Contract with the CLI").
  const made = await as("POST", "/api/v1/developer/apps/register", { id: frag.get("id"), name: frag.get("name"), public_key: pk });
  assert.equal(made.status, 200, JSON.stringify(made.json));
  assert.ok(!JSON.stringify(made.json).includes("wst_"), "the route returned a key");
  const r = await run.done;
  assertNoKey(r.out + r.err, [envKey()]);
  assert.equal(r.code, 0, r.out + r.err);
  assert.match(r.out, /Created e2e-shop-dev\. WAKESHIFT_KEY is in \.env\./);
  assert.match(r.out, new RegExp(`Key code: ${made.json.key_info.key_id}`));
  appId = "e2e-shop-dev";
  assert.match(readFileSync(join(project, ".gitignore"), "utf8"), /^\.env$/m);
});

test("keys new: a second key, confirmed the same way, replaces the first in .env", { skip }, async () => {
  const before = envKey();
  const seen = readFileSync(browserLog, "utf8").split("\n").filter(Boolean).length;
  const run = start(["keys", "new", "--origin", h.core]);
  const link = await nextLink(seen);
  assert.equal(link.pathname, `/apps/${appId}/keys`);
  const pk = new URLSearchParams(link.hash.slice(1)).get("pk");
  const as = await console_();
  const added = await as("POST", `/api/v1/developer/apps/${appId}/keys/register`, { name: "cli", public_key: pk });
  assert.equal(added.status, 200, JSON.stringify(added.json));
  const r = await run.done;
  assertNoKey(r.out + r.err, [envKey(), before]);
  assert.equal(r.code, 0, r.out + r.err);
  assert.notEqual(envKey(), before);
  assert.equal(envKey("WAKESHIFT_KEY_NEW"), undefined);
});

test("status: the app and its key", { skip }, async () => {
  const r = await cli(["status", "--origin", h.core]);
  assert.equal(r.code, 0, r.err);
  assert.match(r.out, new RegExp(`App: +${appId}`));
});

test("verify against Shipyard: synthetic, a tool trigger, and the guard", { skip }, async () => {
  // Shipyard runs with the key in .env, as the developer's server would.
  const keyFile = join(work, "app.key");
  const controlFile = join(work, "control.token");
  writeFileSync(keyFile, envKey() + "\n", { mode: 0o600 });
  writeFileSync(controlFile, "control-token-e2e\n", { mode: 0o600 });
  shipyard = spawn(process.execPath, [shipyardMain], {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: work,
      BENCH_KEY_FILE: keyFile,
      BENCH_CONTROL_FILE: controlFile,
      WAKESHIFT_CORE: h.core,
      BENCH_RUN: "cli-e2e",
      BENCH_APP: "ci",
      WB_CI_WATCH_DESCRIPTION: "subscribes",
      PORT: "0",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let log = "";
  shipyard.stderr!.on("data", (d) => (log += d));
  base = await new Promise<string>((resolve, reject) => {
    let buf = "";
    shipyard!.stdout!.on("data", (d) => {
      buf += d;
      const m = /\{"listening":(\d+)/.exec(buf);
      if (m) resolve(`http://127.0.0.1:${m[1]}`);
    });
    shipyard!.on("exit", (c) => reject(new Error(`Shipyard exited ${c}: ${log}`)));
    setTimeout(() => reject(new Error(`Shipyard didn't start: ${log}`)), 60_000).unref();
  });
  const url = `${base}/mcp/e2e`;
  const checksOf = (r: Ran) => JSON.parse(r.out.trim().split("\n").pop()!).checks as { name: string; status: string; detail: string }[];

  // 1. No trigger: verify emits a synthetic build.finished.
  const v1 = await cli(["verify", "--origin", h.core, "--url", url, "--tool", "watch_build", "--args", '{"number":412}', "--json"]);
  assert.equal(v1.code, 0, v1.out + v1.err + log);
  const c1 = checksOf(v1);
  for (const c of c1) assert.notEqual(c.status, "fail", JSON.stringify(c));
  assert.equal(c1.find((c) => c.name === "description")!.status, "pass");
  assert.match(c1.find((c) => c.name === "arguments")!.detail, /"pipeline":"acme-web-[0-9a-f]+".*from list_subscriptions/);
  assert.equal(c1.find((c) => c.name === "cleanup")!.status, "pass");

  // status now shows the events the server sent.
  const s = await cli(["status", "--origin", h.core]);
  assert.match(s.out, /build\.finished: A build finished/);

  // 2. The developer's own emit site: cancel_build emits build.finished.
  const v2 = await cli(["verify", "--origin", h.core, "--url", url, "--tool", "watch_build", "--args", '{"number":412}', "--trigger", 'tool:cancel_build:{"number":412}']);
  if (process.env.WSCLI_E2E_SHOW) console.log(v2.out.replace(/^/gm, "# "));
  assert.equal(v2.code, 0, v2.out + v2.err + log);
  assert.match(v2.out, /PASS +wake +build\.finished arrived/);
  assert.match(v2.out, /Passed: the tool cancel_build emitted/);

  // 3. Someone else watches build 413: verify must not emit, nor run the trigger.
  const other = new McpClient(url, { fetch });
  await other.connect();
  const started = textOf(await other.callTool("start_build", { branch: "main" }));
  const n = Number(/#(\d+)/.exec(started)![1]);
  const w = await other.callTool("watch_build", { number: n });
  assert.ok(!w.isError, textOf(w));
  const v3 = await cli(["verify", "--origin", h.core, "--url", url, "--tool", "watch_build", "--args", JSON.stringify({ number: n }), "--trigger", `tool:cancel_build:${JSON.stringify({ number: n })}`, "--json"]);
  assert.equal(v3.code, 1, v3.out);
  const c3 = checksOf(v3);
  assert.equal(c3.find((c) => c.name === "guard")!.status, "fail");
  assert.ok(!c3.some((c) => c.name === "trigger"));
  const b = textOf(await other.callTool("get_build", { number: n }));
  assert.match(b, /"status": "running"/, "the guard let the trigger run");
  await other.close();
});

test("init: a person past the sign-up cap is told they're waitlisted, and init exits 3", { skip }, async () => {
  // Another project: its own .env.
  const dir = join(work, "waitlisted");
  execFileSync("mkdir", ["-p", dir]);
  execFileSync("git", ["init", "-q"], { cwd: dir });
  writeFileSync(join(dir, "package.json"), '{"name":"e2e-queue"}');
  const seen = readFileSync(browserLog, "utf8").split("\n").filter(Boolean).length;
  const t0 = Date.now();
  const run = start(["init", "--origin", h.core], dir);
  const link = await nextLink(seen);
  assert.equal(link.pathname, "/new-app");
  const pk = new URLSearchParams(link.hash.slice(1)).get("pk");
  // The waitlist page (the person signed in past the cap: the harness's
  // fake Google) holds the link's public key with their waitlist cookie.
  const held = await fetch(h.core + "/api/v1/waitlist/key", {
    method: "POST",
    headers: { origin: h.core, "content-type": "application/json", cookie: `ws_waitlist=${h.waitlist_cookie}` },
    body: JSON.stringify({ public_key: pk }),
  });
  assert.equal(held.status, 200, await held.text());
  const r = await run.done;
  const key = getVar(readEnvFile(join(dir, ".env")), "WAKESHIFT_KEY");
  assertNoKey(r.out + r.err, [key, h.new_key, h.key]);
  assert.ok(!(r.out + r.err).includes(h.waitlist_cookie), "printed the waitlist cookie");
  assert.equal(r.code, 3, r.out + r.err);
  assert.match(r.err, /You're on Wakeshift's waitlist, so the app wasn't created\. Wakeshift will email you when your account is ready; run init again then/);
  assert.ok(Date.now() - t0 < 60_000, "init waited instead of stopping");
  assert.ok(key, "the key stays in .env for the rerun");
});
