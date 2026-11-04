// The default app ID and name, from the project in the current directory:
// <project>-dev, since developers test with a separate app (the person can
// rename it on the console's page).

import { existsSync, readFileSync } from "node:fs";
import { basename, join } from "node:path";

const ID = /^[a-z][a-z0-9-]{1,39}$/;

/** Whether id is an app ID the console accepts. */
export function validAppId(id: string): boolean {
  return ID.test(id) && !id.endsWith("-") && !id.includes("--") && !id.startsWith("wakeshift");
}

export const APP_ID_RULE = "an app ID is 2-40 lowercase letters, digits and single dashes, starting with a letter (and not starting with wakeshift)";

/** Whether name is a display name the console accepts. */
export function validDisplayName(name: string): boolean {
  const n = name.trim();
  return n.length > 0 && [...n].length <= 50 && !/[<>"`]/.test(n) && !/\p{Cc}/u.test(n);
}

export const DISPLAY_NAME_RULE = "a display name is 1-50 characters, without < > \" ` or control characters";

/** The project's name: package.json's, pyproject.toml's, go.mod's module, else the directory's. */
export function projectName(dir: string): string {
  try {
    const p = join(dir, "package.json");
    if (existsSync(p)) {
      const n = JSON.parse(readFileSync(p, "utf8")).name;
      if (typeof n === "string" && n.trim()) return n.replace(/^@[^/]+\//, "");
    }
  } catch {
    // not JSON: try the next
  }
  try {
    const p = join(dir, "pyproject.toml");
    if (existsSync(p)) {
      const m = /^\s*name\s*=\s*["']([^"']+)["']/m.exec(readFileSync(p, "utf8"));
      if (m) return m[1];
    }
  } catch {
    // unreadable: try the next
  }
  try {
    const p = join(dir, "go.mod");
    if (existsSync(p)) {
      const m = /^module\s+(\S+)/m.exec(readFileSync(p, "utf8"));
      if (m) return m[1].split("/").filter((s) => !/^v\d+$/.test(s)).pop() ?? "";
    }
  } catch {
    // unreadable: the directory's name
  }
  return basename(dir);
}

/** An app ID made from a project's name, ending -dev. */
export function defaultAppId(project: string): string {
  let s = project
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^[^a-z]+/, "")
    .replace(/-+$/, "");
  if (s.startsWith("wakeshift")) s = "my-" + s;
  s = s.slice(0, 36).replace(/-+$/, "");
  if (s.length < 1) s = "my-app";
  return `${s}-dev`;
}

/** A display name made from a project's name: words capitalized, then "(dev)". */
export function defaultDisplayName(project: string): string {
  const words = project
    .replace(/[^\p{L}\p{N}]+/gu, " ")
    .trim()
    .split(/\s+/)
    .filter(Boolean)
    .map((w) => w[0].toUpperCase() + w.slice(1));
  const base = (words.join(" ") || "My app").slice(0, 40).trim();
  return `${base} (dev)`;
}
