package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeBridge stands in for `wakeshift bridge`: each run takes the next step
// from plan (one per line) and logs "start <pid>" / "exit <pid>" to log.
//
//	fail     exits 1 before answering (a transient failure)
//	nolink   answers keep_alive false and exits 0 (nothing to listen to)
//	ok       answers keep_alive true and stays until stdin closes, then exits 0
//	drop     answers keep_alive true, then exits 1 (stopped listening)
//	revoked  answers keep_alive true, then exits 0 (its link was removed)
//	later    answers keep_alive false with retry (Wakeshift was unreachable)
//	slow     answers keep_alive true after 1.5s, then stays
const fakeBridge = `#!/usr/bin/env node
const fs = require("node:fs")
const dir = process.env.FAKE_BRIDGE_DIR
const n = Number(fs.readFileSync(dir + "/n", "utf8") || "0")
fs.writeFileSync(dir + "/n", String(n + 1))
const step = (fs.readFileSync(dir + "/plan", "utf8").split("\n")[n] || "ok").trim()
fs.appendFileSync(dir + "/log", "start " + process.pid + "\n")
process.on("exit", () => fs.appendFileSync(dir + "/log", "exit " + process.pid + "\n"))
process.stdin.once("data", (d) => {
  try { fs.appendFileSync(dir + "/sessions", JSON.parse(String(d).split("\n")[0]).session_id + "\n") } catch {}
  if (step === "fail") process.exit(1)
  if (step === "nolink") { process.stdout.write(JSON.stringify({ additionalContext: "", keep_alive: false }) + "\n"); process.exit(0) }
  if (step === "later") { process.stdout.write(JSON.stringify({ additionalContext: "", keep_alive: false, retry: true }) + "\n"); process.exit(0) }
  const answer = () => {
    fs.appendFileSync(dir + "/log", "answer " + process.pid + "\n")
    process.stdout.write(JSON.stringify({ additionalContext: "Wakeshift: connected", keep_alive: true, note_id: "note-" + process.pid }) + "\n")
  }
  // The plugin's ack of the note, once it's in the tool result.
  let rest = ""
  process.stdin.on("data", (c) => {
    rest += c
    for (const l of rest.split("\n").slice(0, -1)) {
      try { if (JSON.parse(l).ack === "note-" + process.pid) fs.appendFileSync(dir + "/noteacks", process.pid + "\n") } catch {}
    }
    rest = rest.split("\n").pop()
  })
  if (step === "slow") setTimeout(answer, 1500)
  else answer()
  if (step === "drop") setTimeout(() => process.exit(1), 100)
  if (step === "revoked") setTimeout(() => process.exit(0), 100)
  process.stdin.on("end", () => process.exit(0))
  process.stdin.resume()
})
`

