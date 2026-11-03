#!/usr/bin/env node
// pack-npm.mjs packs Wakeshift's npm packages for one release, from the
// connector binaries the installers' release builds:
//
//   @wakeshift/connect-<os>-<arch>   the connector (wakeshift) for one platform
//   @wakeshift/connect               npx -y @wakeshift/connect <harness> (plugins/connect)
//   @wakeshift/opencode              the plugin for opencode (plugins/opencode)
//   @wakeshift/openclaw              the plugin for OpenClaw (plugins/openclaw),
//                                    with --openclaw: its built directory
//
//	node plugins/pack-npm.mjs --version 1.5.0 --bin-dir dist --out dist [--issuer URL] [--openclaw plugins/openclaw]
//
// --bin-dir holds wakeshift-<os>-<arch> for every platform (the release's
// names, Go's arch: amd64). Every package gets --version, and the launcher
// and the plugin depend on exactly that version of each platform package.
// --issuer packs the launcher and the plugin to trust another Wakeshift by
// default (a local core, for the bench); production's is the default.
// --openclaw is plugins/openclaw after its build (npm run pack, which
// stamps Wakeshift's own version into dist/version.js, the one the plugin
// reports): its dist, manifest, README and LICENSE, with package.json at
// --version and without its scripts.
// .github/workflows/installers.yml runs it for a release tag; the tarballs
// go into the signed release, and wakeshift/connectors' npm.yml publishes
// them with provenance. Prints the tarballs' names, platform packages first
// (the order to publish in).
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const PRODUCTION = "https://app.wakeshift.ai";
const ORIGIN = /^https?:\/\/[A-Za-z0-9.-]+(:[0-9]{1,5})?$/;
// Semantic versions only (npm's): no build metadata, which npm drops.
const SEMVER = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/;

// The platforms, as npm names them and as the release names the binary.
export const PLATFORMS = [
  { os: "darwin", cpu: "arm64", bin: "wakeshift-darwin-arm64", label: "macOS on Apple silicon" },
  { os: "darwin", cpu: "x64", bin: "wakeshift-darwin-amd64", label: "macOS on Intel" },
  { os: "linux", cpu: "arm64", bin: "wakeshift-linux-arm64", label: "Linux on arm64" },
  { os: "linux", cpu: "x64", bin: "wakeshift-linux-amd64", label: "Linux on x86-64" },
];
const REPO = "git+https://github.com/wakeshift/connectors.git";

function fail(msg) {
  process.stderr.write(`pack-npm: ${msg}\n`);
  process.exit(1);
}

function args(argv) {
  const o = { issuer: PRODUCTION };
  const keys = { "--version": "version", "--bin-dir": "binDir", "--out": "out", "--issuer": "issuer", "--openclaw": "openclaw" };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const [k, v] = a.includes("=") ? a.split(/=(.*)/s) : [a, argv[++i]];
    if (!keys[k] || v === undefined) fail(`unknown argument ${a}`);
    o[keys[k]] = v;
  }
  if (!o.version || !o.binDir || !o.out) fail("usage: node plugins/pack-npm.mjs --version X.Y.Z --bin-dir DIR --out DIR [--issuer URL]");
  if (!SEMVER.test(o.version)) fail(`${o.version} isn't a version npm takes (X.Y.Z, or X.Y.Z-pre)`);
  o.issuer = o.issuer.replace(/\/$/, "");
  if (!ORIGIN.test(o.issuer)) fail(`${o.issuer} isn't a Wakeshift's address (https://host[:port], no path)`);
  return o;
}

// replaceOnce replaces the one occurrence of from, or fails.
function replaceOnce(src, from, to, what) {
  if (src.split(from).length !== 2) fail(`${what} doesn't have exactly one ${JSON.stringify(from)}`);
  return src.replace(from, () => to);
}

function pack(dir, out) {
  const r = execFileSync("npm", ["pack", "--ignore-scripts", "--json", "--pack-destination", out], { cwd: dir, encoding: "utf8" });
  return JSON.parse(r)[0].filename;
}

function stamp(pkg, version) {
  const j = JSON.parse(readFileSync(pkg, "utf8"));
  j.version = version;
  for (const name of Object.keys(j.optionalDependencies ?? {})) j.optionalDependencies[name] = version;
  delete j.scripts;
  delete j.devDependencies;
  return j;
}

