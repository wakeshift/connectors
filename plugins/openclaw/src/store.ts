// What the plugin keeps on disk, under <OpenClaw state>/wakeshift: each
// connected conversation's stream link (a secret: 0600 files in a 0700
// directory, named by a hash of the conversation so odd IDs can't escape
// it), the issuer's signing keys, and this install's random ID.

import { createHash, randomBytes } from "node:crypto";
import { chmodSync, closeSync, fchmodSync, fsyncSync, mkdirSync, openSync, readdirSync, readFileSync, renameSync, rmSync, statSync, writeSync } from "node:fs";
import { basename, dirname, join } from "node:path";

export interface SavedLink {
  conversation: string; // as Wakeshift knows it: <install>/<session key>
  sessionKey: string;
  agentId: string;
  link: string;
  issuer: string; // the Wakeshift that issued it (the pinned one, when saved)
  saved_at: string;
}

/** Store.pin's answer when the pins exist but can't be read. */
export const PINS_UNREADABLE = "the pins can't be read";

const hash = (s: string, n: number) => createHash("sha256").update(s).digest("hex").slice(0, n);

/**
 * Writes a file only this user can read: a fresh temp file of its own name
 * (O_EXCL, 0600 before any byte, even if the umask or an old file differs),
 * flushed to disk, then renamed over path.
 */
export function writeAtomic(path: string, body: string) {
  const tmp = join(dirname(path), `.${basename(path)}.${randomBytes(6).toString("hex")}.tmp`);
  const fd = openSync(tmp, "wx", 0o600);
  try {
    fchmodSync(fd, 0o600);
    writeSync(fd, body);
    fsyncSync(fd);
  } catch (e) {
    closeSync(fd);
    rmSync(tmp, { force: true });
    throw e;
  }
  closeSync(fd);
  try {
    renameSync(tmp, path);
  } catch (e) {
    rmSync(tmp, { force: true });
    throw e;
  }
}

export class Store {
  constructor(readonly dir: string) {}

  private ensure(sub: string) {
    const d = join(this.dir, sub);
    mkdirSync(d, { recursive: true, mode: 0o700 });
    try {
      chmodSync(this.dir, 0o700);
      chmodSync(d, 0o700);
    } catch {
      /* best effort */
    }
    return d;
  }

  private write(path: string, body: string) {
    writeAtomic(path, body);
  }

  /** This install's ID: random, made once. */
  installId(): string {
    const p = join(this.ensure("."), "install-id");
    try {
      const id = readFileSync(p, "utf8").trim();
      if (/^[0-9a-f]{16}$/.test(id)) return id;
    } catch {
      /* none yet */
    }
    const id = randomBytes(8).toString("hex");
    this.write(p, id + "\n");
    return id;
  }

  private linkPath(conversation: string) {
    return join(this.dir, "links", hash(conversation, 32) + ".json");
  }

  saveLink(l: Omit<SavedLink, "saved_at">) {
    this.ensure("links");
    this.write(this.linkPath(l.conversation), JSON.stringify({ ...l, saved_at: new Date().toISOString() }));
  }

  loadLink(conversation: string): SavedLink | undefined {
    try {
      const s = JSON.parse(readFileSync(this.linkPath(conversation), "utf8")) as SavedLink;
      return s.conversation === conversation && s.link ? s : undefined;
    } catch {
      return undefined;
    }
  }

  forgetLink(conversation: string) {
    rmSync(this.linkPath(conversation), { force: true });
  }

  links(): SavedLink[] {
    let names: string[];
    try {
      names = readdirSync(join(this.dir, "links"));
    } catch {
      return [];
    }
    const out: SavedLink[] = [];
    for (const n of names) {
      if (!n.endsWith(".json")) continue;
      try {
        const s = JSON.parse(readFileSync(join(this.dir, "links", n), "utf8")) as SavedLink;
        if (s.conversation && s.link && s.sessionKey && n === hash(s.conversation, 32) + ".json") out.push(s);
      } catch {
        /* skip */
      }
    }
    return out;
  }

  private keysPath(issuer: string) {
    return join(this.dir, "keys", hash(issuer, 16) + ".json");
  }

  /** The issuer's JWKS as last fetched, and when. */
  loadKeys(issuer: string): { body: string; at: number } | undefined {
    const p = this.keysPath(issuer);
    try {
      return { body: readFileSync(p, "utf8"), at: statSync(p).mtimeMs };
    } catch {
      return undefined;
    }
  }

  saveKeys(issuer: string, body: string) {
    this.ensure("keys");
    this.write(this.keysPath(issuer), body);
  }

  /** Where the pins are (servers.json: MCP server name → app ID). */
  pinsPath() {
    return join(this.dir, "servers.json");
  }

  /**
   * connector.Pins: "" if the MCP server may carry the app's beacons (the
   * pair is pinned the first time either is seen), else why not. A server's
   * results carry one app's beacons, and an app's come from one server.
   */
  pin(server: string, app: string): string {
    if (!server || !app) return "the result named no app";
    let pins: Record<string, string> = {};
    let raw: string | undefined;
    try {
      raw = readFileSync(this.pinsPath(), "utf8");
    } catch (e) {
      // None yet; pins that exist but can't be read fail closed (read as
      // none, any server could pin itself to any app).
      if ((e as NodeJS.ErrnoException).code !== "ENOENT") return PINS_UNREADABLE;
    }
    if (raw !== undefined) {
      try {
        const f = JSON.parse(raw);
        if (!f || typeof f.servers !== "object" || !f.servers) throw new Error("no servers");
        pins = f.servers;
      } catch {
        return PINS_UNREADABLE;
      }
    }
    const got = pins[server];
    if (got === app) return "";
    if (got) return `the MCP server "${server}" carries beacons for "${got}" here, not "${app}"`;
    for (const [s, a] of Object.entries(pins)) if (a === app) return `beacons for "${app}" come from the MCP server "${s}" here, not "${server}"`;
    pins[server] = app;
    this.ensure(".");
    this.write(this.pinsPath(), JSON.stringify({ servers: pins }, null, 2));
    return "";
  }
}
