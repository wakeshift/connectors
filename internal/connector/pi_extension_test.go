package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// piFakeBridge stands in for `wakeshift bridge --harness pi`: it logs every
// line the extension sends to $FAKE_BRIDGE_DIR/in, answers the first as a
// connected bridge does (with a note to ack for a tool result), then hands
// over one message.
const piFakeBridge = `#!/usr/bin/env node
const fs = require("node:fs")
const dir = process.env.FAKE_BRIDGE_DIR
let first = true
let rest = ""
process.stdin.on("data", (d) => {
  rest += d
  const lines = rest.split("\n")
  rest = lines.pop()
  for (const l of lines) {
    fs.appendFileSync(dir + "/in", l + "\n")
    if (!first) continue
    first = false
    const m = JSON.parse(l)
    const answer = m.hook_event_name === "PostToolUse"
      ? { additionalContext: "Wakeshift: connected. New messages for this conversation will reach it soon.", keep_alive: true, note_id: "note-1" }
      : { additionalContext: "", keep_alive: true }
    process.stdout.write(JSON.stringify(answer) + "\n")
    setTimeout(() => {
      process.stdout.write(JSON.stringify({ wake: { id: "wake-1", text: "<wakeshift_message from=\"chat\" trust=\"external\">hello pi</wakeshift_message>" } }) + "\n")
      fs.writeFileSync(dir + "/woke", "")
    }, 300)
  }
})
process.stdin.on("end", () => process.exit(0))
`

// piDriver loads the extension with a fake pi (ExtensionAPI and contexts) and
// plays a scenario, printing what pi was asked to do as JSON.
const piDriver = `
import ext from "./wakeshift.mjs"
import { readFileSync, existsSync } from "node:fs"
const scenario = process.argv[2]
const handlers = {}
const sent = []
const pi = {
  on: (name, h) => { handlers[name] = h; return () => {} },
  sendMessage: (m, o) => sent.push({ m, o }),
  getAllTools: () => [
    { name: "mcp__wakebench__watch", parameters: { type: "object", properties: { label: {}, session_key: {}, harness: {}, conversation_id: {} } } },
    { name: "mcp__wakebench__history", parameters: { type: "object", properties: { channel: {} } } },
  ],
}
ext(pi)
let idle = true
const ctx = { mode: scenario === "print" ? "print" : "tui", isIdle: () => idle, sessionManager: { getSessionId: () => "ses_pi1" } }
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const out = {}
const guard = "If this result's last line starts with \"(For Wakeshift's hook\", it's a one-time code."
const beacon = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: aaaaaaaaaa.bbbbbbbbbb.cccccccccc)"
const result = (toolName, details) => ({
  type: "tool_result", toolName, toolCallId: "call_1", input: {}, isError: false, details,
  content: [{ type: "text", text: "Watching.\n\n" + guard + "\n" + beacon }],
  structuredContent: { content: [{ type: "text", text: "Watching.\n\n" + guard + "\n" + beacon }], isError: false },
})
const bridged = () => existsSync(process.env.FAKE_BRIDGE_DIR + "/in") ? readFileSync(process.env.FAKE_BRIDGE_DIR + "/in", "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l)) : []
// A run: before_agent_start, then the model's reply (or an error), then settled.
const run = async (messageType, stopReason = "stop") => {
  const ev = { type: "before_agent_start", messageType, systemPromptOptions: { sections: {} } }
  await handlers.before_agent_start(ev, ctx)
  await handlers.message_end({ type: "message_end", message: { role: "assistant", stopReason } }, ctx)
  await handlers.agent_settled({ type: "agent_settled", aborted: false }, ctx)
  return ev.systemPromptOptions.sections.wakeshift_events ?? null
}

await handlers.session_start({ type: "session_start", reason: "startup" }, ctx)
if (scenario === "linked") {
  // Until the wake asked pi for a run (up to 10s: a loaded machine is slow).
  for (let i = 0; i < 200 && sent.length === 0; i++) await sleep(50)
  out.bridged = bridged()
  out.sent = sent.length
  out.section = await run("custom")
} else {
  // The app's subscribing tool learns the program and the session; another doesn't change.
  const call = { type: "tool_call", toolName: "mcp__wakebench__watch", toolCallId: "call_1", input: { label: "x", harness: "claude-code", conversation_id: "forged" } }
  await handlers.tool_call(call, ctx)
  const other = { type: "tool_call", toolName: "mcp__wakebench__history", toolCallId: "call_2", input: { channel: "c" } }
  await handlers.tool_call(other, ctx)
  out.input = call.input
  out.otherInput = other.input
  // A built-in tool's result with a beacon line: taken out, nothing started.
  out.builtin = await handlers.tool_result({ ...result("bash", undefined), structuredContent: { data: { note: "x " + beacon }, n: 1 } }, ctx)
  out.bridgedAfterBuiltin = bridged().length
  if (scenario === "busy" || scenario === "userrun") idle = false
  out.patch = await handlers.tool_result(result("mcp__wakebench__watch", { server: "wakebench", tool: "watch" }), ctx)
  // The wake arrives (up to 10s: a loaded machine is slow).
  for (let i = 0; scenario !== "print" && i < 200 && !existsSync(process.env.FAKE_BRIDGE_DIR + "/woke"); i++) await sleep(50)
  await sleep(200)
  if (scenario === "idle") for (let i = 0; i < 200 && sent.length === 0; i++) await sleep(50)
  out.sentWhileBusy = sent.length
  if (scenario === "busy") {
    idle = true
    await handlers.agent_settled({ type: "agent_settled", aborted: false }, ctx)
  }
  out.sent = sent.map((s) => ({ customType: s.m.customType, content: s.m.content, display: s.m.display, o: s.o }))
  if (scenario === "failed") {
    // The run errors before the model answers: nothing acknowledged.
    out.failedSection = await run("custom", "error")
    await sleep(200)
    out.ackedAfterFailure = bridged().some((m) => m.ack === "wake-1")
    // The bridge hands it out again (its ack timed out): the next run has it.
    out.section = null
  } else out.section = await run(scenario === "userrun" ? "user" : "custom")
  out.sectionNext = await run("user") // the next run: nothing queued
  await sleep(200)
  out.bridged = bridged()
}
await handlers.session_shutdown({ type: "session_shutdown" }, ctx)
console.log(JSON.stringify(out))
`

