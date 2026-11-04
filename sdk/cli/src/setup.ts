// init, keys new and status: the key made here, the console link the
// person confirms, and waiting for Wakeshift to know the key.

import { existsSync, statSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";
import { parseFlags } from "./args.js";
import { type Ctx, Out } from "./context.js";
import { type AppInfo, Core, CoreError, unknownKey } from "./core.js";
import { CORE_VAR, type EnvFile, getVar, KEY_COMMENT, KEY_VAR, NEW_KEY_VAR, othersCanRead, readEnvFile, setVar, writeEnvFile } from "./envfile.js";
import { CliError, EXIT, fail, usage } from "./errors.js";
import { checkGit } from "./git.js";
import { type AppKey, generateKey, parseKey } from "./key.js";
import { addKeyLink, isProduction, newAppLink, openBrowser, resolveOrigin } from "./link.js";
import { APP_ID_RULE, defaultAppId, defaultDisplayName, DISPLAY_NAME_RULE, projectName, validAppId, validDisplayName } from "./project.js";

/** How often the CLI asks whether the key works yet, and for how long (the contract with the console). */
export const POLL_INTERVAL_MS = 2_000;
export const POLL_TIMEOUT_MS = 10 * 60_000;

interface Where {
  envPath: string;
  /** The env file as the person would type it (.env). */
  rel: string;
  file: EnvFile;
  origin: string;
}

function locate(ctx: Ctx, envFlag: string | undefined, originFlag: string | undefined): Where {
  const envPath = resolve(ctx.cwd, envFlag ?? ".env");
  const dir = dirname(envPath);
  if (!existsSync(dir) || !statSync(dir).isDirectory()) throw usage(`there's no directory ${relative(ctx.cwd, dir) || dir} for the env file`);
  if (existsSync(envPath) && !statSync(envPath).isFile()) throw usage(`${envFlag ?? ".env"} isn't a file`);
  const rel = relative(ctx.cwd, envPath) || envPath;
  const file = readEnvFile(envPath);
  const origin = resolveOrigin(originFlag, ctx.env.WAKESHIFT_CORE, getVar(file, CORE_VAR));
  return { envPath, rel, file, origin };
}

function waitMs(ctx: Ctx, flag: string | undefined): number {
  if (flag === undefined) {
    const t = Number(ctx.env.WAKESHIFT_CLI_POLL_TIMEOUT_MS);
    return Number.isFinite(t) && t > 0 ? t : POLL_TIMEOUT_MS;
  }
  const s = Number(flag);
  if (!Number.isFinite(s) || s <= 0 || s > 3600) throw usage("--wait is a number of seconds, up to 3600");
  return s * 1000;
}

function pollMs(ctx: Ctx): number {
  const t = Number(ctx.env.WAKESHIFT_CLI_POLL_INTERVAL_MS);
  return Number.isFinite(t) && t > 0 ? t : POLL_INTERVAL_MS;
}

/** A failure from Wakeshift, as the CLI reports it (the message is already redacted). */
export function coreFailure(err: unknown): CliError {
  if (err instanceof CliError) return err;
  if (err instanceof CoreError) {
    if (err.code === "waitlisted" || err.code === "not_signed_in") return new CliError(err.message, EXIT.WAITLISTED);
    if (err.status === 403 && err.code === "suspended") return fail(`the app is suspended: ${err.message}`);
    return fail(err.message);
  }
  return fail(String((err as Error)?.message ?? err));
}

/** Asks GET /app with key every interval until it answers with the app, or the time runs out. */
async function waitForApp(ctx: Ctx, out: Out, core: Core, timeoutMs: number, again: string): Promise<AppInfo> {
  const deadline = ctx.now() + timeoutMs;
  let unreachable = 0;
  let warned = false;
  for (;;) {
    try {
      return await core.app();
    } catch (err) {
      if (!(err instanceof CoreError)) throw err;
      if (err.code === "waitlisted" || err.code === "not_signed_in" || err.code === "suspended") throw coreFailure(err);
      // 401: no app has the key yet. No answer, or a 5xx or 429: keep trying, and say so once.
      if (err.status === 0 || err.status >= 500) unreachable++;
      else unreachable = 0;
      if (unreachable >= 3 && !warned) {
        out.err(`Still trying: ${err.message}`);
        warned = true;
      }
    }
    if (ctx.now() + pollMs(ctx) > deadline) break;
    await ctx.sleep(err429Backoff(pollMs(ctx), unreachable));
  }
  const mins = Math.round(timeoutMs / 60_000);
  throw new CliError(
    `Waited ${mins >= 1 ? `${mins} minute${mins === 1 ? "" : "s"}` : `${Math.round(timeoutMs / 1000)} seconds`} and the key isn't confirmed yet. ` +
      `If the page said the account is on the waitlist, Wakeshift emails when it's ready. Otherwise ${again}`,
    EXIT.TIMEOUT,
  );
}

const err429Backoff = (base: number, failures: number) => Math.min(base * Math.max(1, failures), 30_000);

function showLink(ctx: Ctx, out: Out, link: string, key: AppKey, noBrowser: boolean, what: string): void {
  out.line("");
  out.line(`Ask your user to open this link and ${what} (only they can sign in and confirm):`);
  out.line(`  ${link}`);
  out.line(`Key code: ${key.keyCode} (the page shows the same code)`);
  if (openBrowser(ctx, link, noBrowser) === "opened") out.line("Opened it in the browser.");
  out.line("");
}

function permissionsNote(ctx: Ctx, out: Out, w: Where): void {
  if (ctx.platform !== "win32" && othersCanRead(w.file)) out.line(`Note: other users on this computer can read ${w.rel}; chmod 600 ${w.rel} keeps the key yours.`);
}

async function gitNote(ctx: Ctx, out: Out, w: Where): Promise<void> {
  const g = await checkGit(ctx, w.envPath);
  if (g.repo && g.ignored === "added") out.line(`Added ${w.rel} to .gitignore, so the key is never committed.`);
}

// ── init ─────────────────────────────────────────────────────────────────

export async function init(ctx: Ctx, argv: string[]): Promise<number> {
  const f = parseFlags(argv, { values: ["name", "display-name", "env", "origin", "core", "wait"], booleans: ["replace", "no-browser"] });
  if (f.positionals.length) throw usage(`init takes no arguments: ${f.positionals[0].slice(0, 40)}`);
  if (f.values.origin !== undefined && f.values.core !== undefined) throw usage("pass --origin or --core, not both");
  const project = projectName(ctx.cwd);
  const id = f.values.name ?? defaultAppId(project);
  if (!validAppId(id)) throw usage(`--name: ${APP_ID_RULE}`);
  const name = f.values["display-name"] ?? defaultDisplayName(f.values.name ? id.replace(/-dev$/, "") : project);
  if (!validDisplayName(name)) throw usage(`--display-name: ${DISPLAY_NAME_RULE}`);
  const timeout = waitMs(ctx, f.values.wait);
  const w = locate(ctx, f.values.env, f.values.origin ?? f.values.core);
  const out = new Out(ctx);

  await gitNote(ctx, out, w);

  const existing = getVar(w.file, KEY_VAR);
  let key: AppKey;
  if (existing && !f.booleans.has("replace")) {
    out.secret(existing);
    try {
      key = parseKey(existing);
    } catch {
      throw fail(`${w.rel} has a WAKESHIFT_KEY that isn't a valid app key. Pass --replace to make a new key in its place.`);
    }
    const core = new Core(ctx, w.origin, key);
    try {
      const app = await core.app();
      out.line(`${w.rel} already has the key of ${app.id}${app.name ? ` (${app.name})` : ""}. Nothing to do: pass --replace for a new app with a new key.`);
      return EXIT.OK;
    } catch (err) {
      if (!unknownKey(err)) throw coreFailure(err);
    }
    out.line(`${w.rel} has a key no app has yet, from an earlier run: using it.`);
  } else {
    key = generateKey(ctx.random);
    out.secret(key.envValue());
    setVar(w.file, KEY_VAR, key.envValue(), KEY_COMMENT);
    setVar(w.file, NEW_KEY_VAR, undefined);
    setVar(w.file, CORE_VAR, isProduction(w.origin) ? undefined : w.origin);
    writeEnvFile(w.file);
    out.line(`Made a new app key on this computer and wrote it to ${w.rel} as WAKESHIFT_KEY. It never leaves this computer.`);
  }
  permissionsNote(ctx, out, w);

  showLink(ctx, out, newAppLink(w.origin, key.publicKey, id, name), key, f.booleans.has("no-browser"), `create the app ${id}`);
  out.line(`Waiting for the app to be created (up to ${Math.round(timeout / 60_000) || 1} minutes). If this stops first, run the same command again: it keeps the key and waits again.`);

  const core = new Core(ctx, w.origin, key);
  const app = await waitForApp(ctx, out, core, timeout, `run the same command again to wait longer (it keeps the key), or with --replace if the page said the key is registered already.`);
  out.line(`Created ${app.id}. WAKESHIFT_KEY is in ${w.rel}.`);
  if (!isProduction(w.origin)) out.line(`WAKESHIFT_CORE in ${w.rel} points the SDK at ${w.origin}.`);
  return EXIT.OK;
}

// ── keys new ─────────────────────────────────────────────────────────────

export async function keysNew(ctx: Ctx, argv: string[]): Promise<number> {
  const f = parseFlags(argv, { values: ["app", "env", "origin", "core", "wait"], booleans: ["no-browser"] });
  if (f.positionals.length) throw usage(`keys new takes no arguments: ${f.positionals[0].slice(0, 40)}`);
  if (f.values.origin !== undefined && f.values.core !== undefined) throw usage("pass --origin or --core, not both");
  if (f.values.app !== undefined && !validAppId(f.values.app)) throw usage(`--app: ${APP_ID_RULE}`);
  const timeout = waitMs(ctx, f.values.wait);
  const w = locate(ctx, f.values.env, f.values.origin ?? f.values.core);
  const out = new Out(ctx);

  await gitNote(ctx, out, w);

  // Which app: --app, or the one the current key belongs to.
  const current = getVar(w.file, KEY_VAR);
  out.secret(current);
  let currentApp: string | null = null;
  if (current) {
    try {
      currentApp = (await new Core(ctx, w.origin, parseKey(current)).app()).id;
    } catch (err) {
      if (!unknownKey(err) && err instanceof CoreError) throw coreFailure(err);
      // An invalid or unknown key: it names no app.
    }
  }
  if (f.values.app && currentApp && f.values.app !== currentApp) {
    throw usage(`WAKESHIFT_KEY in ${w.rel} is ${currentApp}'s key, not ${f.values.app}'s: pass --env with another file for ${f.values.app}.`);
  }
  const appId = f.values.app ?? currentApp;
  if (!appId) throw usage(`pass --app <id>: ${w.rel} has no working WAKESHIFT_KEY to say which app the new key is for.`);

  // A new key from an earlier run that wasn't confirmed yet is used again.
  let key: AppKey | undefined;
  const pending = getVar(w.file, NEW_KEY_VAR);
  out.secret(pending);
  if (pending) {
    try {
      key = parseKey(pending);
    } catch {
      key = undefined;
    }
  }
  if (key) {
    const core = new Core(ctx, w.origin, key);
    try {
      const app = await core.app();
      if (app.id !== appId) throw fail(`${NEW_KEY_VAR} in ${w.rel} is ${app.id}'s key, not ${appId}'s. Remove that line and run this again.`);
      return finishNewKey(out, w, key, app);
    } catch (err) {
      if (!unknownKey(err)) throw coreFailure(err);
    }
    out.line(`${w.rel} has a new key from an earlier run that isn't confirmed yet: using it.`);
  } else {
    key = generateKey(ctx.random);
    out.secret(key.envValue());
    setVar(w.file, NEW_KEY_VAR, key.envValue(), KEY_COMMENT);
    if (!getVar(w.file, CORE_VAR) && !isProduction(w.origin)) setVar(w.file, CORE_VAR, w.origin);
    writeEnvFile(w.file);
    out.line(`Made a new key for ${appId} on this computer and wrote it to ${w.rel} as ${NEW_KEY_VAR}, until it's confirmed. It never leaves this computer.`);
  }
  permissionsNote(ctx, out, w);

  showLink(ctx, out, addKeyLink(w.origin, appId, key.publicKey), key, f.booleans.has("no-browser"), `add the key to ${appId}`);
  out.line(`Waiting for the key to be added (up to ${Math.round(timeout / 60_000) || 1} minutes). If this stops first, run the same command again: it keeps the new key and waits again.`);
  const app = await waitForApp(ctx, out, new Core(ctx, w.origin, key), timeout, "run the same command again to wait longer (it keeps the new key).");
  if (app.id !== appId) throw fail(`the new key was added to ${app.id}, not ${appId}. ${w.rel} still has ${appId}'s key as WAKESHIFT_KEY; revoke the new key in ${app.id}'s console.`);
  return finishNewKey(out, w, key, app);
}

function finishNewKey(out: Out, w: Where, key: AppKey, app: AppInfo): number {
  // Re-read: the file may have changed while we waited.
  const file = readEnvFile(w.envPath);
  setVar(file, KEY_VAR, key.envValue(), KEY_COMMENT);
  setVar(file, NEW_KEY_VAR, undefined);
  writeEnvFile(file);
  out.line(`Added key ${key.keyCode} to ${app.id}. WAKESHIFT_KEY in ${w.rel} is the new key; the old one keeps working until you revoke it in the console.`);
  return EXIT.OK;
}

// ── status ───────────────────────────────────────────────────────────────

export async function status(ctx: Ctx, argv: string[]): Promise<number> {
  const f = parseFlags(argv, { values: ["env", "origin", "core"], booleans: ["json"] });
  if (f.positionals.length) throw usage(`status takes no arguments: ${f.positionals[0].slice(0, 40)}`);
  if (f.values.origin !== undefined && f.values.core !== undefined) throw usage("pass --origin or --core, not both");
  const w = locate(ctx, f.values.env, f.values.origin ?? f.values.core);
  const out = new Out(ctx);
  const core = await coreFromEnv(ctx, out, w);
  let app: AppInfo;
  try {
    app = await core.app();
  } catch (err) {
    if (unknownKey(err)) throw fail(`no app has the key in ${w.rel} (it isn't confirmed yet, or it was revoked). Run init to finish creating the app, or keys new for a new key.`);
    throw coreFailure(err);
  }
  let events: { name: string; description: string }[] = [];
  let manifest = false;
  try {
    const m = await core.manifest();
    events = (Array.isArray(m.events) ? m.events : []).map((e) => ({ name: String(e.name), description: String(e.description ?? "") }));
    manifest = events.length > 0 || !!(m.description && String(m.description).trim());
  } catch (err) {
    throw coreFailure(err);
  }
  if (f.booleans.has("json")) {
    out.line(JSON.stringify({ app: { id: app.id, name: app.name }, key: { id: app.key.id, name: app.key.name }, core: w.origin, manifest, events }));
    return EXIT.OK;
  }
  out.line(`App:      ${app.id}${app.name ? ` (${app.name})` : ""}`);
  out.line(`Key:      ${app.key.id}${app.key.name ? ` (${app.key.name})` : ""}`);
  if (!isProduction(w.origin)) out.line(`Wakeshift: ${w.origin}`);
  out.line(`Manifest: ${manifest ? "received" : "not yet: your server sends it when it attaches and first handles a call about events"}`);
  if (events.length) {
    out.line("Events:");
    for (const e of events) out.line(`  ${e.name}${e.description ? `: ${e.description}` : ""}`);
  } else out.line("Events:   none yet");
  return EXIT.OK;
}

/** A Core for the key in the env file, or a failure saying how to get one. */
export async function coreFromEnv(ctx: Ctx, out: Out, w: Where): Promise<Core> {
  const raw = getVar(w.file, KEY_VAR);
  if (!w.file.exists) throw fail(`there's no ${w.rel}: run init first (or pass --env).`);
  if (!raw) throw fail(`${w.rel} has no WAKESHIFT_KEY: run init first.`);
  out.secret(raw);
  let key: AppKey;
  try {
    key = parseKey(raw);
  } catch (err) {
    throw fail(`WAKESHIFT_KEY in ${w.rel} isn't a valid app key: ${(err as Error).message}.`);
  }
  return new Core(ctx, w.origin, key);
}

export { locate };
