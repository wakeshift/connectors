package connector

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// opencode (1.18.7, verified) has no shell hooks; it
// has plugins, loaded from ~/.config/opencode/plugins/ with no trust step.
// A plugin's tool.execute.after gets an MCP tool's whole CallToolResult,
// _meta and all, and client.session.promptAsync starts a turn in an idle
// session. The MCP server learns neither the session nor the call, so
// beacons are unbound (beacon.Binding): opencode handing the result to the
// session's own plugin call is what binds it.
//
// The plugin (opencodePlugin, written by the installer) runs one
// `wakeshift bridge --harness opencode` per connected session, a child
// that exits with opencode, speaking JSON lines:
//
//	plugin → bridge, first line: {"hook_event_name": "PostToolUse" | "SessionIdle", "session_id", "call_id", "tool_name", "tool_response"}
//	bridge → plugin, first line: {"additionalContext": "…", "keep_alive": bool, "retry"?: true, "note_id"?: "<id>"}
//	plugin → bridge: {"ack": "<note_id>"}          once the note (messages waiting at connect) is in the tool result
//	bridge → plugin: {"wake": {"id", "text"}}      a message for the session
//	plugin → bridge: {"ack": "<id>"}               once opencode stored it (message.part.updated)
//
// The bridge acknowledges to Wakeshift on the plugin's ack, so delivery is
// at least once up to opencode storing the message. The bridge's errors go
// to ~/.wakeshift/opencode/logs/bridge.log.
//
// Retries: the plugin starts a session's bridge again, after a backoff, when
// one couldn't start (no first line), answered "retry" (Wakeshift was
// unreachable redeeming the beacon), or exited 1 after connecting (stopped
// listening with its link still saved). Exit 0 (its link was removed or
// ended) and keep_alive false without "retry" are final.
//
// At load (opencode started or restarted), the plugin starts a bridge for
// each of the project's sessions that has a saved link, so a connected
// session is woken again without waiting for a turn.

const opencodeName = "opencode"

func init() {
	register(Spec{Name: opencodeName, Format: opencodeFormat{}, Installer: opencodeInstaller{},
		When: "as soon as they arrive, even while the session is idle, as long as opencode is open", Unbound: true})
}

type opencodeFormat struct{}

