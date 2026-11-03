// The npm packages as plugins/pack-npm.mjs packs them, and the launcher
// (npx -y @wakeshift/connect <harness>) run from them as npm lays them out.
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { after, before, test } from "node:test";
import { fileURLToPath } from "node:url";

const plugins = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const VERSION = "0.0.1-test.1";
const ISSUER = "http://localhost:8399";
const here = `${process.platform}-${process.arch}`;
const supported = ["darwin-arm64", "darwin-x64", "linux-arm64", "linux-x64"].includes(here);

let root, out;

// A stand-in connector per platform (each different, so their checksums
// are): it records how it was run.
before(() => {
  root = mkdtempSync(join(tmpdir(), "wakeshift-connect-test-"));
  const bins = join(root, "bins");
  out = join(root, "out");
  mkdirSync(bins);
  for (const p of ["darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"]) {
    writeFileSync(join(bins, `wakeshift-${p}`), `#!/bin/sh\n# ${p}\necho "$@" > "$WAKESHIFT_HOME/ran"\n`);
  }
  execFileSync(process.execPath, [join(plugins, "pack-npm.mjs"), "--version", VERSION, "--bin-dir", bins, "--out", out, "--issuer", ISSUER + "/"]);
});

after(() => rmSync(root, { recursive: true, force: true }));

const tgz = (name) => join(out, `wakeshift-${name}-${VERSION}.tgz`);

// unpack extracts a tarball to node_modules/<name> under dir.
function unpack(dir, file, name) {
  const to = join(dir, "node_modules", name);
  mkdirSync(dirname(to), { recursive: true });
  const tmp = mkdtempSync(join(dir, ".x-"));
  execFileSync("tar", ["-xzf", file, "-C", tmp]);
  renameSync(join(tmp, "package"), to);
  return to;
}

const manifest = (file) => JSON.parse(execFileSync("tar", ["-xzOf", file, "package/package.json"], { encoding: "utf8" }));

test("every package has the release's version, and the launcher and plugin depend on exactly it", () => {
  for (const p of ["darwin-arm64", "darwin-x64", "linux-arm64", "linux-x64"]) {
    const m = manifest(tgz(`connect-${p}`));
    assert.equal(m.name, `@wakeshift/connect-${p}`);
    assert.equal(m.version, VERSION);
    assert.deepEqual([m.os, m.cpu], [[p.split("-")[0]], [p.split("-")[1]]]);
    assert.equal(m.repository.url, "git+https://github.com/wakeshift/connectors.git"); // npm provenance checks it
    const mode = execFileSync("tar", ["-tvzf", tgz(`connect-${p}`)], { encoding: "utf8" }).split("\n").find((l) => l.endsWith("package/bin/wakeshift"));
    assert.match(mode, /^-rwxr-xr-x/);
  }
  for (const name of ["connect", "opencode"]) {
    const m = manifest(tgz(name));
    assert.equal(m.version, VERSION);
    assert.equal(m.scripts, undefined, "no install scripts");
    assert.deepEqual(Object.values(m.optionalDependencies), [VERSION, VERSION, VERSION, VERSION]);
    assert.equal(m.repository.url, "git+https://github.com/wakeshift/connectors.git");
  }
  const oc = manifest(tgz("opencode"));
  assert.equal(oc.exports["."], "./index.js", "opencode needs a root export");
  const plugin = execFileSync("tar", ["-xzOf", tgz("opencode"), "package/index.js"], { encoding: "utf8" });
  assert.ok(plugin.includes(`const DEFAULT_ISSUERS = ["${ISSUER}"]`), "the plugin trusts --issuer");
});

test("installs the connector and runs its install", { skip: !supported }, () => {
  const dir = mkdtempSync(join(root, "ok-"));
  const pkg = unpack(dir, tgz("connect"), "@wakeshift/connect");
  unpack(dir, tgz(`connect-${here}`), `@wakeshift/connect-${here}`);
  const home = join(dir, "wakeshift-home");
  const r = spawnSync(process.execPath, [join(pkg, "bin", "wakeshift-connect.js"), "cursor"], { env: { ...process.env, WAKESHIFT_HOME: home }, encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  assert.equal(readFileSync(join(home, "ran"), "utf8").trim(), `install --harness cursor --issuer ${ISSUER} --yes`);
  // Run again over it, and to uninstall.
  const again = spawnSync(process.execPath, [join(pkg, "bin", "wakeshift-connect.js"), "cursor", "--uninstall"], { env: { ...process.env, WAKESHIFT_HOME: home }, encoding: "utf8" });
  assert.equal(again.status, 0, again.stderr);
  assert.equal(readFileSync(join(home, "ran"), "utf8").trim(), "uninstall --harness cursor");
});

test("a binary that doesn't match its checksum exits 7 and installs nothing", { skip: !supported }, () => {
  const dir = mkdtempSync(join(root, "bad-"));
  const pkg = unpack(dir, tgz("connect"), "@wakeshift/connect");
  const plat = unpack(dir, tgz(`connect-${here}`), `@wakeshift/connect-${here}`);
  writeFileSync(join(plat, "bin", "wakeshift"), "#!/bin/sh\necho tampered\n");
  chmodSync(join(plat, "bin", "wakeshift"), 0o755);
  const home = join(dir, "wakeshift-home");
  const r = spawnSync(process.execPath, [join(pkg, "bin", "wakeshift-connect.js"), "codex"], { env: { ...process.env, WAKESHIFT_HOME: home }, encoding: "utf8" });
  assert.equal(r.status, 7, r.stderr);
  assert.ok(!existsSync(join(home, "bin", "wakeshift")));
});

test("without its platform package it exits 1 and says why", () => {
  const dir = mkdtempSync(join(root, "none-"));
  const pkg = unpack(dir, tgz("connect"), "@wakeshift/connect");
  const r = spawnSync(process.execPath, [join(pkg, "bin", "wakeshift-connect.js"), "codex"], { env: { ...process.env, WAKESHIFT_HOME: join(dir, "h") }, encoding: "utf8" });
  assert.equal(r.status, 1);
  assert.match(r.stderr, supported ? /isn't installed.*--omit=optional/ : /isn't built for/);
});

test("an unknown harness or argument exits 64", () => {
  const dir = mkdtempSync(join(root, "usage-"));
  const pkg = unpack(dir, tgz("connect"), "@wakeshift/connect");
  for (const a of [["hermes"], [], ["cursor", "--bogus"]]) {
    const r = spawnSync(process.execPath, [join(pkg, "bin", "wakeshift-connect.js"), ...a], { encoding: "utf8" });
    assert.equal(r.status, 64, `${a}: ${r.stderr}`);
  }
});

test("pack-npm refuses a version npm doesn't take and an issuer with a path", () => {
  for (const bad of [["--version", "2026.11.02.1435+abc1234"], ["--version", "1.0.0", "--issuer", "https://x.test/path"]]) {
    const r = spawnSync(process.execPath, [join(plugins, "pack-npm.mjs"), "--version", "1.0.0", "--bin-dir", root, "--out", join(root, "x"), ...bad], { encoding: "utf8" });
    assert.equal(r.status, 1, r.stderr);
  }
});
