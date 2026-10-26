// The OpenClaw plugin entry: wires plugin.ts to OpenClaw's plugin API
// (2026.10.4). Only Node built-ins; no OpenClaw imports, so the same file
// loads in any OpenClaw that has these APIs.

import { existsSync } from "node:fs";
import { join } from "node:path";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";
import { HooksControl } from "./control.js";
import { bindMode, listensBeyondLoopback, type HooksConfig } from "./hooks.js";
import { Wakeshift, hasBeaconLine, stripBeacon, type Host, type Options } from "./plugin.js";
import { Store } from "./store.js";
import { hooksPath, wakeBody, wakeResult, type Route, type WakeRequest, type WakeResult } from "./wake.js";

/* eslint-disable @typescript-eslint/no-explicit-any */
type Api = any;

interface Scheduler {
  schedule(job: { id: string; delayMs?: number; run: () => Promise<unknown> }): unknown;
  /** Aborts when the scheduler closes (the plugin is stopped or replaced). */
  signal?: AbortSignal;
}

const ID = "wakeshift";

/** How long a wake request may wait for its turn to finish. */
export const WAKE_TIMEOUT = 15 * 60 * 1000;

function stateDir(api: Api): string {
  try {
    const d = api.runtime?.state?.resolveStateDir?.(process.env);
    if (typeof d === "string" && d) return join(d, ID);
  } catch {
    /* fall back */
  }
  return join(process.env.OPENCLAW_STATE_DIR || join(homedir(), ".openclaw"), ID);
}

// register never throws into the gateway: a plugin that can't start (its
// setting missing, its state directory unwritable, an OpenClaw without an
// API it needs) logs why and does nothing. OpenClaw registers it again when
// its setting changes.
export function register(api: Api, overrides: Partial<Options> = {}) {
  const log = {
    info: (m: string) => api?.logger?.info?.(m),
    warn: (m: string) => api?.logger?.warn?.(m),
  };
  try {
    start(api, log, overrides);
  } catch (e) {
    log.warn(`wakeshift: couldn't start (${(e as Error)?.message ?? e}); the plugin does nothing until it's installed again`);
  }
}

/** The gateway's config now (the process snapshot). */
const current = (api: Api): { hooks?: HooksConfig; gateway?: Record<string, unknown> } => {
  try {
    return api.runtime.config.current() ?? {};
  } catch {
    return {};
  }
};

/**
 * One wake request to this gateway's own hooks endpoint, on loopback (its
 * --port, which the gateway puts in OPENCLAW_GATEWAY_PORT, or gateway.port).
 * A gateway bound to one other address (custom) is asked there.
 */
export async function wakeOnce(api: Api, f: typeof fetch, r: WakeRequest, timeout = WAKE_TIMEOUT): Promise<WakeResult> {
  const cfg = current(api);
  const hooks = cfg.hooks ?? {};
  if (hooks.enabled !== true || typeof hooks.token !== "string" || !hooks.token) {
    return { kind: "broken", why: "OpenClaw's hooks are off here (hooks.enabled), so a message can't start a turn" };
  }
  const port = Number(process.env.OPENCLAW_GATEWAY_PORT) || Number(cfg.gateway?.port) || 18789;
  const custom = cfg.gateway?.bind === "custom" && typeof cfg.gateway?.customBindHost === "string" ? String(cfg.gateway.customBindHost).trim() : "";
  const hosts = ["127.0.0.1", ...(custom && custom !== "127.0.0.1" ? [custom.includes(":") ? `[${custom}]` : custom] : [])];
  const init = {
    method: "POST",
    headers: { Authorization: `Bearer ${hooks.token}`, "Content-Type": "application/json", "Idempotency-Key": r.idempotencyKey },
    body: JSON.stringify(wakeBody(r)),
    redirect: "error" as const, // the token goes only to this gateway
  };
  for (const host of hosts) {
    let res: Response;
    try {
      res = await f(`http://${host}:${port}${hooksPath(hooks.path)}/agent`, { ...init, signal: AbortSignal.timeout(timeout) });
    } catch (e) {
      if ((e as Error)?.name === "TimeoutError") return { kind: "retry", fresh: false, why: "the woken turn didn't finish in time" };
      continue; // not listening there: the next address, or unreachable
    }
    const text = await res.text().catch(() => "");
    let body: unknown = text;
    try {
      body = JSON.parse(text);
    } catch {
      /* plain text */
    }
    return wakeResult(res.status, body, !!r.route);
  }
  return wakeResult(0, undefined, !!r.route);
}