func (opencodeFormat) Parse(b []byte) (Input, error) {
	var c struct {
		HookEventName string          `json:"hook_event_name"`
		SessionID     string          `json:"session_id"`
		CallID        string          `json:"call_id"`
		ToolName      string          `json:"tool_name"`
		ToolResponse  json.RawMessage `json:"tool_response"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Input{}, err
	}
	in := Input{Conversation: c.SessionID}
	switch c.HookEventName {
	case "PostToolUse":
		// <server>_<tool>, either of which may contain _ (ToolOf).
		in.Event, in.ToolName, in.ToolCallID, in.ToolResult = ToolResult, c.ToolName, c.CallID, c.ToolResponse
		in.ToolSep = "_"
	case "SessionIdle":
		in.Event = Rewake
	}
	return in, nil
}

func (opencodeFormat) Context(in Input, text string) (Output, bool) {
	b, _ := json.Marshal(map[string]any{"additionalContext": text})
	return Output{Stdout: append(b, '\n')}, true
}

func (opencodeFormat) Continue(in Input, text string) (Output, bool) {
	b, _ := json.Marshal(map[string]any{"wake": map[string]any{"text": text}})
	return Output{Stdout: append(b, '\n')}, true
}

// Bridge is the opencode plugin's child for one session.
type Bridge struct {
	Hook       *Hook
	AckTimeout time.Duration // how long to wait for the plugin's ack (default 10 minutes)
	// NoteTimeout: how long to wait for the plugin's ack of the first
	// answer's note (default 30 seconds; the plugin adds it at once).
	NoteTimeout time.Duration
}

// Run reads the first line, answers it, and while the session has a link,
// hands its messages to the plugin until stdin closes.
func (b *Bridge) Run(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := bufio.NewReaderSize(stdin, 1<<20)
	first, err := r.ReadBytes('\n')
	if err != nil && len(first) == 0 {
		return nil
	}
	in, err := b.Hook.Format.Parse(bytes.TrimSpace(first))
	var note string
	transient := false
	run := *b.Hook // this run's own copy: the shared Hook isn't changed
	run.OnTransient = func() { transient = true }
	if err == nil && in.Conversation != "" && in.Event == ToolResult {
		if out, ok := run.toolResult(ctx, in); ok {
			var c struct {
				AdditionalContext string `json:"additionalContext"`
			}
			json.Unmarshal(out.Stdout, &c)
			note = c.AdditionalContext
		}
	}
	keep := err == nil && in.Conversation != "" && b.Hook.Links.Load(in.Conversation) != ""
	var mu sync.Mutex
	write := func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		line, _ := json.Marshal(v)
		_, err := stdout.Write(append(line, '\n'))
		return err
	}
	answer := map[string]any{"additionalContext": note, "keep_alive": keep}
	if !keep && transient {
		answer["retry"] = true // Wakeshift was unreachable: the plugin may try this beacon again
	}
	// The note can hand over messages that were waiting when it connected.
	// The plugin acks them by note_id once it has added the note to the
	// tool result, and only then are they acknowledged to Wakeshift: before
	// October 28 they never were, so the link's next poll handed them out
	// again and the plugin woke the session with them a second time
	// (wakebench S8: "GOT … (duplicate)").
	noteID := ""
	if run.handed != nil && keep {
		noteID = newID()
		answer["note_id"] = noteID
	}
	if err := write(answer); err != nil || !keep {
		return nil
	}

	acks := make(chan string, 16)
	go func() { // the plugin's acks; EOF: opencode is gone
		defer cancel()
		for {
			line, err := r.ReadBytes('\n')
			var m struct {
				Ack string `json:"ack"`
			}
			if json.Unmarshal(bytes.TrimSpace(line), &m) == nil && m.Ack != "" {
				select {
				case acks <- m.Ack:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	wait := b.AckTimeout
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	if noteID != "" {
		// Unacknowledged (a plugin from before note_id, or the note never
		// reached the result), they come again with the first poll, as a wake.
		noteWait := b.NoteTimeout
		if noteWait <= 0 {
			noteWait = 30 * time.Second
		}
		timer := time.NewTimer(noteWait)
	waitNote:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
				break waitNote
			case got := <-acks:
				if got == noteID {
					run.handed()
					break waitNote
				}
			}
		}
		timer.Stop()
	}
	for ctx.Err() == nil {
		text, ack := b.Hook.take(ctx, in.Conversation, 0, false)
		if text == "" {
			if ctx.Err() == nil && b.Hook.Links.Load(in.Conversation) != "" {
				// replaced or unreachable for good: let a later call retry
				return errors.New("stopped listening")
			}
			return nil
		}
		id := newID()
		if err := write(map[string]any{"wake": map[string]any{"id": id, "text": text}}); err != nil {
			return nil
		}
		timer := time.NewTimer(wait)
	waitAck:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
				break waitAck // not acknowledged: Wakeshift hands it out again
			case got := <-acks:
				if got == id {
					ack()
					break waitAck
				}
			}
		}
		timer.Stop()
	}
	return nil
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// OpencodeConfigDir is opencode's config directory
// ($XDG_CONFIG_HOME/opencode, default ~/.config/opencode, on macOS too).
func OpencodeConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "opencode")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "opencode")
}

type opencodeInstaller struct{}

func (opencodeInstaller) path() string {
	return filepath.Join(OpencodeConfigDir(), "plugins", "wakeshift.ts")
}

// Install writes Wakeshift's own plugin file (nothing of anyone else's is
// edited).
func (i opencodeInstaller) Install(bin string, out io.Writer) error {
	return writeOwnFile(i.path(), []byte(strings.Replace(opencodePlugin, "__WAKESHIFT_BIN__", jsString(bin), 1)), out, "plugin")
}

func (i opencodeInstaller) Uninstall(bin string, out io.Writer) error {
	if err := removeOwnFile(i.path(), opencodeMarker, out, "plugin"); err != nil {
		return err
	}
	fmt.Fprintln(out, "Restart opencode to unload it.")
	return nil
}

// jsString is s as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

const opencodeMarker = "Wakeshift's plugin for opencode"

// opencodePlugin is the plugin file. Only a default export (opencode treats
// every named export as a plugin) and only node builtins (no npm install).
// It never throws: a throwing hook fails the user's tool call.
//
// The npm package @wakeshift/opencode ships the same plugin with its own
// head (OpencodePackage): opencodeBody is shared by both.
const opencodePlugin = `// Wakeshift's plugin for opencode: lets Wakeshift messages reach your opencode
// sessions. Written by ` + "`wakeshift install --harness opencode`" + `; remove it with
// ` + "`wakeshift uninstall --harness opencode`" + ` (or delete this file) and restart opencode.
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"
import { createHash } from "node:crypto"
import { closeSync, existsSync, mkdirSync, openSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const BIN = __WAKESHIFT_BIN__
` + opencodeBody

// opencodeBody is the plugin after its head (imports and BIN). Its server
// starts with opencodeServer, which OpencodePackage replaces.
const opencodeBody = `const KEY = "wakeshift/connector"
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
` + opencodeServer + `    // One Wakeshift plugin per project: the file an earlier install wrote
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
`

// opencodeServer is the plugin's server line in opencodeBody.
const opencodeServer = "  server: async ({ client, directory }) => {\n"
