// The console link the person opens to confirm a key made here, and the
// Wakeshift it goes to.

import type { Ctx } from "./context.js";
import { usage } from "./errors.js";

export const PRODUCTION = "https://app.wakeshift.ai";

/**
 * The Wakeshift to use: --origin, else WAKESHIFT_CORE from the environment,
 * else from the env file, else production's. https only, except plain http
 * on this machine (a local core).
 */
export function resolveOrigin(flag: string | undefined, env: string | undefined, file: string | undefined): string {
  const raw = (flag ?? env ?? file ?? "").trim();
  if (!raw) return PRODUCTION;
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    throw usage(`${flag !== undefined ? "--origin" : "WAKESHIFT_CORE"} isn't a URL: ${raw.slice(0, 100)}`);
  }
  const local = ["localhost", "127.0.0.1", "[::1]"].includes(u.hostname);
  if (u.protocol !== "https:" && !(u.protocol === "http:" && local)) {
    throw usage(`${flag !== undefined ? "--origin" : "WAKESHIFT_CORE"} must be an https URL (plain http only for localhost)`);
  }
  if (u.username || u.password) throw usage("the origin can't carry a user name or password");
  return u.origin;
}

export const isProduction = (origin: string) => origin === PRODUCTION;

/**
 * The console page that creates an app with this public key. Everything
 * after # stays in the browser; ?ref=cli tells sign-in where a new account
 * came from (it's saved on the account, and goes nowhere else).
 */
export function newAppLink(origin: string, publicKey: string, id: string, name: string): string {
  return `${origin}/new-app?ref=cli#pk=${publicKey}&id=${encodeURIComponent(id)}&name=${encodeURIComponent(name)}`;
}

/** The console page that adds this public key to an existing app. */
export function addKeyLink(origin: string, appId: string, publicKey: string): string {
  return `${origin}/apps/${encodeURIComponent(appId)}/keys#pk=${publicKey}`;
}

export type BrowserOutcome = "opened" | "skipped";

/**
 * Opens url in the person's browser when there's a screen to show it on,
 * and says whether it tried. $BROWSER wins (the first of a colon-separated
 * list); WAKESHIFT_NO_BROWSER, CI or a session with no display skip it.
 * The link is printed either way.
 */
export function openBrowser(ctx: Ctx, url: string, disabled = false): BrowserOutcome {
  const env = ctx.env;
  if (disabled || env.WAKESHIFT_NO_BROWSER) return "skipped";
  const browser = (env.BROWSER ?? "").split(":")[0].trim();
  if (browser) return ctx.spawnDetached(browser, [url]) ? "opened" : "skipped";
  if (env.CI) return "skipped";
  switch (ctx.platform) {
    case "darwin":
      // Over SSH there's no screen here to open it on.
      if (env.SSH_CONNECTION || env.SSH_TTY) return "skipped";
      return ctx.spawnDetached("open", [url]) ? "opened" : "skipped";
    case "win32":
      // Not cmd's start: the link's & would split the command.
      return ctx.spawnDetached("rundll32", ["url.dll,FileProtocolHandler", url]) ? "opened" : "skipped";
    default:
      if (!env.DISPLAY && !env.WAYLAND_DISPLAY) return "skipped";
      return ctx.spawnDetached("xdg-open", [url]) ? "opened" : "skipped";
  }
}