export function main(argv) {
  const o = args(argv);
  const out = resolve(o.out);
  mkdirSync(out, { recursive: true });
  const stage = mkdtempSync(join(tmpdir(), "wakeshift-npm-"));
  const license = join(here, "connect", "LICENSE");
  const names = [];
  try {
    const sums = [];
    for (const p of PLATFORMS) {
      const name = `@wakeshift/connect-${p.os}-${p.cpu}`;
      let bytes;
      try {
        bytes = readFileSync(join(o.binDir, p.bin));
      } catch {
        fail(`${join(o.binDir, p.bin)} is missing: build the connector for every platform first (make connector)`);
      }
      sums.push(`${createHash("sha256").update(bytes).digest("hex")}  ${p.bin}`);
      const d = join(stage, `${p.os}-${p.cpu}`);
      mkdirSync(join(d, "bin"), { recursive: true });
      writeFileSync(join(d, "bin", "wakeshift"), bytes);
      chmodSync(join(d, "bin", "wakeshift"), 0o755);
      copyFileSync(license, join(d, "LICENSE"));
      writeFileSync(join(d, "README.md"), `# ${name}\n\nWakeshift's connector (\`wakeshift\`) for ${p.label}. You don't install it yourself: [@wakeshift/connect](https://www.npmjs.com/package/@wakeshift/connect) and [@wakeshift/opencode](https://www.npmjs.com/package/@wakeshift/opencode) depend on it, and npm picks the package for your computer. Its source is [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors).\n`);
      const j = {
        name,
        version: o.version,
        description: `Wakeshift's connector (wakeshift) for ${p.label}, installed by @wakeshift/connect and @wakeshift/opencode`,
        license: "Apache-2.0",
        homepage: "https://wakeshift.ai/docs/agents",
        repository: { type: "git", url: REPO, directory: "cmd/wakeshift-connector" },
        os: [p.os],
        cpu: [p.cpu],
        files: ["bin/wakeshift"],
        preferUnplugged: true,
      };
      writeFileSync(join(d, "package.json"), JSON.stringify(j, null, 2) + "\n");
      names.push(pack(d, out));
    }

    // The launcher, with the platform binaries' checksums.
    const c = join(stage, "connect");
    mkdirSync(join(c, "bin"), { recursive: true });
    let launcher = readFileSync(join(here, "connect", "bin", "wakeshift-connect.js"), "utf8");
    launcher = replaceOnce(launcher, `const DEFAULT_ISSUER = "${PRODUCTION}";`, `const DEFAULT_ISSUER = ${JSON.stringify(o.issuer)};`, "plugins/connect/bin/wakeshift-connect.js");
    writeFileSync(join(c, "bin", "wakeshift-connect.js"), launcher, { mode: 0o755 });
    chmodSync(join(c, "bin", "wakeshift-connect.js"), 0o755);
    writeFileSync(join(c, "SHA256SUMS"), sums.join("\n") + "\n");
    copyFileSync(license, join(c, "LICENSE"));
    copyFileSync(join(here, "connect", "README.md"), join(c, "README.md"));
    writeFileSync(join(c, "package.json"), JSON.stringify(stamp(join(here, "connect", "package.json"), o.version), null, 2) + "\n");

    // The plugin for opencode.
    const oc = join(stage, "opencode");
    mkdirSync(oc, { recursive: true });
    let plugin = readFileSync(join(here, "opencode", "index.js"), "utf8");
    plugin = replaceOnce(plugin, `const DEFAULT_ISSUERS = ["${PRODUCTION}"]`, `const DEFAULT_ISSUERS = [${JSON.stringify(o.issuer)}]`, "plugins/opencode/index.js");
    writeFileSync(join(oc, "index.js"), plugin);
    copyFileSync(join(here, "opencode", "LICENSE"), join(oc, "LICENSE"));
    copyFileSync(join(here, "opencode", "README.md"), join(oc, "README.md"));
    writeFileSync(join(oc, "package.json"), JSON.stringify(stamp(join(here, "opencode", "package.json"), o.version), null, 2) + "\n");

    names.push(pack(c, out), pack(oc, out));

    // The plugin for OpenClaw, as built.
    if (o.openclaw) {
      const src = resolve(o.openclaw);
      const cl = join(stage, "openclaw");
      mkdirSync(join(cl, "dist"), { recursive: true });
      let built;
      try {
        built = readFileSync(join(src, "dist", "index.js"), "utf8");
      } catch {
        fail(`${join(src, "dist", "index.js")} is missing: build the plugin first (cd plugins/openclaw && npm run pack)`);
      }
      if (/export const VERSION = "dev";/.test(readFileSync(join(src, "dist", "version.js"), "utf8"))) {
        fail(`${join(src, "dist", "version.js")} says "dev" (version 0 to Wakeshift): build it with WAKESHIFT_VERSION set (npm run pack)`);
      }
      execFileSync("cp", ["-R", join(src, "dist") + "/.", join(cl, "dist")]);
      writeFileSync(join(cl, "dist", "index.js"), replaceOnce(built, `export const DEFAULT_ISSUER = "${PRODUCTION}";`, `export const DEFAULT_ISSUER = ${JSON.stringify(o.issuer)};`, "plugins/openclaw/dist/index.js"));
      for (const f of ["openclaw.plugin.json", "README.md", "LICENSE"]) copyFileSync(join(src, f), join(cl, f));
      writeFileSync(join(cl, "package.json"), JSON.stringify(stamp(join(src, "package.json"), o.version), null, 2) + "\n");
      names.push(pack(cl, out));
    }
  } finally {
    rmSync(stage, { recursive: true, force: true });
  }
  return names;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  for (const n of main(process.argv.slice(2))) process.stdout.write(n + "\n");
}
