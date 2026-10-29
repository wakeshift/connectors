package connector

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The exact rule each harness gets for one app tool, and where: one
// server's one tool, never a wildcard or a whole server.
func TestToolRuleText(t *testing.T) {
	home := setHome(t)
	for name, want := range map[string]struct{ path, rule string }{
		"claude-code": {filepath.Join(home, ".claude", "settings.json"), "mcp__agentchat__post"},
		"cursor":      {filepath.Join(home, ".cursor", "cli-config.json"), "Mcp(agentchat:post)"},
	} {
		spec, _ := Lookup(name)
		if spec.Tools == nil {
			t.Fatalf("%s: no tool rules", name)
		}
		if got := spec.Tools.Rule("agentchat", "post"); got != want.rule {
			t.Errorf("%s: %s, want %s", name, got, want.rule)
		}
		if got := spec.Tools.Path(); got != want.path {
			t.Errorf("%s: %s, want %s", name, got, want.path)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "cc"))
	if spec, _ := Lookup("claude-code"); spec.Tools.Path() != filepath.Join(home, "cc", "settings.json") || claudeJSON() != filepath.Join(home, "cc", ".claude.json") {
		t.Errorf("CLAUDE_CONFIG_DIR: %s %s", spec.Tools.Path(), claudeJSON())
	}
	// Nothing where there are no exact per-tool rules Wakeshift can write
	// (Antigravity: unverified whether mcp(<server>/<tool>) is exact;
	// Copilot: no global rules; Codex: woken by its connector, and its
	// read-only tools don't prompt; the rest don't prompt).
	for _, name := range []string{"antigravity", "copilot", "codex", "opencode", "hermes", "openclaw"} {
		if spec, ok := Lookup(name); ok && spec.Tools != nil {
			t.Errorf("%s has tool rules", name)
		}
	}
	// "__" would make mcp__<server>__<tool> name another server's tool.
	for _, bad := range []string{"", "a*", "a:b", "a/b", "a b", "a(b)", "a?", "[a]", "a.b", "a__b", strings.Repeat("a", 65)} {
		if ValidToolName(bad) {
			t.Errorf("%q passes", bad)
		}
	}
}

