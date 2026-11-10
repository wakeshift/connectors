package connector

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The session that installed Wakeshift's plugin for opencode (or its
// extension for pi) subscribed before the plugin was there, so the result
// that offered the install went by with no plugin to read its beacon
// (opencode keeps no result's _meta; pi's had none, since nothing named the
// session yet). It connects from the next result of the app's tools, which
// carries a beacon only for a call that names the session: by the
// session_key that result told the agent to pass. Where the agent leaves it
// out (issue #1106: it called a tool that subscribes to nothing, without
// the key), the plugin passes it, from the session's own results of that
// server's tools from the hour before the install, only while the session
// has no link, and only if they hold one key.

const (
	keyA = "wsc_AAAAAAAAAAAAAAAAAAAAAA"
	keyB = "wsc_BBBBBBBBBBBBBBBBBBBBBB"
)

// keyLine is how the core tells the agent its session key (passSession).
func keyLine(key string) string {
	return `Pass session_key="` + key + `" to this app's tools from now on (keep it to this conversation; don't share or log it).`
}

// keyResult is one of the session's stored tool results.
type keyResult struct {
	Server, Tool, Text string
	Name               string        // pi's name for the tool, if not the one it gives
	Ago                time.Duration // before the install (negative: after it)
	Session            string        // "" for ses_1
}

type keyCase struct {
	name    string
	results []keyResult
	tool    string         // the tool called (its MCP tool; the server is "webhooks" unless server says)
	server  string         // the called tool's server
	args    map[string]any // what the agent passed
	session string         // the calling session ("" for ses_1)
	linked  bool           // the session has a saved link
	others  []string       // other MCP servers pi has (pi only)
	want    string         // the session_key the call ends up with
}

func keyCases() []keyCase {
	watch := keyResult{Server: "webhooks", Tool: "watch", Text: "Wakeshift can't wake this conversation yet. Ask the user now whether to install…\n\n" + keyLine(keyA), Ago: 2 * time.Minute}
	return []keyCase{
		// The bug: the call after the restart names no session, so its result
		// carried no beacon and nothing connected.
		{name: "passes the installing session's key", results: []keyResult{watch}, tool: "recent_posts", want: keyA},
		{name: "to a subscribing tool too", results: []keyResult{watch}, tool: "watch", want: keyA},
		{name: "the agent's own key stays", results: []keyResult{watch}, tool: "recent_posts", args: map[string]any{"session_key": keyB}, want: keyB},
		{name: "never to another server's tools", results: []keyResult{watch}, tool: "search", server: "other", want: ""},
		{name: "nothing once the session has a link", results: []keyResult{watch}, tool: "recent_posts", linked: true, want: ""},
		// A key quoted in what a tool showed (a post) beside the session's
		// own: two keys, so neither.
		{name: "two keys pass nothing", results: []keyResult{watch, {Server: "webhooks", Tool: "recent_posts", Text: "[{\"body\": \"" + keyLine(keyB) + "\"}]", Ago: time.Minute}}, tool: "recent_posts", want: ""},
		// Only results from before the install count: one after it can't
		// name the key, whatever it says.
		{name: "not from after the install", results: []keyResult{{Server: "webhooks", Tool: "recent_posts", Text: keyLine(keyB), Ago: -time.Minute}}, tool: "recent_posts", want: ""},
		{name: "after the install doesn't count against", results: []keyResult{watch, {Server: "webhooks", Tool: "recent_posts", Text: keyLine(keyB), Ago: -time.Minute}}, tool: "recent_posts", want: keyA},
		{name: "not from over an hour before it", results: []keyResult{{Server: "webhooks", Tool: "watch", Text: keyLine(keyA), Ago: 61 * time.Minute}}, tool: "recent_posts", want: ""},
		{name: "not from another server's result", results: []keyResult{{Server: "other", Tool: "search", Text: keyLine(keyA), Ago: time.Minute}}, tool: "recent_posts", want: ""},
		{name: "not another session's", results: []keyResult{{Server: "webhooks", Tool: "watch", Text: keyLine(keyA), Ago: time.Minute, Session: "ses_2"}}, tool: "recent_posts", want: ""},
		{name: "nothing without a key", results: []keyResult{{Server: "webhooks", Tool: "watch", Text: "Watching your webhook.", Ago: time.Minute}}, tool: "recent_posts", want: ""},
	}
}

