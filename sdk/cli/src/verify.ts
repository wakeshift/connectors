// verify: a scripted test subscriber (docs/agentic-onboarding.md, "Verify").
// It calls the app's subscribing tool as an agent would, checks that the
// tool says it notifies and that its reply carries Wakeshift's block,
// makes sure nobody but itself is subscribed with those arguments before
// anything is emitted (the status guard: verify never wakes real users),
// triggers an occurrence, waits for the wake on its own stream, cleans up,
// and reports each check with the fix for each failure.

import { randomBytes } from "node:crypto";
import { parseFlags } from "./args.js";
import { type Ctx, Out } from "./context.js";
import { type AppInfo, type Core, CoreError, type EventDef } from "./core.js";
import { EXIT, usage } from "./errors.js";
import { argsForEvent, canonical, eventsNamed, listedSubscriptions, type NotifiesSentence, notifiesSentence, parseTrigger, sample, type Trigger } from "./inspect.js";
import { McpClient, textOf, type Tool, type ToolResult } from "./mcp.js";
import { clip } from "./redact.js";
import { coreFromEnv, locate } from "./setup.js";
import { Stream, StreamClosed, sessionKeyOf, waitUrlOf } from "./stream.js";

/** Wakeshift's arguments on every wrapped tool. */
export const WAKESHIFT_INPUTS = ["session_key", "harness", "conversation_id"];
/** attach's own tools. */
export const ATTACH_TOOLS = ["subscribe_events", "unsubscribe_events", "list_subscriptions"];

export type Status = "pass" | "fail" | "warn";

export interface Check {
  name: string;
  status: Status;
  detail: string;
  fix?: string;
}

/** A failed check: what went wrong, and what to change. */
class Failed extends Error {
  constructor(
    message: string,
    readonly fix: string,
  ) {
    super(message);
  }
}

/** Whether an app's ID marks it as a development app (verify emits test events to it). */
export const isDevApp = (id: string) => /(^|-)(dev|test)\d*(-|$)/.test(id);

const WRAP_FIX =
  "Wrap the tool with attach's wrapper and subscribe the conversation in its handler: TypeScript attached.events(name, { subscribes: [event] }, handler) and call.subscribe(event, args); Go appkit.Events with appkit.Subscribes; Python @attached.tool(subscribes=[event]).";

interface Options {
  url: string;
  tool: string;
  args: Record<string, unknown>;
  trigger: Trigger;
  headers: Record<string, string>;
  event?: string;
  eventArgs?: Record<string, unknown>;
  user?: string;
  payload?: unknown;
  json: boolean;
  liveApp: boolean;
  timeoutMs: number;
}

function jsonObject(flag: string, v: string | undefined): Record<string, unknown> | undefined {
  if (v === undefined) return undefined;
  let j: unknown;
  try {
    j = JSON.parse(v);
  } catch {
    throw usage(`--${flag} isn't valid JSON`);
  }
  if (!j || typeof j !== "object" || Array.isArray(j)) throw usage(`--${flag} is a JSON object`);
  return j as Record<string, unknown>;
}

