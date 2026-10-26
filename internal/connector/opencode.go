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
//	bridge → plugin, first line: {"additionalContext": "…", "keep_alive": bool, "retry"?: true}
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
		in.Event, in.ToolName, in.ToolCallID, in.ToolResult = ToolResult, c.ToolName, c.CallID, c.ToolResponse
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
const opencodePlugin = `// Wakeshift's plugin for opencode: lets Wakeshift messages reach your opencode
// sessions. Written by ` + "`wakeshift install --harness opencode`" + `; remove it with
// ` + "`wakeshift uninstall --harness opencode`" + ` (or delete this file) and restart opencode.
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"
import { closeSync, mkdirSync, openSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const BIN = __WAKESHIFT_BIN__
const KEY = "wakeshift/connector"
const FIRST_LINE_MS = 15000
// A bridge that can't start, or stops while its link is still saved, is
// started again after a backoff: 2s, doubling to 2 minutes, with jitter.
const RETRY_MIN_MS = Number(process.env.WAKESHIFT_OPENCODE_RETRY_MIN_MS) || 2000
const RETRY_MAX_MS = Number(process.env.WAKESHIFT_OPENCODE_RETRY_MAX_MS) || 120000

export default {
  id: "wakeshift",
  server: async ({ client, directory }) => {
    const children = new Map() // session ID -> bridge process
    const tried = new Set() // sessions checked for a saved link this run
    const busy = new Set()
    const queue = new Map() // session ID -> [{id, text}]
    const acks = new Map() // wake ID -> send ack
    const retries = new Map() // session ID -> {timer, delay, input}
    const starting = new Map() // session ID -> bridge still starting
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
        if (!children.has(sid) && !starting.has(sid)) void start(sid, r.input)
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

    // Starts the session's bridge; resolves with its note for the model. A
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
        starting.set(sid, child)
        let first = true
        let connected = false
        const done = (text) => {
          if (!first) return
          first = false
          if (starting.get(sid) === child) starting.delete(sid)
          clearTimeout(timer)
          resolve(text)
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
            return done(typeof m.additionalContext === "string" && m.additionalContext ? m.additionalContext : undefined)
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

    return {
      "tool.execute.after": async (input, output) => {
        try {
          const beacon = output?._meta?.[KEY] // the MCP result's _meta; the model never sees it
          if (typeof beacon !== "string" || !Array.isArray(output.content)) return
          const sid = await root(input.sessionID)
          tried.add(sid)
          // A fresh beacon replaces a pending retry, and one still starting.
          settle(sid)
          const pending = starting.get(sid)
          if (pending) stop(pending)
          const text = await start(sid, { hook_event_name: "PostToolUse", session_id: sid, call_id: input.callID,
            tool_name: input.tool, tool_response: output })
          if (text) output.content.push({ type: "text", text })
        } catch {}
      },
      event: async ({ event }) => {
        try {
          const p = event.properties ?? {}
          if (event.type === "session.status" && p.status?.type === "busy") busy.add(p.sessionID)
          if (event.type === "session.idle" && p.sessionID) {
            busy.delete(p.sessionID)
            if (!children.has(p.sessionID) && !starting.has(p.sessionID) && !tried.has(p.sessionID)) {
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
            for (const c of [starting.get(sid), children.get(sid)]) if (c) stop(c)
            starting.delete(sid)
            children.delete(sid)
          }
        } catch {}
      },
      dispose: async () => {
        disposed = true
        for (const r of retries.values()) if (r.timer) clearTimeout(r.timer)
        retries.clear()
        for (const c of starting.values()) stop(c)
        starting.clear()
        for (const c of children.values()) stop(c)
        children.clear()
      },
    }
  },
}
`
