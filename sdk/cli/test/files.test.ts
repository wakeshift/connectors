import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { realCtx } from "../src/context.js";
import { getVar, KEY_COMMENT, readEnvFile, setVar, writeEnvFile } from "../src/envfile.js";
import { CliError } from "../src/errors.js";
import { checkGit } from "../src/git.js";
import { defaultAppId, defaultDisplayName, projectName, validAppId, validDisplayName } from "../src/project.js";

const tmp = () => mkdtempSync(join(tmpdir(), "wscli-files-"));

test("a new env file is readable only by its owner", (t) => {
  if (process.platform === "win32") return t.skip("no POSIX modes");
  const d = tmp();
  const f = readEnvFile(join(d, ".env"));
  setVar(f, "WAKESHIFT_KEY", "wst_x", KEY_COMMENT);
  const old = process.umask(0o000);
  try {
    writeEnvFile(f);
  } finally {
    process.umask(old);
  }
  assert.equal(statSync(join(d, ".env")).mode & 0o777, 0o600);
  assert.equal(readFileSync(join(d, ".env"), "utf8"), `${KEY_COMMENT}\nWAKESHIFT_KEY=wst_x\n`);
  rmSync(d, { recursive: true });
});

test("an existing env file keeps its other lines and its mode", (t) => {
  const d = tmp();
  const p = join(d, ".env");
  writeFileSync(p, "# mine\nPORT=3000\nexport WAKESHIFT_KEY='old'\nDB=postgres://x # comment\nWAKESHIFT_KEY=older", { mode: 0o640 });
  const f = readEnvFile(p);
  assert.equal(getVar(f, "WAKESHIFT_KEY"), "older"); // the last wins
  assert.equal(getVar(f, "DB"), "postgres://x");
  assert.equal(getVar(f, "PORT"), "3000");
  setVar(f, "WAKESHIFT_KEY", "new", KEY_COMMENT);
  setVar(f, "WAKESHIFT_CORE", "http://localhost:8370");
  writeEnvFile(f);
  assert.equal(readFileSync(p, "utf8"), "# mine\nPORT=3000\nWAKESHIFT_KEY=new\nDB=postgres://x # comment\nWAKESHIFT_CORE=http://localhost:8370");
  if (process.platform !== "win32") assert.equal(statSync(p).mode & 0o777, 0o640);
  const g = readEnvFile(p);
  setVar(g, "WAKESHIFT_CORE", undefined);
  writeEnvFile(g);
  assert.ok(!readFileSync(p, "utf8").includes("WAKESHIFT_CORE"));
  rmSync(d, { recursive: true });
});

test("removing a variable we added takes its comment too", () => {
  const f = { path: "/x", exists: true, lines: ["A=1", KEY_COMMENT, "WAKESHIFT_KEY_NEW=x"], trailingNewline: true };
  setVar(f, "WAKESHIFT_KEY_NEW", undefined, KEY_COMMENT);
  assert.deepEqual(f.lines, ["A=1"]);
});

async function git(dir: string, ...args: string[]) {
  const r = await realCtx().run("git", args, { cwd: dir });
  assert.equal(r.code, 0, `git ${args.join(" ")}: ${r.stderr}`);
}

async function repo(): Promise<string> {
  const d = tmp();
  await git(d, "init", "-q");
  await git(d, "config", "user.email", "t@example.com");
  await git(d, "config", "user.name", "T");
  return d;
}

test("git: an env file that isn't ignored is added to .gitignore", async () => {
  const d = await repo();
  writeFileSync(join(d, ".gitignore"), "node_modules/");
  const r = await checkGit(realCtx(), join(d, ".env"));
  assert.deepEqual(r.repo && r.ignored, "added");
  assert.equal(readFileSync(join(d, ".gitignore"), "utf8"), "node_modules/\n\n# Wakeshift's app key (wakeshift-cli)\n.env\n");
  // Again: already ignored, nothing added.
  const again = await checkGit(realCtx(), join(d, ".env"));
  assert.deepEqual(again.repo && again.ignored, "already");
  assert.equal((readFileSync(join(d, ".gitignore"), "utf8").match(/\.env/g) ?? []).length, 1);
  rmSync(d, { recursive: true });
});

test("git: a nested env file is ignored by its path from the root", async () => {
  const d = await repo();
  mkdirSync(join(d, "server"));
  await checkGit(realCtx(), join(d, "server", ".env.local"));
  assert.match(readFileSync(join(d, ".gitignore"), "utf8"), /^server\/\.env\.local$/m);
  rmSync(d, { recursive: true });
});

test("git: a tracked env file is refused", async () => {
  const d = await repo();
  writeFileSync(join(d, ".env"), "PORT=1\n");
  await git(d, "add", ".env");
  await git(d, "commit", "-qm", "x");
  await assert.rejects(checkGit(realCtx(), join(d, ".env")), (err: CliError) => err.code === 1 && /tracked by git/.test(err.message));
  rmSync(d, { recursive: true });
});

test("git: a .gitignore that re-includes the file is refused", async () => {
  const d = await repo();
  mkdirSync(join(d, "sub"));
  writeFileSync(join(d, "sub", ".gitignore"), "!.env\n");
  await assert.rejects(checkGit(realCtx(), join(d, "sub", ".env")), (err: CliError) => /still doesn't ignore/.test(err.message));
  rmSync(d, { recursive: true });
});

test("git: outside a repository there's nothing to check", async () => {
  const d = tmp();
  const r = await checkGit(realCtx(), join(d, ".env"));
  assert.equal(r.repo, false);
  rmSync(d, { recursive: true });
});

test("default app IDs end in -dev and are valid", () => {
  assert.equal(defaultAppId("acme-ci"), "acme-ci-dev");
  assert.equal(defaultAppId("@acme/Shipyard Server"), "acme-shipyard-server-dev");
  assert.equal(defaultAppId("123"), "my-app-dev");
  assert.equal(defaultAppId("wakeshift-demo"), "my-wakeshift-demo-dev");
  assert.ok(validAppId(defaultAppId("x".repeat(80))));
  for (const p of ["acme-ci", "@acme/Shipyard Server", "123", "wakeshift-demo", "A__b", "x".repeat(80), "ünïcode"]) assert.ok(validAppId(defaultAppId(p)), p);
  assert.ok(!validAppId("a"));
  assert.ok(!validAppId("acme-"));
  assert.ok(!validAppId("ac--me"));
  assert.ok(!validAppId("Acme"));
  assert.ok(!validAppId("wakeshift-x"));
  assert.equal(defaultDisplayName("acme-ci"), "Acme Ci (dev)");
  assert.ok(validDisplayName(defaultDisplayName("@acme/<script>")));
  assert.ok(!validDisplayName('a"b'));
  assert.ok(!validDisplayName(""));
});

test("the project's name comes from its manifest", () => {
  const d = tmp();
  assert.equal(projectName(d), d.split(/[\\/]/).pop());
  writeFileSync(join(d, "go.mod"), "module github.com/acme/deployd/v2\n");
  assert.equal(projectName(d), "deployd");
  writeFileSync(join(d, "pyproject.toml"), '[project]\nname = "tracker"\n');
  assert.equal(projectName(d), "tracker");
  writeFileSync(join(d, "package.json"), '{"name":"@acme/shipyard"}');
  assert.equal(projectName(d), "shipyard");
  rmSync(d, { recursive: true });
});
