package connector

import "strings"

// OpencodePackage is the plugin as the npm package @wakeshift/opencode
// ships it (plugins/opencode/index.js, which TestOpencodePackage keeps in
// step: `go test ./internal/connector -run TestOpencodePackage -update`
// writes it). opencode installs it with its own command, `opencode plugin
// @wakeshift/opencode@<version> --global --force`, which adds it to the
// "plugin" list in opencode.json and installs it with npm, install scripts
// off.
//
// It's opencodeBody, the plugin the installer writes, with its own head and
// server line:
//
//   - The connector is this platform's optional dependency,
//     @wakeshift/connect-<os>-<arch>, the package npx -y @wakeshift/connect
//     installs from too, made executable at load (no install script ran).
//   - The Wakeshifts it trusts are the plugin's "issuers" option in
//     opencode.json, else DEFAULT_ISSUERS (production; the packing script,
//     plugins/pack-npm.mjs in github.com/wakeshift/connectors, packs it for
//     another with --issuer), checked as CheckIssuer does and added to
//     <home>/opencode/config.json, where the bridge reads them, as the
//     plugin's (plugin_issuers, as trustForPlugin records them): a changed
//     option drops those it added before.
//   - It removes the plugin file an earlier `wakeshift install --harness
//     opencode` wrote (opencodeInstaller.path), so a user who moves over
//     doesn't load both after their next restart; until then the body's
//     guard runs only the first one loaded.
func OpencodePackage() string {
	return opencodePackageHead + strings.Replace(opencodeBody, opencodeServer, opencodePackageServer, 1)
}

// OpencodeDefaultIssuers is the line of OpencodePackage that names the
// Wakeshifts it trusts by default (the packing script replaces it to pack
// the plugin for another Wakeshift).
const OpencodeDefaultIssuers = `const DEFAULT_ISSUERS = ["https://app.wakeshift.ai"]`

const opencodePackageHead = `// Wakeshift's plugin for opencode, the npm package @wakeshift/opencode: lets
// Wakeshift messages reach your opencode sessions. Installed with
// ` + "`opencode plugin @wakeshift/opencode@<version> --global --force`" + `; to remove it,
// delete its entry from the "plugin" list in ~/.config/opencode/opencode.json
// and restart opencode.
//
// Generated from the plugin ` + "`wakeshift install --harness opencode`" + ` writes
// (internal/connector, OpencodePackage), with three changes: the connector
// is this platform's package, @wakeshift/connect-<os>-<arch>; the Wakeshifts
// it trusts are the plugin's "issuers" option, else DEFAULT_ISSUERS; and it
// removes the plugin file an earlier install wrote.
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"
import { createHash } from "node:crypto"
import { chmodSync, closeSync, existsSync, mkdirSync, openSync, readFileSync, renameSync, statSync, unlinkSync, writeFileSync } from "node:fs"
import { createRequire } from "node:module"
import { isIPv4 } from "node:net"
import { homedir } from "node:os"
import { join } from "node:path"

` + OpencodeDefaultIssuers + `
const ORIGIN = /^https?:\/\/[A-Za-z0-9.-]+(:[0-9]{1,5})?$/

// The connector for this platform, from the optional dependency npm picked.
// opencode installs npm plugins with scripts off, so nothing ran at install:
// make it executable here.
const connector = () => {
  const bin = createRequire(import.meta.url).resolve(` + "`@wakeshift/connect-${process.platform}-${process.arch}/bin/wakeshift`" + `)
  if ((statSync(bin).mode & 0o111) === 0) chmodSync(bin, 0o755)
  return bin
}

// issuer is an issuer address as the connector takes it (CheckIssuer): an
// https origin, or plain http on this machine only (development: localhost,
// 127.x.x.x or [::1]); "" for anything else.
const issuer = (i) => {
  if (typeof i !== "string") return ""
  i = i.replace(/\/$/, "")
  if (/^http:\/\/\[::1\](:[0-9]{1,5})?$/.test(i)) return i
  if (!ORIGIN.test(i)) return ""
  if (i.startsWith("https://")) return i
  const host = i.slice("http://".length).split(":")[0] // ORIGIN: http://<host>[:<port>]
  return host === "localhost" || (isIPv4(host) && host.startsWith("127.")) ? i : ""
}

// The Wakeshifts the connector trusts for opencode, in the file the bridge
// reads (<home>/opencode/config.json), as the connector's own plugins do
// (trustForPlugin): the plugin's that nothing trusts yet are added and
// recorded as the plugin's (plugin_issuers), and those it added before but
// no longer names (its "issuers" option changed) are removed. plugin_issuers
// holds only issuers the plugin alone trusts: one already trusted isn't
// recorded, and trusting one another way (` + "`wakeshift install`" + `) takes it out
// (Settings.Trust), so those stay (` + "`wakeshift uninstall --harness opencode`" + `
// removes the file).
const trust = (home, issuers) => {
  const dir = join(home, "opencode")
  mkdirSync(dir, { recursive: true, mode: 0o700 })
  const file = join(dir, "config.json")
  let cfg = {}
  try { cfg = JSON.parse(readFileSync(file, "utf8")) } catch {}
  if (!cfg || typeof cfg !== "object" || Array.isArray(cfg)) cfg = {}
  const want = [...new Set(issuers.map(issuer).filter(Boolean))]
  const was = Array.isArray(cfg.issuers) ? cfg.issuers : []
  const ours = Array.isArray(cfg.plugin_issuers) ? cfg.plugin_issuers : []
  const gone = ours.filter((i) => !want.includes(i))
  const have = was.filter((i) => !gone.includes(i))
  const add = want.filter((i) => !have.includes(i))
  if (!add.length && !gone.length && cfg.installed_at) return
  cfg.issuers = [...have, ...add]
  const mine = [...ours.filter((i) => want.includes(i)), ...add.filter((i) => !ours.includes(i))]
  if (mine.length) cfg.plugin_issuers = mine
  else delete cfg.plugin_issuers
  cfg.installed_at ??= new Date().toISOString()
  const tmp = file + "." + process.pid + ".tmp"
  writeFileSync(tmp, JSON.stringify(cfg, null, 2) + "\n", { mode: 0o600 })
  renameSync(tmp, file)
}

// The plugin file ` + "`wakeshift install --harness opencode`" + ` wrote, removed once
// this package runs instead (only a file that says it's that plugin).
const retire = () => {
  const dir = process.env.XDG_CONFIG_HOME ? join(process.env.XDG_CONFIG_HOME, "opencode") : join(homedir(), ".config", "opencode")
  const file = join(dir, "plugins", "wakeshift.ts")
  try {
    const src = readFileSync(file, "utf8")
    if (src.includes("` + opencodeMarker + `") && src.includes("wakeshift install --harness opencode")) unlinkSync(file)
  } catch {}
}

let BIN
`

const opencodePackageServer = `  server: async ({ client, directory }, options) => {
    try {
      BIN = connector()
      const issuers = Array.isArray(options?.issuers) && options.issuers.length ? options.issuers : DEFAULT_ISSUERS
      trust(process.env.WAKESHIFT_HOME || join(homedir(), ".wakeshift"), issuers)
    } catch {
      return {} // no connector for this platform, or no home to write: nothing to do
    }
    retire()
`