func TestParseAllowTools(t *testing.T) {
	app, tools, err := ParseAllowTools("agentchat:history,list_channels,post,history")
	if err != nil || app != "agentchat" || !slices.Equal(tools, []string{"history", "list_channels", "post"}) {
		t.Fatalf("%s %v %v", app, tools, err)
	}
	for _, bad := range []string{"", "agentchat", "agentchat:", "Agent:post", "agentchat:po*st", "agentchat:post;rm", "a:post"} {
		if _, _, err := ParseAllowTools(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

func allowList(t *testing.T, path string) []string {
	t.Helper()
	var f struct {
		Permissions struct{ Allow []string }
	}
	json.Unmarshal([]byte(readFile(t, path)), &f)
	return f.Permissions.Allow
}

// toolHook is a harness's hook as the binary runs it, trusting ws.
func toolHook(t *testing.T, name string, ws *fakeWakeshift) *Hook {
	t.Helper()
	spec, _ := Lookup(name)
	h := spec.NewHook(nil, newFakePoller())
	h.Verifier.Settings.Issuers = []string{ws.URL}
	h.Bin = testBin
	h.StopWait = 0
	return h
}

// probeResult runs Claude Code's PostToolUse hook on a result of the
// wsprobe server's probe tool carrying a beacon for its session.
func probeResult(t *testing.T, h *Hook, ws *fakeWakeshift) string {
	t.Helper()
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	conv := "c95dca59-8b63-4ebb-a70e-8d69fde72b92"
	writeFile(t, tr, strings.Replace(claudeTranscriptLine, "BEACON", ws.beacon(t, conv, nil), 1)+"\n")
	return runAs(h, "", strings.Replace(claudePostToolUse, "TRANSCRIPT", tr, 1))
}

const claudeJSONWithProbe = `{"numStartups": 3, "oauthAccount": {"x": 1}, "projects": {"/private/tmp/proj": {"mcpServers": {"wsprobe": {"type": "http", "url": "https://chat.example/mcp"}}}, "/other": {"mcpServers": {}}}}`

// The user's yes at install covers the tools the question named; the
// rules are written when the hooks connect a conversation to the app,
// for the server the harness names, keeping the file's other settings,
// with a backup, once.
func TestClaudeToolRules(t *testing.T) {
	home := setHome(t)
	ws := newFakeWakeshift(t)
	ws.app = &RedeemedApp{ID: "chat", Name: "Chat", PreapproveTools: []string{"probe", "history"}}
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{"model": "opus", "permissions": {"allow": ["Bash(ls)", "mcp__wsprobe__history"], "deny": ["Bash(rm *)"]}}`)
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSONWithProbe)
	spec, _ := Lookup("claude-code")

	// The yes, with a tool the app doesn't name (post): never written.
	var out bytes.Buffer
	if err := spec.AllowTools("chat", []string{"probe", "history", "post"}, &out); err != nil {
		t.Fatal(err)
	}
	if !has(out.String(), "once Wakeshift's hooks connect a conversation") || len(backupsOf(settings)) != 0 {
		t.Fatalf("before a connect: %s", out.String())
	}

	h := toolHook(t, "claude-code", ws)
	got := probeResult(t, h, ws)
	if !has(got, "Wakeshift: connected", "Claude Code now runs Chat's probe and history tools without asking, as the user agreed", settings,
		"allow --harness claude-code --app chat --none") {
		t.Fatalf("connect: %s", got)
	}
	want := []string{"Bash(ls)", "mcp__wsprobe__history", "mcp__wsprobe__probe"}
	if got := allowList(t, settings); !slices.Equal(got, want) {
		t.Fatalf("allow %v, want %v", got, want)
	}
	s := readFile(t, settings)
	if !has(s, `"model": "opus"`, `"deny"`, "Bash(rm *)") || strings.Contains(s, "post") || strings.Contains(s, "__*") {
		t.Fatalf("settings: %s", s)
	}
	if b := backupsOf(settings); len(b) != 1 {
		t.Fatalf("backups %v", b)
	}
	// The user's own rule isn't recorded as ours.
	for _, r := range readRules(h.Verifier.Settings) {
		if r.Rule == "mcp__wsprobe__history" {
			t.Fatal("recorded the user's own rule")
		}
	}

	// Again: nothing to change, no backup, nothing said about it.
	got = probeResult(t, h, ws)
	if strings.Contains(got, "now runs") || len(backupsOf(settings)) != 1 || !slices.Equal(allowList(t, settings), want) {
		t.Fatalf("again: %s %v", got, allowList(t, settings))
	}
	out.Reset()
	spec.AllowTools("chat", []string{"probe"}, &out)
	if !has(out.String(), "already runs Chat's probe and history tools without asking") || len(backupsOf(settings)) != 1 {
		t.Fatalf("allow again: %s", out.String())
	}

	// The app stops naming history: only ours could go, and it isn't ours.
	// It stops naming probe: its rule goes.
	ws.app.PreapproveTools = []string{"history"}
	probeResult(t, h, ws)
	if got := allowList(t, settings); !slices.Equal(got, []string{"Bash(ls)", "mcp__wsprobe__history"}) {
		t.Fatalf("after the app dropped probe: %v", got)
	}
	ws.app.PreapproveTools = []string{"probe", "history"}
	probeResult(t, h, ws)

	// Uninstalling removes exactly what Wakeshift added.
	writeFile(t, filepath.Join(home, ".wakeshift", "bin", "wakeshift"), "#!/bin/sh\n")
	out.Reset()
	if err := spec.Uninstall(testBin, &out); err != nil {
		t.Fatal(err)
	}
	if got := allowList(t, settings); !slices.Equal(got, []string{"Bash(ls)", "mcp__wsprobe__history"}) {
		t.Fatalf("after uninstall: %v (%s)", got, out.String())
	}
	if !has(readFile(t, settings), `"model": "opus"`, "Bash(rm *)") || !has(out.String(), "Removed Wakeshift's rule mcp__wsprobe__probe") {
		t.Fatalf("after uninstall: %s %s", readFile(t, settings), out.String())
	}
	if exists(filepath.Join(home, ".wakeshift", "claude-code", toolsFile)) || len(readRules(h.Verifier.Settings)) != 0 {
		t.Fatal("tools.json or rules.json left")
	}
}

// Without the user's yes, the first connect offers exactly the app's
// tools, with the command for each answer; a no removes the rules and
// isn't asked again; a later yes writes them.
func TestToolOffer(t *testing.T) {
	home := setHome(t)
	ws := newFakeWakeshift(t)
	ws.app = &RedeemedApp{ID: "chat", Name: "Chat", PreapproveTools: []string{"probe", "history"}}
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSONWithProbe)
	h := toolHook(t, "claude-code", ws)
	got := probeResult(t, h, ws)
	cmd := "`" + testBin + " allow --harness claude-code --app chat --tools probe,history`"
	if !has(got, "Optional: Claude Code asks before each call to Chat's probe and history tools.", "Only if they agree, run "+cmd,
		"If they say no, run `"+testBin+" allow --harness claude-code --app chat --none`") {
		t.Fatalf("offer: %s", got)
	}
	if exists(settings) {
		t.Fatal("wrote rules without a yes")
	}
	// A reconnect of the same conversation doesn't offer again.
	if got := probeResult(t, h, ws); strings.Contains(got, "Optional") {
		t.Fatalf("offered on a reconnect: %s", got)
	}
	// The yes: written at once (the server is known).
	spec, _ := Lookup("claude-code")
	var out bytes.Buffer
	if err := spec.AllowTools("chat", []string{"probe", "history"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := allowList(t, settings); !slices.Equal(got, []string{"mcp__wsprobe__probe", "mcp__wsprobe__history"}) || !has(out.String(), "Pre-approved in "+settings) {
		t.Fatalf("yes: %v %s", got, out.String())
	}
	// The no: removed, including what the install made, and the file the
	// rules alone made goes with them on uninstall.
	out.Reset()
	if err := spec.DeclineTools("chat", &out); err != nil {
		t.Fatal(err)
	}
	if s := readFile(t, settings); strings.Contains(s, "mcp__") || strings.Contains(s, "permissions") || !has(out.String(), "no longer pre-approved", "won't offer them again") {
		t.Fatalf("no: %s %s", s, out.String())
	}
	h.Links.Forget("c95dca59-8b63-4ebb-a70e-8d69fde72b92")
	if got := probeResult(t, h, ws); strings.Contains(got, "Optional") || strings.Contains(readFile(t, settings), "mcp__") {
		t.Fatalf("offered after a no: %s", got)
	}
}

// The server's name is used only if the harness's MCP settings agree it's
// one server; otherwise nothing is written, and the note says why.
func TestToolServerResolution(t *testing.T) {
	home := setHome(t)
	claude := toolHook(t, "claude-code", newFakeWakeshift(t))
	in := Input{Server: "wsprobe", ToolName: "mcp__wsprobe__probe", Dirs: []string{"/private/tmp/proj"}}
	check := func(name string, h *Hook, in Input, wantServer, wantWhy string) {
		t.Helper()
		server, why := h.toolServer(in)
		if server != wantServer || !strings.Contains(why, wantWhy) || (wantWhy == "") != (why == "") {
			t.Errorf("%s: %q %q, want %q %q", name, server, why, wantServer, wantWhy)
		}
	}
	check("no settings", claude, in, "", "no MCP server named \"wsprobe\"")
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSONWithProbe)
	check("a project's server", claude, in, "wsprobe", "")
	check("no server named", claude, Input{ToolName: "mcp__wsprobe__probe"}, "", "couldn't tell")
	check("a name a rule can't hold", claude, Input{Server: "ws probe", ToolName: "mcp__ws_probe__probe"}, "", "characters an exact rule can't hold")
	check("a name unlike its tools'", claude, Input{Server: "wsprobe", ToolName: "mcp__other__probe"}, "", "differs from the one in its tools' names")
	// The same name for another server elsewhere: a rule could match it.
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"wsprobe": {"type": "http", "url": "https://evil.example/mcp"}}, "projects": {"/private/tmp/proj": {"mcpServers": {"wsprobe": {"type": "http", "url": "https://chat.example/mcp"}}}}}`)
	check("two servers of one name", claude, in, "", "more than one MCP server is named")
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"wsprobe": {"type": "http", "url": "https://chat.example/mcp"}}}`)
	writeFile(t, filepath.Join(home, "proj", ".mcp.json"), `{"mcpServers": {"wsprobe": {"command": "npx", "args": ["evil"]}}}`)
	check("a project file's other server", claude, Input{Server: "wsprobe", ToolName: "mcp__wsprobe__probe", Dirs: []string{filepath.Join(home, "proj")}}, "", "more than one")
	check("a relative folder isn't read", claude, Input{Server: "wsprobe", ToolName: "mcp__wsprobe__probe", Dirs: []string{"proj"}}, "wsprobe", "")
	writeFile(t, filepath.Join(home, ".claude.json"), `{not json`)
	check("unreadable settings", claude, in, "", "can't be read")

	// Cursor: case-insensitively (its rules are), and the URL the call went
	// to must be the one its settings name.
	cursor := toolHook(t, "cursor", newFakeWakeshift(t))
	cin := Input{Server: "agentchat", ServerURL: "https://chat.example/mcp"}
	check("cursor, no settings", cursor, cin, "", "no MCP server named")
	writeFile(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers": {"agentchat": {"url": "https://chat.example/mcp"}}}`)
	check("cursor", cursor, cin, "agentchat", "")
	check("cursor, another URL", cursor, Input{Server: "agentchat", ServerURL: "https://evil.example/mcp"}, "", "isn't the one the call went to")
	writeFile(t, filepath.Join(home, "ws", ".cursor", "mcp.json"), `{"mcpServers": {"AgentChat": {"url": "https://evil.example/mcp"}}}`)
	check("cursor, the name in another case elsewhere", cursor, Input{Server: "agentchat", ServerURL: "https://chat.example/mcp", Dirs: []string{filepath.Join(home, "ws")}}, "", "more than one")
}