type piRun struct {
	Input, OtherInput map[string]any
	Builtin           *struct {
		Content           []map[string]any
		StructuredContent map[string]any
	}
	BridgedAfterBuiltin int
	Patch               *struct {
		Content           []map[string]any
		StructuredContent struct{ Content []map[string]any }
	}
	SentWhileBusy     int
	FailedSection     *string
	AckedAfterFailure bool
	Sent              json.RawMessage
	Section           *string
	SectionNext       *string
	Bridged           []map[string]any
}

func runPiExtension(t *testing.T, scenario string, setup func(home string)) (piRun, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-bridge")
	os.WriteFile(bin, []byte(piFakeBridge), 0o755)
	os.WriteFile(filepath.Join(dir, "wakeshift.mjs"), []byte(PiExtension(bin)), 0o600)
	os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(piDriver), 0o600)
	home := filepath.Join(dir, "home")
	if setup != nil {
		setup(home)
	}
	cmd := exec.Command(node, "driver.mjs", scenario)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FAKE_BRIDGE_DIR="+dir, "WAKESHIFT_HOME="+home)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var r piRun
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return r, string(b)
}

func textsOf(blocks []map[string]any) string {
	var s []string
	for _, b := range blocks {
		if t, _ := b["text"].(string); t != "" {
			s = append(s, t)
		}
	}
	return strings.Join(s, "\n")
}