// The driver loads the generated plugin and plays one scenario against it
// with a fake opencode client, then prints what happened.
const pluginDriver = `
import plugin from "./wakeshift.mjs"
import { existsSync, readFileSync } from "node:fs"
const dir = process.env.FAKE_BRIDGE_DIR
// session.list: this project's sessions (a subagent's has a parentID).
const sessions = [{ id: "ses_old", time: { updated: 1 } }, { id: "ses_1", time: { updated: 3 } }, { id: "ses_2", time: { updated: 2 } }, { id: "ses_sub", parentID: "ses_1" }]
const client = { session: { get: async () => ({ data: {} }), promptAsync: async () => ({ response: { status: 204 } }),
  ...(process.argv[2] === "load" ? { list: async () => ({ data: sessions }) } : {}) } }
const hooks = await plugin.server({ client, directory: "/tmp" })
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
// Waits (up to 10s) until a bridge has started: a fixed sleep raced it on a
// loaded machine.
const started = async () => {
  for (let i = 0; i < 500; i++) {
    if (readFileSync(dir + "/log", "utf8").includes("start ")) return
    await sleep(20)
  }
  throw new Error("no bridge started within 10s")
}
const scenario = process.argv[2]
let note
if (scenario === "race") {
  // A bridge still starting (from an idle check) is replaced by a fresh beacon's.
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_1" } } })
  await started() // booted and waiting to answer
  const output = { _meta: { "wakeshift/connector": "a.b.c" }, content: [] }
  await hooks["tool.execute.after"]({ sessionID: "ses_1", callID: "c1", tool: "join" }, output)
  note = output.content.map((c) => c.text).join("")
} else if (scenario === "dispose") {
  // Disposed while a bridge is still starting: it's stopped and never connects.
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_1" } } })
  await started()
  await hooks.dispose()
} else if (scenario === "load") {
  // opencode (re)started: nothing else happens; linked sessions listen again.
} else if (scenario === "beacon") {
  const output = { _meta: { "wakeshift/connector": "a.b.c" }, content: [] }
  await hooks["tool.execute.after"]({ sessionID: "ses_1", callID: "c1", tool: "join" }, output)
  note = output.content.map((c) => c.text).join("")
} else {
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_1" } } })
  // More idle events while a retry may be pending must not start a second bridge.
  for (let i = 0; i < 5; i++) {
    await sleep(20)
    await hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_1" } } })
  }
}
await sleep(Number(process.argv[3] || 1500))
await hooks.dispose()
await sleep(200)
const log = readFileSync(dir + "/log", "utf8").trim().split("\n").filter(Boolean)
let live = 0, maxLive = 0
for (const l of log) {
  if (l.startsWith("answer")) continue
  live += l.startsWith("start") ? 1 : -1
  maxLive = Math.max(maxLive, live)
}
const count = (w) => log.filter((l) => l.startsWith(w)).length
const bridged = existsSync(dir + "/sessions") ? readFileSync(dir + "/sessions", "utf8").trim().split("\n").filter(Boolean) : []
console.log(JSON.stringify({ starts: count("start"), answers: count("answer"), maxLive, note, sessions: bridged.join(","), noteAcks: existsSync(dir + "/noteacks") ? readFileSync(dir + "/noteacks", "utf8").trim().split("\n").filter(Boolean).length : 0 }))
`

// The bridge asks the plugin to retry a beacon only when Wakeshift couldn't
// be reached, never when the redemption was refused.
func TestOpencodeBridgeRetryFlag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		retry  bool
	}{
		{"server error", 503, "", true},
		{"rate limited", 429, "", true},
		{"ticket used", 404, "unknown_ticket", false},
		{"grant revoked", 403, "no_grant", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := newFakeWakeshift(t)
			ws.mu.Lock()
			ws.status, ws.code = tc.status, tc.code
			ws.mu.Unlock()
			set := Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}
			spec, _ := Lookup("opencode")
			h := &Hook{Format: spec.Format, Verifier: &Verifier{Settings: set}, Poller: newFakePoller(), Links: LinksFor(set),
				When: spec.When, Unbound: spec.Unbound}
			var out bytes.Buffer
			(&Bridge{Hook: h}).Run(context.Background(), strings.NewReader(opencodeLine("ses_1", unbound(t, ws))), &out)
			var first struct {
				KeepAlive bool `json:"keep_alive"`
				Retry     bool `json:"retry"`
			}
			json.Unmarshal(bytes.TrimSpace(out.Bytes()), &first)
			if first.KeepAlive || first.Retry != tc.retry {
				t.Fatalf("first line %s", out.String())
			}
			if h.OnTransient != nil {
				t.Fatal("the bridge changed the shared hook")
			}
		})
	}
}