export async function verify(ctx: Ctx, argv: string[]): Promise<number> {
  const f = parseFlags(argv, {
    values: ["url", "tool", "args", "trigger", "header-env", "event", "event-args", "user", "payload", "env", "origin", "core", "timeout"],
    booleans: ["json", "live-app"],
  });
  if (f.positionals.length) throw usage(`verify takes no arguments: ${f.positionals[0].slice(0, 40)}`);
  if (f.values.origin !== undefined && f.values.core !== undefined) throw usage("pass --origin or --core, not both");
  const url = f.values.url;
  if (!url) throw usage("verify needs --url, the MCP server's URL (http://localhost:3000/mcp)");
  try {
    const u = new URL(url);
    if (u.protocol !== "http:" && u.protocol !== "https:") throw new Error();
  } catch {
    throw usage("--url is the MCP server's http or https URL");
  }
  if (!f.values.tool) throw usage("verify needs --tool, the tool that subscribes the conversation (watch_build)");
  const trigger = parseTrigger(f.values.trigger);
  if (typeof trigger === "string") throw usage(trigger);
  const headers: Record<string, string> = {};
  const out = new Out(ctx);
  const he = f.values["header-env"];
  if (he !== undefined) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(he)) throw usage("--header-env is the name of an environment variable (TEST_USER_TOKEN), never the token itself");
    const v = (ctx.env[he] ?? "").trim();
    if (!v) throw usage(`--header-env: the environment variable ${he} is empty`);
    out.secret(v);
    headers.authorization = /^bearer\s/i.test(v) ? v : `Bearer ${v}`;
  }
  let timeoutMs = 30_000;
  if (f.values.timeout !== undefined) {
    const s = Number(f.values.timeout);
    if (!Number.isFinite(s) || s <= 0 || s > 600) throw usage("--timeout is a number of seconds, up to 600");
    timeoutMs = s * 1000;
  }
  let payload: unknown;
  if (f.values.payload !== undefined) {
    try {
      payload = JSON.parse(f.values.payload);
    } catch {
      throw usage("--payload isn't valid JSON");
    }
  }
  const o: Options = {
    url,
    tool: f.values.tool,
    args: jsonObject("args", f.values.args) ?? {},
    trigger,
    headers,
    event: f.values.event,
    eventArgs: jsonObject("event-args", f.values["event-args"]),
    user: f.values.user?.trim() || undefined,
    payload,
    json: f.booleans.has("json"),
    liveApp: f.booleans.has("live-app"),
    timeoutMs,
  };
  const w = locate(ctx, f.values.env, f.values.origin ?? f.values.core);
  const core = await coreFromEnv(ctx, out, w);
  const checks = await runChecks(ctx, out, core, o);
  report(out, o, checks);
  return checks.some((c) => c.status === "fail") ? EXIT.FAIL : EXIT.OK;
}

function report(out: Out, o: Options, checks: Check[]): void {
  const ok = !checks.some((c) => c.status === "fail");
  if (o.json) {
    out.line(JSON.stringify({ ok, trigger: o.trigger.kind, checks }));
    return;
  }
  out.line("");
  for (const c of checks) {
    out.line(`  ${c.status.toUpperCase().padEnd(4)}  ${c.name.padEnd(12)} ${c.detail}`);
    if (c.fix) out.line(`        ${" ".repeat(12)} fix: ${c.fix}`);
  }
  out.line("");
  if (!ok) out.line("Failed: fix the first failure above, restart the server, and run verify again.");
  else if (o.trigger.kind === "synthetic")
    out.line("Passed, with a synthetic emit: this tests subscribing, routing and the wake, not your own emit site. --trigger tool:<name> or cmd:<command> tests that too.");
  else out.line(`Passed: ${o.trigger.kind === "tool" ? `the tool ${o.trigger.name}` : "the command"} emitted, and the event woke verify's conversation.`);
}

