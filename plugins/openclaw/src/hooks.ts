// OpenClaw's HTTP hooks (hooks.* in openclaw.json), as Wakeshift's plugin
// uses them to wake a conversation: POST <hooks.path>/agent on the gateway's
// own listener, persistent, into the conversation's session. OpenClaw
// 2026.10.4 safety-wraps that message as outside content; it's the path
// OpenClaw supports for untrusted input (enqueueSystemEvent and /hooks/wake
// are for short trusted notifications, and the plugin runtime's
// runHeartbeatOnce drops a heartbeat prompt).
//
// What the plugin changes in hooks.*, and only that, is recorded so it can
// be undone (<state dir>/hooks.json):
//
//   - hooks off (the usual case): the plugin turns them on with a token of
//     its own, lets a caller name the session (allowRequestSessionKey),
//     gives requests without one a session of its own (defaultSessionKey
//     hook:wakeshift, which OpenClaw requires the prefix list to admit), and
//     allows no agent and no session beyond that until a conversation
//     connects (allowedAgentIds [], allowedSessionKeyPrefixes
//     ["hook:wakeshift"]).
//   - hooks on already (the user's own): the plugin changes nothing it
//     would have to loosen. It goes on only if they already let a caller
//     name the session from a restricted prefix list; otherwise it says what
//     it would need, and leaves them alone.
//   - each connected conversation: its exact session key is added to the
//     prefix list (unless a prefix there admits it already), and its agent
//     to the agent list (unless that list allows every agent); both are
//     removed when the conversation goes, if the plugin added them.
//
// Undoing (restorePatch) removes only what the plugin added, and puts back
// a setting it changed only while that setting still holds the plugin's
// value: what the user changed since is theirs.
//
// Node built-ins only: dist/hooks.js is also copied to the state directory
// as restore.mjs, which Wakeshift's uninstaller runs once the plugin is gone.

import { readFileSync, realpathSync } from "node:fs";
import { fileURLToPath } from "node:url";

export interface HooksConfig {
  enabled?: boolean;
  token?: string;
  path?: string;
  allowRequestSessionKey?: boolean;
  defaultSessionKey?: string;
  allowedSessionKeyPrefixes?: string[];
  allowedAgentIds?: string[];
  [k: string]: unknown;
}

/** The session a request without one goes to, when the plugin sets it. */
export const DEFAULT_KEY = "hook:wakeshift";

type Scalar = "enabled" | "token" | "allowRequestSessionKey" | "defaultSessionKey";
type List = "allowedSessionKeyPrefixes" | "allowedAgentIds";

/** One setting the plugin set: what was there before (had: false, nothing). */
export interface Prior {
  had: boolean;
  prior?: unknown;
  /** The plugin's value (never the token's). */
  ours?: unknown;
}

/** What the plugin changed in hooks.*: <state dir>/hooks.json. */
export interface Changes {
  version: 1;
  /** Hooks were off, and the plugin turned them on. */
  owned: boolean;
  set: Partial<Record<Scalar | List, Prior>>;
  /** Entries the plugin added to the lists, and only those. */
  added: { allowedSessionKeyPrefixes: string[]; allowedAgentIds: string[] };
}

export type Takeover = { ok: true; next: HooksConfig; changes: Changes } | { ok: false; why: string };

const blank = (s: unknown) => typeof s !== "string" || !s.trim();
const lower = (s: string) => s.toLowerCase();
const list = (v: unknown): string[] | undefined => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : undefined);
/** A prefix list that restricts something (an empty or all-blank one allows every key). */
const restricting = (v: unknown) => (list(v) ?? []).some((p) => !blank(p));
/** Whether a prefix in the list admits the key (case-insensitive, as OpenClaw checks). */
export const admits = (prefixes: string[] | undefined, key: string) => (prefixes ?? []).some((p) => !blank(p) && lower(key).startsWith(lower(p.trim())));
/** Whether the agent list allows every agent (absent, or with "*"). */
const everyAgent = (v: unknown) => list(v) === undefined || list(v)!.includes("*");

/** The agent a session key names ("agent:<id>:…"), if it names one. */
export function agentOf(sessionKey: string): string | undefined {
  const m = /^agent:([^:]+):/i.exec(sessionKey);
  return m ? m[1] : undefined;
}

/**
 * What turning on Wakeshift's wake path changes in hooks.* (cur: the
 * current hooks.*, token: a fresh random one, used only when hooks have
 * none). network: the gateway listens beyond loopback; allowNetwork: the
 * user agreed to hooks there.
 */