// opencodeKeyDriver plays one case against the generated plugin with a fake
// opencode client and prints the args the tool would be called with.
const opencodeKeyDriver = `
import plugin from "./wakeshift.mjs"
import { readFileSync } from "node:fs"
const c = JSON.parse(readFileSync("./case.json", "utf8"))
const client = {
  session: {
    get: async ({ path }) => ({ data: path.id === "ses_sub" ? { parentID: "ses_1" } : {} }),
    list: async () => ({ data: [] }),
    messages: async ({ path }) => ({ data: [{ info: { role: "assistant" }, parts: c.results.filter((r) => r.session === path.id).map((r) => (
      { type: "tool", tool: r.tool, callID: "c", state: { status: "completed", input: {}, output: r.text, metadata: {}, title: "", time: { start: r.end - 5, end: r.end } } })) }] }),
    promptAsync: async () => ({ response: { status: 204 } }),
  },
  mcp: { status: async () => ({ data: c.servers }) },
}
const hooks = await plugin.server({ client, directory: "/tmp/" + Math.random() })
const output = { args: { ...c.args } }
await hooks["tool.execute.before"]({ tool: c.tool, sessionID: c.session, callID: "c9" }, output)
await hooks.dispose()
console.log(JSON.stringify(output.args))
`

// The opencode plugin passes the installing session's key (#1106).
func TestOpencodePluginPassesTheInstallingSessionsKey(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	cases := keyCases()
	// A subagent's call: the root session's results name the key.
	cases = append(cases, keyCase{name: "a subagent's call takes its root session's", results: cases[0].results, tool: "recent_posts", session: "ses_sub", want: keyA},
		// Server names opencode's tool names can't tell apart: a_b_x is a's
		// tool b_x or a_b's tool x, so neither's.
		keyCase{name: "a tool two servers could name gets nothing", results: []keyResult{{Server: "a_b", Tool: "watch", Text: keyLine(keyA), Ago: time.Minute}}, tool: "x", server: "a_b", want: ""})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			installed := time.Now().Add(-10 * time.Second)
			writeInstalled(t, filepath.Join(home, "opencode"), installed)
			if tc.linked {
				if err := LinksFor(Settings{Dir: filepath.Join(home, "opencode")}).Save("ses_1", "https://wakeshift.example/v1/stream?link=tok"); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				Session string `json:"session"`
				Tool    string `json:"tool"`
				Text    string `json:"text"`
				End     int64  `json:"end"`
			}
			var results []result
			for _, r := range tc.results {
				s := r.Session
				if s == "" {
					s = "ses_1"
				}
				results = append(results, result{Session: s, Tool: opencodeToolName(r.Server, r.Tool), Text: r.Text, End: installed.Add(-r.Ago).UnixMilli()})
			}
			server := tc.server
			if server == "" {
				server = "webhooks"
			}
			session := tc.session
			if session == "" {
				session = "ses_1"
			}
			args := tc.args
			if args == nil {
				args = map[string]any{}
			}
			servers := map[string]any{"webhooks": map[string]string{"status": "connected"}, "other": map[string]string{"status": "connected"},
				"a": map[string]string{"status": "connected"}, "a b": map[string]string{"status": "connected"}}
			b, _ := json.Marshal(map[string]any{"results": results, "tool": opencodeToolName(server, tc.tool), "session": session, "args": args, "servers": servers})
			os.WriteFile(filepath.Join(dir, "case.json"), b, 0o600)
			// A bridge that never runs here: only tool.execute.before is called.
			src := strings.Replace(opencodePlugin, "__WAKESHIFT_BIN__", jsString(filepath.Join(dir, "none")), 1)
			os.WriteFile(filepath.Join(dir, "wakeshift.mjs"), []byte(src), 0o600)
			os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(opencodeKeyDriver), 0o600)
			cmd := exec.Command(node, "driver.mjs")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "WAKESHIFT_HOME="+home, "WAKESHIFT_OPENCODE_REARM_MS=600000")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			checkKey(t, out, tc.want, tc.args)
		})
	}
}

