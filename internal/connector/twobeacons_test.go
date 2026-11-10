package connector

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Two results with a beacon in one turn (parallel first calls, #1100): the
// plugin starts a bridge for each and stops neither while it's still
// redeeming, so both beacons are redeemed (the connector takes one redeem
// at a time, the second handing back the first's link, and the core
// merges the first's session into the second's). Before, the second
// beacon stopped the bridge still redeeming the first, whose session was
// never connected. A bridge started without a beacon (an idle check) is
// still replaced at once (TestOpencodePluginRetries, "race").

const opencodeTwoDriver = `
import plugin from "./wakeshift.mjs"
import { readFileSync } from "node:fs"
const dir = process.env.FAKE_BRIDGE_DIR
const client = { session: { get: async () => ({ data: {} }), promptAsync: async () => ({ response: { status: 204 } }), list: async () => ({ data: [] }) } }
const hooks = await plugin.server({ client, directory: "/tmp/two" })
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const one = { _meta: { "wakeshift/connector": "a.b.c" }, content: [] }
const two = { _meta: { "wakeshift/connector": "d.e.f" }, content: [] }
await Promise.all([
  hooks["tool.execute.after"]({ sessionID: "ses_1", callID: "c1", tool: "app_watch" }, one),
  (async () => { await sleep(300); await hooks["tool.execute.after"]({ sessionID: "ses_1", callID: "c2", tool: "app_join" }, two) })(),
])
await sleep(500)
await hooks.dispose()
await sleep(200)
const all = readFileSync(dir + "/log", "utf8").trim().split("\n").filter(Boolean)
console.log(JSON.stringify({ starts: all.filter((l) => l.startsWith("start")).length, answers: all.filter((l) => l.startsWith("answer")).length,
  notes: [one, two].map((o) => o.content.map((c) => c.text).join("")) }))
`

const piTwoDriver = `
import ext from "./wakeshift.mjs"
import { readFileSync } from "node:fs"
const dir = process.env.FAKE_BRIDGE_DIR
const handlers = {}
const pi = { on: (name, h) => { handlers[name] = h }, sendMessage: () => {}, getAllTools: () => [] }
ext(pi)
const ctx = { mode: "tui", isIdle: () => true, sessionManager: { getSessionId: () => "ses_pi1", getBranch: () => [] } }
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const beacon = (x) => "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: " + x + "aaaaaaaa.bbbbbbbbbb.cccccccccc)"
const result = (tool, x) => ({ type: "tool_result", toolName: "mcp__app__" + tool, toolCallId: "call_" + tool, input: {}, isError: false,
  details: { server: "app", tool }, content: [{ type: "text", text: "Done.\n\n" + beacon(x) }] })
await handlers.session_start({ type: "session_start", reason: "startup" }, ctx)
const out = await Promise.all([
  handlers.tool_result(result("watch", "x"), ctx),
  (async () => { await sleep(300); return handlers.tool_result(result("join", "y"), ctx) })(),
])
await sleep(500)
await handlers.session_shutdown({ type: "session_shutdown" }, ctx)
await sleep(200)
const all = readFileSync(dir + "/log", "utf8").trim().split("\n").filter(Boolean)
console.log(JSON.stringify({ starts: all.filter((l) => l.startsWith("start")).length, answers: all.filter((l) => l.startsWith("answer")).length,
  notes: out.map((r) => (r?.content ?? []).filter((c) => c.text === "Wakeshift: connected").map((c) => c.text).join("")) }))
`

func TestTwoBeaconsInOneTurnBothRedeem(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	for _, tc := range []struct {
		name, driver string
		plugin       func(bin string) string
	}{
		{"opencode", opencodeTwoDriver, func(bin string) string { return strings.Replace(opencodePlugin, "__WAKESHIFT_BIN__", jsString(bin), 1) }},
		{"pi", piTwoDriver, PiExtension},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "wakeshift")
			os.WriteFile(bin, []byte(fakeBridge), 0o755)
			// Both still redeeming when the other's beacon comes (1.5s each).
			os.WriteFile(filepath.Join(dir, "plan"), []byte("slow\nslow"), 0o600)
			os.WriteFile(filepath.Join(dir, "n"), nil, 0o600)
			os.WriteFile(filepath.Join(dir, "log"), nil, 0o600)
			os.WriteFile(filepath.Join(dir, "wakeshift.mjs"), []byte(tc.plugin(bin)), 0o600)
			os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(tc.driver), 0o600)
			cmd := exec.Command(node, "driver.mjs")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "FAKE_BRIDGE_DIR="+dir, "WAKESHIFT_HOME="+filepath.Join(dir, "home"), "WAKESHIFT_OPENCODE_REARM_MS=600000")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			var got struct {
				Starts, Answers int
				Notes           []string
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if got.Starts != 2 || got.Answers != 2 {
				t.Fatalf("%d bridges started, %d redeemed: want both beacons redeemed (%s)", got.Starts, got.Answers, out)
			}
			for i, n := range got.Notes {
				if n != "Wakeshift: connected" {
					t.Errorf("result %d's note %q", i+1, n)
				}
			}
		})
	}
}