export function takeover(cur: HooksConfig | undefined, token: string, o: { network: boolean; allowNetwork: boolean }): Takeover {
  const c: HooksConfig = { ...(cur ?? {}) };
  const changes: Changes = { version: 1, owned: false, set: {}, added: { allowedSessionKeyPrefixes: [], allowedAgentIds: [] } };
  if (c.enabled === true) {
    // The user's own hooks: nothing to loosen, so only what's additive.
    if (blank(c.token)) return { ok: false, why: "OpenClaw's hooks are on here without a token (hooks.token)" };
    if (c.allowRequestSessionKey !== true) {
      return {
        ok: false,
        why: "OpenClaw's hooks are on here and don't let a caller name the session (hooks.allowRequestSessionKey is false). Wakeshift needs that to wake this conversation, and turning it on would loosen settings you chose, so it left them alone. To allow it, set hooks.allowRequestSessionKey to true, keeping hooks.allowedSessionKeyPrefixes restricted, then call the app's tool again",
      };
    }
    if (!restricting(c.allowedSessionKeyPrefixes)) {
      return {
        ok: false,
        why: "OpenClaw's hooks are on here with no hooks.allowedSessionKeyPrefixes, so any session is allowed. Waking a conversation needs a restricted list, and adding one would restrict your own hooks, so Wakeshift left them alone. To allow it, set hooks.allowedSessionKeyPrefixes to the prefixes your hooks use, then call the app's tool again",
      };
    }
    return { ok: true, next: c, changes };
  }
  if (o.network && !o.allowNetwork) return { ok: false, why: NETWORK };
  changes.owned = true;
  // What's recorded is the value set, except a secret's, which isn't.
  const set = <K extends Scalar | List>(k: K, v: HooksConfig[K], secret = false) => {
    changes.set[k] = secret ? { had: k in c } : { had: k in c, prior: c[k], ours: v };
    c[k] = v;
  };
  set("enabled", true);
  // A token the user set already (hooks off) stays; otherwise ours (never
  // recorded, nor the one it replaces).
  if (blank(c.token)) set("token", token, true);
  if (c.allowRequestSessionKey !== true) set("allowRequestSessionKey", true);
  if (blank(c.defaultSessionKey)) set("defaultSessionKey", DEFAULT_KEY);
  const prefixes = list(c.allowedSessionKeyPrefixes)?.filter((p) => !blank(p));
  const def = c.defaultSessionKey as string;
  if (!prefixes?.length) {
    // Absent or empty (no restriction): the plugin's list, with only the
    // default key, which OpenClaw requires the list to admit.
    set("allowedSessionKeyPrefixes", [def]);
    changes.added.allowedSessionKeyPrefixes.push(def);
  } else if (!admits(prefixes, def)) {
    c.allowedSessionKeyPrefixes = [...prefixes, def];
    changes.added.allowedSessionKeyPrefixes.push(def);
  }
  // Every agent allowed (absent): none, until a conversation connects.
  if (list(c.allowedAgentIds) === undefined) set("allowedAgentIds", []);
  return { ok: true, next: c, changes };
}

export const NETWORK =
  "this OpenClaw gateway listens beyond this computer (its bind isn't loopback), and OpenClaw's hooks share its listener, so turning them on would make the hooks endpoint reachable there too, guarded by a token. Ask the user whether that's all right; only if they agree, run Wakeshift's installer for OpenClaw again with WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1, and suggest a firewall rule that blocks /hooks/ from other machines";

/** hooks.* letting this conversation be woken, or null when it already does. */
export function addScope(cur: HooksConfig, changes: Changes, sessionKey: string, agentId?: string): { next: HooksConfig; changes: Changes } | null {
  const c: HooksConfig = { ...cur };
  const ch: Changes = structuredClone(changes);
  let changed = false;
  const prefixes = list(c.allowedSessionKeyPrefixes) ?? [];
  if (!admits(prefixes, sessionKey)) {
    c.allowedSessionKeyPrefixes = [...prefixes, sessionKey];
    ch.added.allowedSessionKeyPrefixes.push(sessionKey);
    changed = true;
  }
  const agent = agentId || agentOf(sessionKey);
  const agents = list(c.allowedAgentIds);
  if (agent && !everyAgent(c.allowedAgentIds) && !agents!.includes(agent)) {
    c.allowedAgentIds = [...agents!, agent];
    ch.added.allowedAgentIds.push(agent);
    changed = true;
  }
  return changed ? { next: c, changes: ch } : null;
}

/**
 * hooks.* without what the plugin added for conversations no longer
 * connected (inUse: the session keys and agents still connected), or null
 * when nothing changes. What the plugin needs anyway (its default key)
 * stays.
 */