export async function runChecks(ctx: Ctx, out: Out, core: Core, o: Options): Promise<Check[]> {
  const checks: Check[] = [];
  const check = async <T>(name: string, fn: () => Promise<{ detail: string; value: T; status?: Status; fix?: string }>): Promise<T> => {
    try {
      const r = await fn();
      checks.push({ name, status: r.status ?? "pass", detail: out.clean(r.detail), ...(r.fix ? { fix: out.clean(r.fix) } : {}) });
      return r.value;
    } catch (err) {
      const fix = err instanceof Failed ? err.fix : "";
      const detail = err instanceof Error ? err.message : String(err);
      checks.push({ name, status: "fail", detail: out.clean(clip(detail, 600)), ...(fix ? { fix: out.clean(fix) } : {}) });
      throw new Stop();
    }
  };
  const mcp = new McpClient(o.url, { headers: o.headers, fetch: ctx.fetch });
  let subscribed = false;
  let session: string | null = null;
  let ev: EventDef | null = null;
  let eventArgs: Record<string, unknown> | null = null;
  let tools: Tool[] = [];
  let subscriptionId: string | null = null;
  let stream: Stream | null = null;
  const marker = `wsverify-${randomBytes(4).toString("hex")}`;
  try {
    const app = await check<AppInfo>("app", async () => {
      try {
        const a = await core.app();
        return { detail: `${a.id}${a.name ? ` (${a.name})` : ""}, the key in .env`, value: a };
      } catch (err) {
        if (err instanceof CoreError && err.status === 401) throw new Failed("no app has the key in .env", "Run init to finish creating the app (or keys new), then run verify again.");
        throw new Failed(err instanceof Error ? err.message : String(err), "Check your network, and that WAKESHIFT_CORE (or --origin) is the Wakeshift the key came from.");
      }
    });

    await check("live app", async () => {
      if (isDevApp(app.id)) return { detail: `${app.id} is a development app`, value: null };
      if (o.liveApp) return { detail: `${app.id} doesn't look like a development app; going ahead (--live-app), with the status guard`, value: null, status: "warn" };
      throw new Failed(
        `${app.id} doesn't look like a development app (its ID doesn't end in -dev or -test), and verify sends test events`,
        "Run verify against a development app (init makes <project>-dev), or pass --live-app to run it here anyway: the status guard still keeps it from waking anyone else.",
      );
    });

    const tool = await check<Tool>("tool", async () => {
      try {
        await mcp.connect();
        tools = await mcp.listTools();
      } catch (err) {
        throw new Failed(err instanceof Error ? err.message : String(err), "Start the server with .env loaded, and check --url (the MCP endpoint, often /mcp). A server with its own sign-in needs --header-env.");
      }
      const t = tools.find((x) => x.name === o.tool);
      if (!t) {
        const names = tools.map((x) => x.name);
        throw new Failed(`the server lists no tool ${o.tool} (it lists ${names.slice(0, 12).join(", ") || "none"}${names.length > 12 ? ", …" : ""})`, "Pass --tool with the tool that subscribes the conversation, and check the server is the one you changed.");
      }
      const missing = WAKESHIFT_INPUTS.filter((k) => !(t.inputSchema?.properties && k in t.inputSchema.properties));
      if (missing.length) throw new Failed(`${o.tool} lacks Wakeshift's inputs (${missing.join(", ")})`, WRAP_FIX);
      return { detail: `${o.tool} is listed, with Wakeshift's inputs`, value: t };
    });

    const sentence = await check<NotifiesSentence>("description", async () => {
      const s = notifiesSentence(tool.description ?? "");
      if (!s) {
        throw new Failed(
          `${o.tool}'s description doesn't say it notifies: agents set Wakeshift's part of its reply aside without that sentence`,
          "Declare the events the tool subscribes to (TypeScript attached.events(name, { subscribes: [event] }, handler); Go appkit.Subscribes; Python @attached.tool(subscribes=[event])): tools/list then adds the sentence.",
        );
      }
      if (!s.descriptions.length) return { detail: "says it notifies (notifies' own wording); declaring subscribes writes it from the events' descriptions", value: s, status: "warn" };
      return { detail: `says it notifies of ${s.descriptions.length === 1 ? "an event" : `${s.descriptions.length} events`} (subscribes)`, value: s };
    });

    if (!tools.some((t) => ATTACH_TOOLS.includes(t.name))) {
      checks.push({ name: "attach tools", status: "warn", detail: `the server lists none of attach's tools (${ATTACH_TOOLS.join(", ")})`, fix: "Keep attach's default tools on so agents can list and stop their subscriptions (they also let verify find its exact subscription)." });
    }

    const lister = tools.some((t) => t.name === "list_subscriptions");
    const listed = async (): Promise<ReturnType<typeof listedSubscriptions>> => {
      if (!lister) return [];
      try {
        const r = await mcp.callTool("list_subscriptions", session ? { session_key: session } : {});
        return r.isError ? [] : listedSubscriptions(textOf(r));
      } catch {
        return [];
      }
    };
    const before = new Set((await listed()).map((s) => s.id));

    // Who is subscribed before verify is, where it can already tell the
    // event and arguments: a count of one afterwards is then verify's own.
    // (Otherwise only list_subscriptions, naming verify's new subscription,
    // can show the one subscriber is verify.)
    let pre: { key: string; count: number } | null = null;
    try {
      const m = await core.manifest();
      const events = Array.isArray(m.events) ? m.events : [];
      const named = o.event ? events.filter((e) => e.name === o.event) : eventsNamed(sentence, events);
      const e = named.length === 1 ? named[0] : !o.event && !sentence.descriptions.length && events.length === 1 ? events[0] : null;
      const name = e?.name ?? o.event;
      let a: Record<string, unknown> | null = o.eventArgs ?? null;
      if (!a && e) {
        const { picked, missing } = argsForEvent(e, o.args);
        if (!missing.length) a = picked;
      }
      if (name && a) {
        let count = 0;
        try {
          count = (await core.eventStatus(name, a, e?.personal ? o.user : undefined)).subscriptions;
        } catch (err) {
          // An event Wakeshift doesn't know yet has no subscribers.
          if (!(err instanceof CoreError && (err.code === "unknown_event" || err.status === 404))) throw err;
        }
        pre = { key: `${name} ${canonical(a)}`, count };
      }
    } catch {
      pre = null;
    }

    await check("subscribe", async () => {
      let r: ToolResult;
      try {
        r = await mcp.callTool(o.tool, { ...o.args });
      } catch (err) {
        throw new Failed(err instanceof Error ? err.message : String(err), "Check --args against the tool's input schema.");
      }
      const text = textOf(r);
      if (r.isError) throw new Failed(`${o.tool} answered with an error: ${clip(text, 300)}`, "Fix the call (--args) or the tool, so it succeeds for a test user.");
      session = sessionKeyOf(text);
      const wait = waitUrlOf(text);
      if (!wait) {
        if (/Wakeshift/.test(text)) throw new Failed("the reply has Wakeshift's block, but no command to wait with", "Check the tool subscribes the calling conversation (call.subscribe) rather than relaying another result.");
        throw new Failed(`${o.tool}'s reply has no Wakeshift block, so it didn't subscribe the conversation`, WRAP_FIX);
      }
      subscribed = true;
      stream = new Stream(wait, ctx.fetch);
      return { detail: "the reply carries Wakeshift's block; holding the conversation's stream", value: null };
    });

    ev = await check<EventDef>("event", async () => {
      let events: EventDef[];
      try {
        const m = await core.manifest();
        events = Array.isArray(m.events) ? m.events : [];
      } catch (err) {
        throw new Failed(err instanceof Error ? err.message : String(err), "Try again.");
      }
      const names = events.map((e) => e.name).join(", ") || "none";
      if (o.event) {
        const e = events.find((x) => x.name === o.event);
        if (!e) throw new Failed(`Wakeshift has no event ${o.event} for ${app.id} (it has ${names})`, "Define the event at startup (defineEvent) and attach; the SDK sends it on the server's first call about events. Check the server runs with the WAKESHIFT_KEY in .env.");
        return { detail: `Wakeshift has ${e.name}`, value: e };
      }
      const named = eventsNamed(sentence, events);
      if (named.length === 1) return { detail: `Wakeshift has ${named[0].name}, the event ${o.tool} declares`, value: named[0] };
      if (named.length > 1) throw new Failed(`${o.tool} declares ${named.map((e) => e.name).join(", ")}`, "Pass --event with the one to verify.");
      if (sentence.descriptions.length) {
        throw new Failed(`Wakeshift has no event with the description ${o.tool}'s sentence names (it has ${names})`, "Check the server runs with the WAKESHIFT_KEY in .env, so the app it sends its events to is this one.");
      }
      if (events.length === 1) return { detail: `Wakeshift has ${events[0].name}, the app's one event`, value: events[0] };
      throw new Failed(`can't tell which event ${o.tool} subscribes to (Wakeshift has ${names})`, events.length ? "Pass --event with its name, or declare subscribes on the tool." : "Define the event at startup (defineEvent) and attach: the SDK sends it on the first call about events.");
    });

    eventArgs = await check<Record<string, unknown>>("arguments", async () => {
      const e = ev!;
      if (o.eventArgs) return { detail: `${e.name} ${canonical(o.eventArgs)} (--event-args)`, value: o.eventArgs };
      const fresh = (await listed()).filter((s) => s.event === e.name && !before.has(s.id));
      if (fresh.length === 1) {
        subscriptionId = fresh[0].id;
        return { detail: `${e.name} ${canonical(fresh[0].arguments)} (from list_subscriptions)`, value: fresh[0].arguments };
      }
      const { picked, missing } = argsForEvent(e, o.args);
      if (missing.length) {
        throw new Failed(`can't tell the arguments ${o.tool} subscribed with: ${e.name} needs ${missing.join(", ")}, which --args doesn't give`, `Pass --event-args with the arguments the tool subscribes with, e.g. --event-args '${canonical(Object.fromEntries(missing.map((m) => [m, "…"])))}'.`);
      }
      return { detail: `${e.name} ${canonical(picked)} (from --args)`, value: picked };
    });

    // The guard: nobody but verify's own conversation may be subscribed
    // with these arguments before anything is emitted.
    await check("guard", async () => {
      const e = ev!;
      if (e.personal && !o.user) throw new Failed(`${e.name} is personal, so its status and emit name the subscriber`, "Pass --user with the app's ID for the test user verify signs in as.");
      let st: { subscriptions: number };
      try {
        st = await core.eventStatus(e.name, eventArgs!, e.personal ? o.user : undefined);
      } catch (err) {
        throw new Failed(err instanceof Error ? err.message : String(err), "Check --event-args (or --args) against the event's arguments.");
      }
      const others = () =>
        new Failed(
          `Other conversations are subscribed with these arguments (${st.subscriptions} in all), so verify won't emit: it would wake them`,
          `Use arguments only verify uses: put wsverify- and a random suffix in a free-text routing value, e.g. --args '{"…":"${marker}"}'.`,
        );
      if (st.subscriptions === 1) {
        // One subscriber: verify's own only if verify can show it.
        if (subscriptionId) return { detail: `only verify's conversation is subscribed to ${e.name} ${canonical(eventArgs)}`, value: null };
        if (pre && pre.key === `${e.name} ${canonical(eventArgs)}`) {
          if (pre.count === 0) return { detail: `only verify's conversation is subscribed to ${e.name} ${canonical(eventArgs)} (nobody was before it)`, value: null };
          throw others();
        }
        throw new Failed(
          `one conversation is subscribed to ${e.name} ${canonical(eventArgs)}, and verify can't tell it's its own, so it won't emit`,
          "Keep attach's list_subscriptions tool on, or pass --event and --event-args with exactly the arguments the tool subscribes with, so verify can check nobody is subscribed before it.",
        );
      }
      if (st.subscriptions === 0) {
        throw new Failed(
          `Wakeshift counts no subscription to ${e.name} ${canonical(eventArgs)}, not even verify's, so verify can't tell who an emit would reach`,
          "Pass --event-args with exactly the arguments the tool subscribes with, and check the server runs with the WAKESHIFT_KEY in .env.",
        );
      }
      throw others();
    });
    // What's left between this check and the emit is a window of
    // milliseconds: arguments only verify uses close it.

    await check("trigger", async () => {
      const e = ev!;
      const t = o.trigger;
      if (t.kind === "synthetic") {
        const body = {
          name: e.name,
          arguments: eventArgs!,
          payload: o.payload !== undefined ? o.payload : sample(e.payload, marker),
          text: `Wakeshift verify: a test ${e.name} (${marker}).`,
          id: marker,
          ...(e.personal ? { to: subscriptionId ? [{ user: o.user!, subscription: subscriptionId }] : o.user! } : {}),
        };
        let r: { matched: number };
        try {
          r = await core.emit(body);
        } catch (err) {
          throw new Failed(`the synthetic emit failed: ${err instanceof Error ? err.message : String(err)}`, `Pass --payload with a payload that fits ${e.name}'s schema, or --trigger to emit from your own code.`);
        }
        if (!r.matched) throw new Failed("the synthetic emit reached no subscription", "Check --event-args are exactly the arguments the tool subscribes with.");
        return { detail: `a synthetic ${e.name} (${marker}), sent by verify through the app API`, value: null };
      }
      if (t.kind === "tool") {
        if (!tools.some((x) => x.name === t.name)) throw new Failed(`the server lists no tool ${t.name}`, "Pass --trigger tool:<name> with a tool that makes the event happen.");
        let r: ToolResult;
        try {
          r = await mcp.callTool(t.name, { ...t.args });
        } catch (err) {
          throw new Failed(err instanceof Error ? err.message : String(err), "Check the trigger's arguments against the tool's input schema.");
        }
        if (r.isError) throw new Failed(`${t.name} answered with an error: ${clip(textOf(r), 300)}`, "Fix the trigger's arguments, or the tool.");
        return { detail: `called ${t.name}`, value: null };
      }
      const r = await ctx.run(t.command, [], { shell: true, cwd: ctx.cwd, timeoutMs: 60_000 });
      if (r.code !== 0) {
        const said = clip(`${r.stderr}\n${r.stdout}`.trim(), 300);
        throw new Failed(`the trigger command ${r.error ? `couldn't run (${r.error})` : `exited ${r.code}`}${said ? `: ${said}` : ""}`, "Fix the command so it makes the event happen (a request to your webhook route, a script), and exits 0.");
      }
      return { detail: "ran the trigger command", value: null };
    });

    await check("wake", async () => {
      const e = ev!;
      const started = Date.now();
      const match = o.trigger.kind === "synthetic" ? (x: object) => JSON.stringify(x).includes(marker) : () => true;
      let got;
      try {
        got = await stream!.next(started + o.timeoutMs, match);
      } catch (err) {
        if (err instanceof StreamClosed) throw new Failed(err.message, "Run verify again; if it keeps closing, check the server isn't relaying another conversation's result.");
        throw err;
      }
      if (!got) {
        const secs = Math.round(o.timeoutMs / 1000);
        if (o.trigger.kind === "synthetic") throw new Failed(`the synthetic ${e.name} didn't reach verify's conversation within ${secs}s`, "Run verify again; if it fails again, report it to Wakeshift with the app's ID.");
        throw new Failed(
          `no ${e.name} reached verify's conversation within ${secs}s of the trigger`,
          `Check the trigger makes ${e.name} happen, and the code emits it with the same arguments the tool subscribes with (${canonical(eventArgs)}), after the change it reports is saved. --timeout waits longer.`,
        );
      }
      await stream!.confirm();
      const secs = ((Date.now() - started) / 1000).toFixed(1);
      const hasText = !!(got.subject || got.body || got.data || got.data_preview);
      return hasText
        ? { detail: `${e.name} arrived in ${secs}s and was acknowledged`, value: null }
        : { detail: `${e.name} arrived in ${secs}s, with no text or payload`, value: null, status: "warn", fix: "Give the event a text function, so agents read what happened." };
    });
  } catch (err) {
    if (!(err instanceof Stop)) throw err;
  } finally {
    if (subscribed) await cleanup(core, mcp, o, checks, session, ev, eventArgs, tools, subscriptionId);
    await mcp.close();
  }
  return checks;
}

class Stop extends Error {}

/** Ends verify's subscription, through the server's tools where it has them. */
async function cleanup(core: Core, mcp: McpClient, o: Options, checks: Check[], session: string | null, ev: EventDef | null, eventArgs: Record<string, unknown> | null, tools: Tool[], subscriptionId: string | null): Promise<void> {
  const has = (n: string) => tools.some((t) => t.name === n);
  const sk = session ? { session_key: session } : {};
  let how = "";
  try {
    if (ev && eventArgs && has("unsubscribe_events")) {
      const r = await mcp.callTool("unsubscribe_events", { event: ev.name, arguments: eventArgs, ...sk });
      if (!r.isError && /^Stopped/.test(textOf(r))) how = "unsubscribe_events";
    }
    const un = o.tool.startsWith("watch") ? "un" + o.tool : "";
    if (!how && un && has(un)) {
      const r = await mcp.callTool(un, { ...o.args, ...sk });
      if (!r.isError) how = un;
    }
    if (!how && ev?.personal && o.user && subscriptionId) {
      const r = await core.endSubscription(o.user, subscriptionId);
      if (r.ended) how = "the app API";
    }
  } catch {
    // reported below
  }
  if (ev && eventArgs) {
    try {
      const st = await core.eventStatus(ev.name, eventArgs, ev.personal ? o.user : undefined);
      if (how && st.subscriptions === 0) {
        checks.push({ name: "cleanup", status: "pass", detail: `unsubscribed (${how})` });
        return;
      }
    } catch {
      // reported below
    }
  }
  checks.push({
    name: "cleanup",
    status: "warn",
    detail: how ? `asked ${how} to unsubscribe, but a subscription may be left` : "couldn't unsubscribe verify's conversation; its subscription ends when it expires",
    fix: "Keep attach's unsubscribe_events tool on (or name an un<tool> next to your watch tool) so verify can clean up.",
  });
}
