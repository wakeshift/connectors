import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { redact } from "./redact.js";

/** What a command needs from the world, so tests can stand in for each part. */
export interface Ctx {
  cwd: string;
  env: Record<string, string | undefined>;
  platform: NodeJS.Platform;
  stdout: (s: string) => void;
  stderr: (s: string) => void;
  fetch: typeof fetch;
  sleep: (ms: number) => Promise<void>;
  now: () => number;
  random: (n: number) => Buffer;
  /** Starts a program in the background, without waiting (the browser). False if it couldn't start. */
  spawnDetached: (cmd: string, args: string[]) => boolean;
  /** Runs a program to completion (git, a trigger command). */
  run: (cmd: string, args: string[], opts?: RunOptions) => Promise<RunResult>;
}

export interface RunOptions {
  cwd?: string;
  shell?: boolean;
  timeoutMs?: number;
  input?: string;
}

export interface RunResult {
  /** The exit code; -1 if it couldn't start or was killed. */
  code: number;
  stdout: string;
  stderr: string;
  /** Set when the program couldn't start (ENOENT: not installed). */
  error?: string;
}

export function realRun(cmd: string, args: string[], opts: RunOptions = {}): Promise<RunResult> {
  return new Promise((resolve) => {
    let stdout = "";
    let stderr = "";
    let done = false;
    const finish = (r: RunResult) => {
      if (done) return;
      done = true;
      resolve(r);
    };
    let p;
    try {
      p = spawn(cmd, args, { cwd: opts.cwd, shell: opts.shell ?? false, stdio: ["pipe", "pipe", "pipe"], windowsHide: true });
    } catch (err) {
      finish({ code: -1, stdout, stderr, error: (err as Error).message });
      return;
    }
    const timer = opts.timeoutMs ? setTimeout(() => p.kill("SIGKILL"), opts.timeoutMs) : undefined;
    p.stdout.on("data", (d) => (stdout += d));
    p.stderr.on("data", (d) => (stderr += d));
    p.on("error", (err) => {
      if (timer) clearTimeout(timer);
      finish({ code: -1, stdout, stderr, error: (err as NodeJS.ErrnoException).code ?? err.message });
    });
    p.on("close", (code) => {
      if (timer) clearTimeout(timer);
      finish({ code: code ?? -1, stdout, stderr });
    });
    if (opts.input !== undefined) p.stdin.end(opts.input);
    else p.stdin.end();
  });
}

export function realSpawnDetached(cmd: string, args: string[]): boolean {
  try {
    const p = spawn(cmd, args, { detached: true, stdio: "ignore", windowsHide: true });
    p.on("error", () => {});
    p.unref();
    return true;
  } catch {
    return false;
  }
}

export function realCtx(): Ctx {
  return {
    cwd: process.cwd(),
    env: process.env,
    platform: process.platform,
    stdout: (s) => process.stdout.write(s),
    stderr: (s) => process.stderr.write(s),
    fetch: globalThis.fetch,
    sleep: (ms) => new Promise((r) => setTimeout(r, ms)),
    now: Date.now,
    random: randomBytes,
    spawnDetached: realSpawnDetached,
    run: realRun,
  };
}

/** Prints through redact, always: the one way commands write to the terminal. */
export class Out {
  readonly secrets = new Set<string>();
  constructor(private ctx: Ctx) {}
  /** Values to redact wherever they appear (the key, a bearer token from --header-env). */
  secret(v: string | undefined): void {
    if (v) this.secrets.add(v);
  }
  line(s = ""): void {
    this.ctx.stdout(redact(s, this.secrets) + "\n");
  }
  err(s: string): void {
    this.ctx.stderr(redact(s, this.secrets) + "\n");
  }
  clean(s: string): string {
    return redact(s, this.secrets);
  }
}