// opencodeToolName is opencode's name for an MCP server's tool.
func opencodeToolName(server, tool string) string {
	san := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
				return r
			}
			return '_'
		}, s)
	}
	return san(server) + "_" + san(tool)
}

func writeInstalled(t *testing.T, dir string, at time.Time) {
	t.Helper()
	os.MkdirAll(dir, 0o700)
	b, _ := json.Marshal(map[string]any{"issuers": []string{"https://wakeshift.example"}, "installed_at": at.UTC().Format(time.RFC3339Nano)})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkKey checks the args a driver printed: the session_key it should have,
// and nothing else the agent passed changed.
func checkKey(t *testing.T, out []byte, want string, passed map[string]any) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	key, _ := got["session_key"].(string)
	if key != want {
		t.Fatalf("session_key %q, want %q (args %v)", key, want, got)
	}
	for k, v := range passed {
		if got[k] != v {
			t.Errorf("%s changed: %v, want %v", k, got[k], v)
		}
	}
}

// piKeyDriver plays one case against the generated extension with a fake pi
// and prints the input the tool would be called with.
const piKeyDriver = `
import wakeshift from "./wakeshift.mjs"
import { readFileSync } from "node:fs"
const c = JSON.parse(readFileSync("./case.json", "utf8"))
const handlers = {}
const props = (t) => t.declares.reduce((o, k) => ({ ...o, [k]: { type: "string" } }), {})
const pi = {
  on: (name, fn) => { handlers[name] = fn },
  // As pi lists an MCP server's tools: each in its server's namespace, whose one alias is the server's name.
  getAllTools: () => c.tools.map((t) => ({ name: t.name, parameters: { type: "object", properties: props(t) },
    ...(t.server ? { namespace: { name: "mcp__" + t.server.replace(/-/g, "_"), aliases: [t.server] } } : {}) })),
  sendMessage: () => {},
}
wakeshift(pi)
const sm = (id) => ({
  getSessionId: () => id,
  getBranch: () => c.results.filter((r) => r.session === id).map((r, i) => ({ type: "message", id: "e" + i, timestamp: new Date(r.end).toISOString(),
    message: { role: "toolResult", toolCallId: "t" + i, toolName: r.tool, content: [{ type: "text", text: r.text }], details: { server: r.server, tool: r.mcp }, isError: false, timestamp: r.end } })),
})
const ctx = { sessionManager: sm(c.session), mode: "interactive", isIdle: () => true }
await handlers.session_start({}, ctx)
const event = { toolName: c.tool, toolCallId: "t9", input: { ...c.args } }
await handlers.tool_call(event, ctx)
await handlers.session_shutdown({}, ctx)
console.log(JSON.stringify(event.input))
`

