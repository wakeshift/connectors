// The OpenClaw plugin entry: wires plugin.ts to OpenClaw's plugin API
// (2026.10.4). Only Node built-ins; no OpenClaw imports, so the same file
// loads in any OpenClaw that has these APIs.

import { join } from "node:path";
import { homedir } from "node:os";
import { Wakeshift, hasBeaconLine, stripBeacon, type Host, type Options } from "./plugin.js";
import { Store } from "./store.js";

/* eslint-disable @typescript-eslint/no-explicit-any */
type Api = any;

interface Scheduler {
  schedule(job: { id: string; delayMs?: number; run: () => Promise<unknown> }): unknown;
  /** Aborts when the scheduler closes (the plugin is stopped or replaced). */
  signal?: AbortSignal;
}

const ID = "wakeshift";

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
  const host: Host = {
    enqueueSystemEvent: (text, opts) => api.runtime.system.enqueueSystemEvent(text, opts),
    runHeartbeatOnce: (opts) => api.runtime.system.runHeartbeatOnce(opts),
    // Work that starts turns must run inside the gateway's admission: a
    // scheduler job of the plugin's service.
    // A job settles when its work does, or at once when the scheduler
    // closes: a stop or a replacement (a reinstall) never waits on a turn
    // the job started.
    run: (fn) =>
      new Promise((resolve, reject) => {
        const s = scheduler;
        if (!s || s.signal?.aborted) return reject(new Error("the Wakeshift service isn't running"));
        s.schedule({ id: `wake-${++n}`, delayMs: 0, run: () => settleOrAbort(fn(), s.signal).then(resolve, reject) });
      }),
    log,
  };
  const ws = new Wakeshift({ issuer, store: new Store(stateDir(api)), host, target: api.pluginConfig?.heartbeatTarget, ...overrides });

  api.registerService({
    id: "wakeshift-streams",
    apiVersion: 2,
    start(ctx: { scheduler?: Scheduler }) {
      scheduler = ctx?.scheduler;
      ws.start();
    },
    stop() {
      ws.stop();
      scheduler = undefined;
    },
  });

  // A note for the session's next turn, as a system event, queued in the
  // gateway's scheduler like every other runtime call.
  const noteLater = (note: string, sessionKey: string, agentId?: string) =>
    void host
      .run(async () => host.enqueueSystemEvent(note, { sessionKey, ...(agentId ? { agentId } : {}) }))
      .catch((e) => log.warn(`wakeshift: couldn't queue the note: ${(e as Error).message}`));

  // OpenClaw's runtime: the result the model sees, before it does. Takes the
  // beacon line out and says "Wakeshift: connected". (With Codex running
  // the turn, Codex has already shown the model its result: _meta, which
  // Codex keeps from it, carries the beacon there, and the note comes at
  // the next turn.)
  const middleware = async (event: any, ctx: any) => {
    const result = event?.result;
    const sessionKey = ctx?.sessionKey;
    if (!result || !sessionKey) return undefined;
    const note = await ws.onResult(result, { sessionKey, agentId: ctx?.agentId, toolCallId: event?.toolCallId, toolName: event?.toolName });
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
    void ws.onResult(event.result, { sessionKey, agentId: ctx?.agentId, toolCallId: event?.toolCallId ?? ctx?.toolCallId, toolName: event?.toolName }).then((note) => {
      if (note && !filtering) noteLater(note, sessionKey, ctx?.agentId);
    });
  });
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