function start(api: Api, log: { info: (m: string) => void; warn: (m: string) => void }, overrides: Partial<Options>) {
  const issuer = typeof api.pluginConfig?.issuer === "string" ? api.pluginConfig.issuer.trim().replace(/\/$/, "") : "";
  if (!issuer) {
    log.warn(
      "wakeshift: no issuer set, so the plugin does nothing until it is: run Wakeshift's installer for OpenClaw again (it sets plugins.entries.wakeshift.config.issuer)",
    );
    return;
  }
  let scheduler: Scheduler | undefined;
  let n = 0;
  // Runtime calls run inside the gateway's admission: a scheduler job of the
  // plugin's service. A job settles when its work does, or at once when the
  // scheduler closes: a stop or a replacement (a reinstall) never waits on
  // work the job started.
  const run = <T>(fn: () => Promise<T>) =>
    new Promise<T>((resolve, reject) => {
      const s = scheduler;
      if (!s || s.signal?.aborted) return reject(new Error("the Wakeshift service isn't running"));
      s.schedule({ id: `wake-${++n}`, delayMs: 0, run: () => settleOrAbort(fn(), s.signal).then(resolve, reject) });
    });
  const host: Host = { wake: (r) => wakeOnce(api, overrides.fetch ?? fetch, r), log };
  const store = new Store(stateDir(api));
  const hooks = new HooksControl({
    dir: store.dir,
    log,
    allowNetwork: api.pluginConfig?.allowNetworkHooks === true,
    network: () => {
      const gw = current(api).gateway;
      return listensBeyondLoopback(bindMode(process.argv, gw, existsSync("/.dockerenv")), gw?.customBindHost);
    },
    restoreScript: fileURLToPath(new URL("./hooks.js", import.meta.url)),
    config: {
      hooks: () => current(api).hooks,
      mutateHooks: (fn) =>
        run(() =>
          api.runtime.config.mutateConfigFile({
            mutate: (draft: Record<string, unknown>) => {
              const next = fn(draft.hooks as HooksConfig | undefined);
              if (next === undefined) delete draft.hooks;
              else draft.hooks = next;
            },
            afterWrite: { mode: "auto" },
          }),
        ).then(() => {}),
    },
  });
  const wake = new Wakeshift({ issuer, store, host, hooks, ...overrides });

  api.registerService({
    id: "wakeshift-streams",
    apiVersion: 2,
    start(ctx: { scheduler?: Scheduler }) {
      scheduler = ctx?.scheduler;
      wake.start();
    },
    stop() {
      wake.stop();
      scheduler = undefined;
    },
  });

  // A note for the session's next turn: a short, trusted notification of
  // Wakeshift's own, queued in the gateway's scheduler like every other
  // runtime call.
  const noteLater = (note: string, sessionKey: string, agentId?: string) =>
    void run(async () => api.runtime.system.enqueueSystemEvent(note, { sessionKey, ...(agentId ? { agentId } : {}) })).catch((e) =>
      log.warn(`wakeshift: couldn't queue the note: ${(e as Error).message}`),
    );

  // The channel each session last spoke on, for a woken turn's reply.
  try {
    api.on("message_received", (_event: any, ctx: any) => {
      const sessionKey = ctx?.sessionKey;
      if (typeof sessionKey !== "string" || !sessionKey) return;
      wake.onMessage(sessionKey, routeOf(ctx));
    });
  } catch (e) {
    log.warn(`wakeshift: no message_received hook (${(e as Error).message}); woken turns' replies stay in their session`);
  }

  // OpenClaw's runtime: the result the model sees, before it does. Takes the
  // beacon line out and says "Wakeshift: connected" (or why a message
  // couldn't wake the session). With Codex running the turn, Codex has
  // already shown the model its result: _meta, which Codex keeps from it,
  // carries the beacon there, and the note comes at the next turn.
  const middleware = async (event: any, ctx: any) => {
    const result = event?.result;
    const sessionKey = ctx?.sessionKey;
    if (!result || !sessionKey) return undefined;
    const note = (await wake.onResult(result, { sessionKey, agentId: ctx?.agentId, toolCallId: event?.toolCallId, toolName: event?.toolName })) || wake.takeNote(sessionKey);
    if (ctx?.runtime === "codex") {
      if (note) noteLater(note, sessionKey, ctx?.agentId);
      return undefined;
    }
    if (!note && !hasBeaconLine(result)) return undefined;
    const stripped = stripBeacon(result);
    if (note) stripped.content = [...(Array.isArray(stripped.content) ? stripped.content : []), { type: "text", text: note }];
    return { result: stripped };
  };
  let filtering = false;
  try {
    api.registerAgentToolResultMiddleware(middleware, { runtimes: ["openclaw", "codex"] });
    filtering = true;
  } catch (e) {
    log.warn(`wakeshift: no tool-result filter (${(e as Error).message}); beacons stay in what the model sees`);
  }
  // Every tool's result, as a fallback (and for the call inside OpenClaw's
  // code mode, which the filter sees only as part of the exec result).
  api.on("after_tool_call", (event: any, ctx: any) => {
    const sessionKey = ctx?.sessionKey;
    if (!event?.result || !sessionKey) return;
    void wake.onResult(event.result, { sessionKey, agentId: ctx?.agentId, toolCallId: event?.toolCallId ?? ctx?.toolCallId, toolName: event?.toolName }).then((note) => {
      if (note && !filtering) noteLater(note, sessionKey, ctx?.agentId);
    });
  });
}

// Channels that are OpenClaw's own surfaces, not a channel a reply can be
// sent to: a session there keeps its reply in its transcript.
const LOCAL_CHANNELS = new Set(["webchat", "tui", "cli", "control-ui", "internal", "gateway"]);

/** A message's route (message_received's context), when it came from a channel. */
export function routeOf(ctx: { channelId?: unknown; conversationId?: unknown; accountId?: unknown }): Route | undefined {
  const channel = typeof ctx.channelId === "string" ? ctx.channelId.trim() : "";
  const to = typeof ctx.conversationId === "string" ? ctx.conversationId.trim() : "";
  if (!channel || !to || LOCAL_CHANNELS.has(channel.toLowerCase())) return undefined;
  const accountId = typeof ctx.accountId === "string" && ctx.accountId.trim() ? ctx.accountId.trim() : undefined;
  return { channel, to, ...(accountId ? { accountId } : {}) };
}

// p, or a rejection once signal aborts (whichever is first).
export function settleOrAbort<T>(p: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return p;
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(new Error("the Wakeshift service stopped"));
    if (signal.aborted) return onAbort();
    signal.addEventListener("abort", onAbort, { once: true });
    p.then(resolve, reject).finally(() => signal.removeEventListener("abort", onAbort));
  });
}

export default {
  id: ID,
  name: "Wakeshift",
  description: "Wakes OpenClaw sessions when an app they watch through Wakeshift sends something.",
  register,
};
