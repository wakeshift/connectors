// Keeping OpenClaw's hooks.* set up for Wakeshift's wake (hooks.ts says
// what changes and why): turned on at the first connect, a connected
// conversation's session and agent allowed, and allowed no longer once it
// has gone. Every change is recorded in <state dir>/hooks.json before it's
// written, so the uninstaller can undo it (a crash between the two leaves
// the record naming one entry more than the config has, which undoing
// skips). Changes are made one at a time, through OpenClaw's own config
// writer (api.runtime.config.mutateConfigFile), which hot-reloads hooks.

import { randomBytes } from "node:crypto";
import { copyFileSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { addScope, releaseScope, takeover, type Changes, type HooksConfig } from "./hooks.js";
import { writeAtomic } from "./store.js";

/** What the control needs from OpenClaw's config. */
export interface ConfigApi {
  /** hooks.* now. */
  hooks(): HooksConfig | undefined;
  /** Writes hooks.*: fn gets them as the config file has them now. */
  mutateHooks(fn: (cur: HooksConfig | undefined) => HooksConfig | undefined): Promise<void>;
}

export interface ControlOptions {
  config: ConfigApi;
  dir: string; // the plugin's state directory
  /** The gateway listens beyond this computer. */
  network: () => boolean;
  /** The user agreed to hooks there (plugins.entries.wakeshift.config.allowNetworkHooks). */
  allowNetwork: boolean;
  log: { info(m: string): void; warn(m: string): void };
  token?: () => string;
  /** dist/hooks.js, copied next to hooks.json as the uninstaller's restore.mjs. */
  restoreScript?: string;
}

const CHANGED = "OpenClaw's hooks settings changed meanwhile; call the app's tool again";

export class HooksControl {
  private chain: Promise<unknown> = Promise.resolve();
  constructor(private readonly o: ControlOptions) {}

  private get file() {
    return join(this.o.dir, "hooks.json");
  }

  /** What the plugin changed so far (undefined: nothing yet). */
  changes(): Changes | undefined {
    try {
      const c = JSON.parse(readFileSync(this.file, "utf8")) as Changes;
      return c && c.version === 1 && c.added ? c : undefined;
    } catch {
      return undefined;
    }
  }

  private save(c: Changes) {
    writeAtomic(this.file, JSON.stringify(c, null, 2) + "\n");
    if (this.o.restoreScript) {
      try {
        copyFileSync(this.o.restoreScript, join(this.o.dir, "restore.mjs"));
      } catch (e) {
        this.o.log.warn(`wakeshift: couldn't copy the uninstaller's restore script: ${(e as Error).message}`);
      }
    }
  }

  private on<T>(fn: () => Promise<T>): Promise<T> {
    const p = this.chain.then(fn, fn);
    this.chain = p.catch(() => {});
    return p;
  }

  /**
   * Makes sure OpenClaw's hooks let Wakeshift wake this session. Resolves
   * to "" when they do, or to why not (said to the agent).
   */
  allow(sessionKey: string, agentId?: string): Promise<string> {
    return this.on(async () => {
      let ch = this.changes();
      const cur = this.o.config.hooks();
      if (!ch) {
        const token = this.o.token ? this.o.token() : randomBytes(32).toString("hex");
        const t = takeover(cur, token, { network: this.o.network(), allowNetwork: this.o.allowNetwork });
        if (!t.ok) return t.why;
        // What's written, and recorded, is worked out from the config file as
        // it is at the write (the snapshot can lag it): the record always says
        // exactly what this write changed. Recorded before the file is.
        let rec: Changes | undefined;
        try {
          await this.o.config.mutateHooks((now) => {
            const again = takeover(now, token, { network: false, allowNetwork: true });
            if (!again.ok || again.changes.owned !== t.changes.owned) throw new Error(CHANGED);
            rec = again.changes;
            this.save(rec);
            return again.next;
          });
        } catch (e) {
          rmSync(this.file, { force: true }); // nothing was turned on: no record of it
          return `OpenClaw didn't save Wakeshift's hooks settings (${(e as Error).message})`;
        }
        ch = rec!;
        this.o.log.info("wakeshift: turned on OpenClaw's hooks for waking conversations (recorded in hooks.json)");
      } else {
        const why = stillUsable(cur, ch);
        if (why) return why;
      }
      // Nothing to add, as far as the snapshot shows: no write.
      if (!addScope(this.o.config.hooks() ?? cur ?? {}, ch, sessionKey, agentId)) return "";
      // Worked out, and recorded, from the config file as it is at the write,
      // so the record says exactly what this write adds (never an entry the
      // user added meanwhile, which an undo would then take out). A write
      // that fails puts the record back as it was.
      const before = ch;
      try {
        await this.o.config.mutateHooks((now) => {
          const again = addScope(now ?? {}, before, sessionKey, agentId);
          if (!again) return now;
          this.save(again.changes);
          return again.next;
        });
      } catch (e) {
        this.save(before);
        return `OpenClaw didn't save this conversation's hooks settings (${(e as Error).message})`;
      }
      return "";
    });
  }

  /** Allows no longer what the plugin added for conversations that have gone. */
  release(inUse: { sessionKeys: string[]; agentIds: string[] }): Promise<void> {
    return this.on(async () => {
      const ch = this.changes();
      if (!ch) return;
      // Worked out from the config file at the write, and recorded only once
      // the write has landed (until then, the entries are still there and
      // still the plugin's).
      let rec: Changes | undefined;
      try {
        await this.o.config.mutateHooks((now) => {
          const r = now ? releaseScope(now, ch, inUse) : null;
          rec = r?.changes;
          return r ? r.next : now;
        });
        if (rec) this.save(rec);
      } catch (e) {
        this.o.log.warn(`wakeshift: couldn't remove a gone conversation from OpenClaw's hooks settings: ${(e as Error).message}`);
      }
    });
  }
}

/** Why hooks the plugin set up (or the user's, which it used) can't wake now, or "". */
export function stillUsable(cur: HooksConfig | undefined, ch: Changes): string {
  if (cur?.enabled !== true) {
    return ch.owned
      ? "OpenClaw's hooks were turned off since Wakeshift's plugin turned them on (hooks.enabled is false). Ask the user whether to turn them on again; only if they agree, set hooks.enabled to true"
      : "OpenClaw's hooks are off here now (hooks.enabled is false)";
  }
  if (cur.allowRequestSessionKey !== true) return "OpenClaw's hooks no longer let a caller name the session (hooks.allowRequestSessionKey is false)";
  if (typeof cur.token !== "string" || !cur.token.trim()) return "OpenClaw's hooks have no token (hooks.token)";
  return "";
}