// The extension, run in Node against a fake pi and a fake bridge.
func TestPiExtension(t *testing.T) {
	for _, scenario := range []string{"idle", "busy", "userrun"} {
		t.Run(scenario, func(t *testing.T) {
			r, raw := runPiExtension(t, scenario, nil)
			// The app's tool that declares Wakeshift's arguments gets the
			// session and the program, whatever the model passed; another tool
			// is left alone.
			if r.Input["harness"] != "pi" || r.Input["conversation_id"] != "ses_pi1" || r.Input["label"] != "x" {
				t.Fatalf("tool_call input %v", r.Input)
			}
			if _, set := r.OtherInput["conversation_id"]; set || len(r.OtherInput) != 1 {
				t.Fatalf("another tool's input %v", r.OtherInput)
			}
			// Every beacon line leaves what the model sees; a built-in tool's
			// starts nothing.
			if r.Builtin == nil || strings.Contains(textsOf(r.Builtin.Content), "wakeshift-beacon: a") || r.BridgedAfterBuiltin != 0 {
				t.Fatalf("built-in result: %s", raw)
			}
			// Its structured result comes back (not dropped), the token out of every field.
			if sc, _ := json.Marshal(r.Builtin.StructuredContent); r.Builtin.StructuredContent["n"] != float64(1) || strings.Contains(string(sc), "aaaaaaaaaa") {
				t.Fatalf("built-in structured result: %s", sc)
			}
			// The MCP tool's result: the beacon line out of content and
			// structuredContent (what a codemode script gets), the Guard line
			// kept, the connector's note added to both.
			if r.Patch == nil {
				t.Fatalf("no patch: %s", raw)
			}
			for name, blocks := range map[string][]map[string]any{"content": r.Patch.Content, "structuredContent": r.Patch.StructuredContent.Content} {
				txt := textsOf(blocks)
				if strings.Contains(txt, "wakeshift-beacon") || !strings.Contains(txt, "Watching.") || !strings.Contains(txt, "Wakeshift: connected") || !strings.Contains(txt, "one-time code") {
					t.Errorf("%s: %q", name, txt)
				}
			}
			// The bridge got the result whole (beacon and all), with the
			// session, the server and the bare tool, then the note's ack and
			// the wake's ack, once its run started.
			if len(r.Bridged) < 3 {
				t.Fatalf("bridge lines %v", r.Bridged)
			}
			first := r.Bridged[0]
			resp, _ := json.Marshal(first["tool_response"])
			if first["hook_event_name"] != "PostToolUse" || first["session_id"] != "ses_pi1" || first["server"] != "wakebench" || first["mcp_tool"] != "watch" ||
				first["tool_name"] != "mcp__wakebench__watch" || !strings.Contains(string(resp), "wakeshift-beacon") {
				t.Fatalf("first line %v", first)
			}
			if r.Bridged[1]["ack"] != "note-1" || r.Bridged[2]["ack"] != "wake-1" {
				t.Fatalf("acks %v", r.Bridged[1:])
			}
			// Delivered as a system prompt section, never mid-turn: a busy
			// session gets nothing until it settles (or its next run starts).
			if r.SentWhileBusy != map[string]int{"idle": 1, "busy": 0, "userrun": 0}[scenario] {
				t.Fatalf("sent while busy: %d", r.SentWhileBusy)
			}
			if r.Section == nil || !strings.Contains(*r.Section, "hello pi") || !strings.Contains(*r.Section, `trust="external"`) {
				t.Fatalf("section %v: %s", r.Section, raw)
			}
			if r.SectionNext != nil {
				t.Fatalf("the next run got it again: %q", *r.SectionNext)
			}
			var sent []struct {
				CustomType, Content string
				Display             bool
				O                   map[string]any
			}
			json.Unmarshal(r.Sent, &sent)
			if scenario == "userrun" {
				if len(sent) != 0 {
					t.Fatalf("a wake while the user's run was starting: %s", r.Sent)
				}
				return
			}
			// The run is started by the fixed custom message alone, which
			// never carries the messages, and waits for a run in between.
			if len(sent) != 1 || sent[0].CustomType != "wakeshift" || strings.Contains(sent[0].Content, "hello pi") || !strings.Contains(sent[0].Content, "wakeshift_events") ||
				sent[0].O["triggerTurn"] != true || sent[0].O["deliverAs"] != "followUp" {
				t.Fatalf("sent %s", r.Sent)
			}
		})
	}

	// A run that errors before the model answers acknowledges nothing, so
	// Wakeshift hands the messages out again.
	t.Run("failed", func(t *testing.T) {
		r, raw := runPiExtension(t, "failed", nil)
		if r.FailedSection == nil || !strings.Contains(*r.FailedSection, "hello pi") || r.AckedAfterFailure {
			t.Fatalf("failed run: %s", raw)
		}
	})

	// pi -p: beacon lines go, nothing connects.
	t.Run("print", func(t *testing.T) {
		r, raw := runPiExtension(t, "print", nil)
		if r.Patch == nil || strings.Contains(textsOf(r.Patch.Content), "wakeshift-beacon") || strings.Contains(textsOf(r.Patch.Content), "connected") || len(r.Bridged) != 0 {
			t.Fatalf("print mode: %s", raw)
		}
	})

	// A session connected before listens again when it starts (pi restarted,
	// /reload, --continue), with no beacon.
	t.Run("linked", func(t *testing.T) {
		r, raw := runPiExtension(t, "linked", func(home string) {
			h := sha256.Sum256([]byte("ses_pi1"))
			p := filepath.Join(home, "pi", "links", hex.EncodeToString(h[:])[:32]+".json")
			os.MkdirAll(filepath.Dir(p), 0o700)
			os.WriteFile(p, []byte("{}"), 0o600)
		})
		if len(r.Bridged) < 1 || r.Bridged[0]["hook_event_name"] != "SessionIdle" || r.Bridged[0]["session_id"] != "ses_pi1" {
			t.Fatalf("not listening again: %s", raw)
		}
		if r.Section == nil || !strings.Contains(*r.Section, "hello pi") {
			t.Fatalf("no wake after the restart: %s", raw)
		}
	})
}