// No rules where the server can't be resolved: the user's yes stays
// recorded, the note says why, and no file is touched.
func TestNoToolRulesUnresolved(t *testing.T) {
	home := setHome(t)
	ws := newFakeWakeshift(t)
	ws.app = &RedeemedApp{ID: "chat", Name: "Chat", PreapproveTools: []string{"probe"}}
	spec, _ := Lookup("claude-code")
	spec.AllowTools("chat", []string{"probe"}, &bytes.Buffer{})
	h := toolHook(t, "claude-code", ws)
	got := probeResult(t, h, ws) // no ~/.claude.json naming wsprobe
	if !has(got, "Wakeshift: connected", "didn't pre-approve Chat's probe tools in Claude Code, as the user agreed, because no MCP server named", "Claude Code asks before each call") {
		t.Fatalf("note: %s", got)
	}
	if exists(filepath.Join(home, ".claude", "settings.json")) || len(readRules(h.Verifier.Settings)) != 0 {
		t.Fatal("wrote a rule")
	}
	// A redeem from a Wakeshift that says nothing about the app: nothing.
	ws.app = nil
	h.Links.Forget("c95dca59-8b63-4ebb-a70e-8d69fde72b92")
	if got := probeResult(t, h, ws); strings.Contains(got, "pre-approve") || strings.Contains(got, "Optional") {
		t.Fatalf("without app: %s", got)
	}
	// What the redeem says about another app than the beacon's is ignored.
	ws.app = &RedeemedApp{ID: "other", Name: "Other", PreapproveTools: []string{"probe"}}
	h.Links.Forget("c95dca59-8b63-4ebb-a70e-8d69fde72b92")
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSONWithProbe)
	if got := probeResult(t, h, ws); strings.Contains(got, "Other") || exists(filepath.Join(home, ".claude", "settings.json")) {
		t.Fatalf("another app: %s", got)
	}
}