// When opencode starts (a restart, opencode --continue), the sessions of
// its project that have a saved link listen again at once, the newest
// first, without waiting for a message; a subagent's session, or one with
// no link, gets no bridge.
func TestOpencodePluginRearmsAtLoad(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "wakeshift")
	os.WriteFile(bin, []byte(fakeBridge), 0o755)
	os.WriteFile(filepath.Join(dir, "plan"), []byte("ok\nok"), 0o600)
	os.WriteFile(filepath.Join(dir, "n"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "log"), nil, 0o600)
	home := filepath.Join(dir, "home")
	links := LinksFor(Settings{Dir: filepath.Join(home, "opencode")})
	for _, s := range []string{"ses_1", "ses_old", "ses_sub"} {
		if err := links.Save(s, "https://wakeshift.example/v1/stream?link=tok-"+s); err != nil {
			t.Fatal(err)
		}
	}
	src := strings.Replace(opencodePlugin, "__WAKESHIFT_BIN__", jsString(bin), 1)
	os.WriteFile(filepath.Join(dir, "wakeshift.mjs"), []byte(src), 0o600)
	os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(pluginDriver), 0o600)
	cmd := exec.Command(node, "driver.mjs", "load", "800")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FAKE_BRIDGE_DIR="+dir, "WAKESHIFT_HOME="+home, "WAKESHIFT_OPENCODE_REARM_MS=50")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var got struct {
		Starts, Answers int
		Sessions        string
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// The bridges start in that order, but each records itself as it reads
	// its first line, so compare them as a set.
	bridged := strings.Split(got.Sessions, ",")
	slices.Sort(bridged)
	if strings.Join(bridged, ",") != "ses_1,ses_old" || got.Answers != 2 {
		t.Errorf("bridges for %q (%d connected), want ses_1,ses_old (2)", got.Sessions, got.Answers)
	}
}

func TestOpencodePluginRetries(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	cases := []struct {
		name, scenario, plan string
		wantStarts           int
		wantNote             string
	}{
		// A bridge that can't start is retried until one connects.
		{"transient then connects", "beacon", "fail\nfail\nok", 3, ""},
		// The first answer is the model's note; a later retry has no tool call to add it to.
		{"connects first time", "beacon", "ok", 1, "Wakeshift: connected"},
		// Nothing to listen to: not retried.
		{"no link", "idle", "nolink", 1, ""},
		// Connected, then its link was removed: not retried.
		{"revoked", "idle", "revoked", 1, ""},
		// Connected, then stopped listening with the link still saved: started again.
		{"dropped", "idle", "drop\nok", 2, ""},
		// Wakeshift unreachable while redeeming the beacon: the same beacon is tried again.
		{"redeem unreachable", "beacon", "later\nlater\nok", 3, ""},
		// A fresh beacon stops a bridge still starting: only the new one connects.
		{"beacon replaces a starting bridge", "race", "slow\nok", 2, "Wakeshift: connected"},
		// Disposed while starting: the bridge is stopped and never connects.
		{"dispose stops a starting bridge", "dispose", "slow", 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "wakeshift")
			if err := os.WriteFile(bin, []byte(fakeBridge), 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(dir, "plan"), []byte(tc.plan), 0o600)
			os.WriteFile(filepath.Join(dir, "n"), nil, 0o600)
			os.WriteFile(filepath.Join(dir, "log"), nil, 0o600)
			src := strings.Replace(opencodePlugin, "__WAKESHIFT_BIN__", jsString(bin), 1)
			os.WriteFile(filepath.Join(dir, "wakeshift.mjs"), []byte(src), 0o600)
			os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(pluginDriver), 0o600)
			cmd := exec.Command(node, "driver.mjs", tc.scenario)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "FAKE_BRIDGE_DIR="+dir, "WAKESHIFT_HOME="+filepath.Join(dir, "home"),
				"WAKESHIFT_OPENCODE_RETRY_MIN_MS=40", "WAKESHIFT_OPENCODE_RETRY_MAX_MS=160")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			var got struct {
				Starts, Answers, MaxLive int
				Note                     string
				NoteAcks                 int
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if got.Starts != tc.wantStarts {
				t.Errorf("bridges started %d, want %d", got.Starts, tc.wantStarts)
			}
			if tc.scenario == "race" {
				if got.Answers != 1 {
					t.Errorf("%d bridges connected, want 1 (the replaced one must be stopped)", got.Answers)
				}
			} else if tc.scenario == "dispose" {
				if got.Answers != 0 {
					t.Error("a bridge connected after dispose")
				}
			} else if got.MaxLive > 1 {
				t.Errorf("%d bridges ran at once for one session", got.MaxLive)
			}
			if got.Note != tc.wantNote {
				t.Errorf("note %q, want %q", got.Note, tc.wantNote)
			}
			// A note added to the result is acked to its bridge, once; with no
			// tool result to add it to, nothing is.
			if want := map[bool]int{true: 1, false: 0}[tc.wantNote != ""]; got.NoteAcks != want {
				t.Errorf("note acked %d times, want %d", got.NoteAcks, want)
			}
		})
	}
}
