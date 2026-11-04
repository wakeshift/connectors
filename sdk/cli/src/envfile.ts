// The env file (.env, or --env): read and written line by line, so
// every other line, comment and blank stays as the developer had it. A new
// file is created readable only by its owner (0600).

import { chmodSync, existsSync, readFileSync, statSync, writeFileSync } from "node:fs";

export const KEY_VAR = "WAKESHIFT_KEY";
export const CORE_VAR = "WAKESHIFT_CORE";
/** A key `keys new` made that the console hasn't confirmed yet: it becomes WAKESHIFT_KEY once it works. */
export const NEW_KEY_VAR = "WAKESHIFT_KEY_NEW";

export const KEY_COMMENT = "# Wakeshift app key, made on this machine by wakeshift-cli. Never print, paste or commit it.";

export interface EnvFile {
  path: string;
  exists: boolean;
  lines: string[];
  /** Whether the file ended with a newline. */
  trailingNewline: boolean;
  /** Its permission bits, if it exists. */
  mode?: number;
}

const ASSIGN = /^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$/;

export function readEnvFile(path: string): EnvFile {
  if (!existsSync(path)) return { path, exists: false, lines: [], trailingNewline: true };
  const text = readFileSync(path, "utf8");
  const lines = text.split(/\r?\n/);
  const trailingNewline = text.endsWith("\n");
  if (trailingNewline) lines.pop();
  return { path, exists: true, lines, trailingNewline, mode: statSync(path).mode & 0o777 };
}

function valueOf(raw: string): string {
  const v = raw.trim();
  const q = v[0];
  if ((q === '"' || q === "'") && v.length >= 2) {
    const end = v.indexOf(q, 1);
    if (end > 0) return v.slice(1, end);
  }
  // An unquoted value ends at a comment.
  return v.replace(/\s+#.*$/, "");
}

/** The value a name is set to (the last assignment wins), or undefined. */
export function getVar(f: EnvFile, name: string): string | undefined {
  let out: string | undefined;
  for (const l of f.lines) {
    const m = ASSIGN.exec(l);
    if (m && m[1] === name) out = valueOf(m[2]);
  }
  return out;
}

/**
 * Sets name to value (undefined removes it): the first assignment is
 * replaced and any later ones removed; with none, it's appended, after
 * comment if given.
 */
export function setVar(f: EnvFile, name: string, value: string | undefined, comment?: string): void {
  const out: string[] = [];
  let placed = false;
  for (const l of f.lines) {
    const m = ASSIGN.exec(l);
    if (m && m[1] === name) {
      if (!placed && value !== undefined) out.push(`${name}=${value}`);
      placed = true;
      continue;
    }
    out.push(l);
  }
  if (!placed && value !== undefined) {
    if (comment && !out.includes(comment)) out.push(comment);
    out.push(`${name}=${value}`);
  }
  // A comment left above nothing of ours.
  if (value === undefined && comment) {
    const i = out.indexOf(comment);
    if (i >= 0 && !ASSIGN.test(out[i + 1] ?? "")) out.splice(i, 1);
  }
  f.lines = out;
}

/** Writes the file: a new one with mode 0600, an existing one in place, keeping its mode. */
export function writeEnvFile(f: EnvFile): void {
  const text = f.lines.join("\n") + (f.lines.length && f.trailingNewline ? "\n" : "");
  if (!f.exists) {
    writeFileSync(f.path, text, { mode: 0o600, flag: "wx" });
    chmodSync(f.path, 0o600); // whatever the umask
    f.exists = true;
    f.mode = 0o600;
    return;
  }
  writeFileSync(f.path, text, { flag: "w" });
}

/** Whether the file's mode lets other users on the machine read it. */
export function othersCanRead(f: EnvFile): boolean {
  return f.mode !== undefined && (f.mode & 0o077) !== 0;
}