// Cursor: afterMCPExecution names the call's server and URL; the
// postToolUse for the same call ID connects, pins the server, and writes
// Mcp(<server>:<tool>) into cli-config.json, which must exist.
func TestCursorToolRules(t *testing.T) {
	home := setHome(t)
	ws := newFakeWakeshift(t)
	ws.app = &RedeemedApp{ID: "chat", Name: "Chat", PreapproveTools: []string{"probe"}}
	writeFile(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers": {"probe": {"url": "https://chat.example/mcp"}}}`)
	spec, _ := Lookup("cursor")
	spec.AllowTools("chat", []string{"probe"}, &bytes.Buffer{})
	h := toolHook(t, "cursor", ws)
	conv := "27188d7e-37d9-4a60-b001-465a6950b24b"
	after := `{"conversation_id":"` + conv + `","hook_event_name":"afterMCPExecution","tool_name":"probe","tool_input":"{}","result_json":"{}","duration":3,"tool_use_id":"078f8363-6783-4d14-bb39-c6a4d5cd1361","mcp_server_name":"probe","mcp_server_url":"https://chat.example/mcp","workspace_roots":["/p"]}`
	connect := func() string {
		t.Helper()
		if out := runAs(h, "", after); out != "" {
			t.Fatalf("afterMCPExecution said %q", out)
		}
		return runAs(h, "", cursorOutput("joined #general\n\n"+BeaconText(ws.beacon(t, conv, nil))))
	}
	// cli-config.json isn't there: nothing written, and said.
	if got := connect(); !has(got, "connected", "didn't pre-approve Chat's probe tools in Cursor", "isn't there yet") {
		t.Fatalf("no cli-config: %s", got)
	}
	cfg := filepath.Join(home, ".cursor", "cli-config.json")
	writeFile(t, cfg, `{"version": 1, "permissions": {"allow": ["Shell(ls)"], "deny": []}, "model": {"modelId": "x"}}`)
	h.Links.Forget(conv)
	if got := connect(); !has(got, "Cursor now runs Chat's probe tools without asking") {
		t.Fatalf("connect: %s", got)
	}
	if got := allowList(t, cfg); !slices.Equal(got, []string{"Shell(ls)", "Mcp(probe:probe)"}) || !has(readFile(t, cfg), `"modelId": "x"`) {
		t.Fatalf("cli-config: %s", readFile(t, cfg))
	}
	if !has(readFile(t, PinsFor(h.Verifier.Settings).Path), `"probe": "chat"`) {
		t.Fatal("the server wasn't pinned")
	}
	// A postToolUse without its afterMCPExecution names no server: no rule.
	if s, _ := h.calledServer(Input{Conversation: conv, ToolCallID: "other"}); s != "" {
		t.Fatalf("server for another call: %s", s)
	}
}

// The install's own hooks for Cursor include afterMCPExecution.
func TestCursorInstallsAfterMCPExecution(t *testing.T) {
	home := setHome(t)
	spec, _ := Lookup("cursor")
	install(t, spec, testBin)
	s := readFile(t, filepath.Join(home, ".cursor", "hooks.json"))
	if !has(s, `"afterMCPExecution"`, `"postToolUse"`, `"stop"`) || strings.Count(s, "hook --harness cursor") != 3 {
		t.Fatalf("hooks.json: %s", s)
	}
	uninstall(t, spec, testBin)
	if exists(filepath.Join(home, ".cursor", "hooks.json")) {
		t.Fatalf("hooks.json left: %s", readFile(t, filepath.Join(home, ".cursor", "hooks.json")))
	}
}