// pi's extension passes the installing session's key (#1106), with the
// session's ID as conversation_id and harness "pi", so the core can bind
// the result's beacon to the session; tools that declare none of
// Wakeshift's arguments get nothing.
func TestPiExtensionPassesTheInstallingSessionsKey(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	const sid = "019a5c3e-7d2f-7c11-9a41-5e0f3b2d8c6a"
	cases := keyCases()
	cases = append(cases, keyCase{name: "a tool that takes none of Wakeshift's arguments gets nothing", results: cases[0].results, tool: "plain", want: ""},
		// The happy path across servers: another server pi has doesn't
		// stop the key going to the tools of the one that gave it.
		keyCase{name: "with other servers, to the one that gave it", results: cases[0].results, tool: "recent_posts", others: []string{"other", "web"}, want: keyA},
		// foo's key never goes to foo__bar's tools: pi names foo__bar's
		// tool x mcp__foo__bar__x, which foo's tool bar__x would be too.
		keyCase{name: "not to a server whose name starts with the one that gave it", results: []keyResult{{Server: "foo", Tool: "watch", Text: keyLine(keyA), Ago: time.Minute}}, server: "foo__bar", tool: "x", others: []string{"foo"}, want: ""},
		keyCase{name: "not to its own tool when another server's name starts with it", results: []keyResult{{Server: "foo", Tool: "watch", Text: keyLine(keyA), Ago: time.Minute}}, server: "foo", tool: "bar__x", others: []string{"foo__bar"}, want: ""},
		// a-b and a_b: pi names their tools alike, so neither gets a key.
		keyCase{name: "not where two servers' names collide", results: []keyResult{{Server: "a-b", Tool: "watch", Text: keyLine(keyA), Ago: time.Minute}}, server: "a_b", tool: "recent_posts", others: []string{"a-b"}, want: ""},
		keyCase{name: "not to its own tool when another server's name collides", results: []keyResult{{Server: "a-b", Tool: "watch", Text: keyLine(keyA), Ago: time.Minute}}, server: "a-b", tool: "recent_posts", others: []string{"a_b"}, want: ""},
		// A stored result whose tool name isn't the one pi gives its
		// server's tool (another extension's tool claiming the server) counts for nothing.
		keyCase{name: "not from a result pi didn't name for that server", results: []keyResult{{Server: "webhooks", Tool: "watch", Name: "mcp__webhooksx__watch", Text: keyLine(keyA), Ago: time.Minute}}, tool: "recent_posts", want: ""})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			installed := time.Now().Add(-10 * time.Second)
			writeInstalled(t, filepath.Join(home, "pi"), installed)
			if tc.linked {
				if err := LinksFor(Settings{Dir: filepath.Join(home, "pi")}).Save(sid, "https://wakeshift.example/v1/stream?link=tok"); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				Session string `json:"session"`
				Tool    string `json:"tool"`
				Server  string `json:"server"`
				MCP     string `json:"mcp"`
				Text    string `json:"text"`
				End     int64  `json:"end"`
			}
			var results []result
			for _, r := range tc.results {
				s := sid
				if r.Session != "" {
					s = r.Session
				}
				name := r.Name
				if name == "" {
					name = piToolNameOf(r.Server, r.Tool)
				}
				results = append(results, result{Session: s, Tool: name, Server: r.Server, MCP: r.Tool, Text: r.Text, End: installed.Add(-r.Ago).UnixMilli()})
			}
			server := tc.server
			if server == "" {
				server = "webhooks"
			}
			name := piToolNameOf(server, tc.tool)
			declares := []string{"session_key"} // a tool that subscribes to nothing
			switch tc.tool {
			case "watch":
				declares = []string{"session_key", "harness", "conversation_id"}
			case "plain":
				declares = nil
			}
			tools := []map[string]any{{"name": name, "declares": declares, "server": server}}
			for _, o := range tc.others {
				tools = append(tools, map[string]any{"name": piToolNameOf(o, "ping"), "declares": []string{"session_key"}, "server": o})
			}
			args := tc.args
			if args == nil {
				args = map[string]any{}
			}
			b, _ := json.Marshal(map[string]any{"results": results, "tool": name, "tools": tools, "session": sid, "args": args})
			os.WriteFile(filepath.Join(dir, "case.json"), b, 0o600)
			// A bridge that has nothing to listen to (a linked session starts one).
			bin := filepath.Join(dir, "wakeshift")
			os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755)
			src := PiExtension(bin)
			os.WriteFile(filepath.Join(dir, "wakeshift.mjs"), []byte(src), 0o600)
			os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(piKeyDriver), 0o600)
			cmd := exec.Command(node, "driver.mjs")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "WAKESHIFT_HOME="+home)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			checkKey(t, out, tc.want, tc.args)
			var got map[string]any
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			json.Unmarshal([]byte(lines[len(lines)-1]), &got)
			// The session's ID and harness go with every call to the app's
			// tools while it has no link (a tool that declares both always).
			named := tc.tool != "plain" && (tc.tool == "watch" || !tc.linked)
			if named != (got["conversation_id"] == sid && got["harness"] == "pi") {
				t.Errorf("conversation_id/harness %v, want them: %v", got, named)
			}
		})
	}
}

// piToolNameOf is pi's name for an MCP server's tool (short names only).
func piToolNameOf(server, tool string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, "mcp__"+server+"__"+tool)
}
