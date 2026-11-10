// Wakeshift's plugin for opencode, the npm package @wakeshift/opencode: lets
// Wakeshift messages reach your opencode sessions. Installed with
// `opencode plugin @wakeshift/opencode@<version> --global --force`; to remove it,
// delete its entry from the "plugin" list in ~/.config/opencode/opencode.json
// and restart opencode.
//
// Generated from the plugin `wakeshift install --harness opencode` writes
// (internal/connector, OpencodePackage), with three changes: the connector
// is this platform's package, @wakeshift/connect-<os>-<arch>; the Wakeshifts
// it trusts are the plugin's "issuers" option, else DEFAULT_ISSUERS; and it
// removes the plugin file an earlier install wrote.
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"
import { createHash } from "node:crypto"
import { chmodSync, closeSync, existsSync, mkdirSync, openSync, readFileSync, renameSync, statSync, unlinkSync, writeFileSync } from "node:fs"
import { createRequire } from "node:module"
import { homedir } from "node:os"
import { join } from "node:path"

const DEFAULT_ISSUERS = ["https://app.wakeshift.ai"]
const ORIGIN = /^https?:\/\/[A-Za-z0-9.-]+(:[0-9]{1,5})?$/

// The connector for this platform, from the optional dependency npm picked.
// opencode installs npm plugins with scripts off, so nothing ran at install:
// make it executable here.
const connector = () => {
  const bin = createRequire(import.meta.url).resolve(`@wakeshift/connect-${process.platform}-${process.arch}/bin/wakeshift`)
  if ((statSync(bin).mode & 0o111) === 0) chmodSync(bin, 0o755)
  return bin
}

// The Wakeshifts the connector trusts for opencode, in the file the bridge
// reads (<home>/opencode/config.json): the plugin's are added, nothing is
// removed (`wakeshift uninstall --harness opencode` removes the file).
const trust = (home, issuers) => {
  const dir = join(home, "opencode")
  mkdirSync(dir, { recursive: true, mode: 0o700 })
  const file = join(dir, "config.json")
  let cfg = {}
  try { cfg = JSON.parse(readFileSync(file, "utf8")) } catch {}
  if (!cfg || typeof cfg !== "object" || Array.isArray(cfg)) cfg = {}
  const have = Array.isArray(cfg.issuers) ? cfg.issuers : []
  const add = issuers.map((i) => (typeof i === "string" ? i.replace(/\/$/, "") : i)).filter((i) => typeof i === "string" && ORIGIN.test(i) && !have.includes(i))
  if (!add.length && cfg.installed_at) return
  cfg.issuers = [...have, ...add]
  cfg.installed_at ??= new Date().toISOString()
  const tmp = file + "." + process.pid + ".tmp"
  writeFileSync(tmp, JSON.stringify(cfg, null, 2) + "\n", { mode: 0o600 })
  renameSync(tmp, file)
}

// The plugin file `wakeshift install --harness opencode` wrote, removed once
// this package runs instead (only a file that says it's that plugin).
const retire = () => {
  const dir = process.env.XDG_CONFIG_HOME ? join(process.env.XDG_CONFIG_HOME, "opencode") : join(homedir(), ".config", "opencode")
  const file = join(dir, "plugins", "wakeshift.ts")
  try {
    const src = readFileSync(file, "utf8")
    if (src.includes("Wakeshift's plugin for opencode") && src.includes("wakeshift install --harness opencode")) unlinkSync(file)
  } catch {}
}

