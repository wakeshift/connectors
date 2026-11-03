#!/usr/bin/env node
// npx -y @wakeshift/connect <harness> [--issuer URL] [--uninstall]
//
// Puts Wakeshift's connector at ~/.wakeshift/bin/wakeshift (the path the
// hooks and allow rules name, so it stays the same across updates), from
// this release's platform package (@wakeshift/connect-<os>-<arch>), checked
// against the SHA-256 this package carries, then runs `wakeshift install
// --harness <harness> --issuer <issuer> --yes`: the same as the install
// script from that Wakeshift. Nothing runs at npm install time.
//
// Exits non-zero on any failure, with the reason on stderr: 64 for a usage
// error (an unknown harness), 7 when the binary doesn't match its checksum
// (nothing is installed), 1 for anything else, and otherwise what the
// connector exits with.
"use strict";
const { createHash } = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

// The harnesses `wakeshift install` sets up (internal/connector; Hermes and
// OpenClaw take their own plugins). TestConnectHarnesses keeps it in step.
const HARNESSES = ["antigravity", "claude-code", "codex", "copilot", "cursor", "opencode"];
// The Wakeshift this package trusts unless --issuer names another
// (plugins/pack-npm.mjs --issuer packs it for another).
const DEFAULT_ISSUER = "https://app.wakeshift.ai";
const EXIT_USAGE = 64;
const EXIT_VERIFY = 7;

function die(msg, code = 1) {
  process.stderr.write(`wakeshift-connect: ${msg}\n`);
  process.exit(code);
}

const usage = `usage: npx -y @wakeshift/connect <${HARNESSES.join("|")}> [--issuer URL] [--uninstall]`;
const args = process.argv.slice(2);
let harness = "";
let issuer = DEFAULT_ISSUER;
let uninstall = false;
for (let i = 0; i < args.length; i++) {
  const a = args[i];
  if (a === "--issuer") issuer = args[++i] || "";
  else if (a.startsWith("--issuer=")) issuer = a.slice("--issuer=".length);
  else if (a === "--uninstall") uninstall = true;
  else if (a === "-h" || a === "--help") {
    process.stdout.write(usage + "\n");
    process.exit(0);
  } else if (!a.startsWith("-") && !harness) harness = a.toLowerCase();
  else die(`unknown argument ${a}\n${usage}`, EXIT_USAGE);
}
if (!harness) die(usage, EXIT_USAGE);
if (!HARNESSES.includes(harness)) die(`unknown harness ${harness}\n${usage}`, EXIT_USAGE);
if (!issuer) die(`--issuer needs a URL\n${usage}`, EXIT_USAGE);

const plat = { darwin: "darwin", linux: "linux" }[process.platform];
const arch = { arm64: "arm64", x64: "x64" }[process.arch];
if (!plat || !arch) die(`Wakeshift's connector isn't built for ${process.platform}-${process.arch}`);
const pkg = `@wakeshift/connect-${plat}-${arch}`;
const sumName = `wakeshift-${plat}-${arch === "x64" ? "amd64" : arch}`; // as the release names it

let src;
try {
  src = path.join(path.dirname(require.resolve(`${pkg}/package.json`)), "bin", "wakeshift");
} catch {
  die(`${pkg} isn't installed: npm skipped it (installs with --omit=optional skip it). Run this again without --omit=optional, or install with the script from the Wakeshift you use.`);
}

// The SHA-256 of each platform's binary, written into this package when the
// release was packed.
let want;
try {
  const sums = fs.readFileSync(path.join(__dirname, "..", "SHA256SUMS"), "utf8");
  want = sums.split("\n").map((l) => l.trim().split(/\s+/)).find((f) => f[1] === sumName)?.[0];
} catch {}
if (!want) die(`this release has no checksum for ${sumName}`);
let bytes;
try {
  bytes = fs.readFileSync(src);
} catch (e) {
  die(`couldn't read ${src}: ${e.message}`);
}
if (createHash("sha256").update(bytes).digest("hex") !== want) {
  die(`${pkg}'s binary doesn't match its checksum; nothing was installed`, EXIT_VERIFY);
}

const home = process.env.WAKESHIFT_HOME || path.join(os.homedir(), ".wakeshift");
const binDir = path.join(home, "bin");
const bin = path.join(binDir, "wakeshift");
try {
  fs.mkdirSync(binDir, { recursive: true, mode: 0o700 });
  fs.chmodSync(home, 0o700);
  let current = "";
  try {
    current = createHash("sha256").update(fs.readFileSync(bin)).digest("hex");
  } catch {}
  if (current !== want) {
    const tmp = path.join(binDir, `.wakeshift.${process.pid}`);
    fs.writeFileSync(tmp, bytes, { mode: 0o755 });
    fs.chmodSync(tmp, 0o755);
    fs.renameSync(tmp, bin);
  }
} catch (e) {
  die(`couldn't install ${bin}: ${e.message}`);
}

const cmd = uninstall ? ["uninstall", "--harness", harness] : ["install", "--harness", harness, "--issuer", issuer, "--yes"];
const r = spawnSync(bin, cmd, { stdio: "inherit", env: process.env });
if (r.error) die(`couldn't run ${bin}: ${r.error.message}`);
process.exit(r.status ?? 1);
