// `openclaw wakeshift reset`: undoes what the plugin turned on, before an
// uninstall (OpenClaw has no uninstall hook, and `openclaw plugins
// uninstall` leaves hooks.* alone). Run where the gateway runs:
//
//   openclaw wakeshift reset && openclaw plugins uninstall wakeshift --force
//
// It puts OpenClaw's hooks settings back from the record the plugin kept
// (<state dir>/hooks.json, restorePatch in hooks.ts: only what the plugin
// added goes, and a setting it changed is put back only while it still
// holds the plugin's value), drops the plugin's own hooks token with them,
// removes the user's network-hooks consent (allowNetworkHooks: the next
// install asks again), and deletes the plugin's state directory (its
// conversations' stream links and that record). The Wakeshift the plugin
// trusts (issuer) stays: it says where the plugin connects, not something
// it turned on, and the uninstall removes it with the plugin's settings.

import { existsSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { restorePatch, type Changes, type HooksConfig } from "./hooks.js";

/** What reset needs from OpenClaw's config. */
export interface ResetConfig {
  /** The config as the process sees it now, if it can say: nothing to undo there means no write. */
  current?(): Record<string, unknown> | undefined;
  /** Changes the config file: mutate gets it as it is now, and edits it in place. */
  mutate(fn: (draft: Record<string, unknown>) => void): Promise<void>;
}

/** The plugin's record of what it changed in hooks.*, if there is one. */
export function readChanges(dir: string): Changes | undefined {
  try {
    const c = JSON.parse(readFileSync(join(dir, "hooks.json"), "utf8")) as Changes;
    return c && c.version === 1 && c.added && c.set ? c : undefined;
  } catch {
    return undefined;
  }
}

/** hooks.* with restorePatch's patch applied (null deletes a setting); undefined when none is left. */
export function applyPatch(cur: HooksConfig | undefined, patch: Record<string, unknown>): HooksConfig | undefined {
  const next: HooksConfig = { ...(cur ?? {}) };
  for (const [k, v] of Object.entries(patch)) {
    if (v === null || v === undefined) delete next[k];
    else next[k] = v;
  }
  return Object.keys(next).length ? next : undefined;
}

const obj = (v: unknown): Record<string, unknown> | undefined => (v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined);

/** The plugin's own settings in a config draft (plugins.entries.wakeshift.config), if any. */
function pluginConfig(draft: Record<string, unknown>, id: string): Record<string, unknown> | undefined {
  return obj(obj(obj(obj(draft.plugins)?.entries)?.[id])?.config);
}

/** Changes the config to undo the plugin's changes; says what it did, one line each. */
export function undo(draft: Record<string, unknown>, changes: Changes | undefined, id = "wakeshift"): string[] {
  const done: string[] = [];
  if (changes) {
    const cur = obj(draft.hooks) as HooksConfig | undefined;
    const patch = restorePatch(cur, changes);
    if (patch) {
      const next = applyPatch(cur, patch);
      if (next === undefined) delete draft.hooks;
      else draft.hooks = next;
      done.push(
        changes.owned
          ? "OpenClaw's hooks settings are back as they were before Wakeshift turned them on (its token is gone), apart from what you changed since."
          : "Wakeshift's additions to your OpenClaw hooks settings are removed.",
      );
    }
  }
  const cfg = pluginConfig(draft, id);
  if (cfg && "allowNetworkHooks" in cfg) {
    delete cfg.allowNetworkHooks;
    done.push("The agreement to OpenClaw's hooks endpoint on a gateway that listens beyond this computer (allowNetworkHooks) is removed: installing again asks again.");
  }
  return done;
}

/**
 * Undoes what the plugin turned on (see the top of this file), and says
 * what it did. Config first: if OpenClaw refuses the write, the record of
 * what to undo stays, for another try.
 */
export async function reset(o: { dir: string; config: ResetConfig; id?: string }): Promise<string[]> {
  const changes = readChanges(o.dir);
  // A record that's there but can't be read stays, with the directory:
  // without it nothing says what to put back.
  if (!changes && existsSync(join(o.dir, "hooks.json"))) {
    throw new Error(`${join(o.dir, "hooks.json")}, the record of what the plugin changed in OpenClaw's hooks settings, can't be read; nothing was changed`);
  }
  let done: string[] = [];
  const now = o.config.current?.();
  // No record, and no consent in the config as it is: no write (a write to
  // the plugin's settings makes the gateway load the plugin again). With a
  // record it always goes through the config file itself, where undo works
  // out the patch, before the record is deleted.
  if (changes || !now || undo(structuredClone(now), undefined, o.id).length > 0) {
    await o.config.mutate((draft) => {
      done = undo(draft, changes, o.id);
    });
  }
  rmSync(o.dir, { recursive: true, force: true });
  done.push(`Wakeshift's state for OpenClaw (${o.dir}) is deleted.`);
  return done;
}
