// Keeping the env file out of the repository: refuse a file git tracks
// (anything written there would be committed), and make sure git ignores
// it, adding it to the repository's .gitignore if not.

import { appendFileSync, existsSync, readFileSync, realpathSync } from "node:fs";
import { basename, dirname, join, relative, sep } from "node:path";
import type { Ctx } from "./context.js";
import { fail } from "./errors.js";

export type GitCheck =
  | { repo: false; why: string }
  | { repo: true; root: string; ignored: "already" | "added" };

/** Checks (and fixes) how git sees the env file at path. Throws a CliError if it's tracked or can't be ignored. */
export async function checkGit(ctx: Ctx, file: string): Promise<GitCheck> {
  // git names the repository by its real path (/tmp is /private/tmp on macOS).
  const dir = realpathSync(dirname(file));
  const path = join(dir, basename(file));
  const top = await ctx.run("git", ["rev-parse", "--show-toplevel"], { cwd: dir });
  if (top.error) return { repo: false, why: "git isn't installed" };
  if (top.code !== 0) return { repo: false, why: "not in a git repository" };
  const root = top.stdout.trim();
  const name = basename(path);

  const tracked = await ctx.run("git", ["ls-files", "--error-unmatch", "--", name], { cwd: dir });
  if (tracked.code === 0) {
    const rel = relPath(root, path);
    throw fail(
      `${rel} is tracked by git, so a key written there would be committed. ` +
        `Stop tracking it (git rm --cached ${rel}, then commit), or pass --env with a file git ignores, and run this again.`,
    );
  }

  if (await ignored(ctx, dir, name)) return { repo: true, root, ignored: "already" };

  // Add it to the repository's .gitignore, then check again: a negated
  // pattern elsewhere could still un-ignore it.
  const gi = join(root, ".gitignore");
  const pattern = relPath(root, path);
  const before = existsSync(gi) ? readFileSync(gi, "utf8") : "";
  const lead = before === "" || before.endsWith("\n") ? "" : "\n";
  appendFileSync(gi, `${lead}${before === "" ? "" : "\n"}# Wakeshift's app key (wakeshift-cli)\n${pattern}\n`);
  if (!(await ignored(ctx, dir, name))) {
    throw fail(`Added ${pattern} to .gitignore, but git still doesn't ignore it (a pattern starting with ! may re-include it). Fix .gitignore so git ignores ${pattern}, then run this again.`);
  }
  return { repo: true, root, ignored: "added" };
}

async function ignored(ctx: Ctx, dir: string, name: string): Promise<boolean> {
  const r = await ctx.run("git", ["check-ignore", "-q", "--", name], { cwd: dir });
  return r.code === 0;
}

/** path relative to root, with forward slashes, as .gitignore and messages write it. */
function relPath(root: string, path: string): string {
  return relative(root, path).split(sep).join("/");
}
