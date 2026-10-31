// What the plugin keeps on disk, under <OpenClaw state>/wakeshift: each
// connected conversation's stream links, one per app it watches (a secret:
// 0600 files in a 0700 directory, named by a hash of the conversation, the
// issuer and the app, so odd IDs can't escape it), the issuer's signing
// keys, and this install's random ID.

import { createHash, randomBytes } from "node:crypto";
import { chmodSync, closeSync, fchmodSync, fsyncSync, mkdirSync, openSync, readdirSync, readFileSync, renameSync, rmSync, statSync, writeSync } from "node:fs";
import { basename, dirname, join } from "node:path";

export interface SavedLink {
  conversation: string; // as Wakeshift knows it: <install>/<session key>
  sessionKey: string;
  agentId: string;
  link: string;
  issuer: string; // the Wakeshift that issued it (the pinned one, when saved)
  // The app whose beacon it was redeemed from: a conversation has a link
  // per app. Absent from a link saved before links named their app (one
  // per conversation): the first beacon after the update replaces it.
  app?: string;
  // This link's stream, random per redeem and kept when the link rotates:
  // delivery records name it, so each is acknowledged only on the stream it
  // came from ("" for a link saved before).
  stream?: string;
  saved_at: string;
}

/** A link's place among its conversation's: its issuer and app ("" for a link that names no app). */
export function linkKey(l: { issuer?: string; app?: string }): string {
  return l.app ? `${l.issuer ?? ""}\n${l.app}` : "";
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

function valid(s: SavedLink): boolean {
  return (
    !!s &&
    typeof s.conversation === "string" &&
    !!s.conversation &&
    typeof s.link === "string" &&
    !!s.link &&
    typeof s.sessionKey === "string" &&
    (s.issuer === undefined || typeof s.issuer === "string") &&
    (s.app === undefined || typeof s.app === "string") &&
    (s.stream === undefined || typeof s.stream === "string")
  );
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

  // links/<hash>.json: a conversation's link for one app (key: linkKey),
  // or its one link from before links named their app (key "").
  private linkName(conversation: string, key: string) {
    return hash(key ? `${conversation}\n${key}` : conversation, 32) + ".json";
  }

  private linkPath(conversation: string, key: string) {
    return join(this.dir, "links", this.linkName(conversation, key));
  }

  saveLink(l: Omit<SavedLink, "saved_at">) {
    this.ensure("links");
    this.write(this.linkPath(l.conversation, linkKey(l)), JSON.stringify({ ...l, saved_at: new Date().toISOString() }));
  }

  /** The conversation's link for one app (key: linkKey; "" for the one saved before links named their app). */
  loadLink(conversation: string, key = ""): SavedLink | undefined {
    try {
      const s = JSON.parse(readFileSync(this.linkPath(conversation, key), "utf8")) as SavedLink;
      return valid(s) && s.conversation === conversation && linkKey(s) === key ? s : undefined;
    } catch {
      return undefined;
    }
  }

  forgetLink(conversation: string, key = "") {
    rmSync(this.linkPath(conversation, key), { force: true });
  }

  /** Every saved link: only a file named for what it holds (a copy under another name isn't read). */
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
        if (valid(s) && s.sessionKey && n === this.linkName(s.conversation, linkKey(s))) out.push(s);
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