let BIN
const KEY = "wakeshift/connector"
const FIRST_LINE_MS = 15000
// A bridge that can't start, or stops while its link is still saved, is
// started again after a backoff: 2s, doubling to 2 minutes, with jitter.
const RETRY_MIN_MS = Number(process.env.WAKESHIFT_OPENCODE_RETRY_MIN_MS) || 2000
const RETRY_MAX_MS = Number(process.env.WAKESHIFT_OPENCODE_RETRY_MAX_MS) || 120000
// At load, this project's sessions Wakeshift connected before listen again,
// after this long (opencode's server answers once its plugins have loaded),
// the most recently updated first, at most REARM_MAX of them: each is a
// process holding a long poll, and a project's older sessions are rarely
// still wanted. One left out listens again at its next turn
// (session.idle), as every session did before.
const REARM_MS = Number(process.env.WAKESHIFT_OPENCODE_REARM_MS) || 1000
const REARM_MAX = 20

export default {
  id: "wakeshift",
  server: async ({ client, directory }, options) => {
    try {
      BIN = connector()
      const issuers = Array.isArray(options?.issuers) && options.issuers.length ? options.issuers : DEFAULT_ISSUERS
      trust(process.env.WAKESHIFT_HOME || join(homedir(), ".wakeshift"), issuers)
    } catch {
      return {} // no connector for this platform, or no home to write: nothing to do
    }
    retire()
    // One Wakeshift plugin per project: the file an earlier install wrote
    // and the npm package can both be loaded once, while moving over.
    const loaded = (globalThis[Symbol.for("wakeshift.opencode")] ??= new Set())
    if (loaded.has(directory)) return {}
    loaded.add(directory)
    const children = new Map() // session ID -> bridge process
    const tried = new Set() // sessions checked for a saved link this run
    const busy = new Set()
    const queue = new Map() // session ID -> [{id, text}]
    const acks = new Map() // wake ID -> send ack
    const retries = new Map() // session ID -> {timer, delay, input}
    // Session ID -> its bridges still starting. More than one only for
    // beacons: two results in one turn each carry one, and each bridge
    // redeems its own (the connector takes one redeem at a time, the second
    // handing back the first's link), so neither is stopped for the other.
    const starting = new Map()
    const carrying = new WeakSet() // bridges started with a beacon
    const isStarting = (sid) => (starting.get(sid)?.size ?? 0) > 0
    const gone = new Set() // deleted sessions: never started again
    const stopped = new WeakSet() // bridges this plugin ended on purpose
    let disposed = false

    // Starts the session's bridge again after a backoff; one timer per session.
    const retry = (sid, input) => {
      if (disposed || gone.has(sid)) return
      const r = retries.get(sid) ?? { delay: RETRY_MIN_MS }
      r.input = input
      retries.set(sid, r)
      if (r.timer) return
      const wait = Math.min(RETRY_MAX_MS, Math.round(r.delay * (0.5 + Math.random())))
      r.delay = Math.min(r.delay * 2, RETRY_MAX_MS)
      r.timer = setTimeout(() => {
        r.timer = undefined
        if (!children.has(sid) && !isStarting(sid)) void start(sid, r.input)
      }, wait)
      r.timer.unref?.()
    }
    // Connected, or told there's nothing to listen to: the backoff starts over.
    const settle = (sid) => {
      const r = retries.get(sid)
      if (r?.timer) clearTimeout(r.timer)
      retries.delete(sid)
    }
    const stop = (child) => {
      stopped.add(child)
      try { child.kill() } catch {}
    }

    // Subagents' tool calls report their own session: wake the root one.
    const root = async (id) => {
      for (let i = 0; i < 10; i++) {
        const s = await client.session.get({ path: { id } }).catch(() => undefined)
        const parent = s?.data?.parentID
        if (!parent) return id
        id = parent
      }
      return id
    }

    const deliver = async (sid) => {
      if (busy.has(sid)) return // after session.idle
      const next = queue.get(sid)?.shift()
      if (!next) return
      busy.add(sid)
      const r = await client.session
        .promptAsync({ path: { id: sid }, query: { directory },
          body: { parts: [{ type: "text", text: next.text, synthetic: true, metadata: { wakeshift: next.id } }] } })
        .catch(() => undefined)
      const ok = r && (r.response?.status === 204 || r.response?.ok)
      if (!ok) {
        busy.delete(sid)
        return // not acknowledged: Wakeshift hands it out again
      }
      // Acknowledged once opencode has stored it (message.part.updated).
    }

    // The bridge's errors, for the user to look at.
    const log = () => {
      try {
        const dir = join(process.env.WAKESHIFT_HOME || join(homedir(), ".wakeshift"), "opencode", "logs")
        mkdirSync(dir, { recursive: true, mode: 0o700 })
        return openSync(join(dir, "bridge.log"), "a", 0o600)
      } catch {
        return "ignore"
      }
    }

    // Starts the session's bridge; resolves with its note for the model (and
    // a function that acks the messages it carries, once it's added). A
    // bridge that can't start, or stops while its link is still saved (exit
    // code 1), is retried; one with nothing to listen to, or whose link was
    // removed or ended (exit code 0), isn't.
    const start = (sid, input) =>
      new Promise((resolve) => {
        if (disposed || gone.has(sid)) return resolve(undefined)
        let child
        const errs = log()
        try {
          child = spawn(BIN, ["bridge", "--harness", "opencode"], { stdio: ["pipe", "pipe", errs] })
        } catch {
          retry(sid, input)
          return resolve(undefined)
        } finally {
          if (typeof errs === "number") { try { closeSync(errs) } catch {} } // the child has its own
        }
        if (!starting.has(sid)) starting.set(sid, new Set())
        starting.get(sid).add(child)
        if (input?.hook_event_name === "PostToolUse") carrying.add(child)
        let first = true
        let connected = false
        const done = (text, noteId) => {
          if (!first) return
          first = false
          starting.get(sid)?.delete(child)
          if (starting.get(sid)?.size === 0) starting.delete(sid)
          clearTimeout(timer)
          const ack = noteId ? () => { try { child.stdin.write(JSON.stringify({ ack: noteId }) + "\n") } catch {} } : undefined
          resolve(text ? { text, ack } : undefined)
        }
        const failed = () => { // no first line; one this plugin stopped isn't retried
          if (!first) return
          done(undefined)
          if (!stopped.has(child)) retry(sid, input)
        }
        const timer = setTimeout(() => { failed(); stop(child) }, FIRST_LINE_MS)
        child.on("error", failed)
        child.on("exit", (code) => {
          failed()
          if (children.get(sid) === child) children.delete(sid)
          if (connected && code !== 0 && !stopped.has(child)) {
            retry(sid, { hook_event_name: "SessionIdle", session_id: sid }) // its link is saved by now
          }
        })
        child.stdin.on("error", () => {})
        child.stdin.write(JSON.stringify(input) + "\n")
        createInterface({ input: child.stdout }).on("line", (line) => {
          let m
          try { m = JSON.parse(line) } catch { return }
          if (first) {
            if (stopped.has(child)) return done(undefined) // replaced while starting
            if (m.retry && !m.keep_alive) retry(sid, input) // Wakeshift was unreachable: try the same beacon again
            else settle(sid)
            if (m.keep_alive) {
              connected = true
              const old = children.get(sid)
              if (old && old !== child) stop(old)
              children.set(sid, child)
            } else {
              try { child.stdin.end() } catch {}
            }
            return done(typeof m.additionalContext === "string" && m.additionalContext ? m.additionalContext : undefined, typeof m.note_id === "string" ? m.note_id : undefined)
          }
          if (m.wake && typeof m.wake.id === "string" && typeof m.wake.text === "string") {
            const q = queue.get(sid) ?? []
            q.push(m.wake)
            queue.set(sid, q)
            acks.set(m.wake.id, () => { try { child.stdin.write(JSON.stringify({ ack: m.wake.id }) + "\n") } catch {} })
            void deliver(sid)
          }
        })
      })

    // opencode started (or restarted, opencode --continue): listen again for
    // this project's sessions that were connected, without waiting for the
    // user's next message. Only a session with a saved link gets a bridge:
    // the link's file, or the directory of its links per app (named by a
    // hash of the session ID, as the bridge names them), is only checked
    // for, never read here.
    const linksDir = join(process.env.WAKESHIFT_HOME || join(homedir(), ".wakeshift"), "opencode", "links")
    const linked = (sid) => {
      try {
        const h = join(linksDir, createHash("sha256").update(sid).digest("hex").slice(0, 32))
        return existsSync(h + ".json") || existsSync(h)
      } catch {
        return false
      }
    }
    const rearm = async () => {
      for (let i = 0; i < 5 && !disposed; i++) {
        // Every session of the project: GET /session has no limit unless
        // one is asked for (opencode 1.19.3).
        const r = await client.session.list({ query: { directory } }).catch(() => undefined)
        const list = Array.isArray(r?.data) ? r.data : undefined
        if (list) {
          // Linked first, then the limit: older connected sessions aren't
          // crowded out by newer ones that never connected.
          const roots = list
            .filter((s) => s && typeof s.id === "string" && !s.parentID && linked(s.id))
            .sort((a, b) => (b.time?.updated ?? 0) - (a.time?.updated ?? 0))
            .slice(0, REARM_MAX)
          for (const s of roots) {
            if (disposed || gone.has(s.id) || tried.has(s.id) || children.has(s.id) || isStarting(s.id)) continue
            tried.add(s.id)
            void start(s.id, { hook_event_name: "SessionIdle", session_id: s.id })
          }
          return
        }
        await new Promise((res) => setTimeout(res, 1000 * (i + 1)))
      }
    }
    const rearmTimer = setTimeout(() => void rearm().catch(() => {}), REARM_MS)
    rearmTimer.unref?.()

    // The session key of the session that installed this plugin (issue
    // #1106). Its result that offered the install went by before there was
    // a plugin, and opencode keeps no result's _meta, so its beacon is gone:
    // the session connects from the next result of the app's tools, whose
    // beacon the core adds only for a call that names the session, by the
    // session_key that result told the agent to pass. Where the agent
    // leaves it out of a call to that server's tools, the plugin passes it,
    // while the session has no link here: taken from the session's own
    // results of that server's tools from the hour before this plugin was
    // installed (installed_at, which no result after the install can
    // change), and only if they hold one key (one quoted in what a tool
    // showed, beside the session's own, passes nothing).
    const KEY_LINE = /Pass session_key="(wsc_[A-Za-z0-9_-]{22,26})" to this app's tools/g
    const KEY_WINDOW_MS = 3600000
    const installedAt = () => {
      try {
        const file = join(process.env.WAKESHIFT_HOME || join(homedir(), ".wakeshift"), "opencode", "config.json")
        const t = Date.parse(JSON.parse(readFileSync(file, "utf8")).installed_at)
        return Number.isFinite(t) ? t : 0
      } catch {
        return 0
      }
    }
    // The MCP servers' tool prefixes, as opencode names their tools
    // (<server>_<tool>, each with every character but letters, digits, _
    // and - turned into _); a tool two of them could name is neither's.
    let prefixes
    const serverOf = async (tool) => {
      if (!prefixes) {
        const r = await client.mcp?.status?.().catch(() => undefined)
        if (!r?.data || typeof r.data !== "object") return undefined
        prefixes = Object.keys(r.data).map((n) => n.replace(/[^a-zA-Z0-9_-]/g, "_") + "_")
      }
      const m = prefixes.filter((p) => tool.startsWith(p) && tool.length > p.length)
      return m.length === 1 ? m[0] : undefined
    }
    const keys = new Map() // root session ID and server -> its key ("" for none)
    const sessionKey = async (sid, server) => {
      const k = sid + " " + server
      if (keys.has(k)) return keys.get(k)
      const at = installedAt()
      if (!at) return ""
      const r = await client.session.messages({ path: { id: sid } }).catch(() => undefined)
      if (!Array.isArray(r?.data)) return "" // not kept: asked again at the next call
      const found = new Set()
      for (const m of r.data) {
        for (const p of Array.isArray(m?.parts) ? m.parts : []) {
          const st = p?.state
          if (p?.type !== "tool" || typeof p.tool !== "string" || st?.status !== "completed" || typeof st.output !== "string") continue
          const end = st.time?.end
          if (typeof end !== "number" || !(end < at && end >= at - KEY_WINDOW_MS)) continue
          if ((await serverOf(p.tool)) !== server) continue
          for (const x of st.output.matchAll(KEY_LINE)) found.add(x[1])
        }
      }
      const key = found.size === 1 ? [...found][0] : ""
      keys.set(k, key)
      return key
    }

    return {
      // The session key, where the agent left it out (sessionKey).
      "tool.execute.before": async (input, output) => {
        try {
          const args = output?.args
          if (!args || typeof args !== "object" || Array.isArray(args) || typeof input?.tool !== "string") return
          if (typeof args.session_key === "string" && args.session_key.trim()) return
          const server = await serverOf(input.tool)
          if (!server) return
          const sid = await root(input.sessionID)
          if (children.has(sid) || isStarting(sid) || linked(sid)) return
          const key = await sessionKey(sid, server)
          if (key) args.session_key = key // opencode passes these same args to the tool
        } catch {}
      },
      "tool.execute.after": async (input, output) => {
        try {
          const beacon = output?._meta?.[KEY] // the MCP result's _meta; the model never sees it
          if (typeof beacon !== "string" || !Array.isArray(output.content)) return
          const sid = await root(input.sessionID)
          tried.add(sid)
          // A fresh beacon replaces a pending retry, and a bridge still
          // starting without one; one still redeeming another beacon (an
          // earlier result of the same turn) carries on, so that beacon's
          // session is merged into this one's rather than lost.
          settle(sid)
          for (const pending of [...(starting.get(sid) ?? [])]) if (!carrying.has(pending)) stop(pending)
          const note = await start(sid, { hook_event_name: "PostToolUse", session_id: sid, call_id: input.callID,
            tool_name: input.tool, tool_response: output })
          if (note) {
            output.content.push({ type: "text", text: note.text })
            note.ack?.() // the messages in it reach the model with this result
          }
        } catch {}
      },
      event: async ({ event }) => {
        try {
          const p = event.properties ?? {}
          if (event.type === "session.status" && p.status?.type === "busy") busy.add(p.sessionID)
          if (event.type === "session.idle" && p.sessionID) {
            busy.delete(p.sessionID)
            if (!children.has(p.sessionID) && !isStarting(p.sessionID) && !tried.has(p.sessionID)) {
              tried.add(p.sessionID) // after a restart: listen again if it was connected
              void start(p.sessionID, { hook_event_name: "SessionIdle", session_id: p.sessionID })
            }
            void deliver(p.sessionID)
          }
          if (event.type === "message.part.updated") {
            const id = p.part?.metadata?.wakeshift
            if (typeof id === "string" && acks.has(id)) {
              acks.get(id)()
              acks.delete(id)
            }
          }
          if (event.type === "session.deleted") {
            const sid = p.info?.id ?? p.sessionID
            gone.add(sid)
            settle(sid)
            for (const c of [...(starting.get(sid) ?? []), children.get(sid)]) if (c) stop(c)
            starting.delete(sid)
            children.delete(sid)
          }
        } catch {}
      },
      dispose: async () => {
        disposed = true
        loaded.delete(directory)
        clearTimeout(rearmTimer)
        for (const r of retries.values()) if (r.timer) clearTimeout(r.timer)
        retries.clear()
        for (const set of starting.values()) for (const c of set) stop(c)
        starting.clear()
        for (const c of children.values()) stop(c)
        children.clear()
      },
    }
  },
}
