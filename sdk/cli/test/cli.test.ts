// The commands end to end against stand-ins (fakes.ts): exit codes, what
// they write, and that nothing they print, on any path, shows a key.

import assert from "node:assert/strict";
import { execFile, spawnSync } from "node:child_process";
import { promisify } from "node:util";
import { chmodSync, existsSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { after, before, test } from "node:test";
import { fileURLToPath } from "node:url";
import { main } from "../src/cli.js";
import { getVar, readEnvFile } from "../src/envfile.js";
import { generateKey, parseKey } from "../src/key.js";
import { VERSION } from "../src/version.js";
import { assertNoKey, BUILD_FINISHED, FakeCore, FakeMcp, TestCtx } from "./fakes.js";

let core: FakeCore;
before(async () => {
  core = await new FakeCore().start();
});
after(() => core.stop());

const keyIn = (c: TestCtx, name = "WAKESHIFT_KEY") => getVar(readEnvFile(join(c.cwd, ".env")), name);

/** Runs the CLI in c; fails if anything it printed shows a key in c's .env (or extra). */
async function run(c: TestCtx, argv: string[], extra: string[] = []): Promise<number> {
  const code = await main(argv, c);
  const keys = [keyIn(c), keyIn(c, "WAKESHIFT_KEY_NEW"), ...extra];
  assertNoKey(c.all(), keys, `${argv.join(" ")} (exit ${code})`);
  return code;
}

function ctx(env: Record<string, string> = {}): TestCtx {
  const c = new TestCtx({ WAKESHIFT_CORE: core.origin, ...env });
  writeFileSync(join(c.cwd, "package.json"), '{"name":"@acme/shipyard"}');
  return c;
}

/** The console's confirm, after a few polls: registers the key in .env (or WAKESHIFT_KEY_NEW) on appId. */
function confirmAfter(c: TestCtx, appId: string, polls = 3, name = "WAKESHIFT_KEY") {
  core.onUnknownPoll = (n) => {
    if (n < polls) return;
    const raw = keyIn(c, name);
    if (!raw) return;
    const k = parseKey(raw);
    core.register(appId, k.publicKey, k.kid);
    core.onUnknownPoll = null;
  };
}

test("init: makes the key, writes .env (0600), prints the link and code, waits, then the app", async () => {
  const c = ctx();
  confirmAfter(c, "shipyard-dev");
  assert.equal(await run(c, ["init"]), 0);
  const raw = keyIn(c)!;
  const k = parseKey(raw);
  assert.match(c.out, new RegExp(`/new-app#pk=${k.publicKey}&id=shipyard-dev&name=Shipyard%20\\(dev\\)`));
  assert.match(c.out, new RegExp(`Key code: ${k.keyCode}`));
  assert.match(c.out, /Created shipyard-dev\. WAKESHIFT_KEY is in \.env\./);
  if (process.platform !== "win32") assert.equal(statSync(join(c.cwd, ".env")).mode & 0o777, 0o600);
  // A local core: the SDK needs to know it.
  assert.equal(keyIn(c, "WAKESHIFT_CORE"), core.origin);
  assert.deepEqual(c.opened, []);
  // Only public things went over the wire: assertions, never the key or seed.
  for (const r of core.requests) assert.ok(!r.auth.includes(raw.slice(4)) && !r.body.includes(raw.slice(4)));

  // Again: it's set up already.
  const again = new TestCtx({ WAKESHIFT_CORE: core.origin }, c.cwd);
  assert.equal(await run(again, ["init"]), 0);
  assert.match(again.out, /already has the key of shipyard-dev/);
  assert.equal(keyIn(c), raw);
  c.cleanup();
});

test("init: opens the browser through $BROWSER when there is one", async () => {
  const c = ctx({ BROWSER: "/bin/echo" });
  c.env.WAKESHIFT_NO_BROWSER = undefined;
  confirmAfter(c, "browser-dev");
  assert.equal(await run(c, ["init", "--name", "browser-dev"]), 0);
  assert.equal(c.opened.length, 1);
  assert.equal(c.opened[0].cmd, "/bin/echo");
  assert.match(c.opened[0].args[0], /\/new-app#pk=.*&id=browser-dev&name=Browser%20\(dev\)$/);
  assert.match(c.out, /Opened it in the browser/);
  const n = new TestCtx({ WAKESHIFT_CORE: core.origin, BROWSER: "/bin/echo", WAKESHIFT_NO_BROWSER: undefined });
  confirmAfter(n, "x-dev");
  assert.equal(await run(n, ["init", "--name", "x-dev", "--no-browser"]), 0);
  assert.deepEqual(n.opened, []);
  c.cleanup();
  n.cleanup();
});

test("init: times out with exit 4, keeps the key, and a second run waits with it again", async () => {
  const c = ctx({ WAKESHIFT_CLI_POLL_TIMEOUT_MS: "50" });
  core.onUnknownPoll = null;
  assert.equal(await run(c, ["init", "--name", "slow-dev"]), 4);
  assert.match(c.errOut, /isn't confirmed yet/);
  const first = keyIn(c)!;
  const again = new TestCtx({ WAKESHIFT_CORE: core.origin }, c.cwd);
  confirmAfter(again, "slow-dev");
  assert.equal(await run(again, ["init", "--name", "slow-dev"]), 0);
  assert.match(again.out, /a key no app has yet, from an earlier run/);
  assert.equal(keyIn(c), first);
  assert.match(again.out, /Created slow-dev\./);
  c.cleanup();
});

test("init: --replace makes a new key; without it an unusable key is refused", async () => {
  const c = ctx();
  writeFileSync(join(c.cwd, ".env"), "PORT=1\nWAKESHIFT_KEY=wst_notakey\n");
  assert.equal(await run(c, ["init", "--name", "r-dev"]), 1);
  assert.match(c.errOut, /isn't a valid app key\. Pass --replace/);
  confirmAfter(c, "r-dev");
  assert.equal(await run(c, ["init", "--name", "r-dev", "--replace"]), 0);
  assert.match(readFileSync(join(c.cwd, ".env"), "utf8"), /^PORT=1\nWAKESHIFT_KEY=wst_[1-9A-HJ-NP-Za-km-z]{50}\nWAKESHIFT_CORE=/);
  c.cleanup();
});

test("init: a waitlisted account exits 3", async () => {
  const c = ctx();
  core.appAnswer = (req) => (String(req.headers.authorization).length > 10 ? { status: 403, body: { error: { code: "waitlisted", message: "the account is on the waitlist" } } } : null);
  try {
    assert.equal(await run(c, ["init", "--name", "w-dev"]), 3);
    assert.match(c.errOut, /waitlist/);
  } finally {
    core.appAnswer = null;
  }
  c.cleanup();
});

test("init: usage errors exit 2", async () => {
  const c = ctx();
  for (const argv of [["init", "--name", "Bad_ID"], ["init", "--display-name", 'a"b'], ["init", "--bogus"], ["init", "extra"], ["init", "--name"], ["init", "--origin", "http://example.com"], ["init", "--wait", "0"], ["nope"], ["keys"], []]) {
    c.out = c.errOut = "";
    assert.equal(await run(c, argv), 2, argv.join(" "));
  }
  assert.ok(!existsSync(join(c.cwd, ".env")));
  c.cleanup();
});

test("a key on the command line is refused, and never echoed", async () => {
  const c = ctx();
  const k = generateKey().envValue();
  for (const argv of [["init", "--name", k], ["status", `--env=${k}`], ["verify", "--args", `{"key":"${k}"}`], [k]]) {
    c.out = c.errOut = "";
    assert.equal(await run(c, argv, [k]), 2);
    assert.match(c.errOut, /never takes an app key/);
  }
  c.cleanup();
});

test("errors Wakeshift relays are redacted, whatever they quote", async () => {
  const c = ctx();
  confirmAfter(c, "leaky-dev");
  assert.equal(await run(c, ["init", "--name", "leaky-dev"]), 0);
  const raw = keyIn(c)!;
  core.appAnswer = () => ({ status: 500, body: { error: { code: "internal", message: `boom: ${raw} and wst_test_ci-bot_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA` } } });
  try {
    for (const argv of [["status"], ["init"], ["keys", "new"], ["verify", "--url", "http://127.0.0.1:1/mcp", "--tool", "x"]]) {
      c.out = c.errOut = "";
      const code = await run(c, argv, [raw]);
      assert.notEqual(code, 0, argv.join(" "));
      assert.match(c.all(), /wst_\[redacted\]/, argv.join(" "));
    }
  } finally {
    core.appAnswer = null;
  }
  c.cleanup();
});

test("status: the app, key and events; failures say what to do", async () => {
  const c = ctx();
  assert.equal(await run(c, ["status"]), 1);
  assert.match(c.errOut, /there's no \.env: run init first/);
  writeFileSync(join(c.cwd, ".env"), "PORT=1\n");
  c.errOut = "";
  assert.equal(await run(c, ["status"]), 1);
  assert.match(c.errOut, /has no WAKESHIFT_KEY/);
  const k = generateKey();
  writeFileSync(join(c.cwd, ".env"), `WAKESHIFT_KEY=${k.envValue()}\n`);
  c.errOut = "";
  assert.equal(await run(c, ["status"]), 1);
  assert.match(c.errOut, /no app has the key in \.env/);
  core.addApp("status-dev", "Status (dev)", { description: "", events: [BUILD_FINISHED] }).keys.set(k.kid, k.publicKey);
  c.out = "";
  assert.equal(await run(c, ["status"]), 0);
  assert.match(c.out, /App: +status-dev \(Status \(dev\)\)/);
  assert.match(c.out, new RegExp(`Key: +${k.keyCode}`));
  assert.match(c.out, /build\.finished: A build finished/);
  c.out = "";
  assert.equal(await run(c, ["status", "--json"]), 0);
  const j = JSON.parse(c.out);
  assert.equal(j.app.id, "status-dev");
  assert.equal(j.manifest, true);
  c.cleanup();
});

test("keys new: a new key for the app, confirmed in the console, then swapped in", async () => {
  const c = ctx();
  const old = generateKey();
  core.addApp("keys-dev", "Keys").keys.set(old.kid, old.publicKey);
  writeFileSync(join(c.cwd, ".env"), `PORT=1\nWAKESHIFT_KEY=${old.envValue()}\n`);
  confirmAfter(c, "keys-dev", 2, "WAKESHIFT_KEY_NEW");
  assert.equal(await run(c, ["keys", "new"], [old.envValue()]), 0);
  const now = keyIn(c)!;
  const k = parseKey(now);
  assert.notEqual(now, old.envValue());
  assert.equal(keyIn(c, "WAKESHIFT_KEY_NEW"), undefined);
  assert.match(c.out, new RegExp(`/apps/keys-dev/keys#pk=${k.publicKey}`));
  assert.match(c.out, new RegExp(`Added key ${k.keyCode} to keys-dev`));
  // --app naming another app than .env's key is refused.
  c.out = c.errOut = "";
  assert.equal(await run(c, ["keys", "new", "--app", "other-dev"]), 2);
  assert.match(c.errOut, /is keys-dev's key, not other-dev's/);
  // No working key and no --app: which app?
  const d = ctx();
  assert.equal(await run(d, ["keys", "new"]), 2);
  assert.match(d.errOut, /pass --app/);
  c.cleanup();
  d.cleanup();
});

test("keys new: times out with exit 4 and resumes with the same new key", async () => {
  const c = ctx({ WAKESHIFT_CLI_POLL_TIMEOUT_MS: "40" });
  core.onUnknownPoll = null;
  assert.equal(await run(c, ["keys", "new", "--app", "resume-dev"]), 4);
  const pending = keyIn(c, "WAKESHIFT_KEY_NEW")!;
  assert.ok(pending);
  const again = new TestCtx({ WAKESHIFT_CORE: core.origin }, c.cwd);
  confirmAfter(again, "resume-dev", 1, "WAKESHIFT_KEY_NEW");
  assert.equal(await run(again, ["keys", "new", "--app", "resume-dev"], [pending]), 0);
  assert.equal(keyIn(c), pending);
  c.cleanup();
});

// ── verify ───────────────────────────────────────────────────────────────

async function verifyApp(id = "verify-dev") {
  const c = ctx();
  const k = generateKey();
  core.addApp(id, "Shipyard", { description: "", events: [BUILD_FINISHED] }).keys.set(k.kid, k.publicKey);
  writeFileSync(join(c.cwd, ".env"), `WAKESHIFT_KEY=${k.envValue()}\n`);
  return { c, k };
}

const checksOf = (out: string) => JSON.parse(out.trim().split("\n").pop()!).checks as { name: string; status: string; detail: string; fix?: string }[];

test("verify: passes with a tool trigger, the subscription found by list_subscriptions, and cleans up", async () => {
  const { c } = await verifyApp();
  const mcp = await new FakeMcp(core).start();
  try {
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--args", '{"number":412}', "--trigger", 'tool:cancel_build:{"number":412}', "--json"]), 0);
    const checks = checksOf(c.out);
    assert.deepEqual(checks.map((x) => [x.name, x.status]), [
      ["app", "pass"],
      ["live app", "pass"],
      ["tool", "pass"],
      ["description", "pass"],
      ["subscribe", "pass"],
      ["event", "pass"],
      ["arguments", "pass"],
      ["guard", "pass"],
      ["trigger", "pass"],
      ["wake", "pass"],
      ["cleanup", "pass"],
    ]);
    assert.match(checks.find((x) => x.name === "arguments")!.detail, /\{"number":412,"pipeline":"acme-web-1a2b"\} \(from list_subscriptions\)/);
    assert.equal(core.subs.filter((s) => !s.ended).length, 0);
    // An unrecognized client, as a generic agent.
    assert.deepEqual(mcp.calls[0].client, { name: "wakeshift-verify", version: "1" });
    // The stream link and session key never show.
    assert.ok(!c.out.includes("link1x") && !c.out.includes("wsc_secret"));
  } finally {
    await mcp.stop();
    c.cleanup();
  }
});

test("verify: a synthetic emit when no trigger is given, with a payload fitting the schema", async () => {
  const { c } = await verifyApp("synthetic-dev");
  const mcp = await new FakeMcp(core, { stateless: true }, "pipe-2").start();
  try {
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--args", '{"number":7}']), 0);
    assert.match(c.out, /PASS +wake/);
    assert.match(c.out, /Passed, with a synthetic emit/);
    const emit = core.emits[core.emits.length - 1] as { name: string; arguments: unknown; payload: { status: string; number: number }; id: string };
    assert.equal(emit.name, "build.finished");
    assert.deepEqual(emit.arguments, { pipeline: "pipe-2", number: 7 });
    assert.match(emit.id, /^wsverify-[0-9a-f]{8}$/);
    assert.equal(emit.payload.status, emit.id);
    assert.equal(mcp.initialized, 0);
  } finally {
    await mcp.stop();
    c.cleanup();
  }
});

test("verify: the guard refuses to emit when anyone else is subscribed", async () => {
  const { c } = await verifyApp("guard-dev");
  const mcp = await new FakeMcp(core, {}, "pipe-guard").start();
  core.others = 1;
  const emitsBefore = core.emits.length;
  try {
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--args", '{"number":1}', "--json"]), 1);
    const checks = checksOf(c.out);
    const guard = checks.find((x) => x.name === "guard")!;
    assert.equal(guard.status, "fail");
    assert.match(guard.detail, /Other conversations are subscribed with these arguments/);
    assert.match(guard.fix!, /wsverify-/);
    assert.ok(!checks.some((x) => x.name === "trigger" || x.name === "wake"));
    assert.equal(core.emits.length, emitsBefore, "verify emitted");
    assert.ok(!mcp.calls.some((x) => x.name === "cancel_build"));
    // Its own subscription is still cleaned up.
    assert.equal(checks[checks.length - 1].name, "cleanup");
    assert.equal(core.subs.filter((s) => !s.ended).length, 0);
  } finally {
    core.others = 0;
    await mcp.stop();
    c.cleanup();
  }
});

test("verify: without list_subscriptions, a count of one passes only if nobody was subscribed before verify", async () => {
  const { c } = await verifyApp("pre-dev");
  const mcp = await new FakeMcp(core, { noAttachTools: true }, "pipe-pre").start();
  try {
    // The arguments verify derives are the tool's: nobody before it, then just verify.
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--args", '{"number":8,"pipeline":"pipe-pre"}', "--json"]), 0, c.out);
    assert.match(checksOf(c.out).find((x) => x.name === "guard")!.detail, /nobody was before it/);
    // Arguments that aren't the tool's, and someone else subscribed with them:
    // the count is one, but not verify's.
    const stranger = core.subscribe("build.finished", { pipeline: "elsewhere", number: 8 }, "someone");
    const emits = core.emits.length;
    c.out = "";
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--args", '{"number":8,"pipeline":"elsewhere"}', "--json"]), 1);
    const guard = checksOf(c.out).find((x) => x.name === "guard")!;
    assert.equal(guard.status, "fail");
    assert.equal(core.emits.length, emits, "verify emitted to someone else's conversation");
    assert.equal(core.queues.get(stranger.link)!.length, 0);
    stranger.ended = true;
  } finally {
    await mcp.stop();
    c.cleanup();
  }
});

test("verify: a tool trigger is guarded too", async () => {
  const { c } = await verifyApp("guard2-dev");
  const mcp = await new FakeMcp(core, {}, "pipe-guard2").start();
  core.others = 2;
  try {
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--args", '{"number":1}', "--trigger", 'tool:cancel_build:{"number":1}']), 1);
    assert.ok(!mcp.calls.some((x) => x.name === "cancel_build"), "the trigger ran despite the guard");
    assert.match(c.out, /FAIL +guard/);
  } finally {
    core.others = 0;
    await mcp.stop();
    c.cleanup();
  }
});

test("verify: each failure says what to fix", async () => {
  const cases: { name: string; o: ConstructorParameters<typeof FakeMcp>[1]; args?: string[]; check: string; want: RegExp; fix: RegExp }[] = [
    { name: "no sentence", o: { description: "Watch a build." }, check: "description", want: /doesn't say it notifies/, fix: /subscribes/ },
    { name: "no inputs", o: { noInputs: true }, check: "tool", want: /lacks Wakeshift's inputs/, fix: /attached\.events/ },
    { name: "no block", o: { noBlock: true }, check: "subscribe", want: /no Wakeshift block/, fix: /call\.subscribe/ },
    { name: "error reply quoting a key", o: { errorText: "denied for wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV" }, check: "subscribe", want: /answered with an error: denied for wst_\[redacted\]/, fix: /--args/ },
    { name: "no tool", o: {}, args: ["--tool", "nope"], check: "tool", want: /lists no tool nope/, fix: /--tool/ },
    { name: "a trigger that leaks", o: {}, args: ["--trigger", "tool:leak"], check: "trigger", want: /wst_\[redacted\]/, fix: /arguments/ },
    { name: "a command that fails", o: {}, args: ["--trigger", "cmd:cat .env; exit 3"], check: "trigger", want: /exited 3: WAKESHIFT_KEY=wst_\[redacted\]/, fix: /exits 0/ },
    { name: "a trigger that emits nothing", o: {}, args: ["--trigger", "cmd:true", "--timeout", "1"], check: "wake", want: /no build\.finished reached/, fix: /same arguments/ },
    { name: "needs a credential", o: { token: "s3cret-token-value" }, check: "tool", want: /needs a credential/, fix: /--header-env/ },
    { name: "wrong event", o: {}, args: ["--event", "nope.nope"], check: "event", want: /no event nope\.nope/, fix: /defineEvent/ },
    { name: "no attach tools and no event args", o: { noAttachTools: true }, args: ["--args", "{}"], check: "arguments", want: /needs pipeline, number/, fix: /--event-args/ },
  ];
  let i = 0;
  for (const tc of cases) {
    const { c } = await verifyApp(`fail${i++}-dev`);
    const mcp = await new FakeMcp(core, tc.o, `pipe-fail-${i}`).start();
    try {
      const extra = tc.args ?? [];
      const argv = ["verify", "--url", mcp.url, ...(extra.includes("--tool") ? [] : ["--tool", "watch_build"]), ...(extra.includes("--args") ? [] : ["--args", '{"number":5}']), "--json", ...extra];
      assert.equal(await run(c, argv, ["s3cret-token-value"]), 1, tc.name);
      const checks = checksOf(c.out);
      const bad = checks.find((x) => x.status === "fail")!;
      assert.equal(bad.name, tc.check, `${tc.name}: ${JSON.stringify(checks)}`);
      assert.match(bad.detail, tc.want, tc.name);
      assert.match(bad.fix ?? "", tc.fix, tc.name);
    } finally {
      await mcp.stop();
      c.cleanup();
    }
  }
  assert.equal(core.subs.filter((s) => !s.ended).length, 0, "a failing verify left a subscription");
});

test("verify: a server with sign-in, through --header-env, never printing the token", async () => {
  const { c } = await verifyApp("auth-dev");
  const mcp = await new FakeMcp(core, { token: "tok-abcdefghijkl" }, "pipe-auth").start();
  c.env.TEST_USER_TOKEN = "tok-abcdefghijkl";
  try {
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--args", '{"number":3}', "--header-env", "TEST_USER_TOKEN"], ["tok-abcdefghijkl"]), 0);
    assert.ok(!c.all().includes("tok-abcdefghijkl"));
    c.out = c.errOut = "";
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--header-env", "tok-abcdefghijkl"], ["tok-abcdefghijkl"]), 2);
    c.out = c.errOut = "";
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--header-env", "EMPTY_VAR"]), 2);
  } finally {
    await mcp.stop();
    c.cleanup();
  }
});

test("verify: a live app needs --live-app; a lost wake fails", async () => {
  const { c } = await verifyApp("shipyard");
  const mcp = await new FakeMcp(core, {}, "pipe-live").start();
  try {
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--args", '{"number":2}']), 1);
    assert.match(c.out, /FAIL +live app +shipyard doesn't look like a development app/);
    c.out = "";
    core.dropEmits = true;
    assert.equal(await run(c, ["verify", "--url", mcp.url, "--tool", "watch_build", "--args", '{"number":2}', "--live-app", "--timeout", "1"]), 1);
    assert.match(c.out, /WARN +live app/);
    assert.match(c.out, /FAIL +wake +the synthetic build\.finished didn't reach/);
  } finally {
    core.dropEmits = false;
    await mcp.stop();
    c.cleanup();
  }
});

test("verify: usage errors exit 2", async () => {
  const { c } = await verifyApp("usage-dev");
  for (const argv of [["verify"], ["verify", "--url", "x"], ["verify", "--url", "http://h/mcp"], ["verify", "--url", "http://h/mcp", "--tool", "t", "--args", "[1]"], ["verify", "--url", "http://h/mcp", "--tool", "t", "--trigger", "bogus"], ["verify", "--url", "http://h/mcp", "--tool", "t", "--timeout", "0"]]) {
    c.out = c.errOut = "";
    assert.equal(await run(c, argv), 2, argv.join(" "));
  }
  c.cleanup();
});

// ── the built CLI, as npx runs it ────────────────────────────────────────

const pkg = join(dirname(fileURLToPath(import.meta.url)), "../..");

test("the package: version, bin and no runtime dependencies", () => {
  const j = JSON.parse(readFileSync(join(pkg, "package.json"), "utf8"));
  assert.equal(j.version, VERSION);
  assert.deepEqual(j.bin, { "wakeshift-cli": "dist/cli.js" });
  assert.equal(j.dependencies, undefined);
  assert.equal(j.name, "@wakeshift/cli");
});

test("the built bin: help, exit codes, and a key in an uncaught error is redacted", async (t) => {
  const bin = join(pkg, "build-test/src/cli.js");
  if (!existsSync(bin)) return t.skip("not built");
  const help = spawnSync(process.execPath, [bin, "--help"], { encoding: "utf8" });
  assert.equal(help.status, 0);
  assert.match(help.stdout, /npx -y @wakeshift\/cli@/);
  assert.ok(!/staging/i.test(help.stdout));
  assert.equal(spawnSync(process.execPath, [bin], { encoding: "utf8" }).status, 2);
  const k = generateKey().envValue();
  // --env passes through Node to the CLI (Node intercepts its own --env-file).
  const r = spawnSync(process.execPath, [bin, "status", "--env", k], { encoding: "utf8" });
  assert.equal(r.status, 2);
  assertNoKey(r.stdout + r.stderr, [k]);
  // A real run in a temp directory against the fake core, never a real browser.
  const c = ctx();
  try {
    writeFileSync(join(c.cwd, ".env"), `WAKESHIFT_KEY=${k}\nWAKESHIFT_CORE=${core.origin}\n`);
    chmodSync(join(c.cwd, ".env"), 0o600);
    // Not spawnSync: the fake core answers from this process.
    const s = await promisify(execFile)(process.execPath, [bin, "status"], { cwd: c.cwd, encoding: "utf8", env: { PATH: process.env.PATH, WAKESHIFT_NO_BROWSER: "1" } }).then(
      () => ({ code: 0, stdout: "", stderr: "" }),
      (e: { code: number; stdout: string; stderr: string }) => e,
    );
    assert.equal(s.code, 1);
    assert.match(s.stderr, /no app has the key/);
    assertNoKey(s.stdout + s.stderr, [k]);
  } finally {
    c.cleanup();
  }
});