export function releaseScope(cur: HooksConfig, changes: Changes, inUse: { sessionKeys: string[]; agentIds: string[] }): { next: HooksConfig; changes: Changes } | null {
  const c: HooksConfig = { ...cur };
  const ch: Changes = structuredClone(changes);
  let changed = false;
  const keep = new Set([DEFAULT_KEY, String(changes.set.defaultSessionKey?.ours ?? "")].map(lower));
  const usedKeys = new Set(inUse.sessionKeys.map(lower));
  const goneKeys = ch.added.allowedSessionKeyPrefixes.filter((k) => !keep.has(lower(k)) && !usedKeys.has(lower(k)));
  if (goneKeys.length) {
    const gone = new Set(goneKeys.map(lower));
    c.allowedSessionKeyPrefixes = (list(c.allowedSessionKeyPrefixes) ?? []).filter((p) => !gone.has(lower(p)));
    ch.added.allowedSessionKeyPrefixes = ch.added.allowedSessionKeyPrefixes.filter((k) => !gone.has(lower(k)));
    changed = true;
  }
  const usedAgents = new Set(inUse.agentIds);
  const goneAgents = ch.added.allowedAgentIds.filter((a) => !usedAgents.has(a));
  if (goneAgents.length) {
    const gone = new Set(goneAgents);
    c.allowedAgentIds = (list(c.allowedAgentIds) ?? []).filter((a) => !gone.has(a));
    ch.added.allowedAgentIds = ch.added.allowedAgentIds.filter((a) => !gone.has(a));
    changed = true;
  }
  return changed ? { next: c, changes: ch } : null;
}

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/**
 * The hooks.* patch that undoes what the plugin changed (for `openclaw
 * config patch`: null deletes a setting), or null when there's nothing to
 * undo. cur is hooks.* now, as `openclaw config get hooks --json` prints it
 * (its token redacted, which this never writes back).
 */
export function restorePatch(cur: HooksConfig | undefined, changes: Changes): Record<string, unknown> | null {
  const c = cur ?? {};
  const patch: Record<string, unknown> = {};
  for (const k of ["allowedSessionKeyPrefixes", "allowedAgentIds"] as const) {
    const added = new Set(changes.added[k].map(k === "allowedSessionKeyPrefixes" ? lower : (x: string) => x));
    const now = list(c[k]);
    const was = changes.set[k];
    if (now === undefined) continue;
    const rest = now.filter((x) => !added.has(k === "allowedSessionKeyPrefixes" ? lower(x) : x));
    if (was && rest.length === 0) patch[k] = was.had ? (was.prior ?? null) : null;
    else if (rest.length !== now.length) patch[k] = rest;
  }
  for (const k of ["enabled", "allowRequestSessionKey", "defaultSessionKey"] as const) {
    const was = changes.set[k];
    if (!was || !same(c[k], was.ours)) continue; // changed since: the user's
    patch[k] = was.had ? (was.prior ?? null) : null;
  }
  // The plugin's own token goes with it.
  if (changes.set.token && !changes.set.token.had && "token" in c) patch.token = null;
  return Object.keys(patch).length ? patch : null;
}

/** The gateway's bind mode, as it started (its --bind wins over gateway.bind). */
export function bindMode(argv: string[], gateway: { bind?: unknown } | undefined, inContainer: boolean): string {
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === "--bind" && typeof argv[i + 1] === "string") return argv[i + 1];
    if (argv[i].startsWith("--bind=")) return argv[i].slice("--bind=".length);
  }
  if (typeof gateway?.bind === "string" && gateway.bind) return gateway.bind;
  // OpenClaw's default: loopback, or auto in a container.
  return inContainer ? "auto" : "loopback";
}

const LOOPBACK_HOSTS = new Set(["127.0.0.1", "::1", "localhost"]);

/** Whether the gateway listens beyond this computer. */
export function listensBeyondLoopback(mode: string, customBindHost?: unknown): boolean {
  if (mode === "loopback") return false;
  if (mode === "custom" && typeof customBindHost === "string" && LOOPBACK_HOSTS.has(customBindHost.trim())) return false;
  return true;
}

// restore.mjs: node restore.mjs <hooks.json>, with `openclaw config get
// hooks --json` on stdin; prints the patch for `openclaw config patch
// --stdin`, or nothing when there's nothing to undo.
function main() {
  const changes = JSON.parse(readFileSync(process.argv[2], "utf8")) as Changes;
  let cur: HooksConfig | undefined;
  try {
    const v = JSON.parse(readFileSync(0, "utf8"));
    // A setting that isn't set prints a failure envelope, not hooks.
    cur = v && typeof v === "object" && !Array.isArray(v) && !("ok" in v && v.ok === false) ? (v as HooksConfig) : undefined;
  } catch {
    cur = undefined;
  }
  const patch = restorePatch(cur, changes);
  if (patch) process.stdout.write(JSON.stringify({ hooks: patch }) + "\n");
}

// Run as a script (the module's real path is the one node was given, which
// may be through a symlink, as /var is on macOS), not imported.
const isMain = () => {
  try {
    return !!process.argv[1] && realpathSync(fileURLToPath(import.meta.url)) === realpathSync(process.argv[1]);
  } catch {
    return false;
  }
};
if (isMain()) main();
