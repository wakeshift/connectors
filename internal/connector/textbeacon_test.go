package connector

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runAs(h *Hook, event, in string) string {
	h.EventName = event
	var out bytes.Buffer
	h.Run(context0(), strings.NewReader(in), &out, &out)
	return out.String()
}

func specHook(t *testing.T, name string, ws *fakeWakeshift, p *fakePoller) *Hook {
	t.Helper()
	spec, _ := Lookup(name)
	set := Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}
	return &Hook{Format: spec.Format, Verifier: &Verifier{Settings: set}, Poller: p, Links: LinksFor(set), When: spec.When,
		Unbound: spec.Unbound, StopWait: 100 * time.Millisecond}
}

func jsonText(s string) string { b, _ := json.Marshal(s); return string(b) }

// Copilot CLI's real hook inputs (1.0.98: camelCase, no event name).
const (
	copilotPreCall  = `{"sessionId":"1b47c0de-2f6e-4a8e-9c1d-6a1e2b3c4d5e","timestamp":1792349176755,"cwd":"/p","toolCallId":"call_d2d4f3dd6c","serverName":"probe","toolName":"probe","arguments":{},"_meta":{"other/key":1}}`
	copilotPostTool = `{"sessionId":"1b47c0de-2f6e-4a8e-9c1d-6a1e2b3c4d5e","timestamp":1792349176900,"cwd":"/p","toolName":"probe-probe","toolArgs":{},"toolResult":{"resultType":"success","textResultForLlm":TEXT}}`
	copilotStop     = `{"sessionId":"1b47c0de-2f6e-4a8e-9c1d-6a1e2b3c4d5e","timestamp":1,"cwd":"/p","transcriptPath":"/x/events.jsonl","stopReason":"end_turn","stop_hook_active":false}`
)

func TestCopilot(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := specHook(t, "copilot", ws, p)
	conv := "1b47c0de-2f6e-4a8e-9c1d-6a1e2b3c4d5e"
	// The server's own name for the session goes to it in _meta, keeping
	// earlier hooks' keys; never the session ID itself.
	var pre struct {
		MetaToUse map[string]any `json:"metaToUse"`
	}
	if json.Unmarshal([]byte(runAs(h, "preMcpToolCall", copilotPreCall)), &pre) != nil || pre.MetaToUse["other/key"] != 1.0 {
		t.Fatalf("pre: %+v", pre)
	}
	name, _ := pre.MetaToUse[CopilotConversationMeta].(string)
	if name == "" || strings.Contains(name, conv) {
		t.Fatalf("the server was told %q", name)
	}
	post := func(text string) string { return strings.Replace(copilotPostTool, "TEXT", jsonText(text), 1) }
	// The beacon connects, and leaves the text the model sees.
	out := runAs(h, "postToolUse", post("probe ok\n\n"+BeaconText(ws.beacon(t, name, nil))))
	var r struct {
		AdditionalContext string `json:"additionalContext"`
		ModifiedResult    struct {
			ResultType       string `json:"resultType"`
			TextResultForLlm string `json:"textResultForLlm"`
		} `json:"modifiedResult"`
	}
	if json.Unmarshal([]byte(out), &r) != nil || !strings.Contains(r.AdditionalContext, "connected") ||
		r.ModifiedResult.TextResultForLlm != "probe ok" || r.ModifiedResult.ResultType != "success" || h.Links.Load(conv) == "" {
		t.Fatalf("post: %s", out)
	}
	if red := ws.redemptions(); len(red) != 1 || red[0] != "wst_t@"+name {
		t.Fatalf("redeemed %v", red)
	}
	// Another conversation's beacon: not acted on, but still out of the model's view.
	r.AdditionalContext = ""
	out = runAs(h, "postToolUse", post("ok\n"+BeaconText(ws.beacon(t, "another", nil))))
	if json.Unmarshal([]byte(out), &r) != nil || r.ModifiedResult.TextResultForLlm != "ok" || r.AdditionalContext != "" {
		t.Fatalf("other: %s", out)
	}
	if out := runAs(h, "postToolUse", post("plain")); out != "" {
		t.Fatalf("plain: %s", out)
	}
	p.add(h.Links.Load(conv), msg("d1", "hi copilot"))
	var stop struct{ Decision, Reason string }
	if json.Unmarshal([]byte(runAs(h, "agentStop", copilotStop)), &stop) != nil || stop.Decision != "block" || !strings.Contains(stop.Reason, "hi copilot") {
		t.Fatalf("stop: %+v", stop)
	}
}

// copilotCall is a preMcpToolCall to server's tool, returning the name the
// server was told for the session.
func copilotCall(t *testing.T, h *Hook, server, tool string) string {
	t.Helper()
	in := strings.NewReplacer(`"serverName":"probe"`, `"serverName":"`+server+`"`, `"toolName":"probe"`, `"toolName":"`+tool+`"`).Replace(copilotPreCall)
	var pre struct {
		MetaToUse map[string]any `json:"metaToUse"`
	}
	json.Unmarshal([]byte(runAs(h, "preMcpToolCall", in)), &pre)
	name, _ := pre.MetaToUse[CopilotConversationMeta].(string)
	return name
}

func copilotResult(tool, text string) string {
	return strings.Replace(strings.Replace(copilotPostTool, "TEXT", jsonText(text), 1), `"toolName":"probe-probe"`, `"toolName":"`+tool+`"`, 1)
}

// No MCP server learns the Copilot session ID, and a beacon is acted on only
// from the result of the server it names, pinned to its app: a server can't
// attach the conversation through another server's result, a built-in
// tool's, or with the session ID learned some other way.
func TestCopilotBeaconsPinnedToServer(t *testing.T) {
	ws := newFakeWakeshift(t)
	ws.tool = "watch" // chat's tool below
	h := specHook(t, "copilot", ws, newFakePoller())
	conv := "1b47c0de-2f6e-4a8e-9c1d-6a1e2b3c4d5e"
	chat, evil := copilotCall(t, h, "chat", "watch"), copilotCall(t, h, "evil", "fetch")
	if chat == "" || evil == "" || chat == evil || chat == conv || evil == conv {
		t.Fatalf("names: chat %q, evil %q", chat, evil)
	}
	if again := copilotCall(t, h, "chat", "watch"); again != chat {
		t.Fatalf("not stable: %q, %q", chat, again)
	}
	// A call that doesn't name its server is told nothing.
	if out := runAs(h, "preMcpToolCall", strings.Replace(copilotPreCall, `"serverName":"probe",`, "", 1)); out != "" {
		t.Fatalf("unnamed server: %s", out)
	}
	refused := func(name, tool, token string) {
		t.Helper()
		out := runAs(h, "postToolUse", copilotResult(tool, "ok\n"+BeaconText(token)))
		if strings.Contains(out, "connected") || h.Links.Load(conv) != "" || len(ws.redemptions()) != 0 {
			t.Fatalf("%s: acted on (%s)", name, out)
		}
		if !strings.Contains(out, `"textResultForLlm":"ok"`) {
			t.Fatalf("%s: beacon left for the model (%s)", name, out)
		}
	}
	refused("the session ID itself", "chat-watch", ws.beacon(t, conv, nil))
	refused("evil's beacon in chat's result", "chat-watch", ws.beaconFor(t, evil, "evil"))
	refused("chat's name in a built-in tool's result", "bash", ws.beacon(t, chat, nil))
	refused("chat's name in evil's result", "evil-fetch", ws.beacon(t, chat, nil))
	refused("a tool no call was recorded for", "other-tool", ws.beacon(t, chat, nil))

	// chat's own beacon from chat's result connects, and pins chat to its app.
	out := runAs(h, "postToolUse", copilotResult("chat-watch", "ok\n"+BeaconText(ws.beacon(t, chat, nil))))
	if !strings.Contains(out, "connected") || h.Links.Load(conv) == "" {
		t.Fatalf("chat: %s", out)
	}
	// Then another app's beacon from chat's server is refused by the pins.
	out = runAs(h, "postToolUse", copilotResult("chat-watch", "ok\n"+BeaconText(ws.beaconFor(t, chat, "other"))))
	if !strings.Contains(out, "didn't connect") || len(ws.redemptions()) != 1 {
		t.Fatalf("other app from chat's server: %s", out)
	}
}

func TestCopilotInstall(t *testing.T) {
	home := setHome(t)
	spec, _ := Lookup("copilot")
	var out bytes.Buffer
	const bin = "/Users/ada/.wakeshift/bin/wakeshift"
	if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".copilot", "hooks", "wakeshift.json")
	b, _ := os.ReadFile(path)
	var f struct {
		Version int
		Hooks   map[string][]struct {
			Type, Bash string
			TimeoutSec int
		}
	}
	if json.Unmarshal(b, &f) != nil || f.Version != 1 || f.Hooks["preMcpToolCall"][0].Bash != bin+" hook --harness copilot --event preMcpToolCall" ||
		f.Hooks["agentStop"][0].TimeoutSec == 0 || len(f.Hooks) != 3 {
		t.Fatalf("%s", b)
	}
	out.Reset()
	spec.Install(bin, "https://app.wakeshift.ai", &out)
	if !has(out.String(), "already") {
		t.Fatal(out.String())
	}
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("still there")
	}
	t.Setenv("COPILOT_HOME", filepath.Join(home, "c"))
	spec.Install(bin, "https://app.wakeshift.ai", &out)
	if _, err := os.Stat(filepath.Join(home, "c", "hooks", "wakeshift.json")); err != nil {
		t.Fatal("COPILOT_HOME ignored")
	}
	os.WriteFile(filepath.Join(home, "c", "hooks", "wakeshift.json"), []byte(`{"version":1,"hooks":{}}`), 0o600)
	if err := spec.Uninstall(bin, &out); err == nil {
		t.Fatal("removed a file that isn't ours")
	}
}

// Cursor's real hook inputs (cursor-agent 2026.09.23).
const (
	cursorPostTool = `{"conversation_id":"27188d7e-37d9-4a60-b001-465a6950b24b","generation_id":"27188d7e-37d9-4a60-b001-465a6950b24b","model":"gemini-3.7-flash","tool_name":"MCP:probe","tool_input":{"note":"hi"},"tool_output":OUTPUT,"duration":3.209,"tool_use_id":"078f8363-6783-4d14-bb39-c6a4d5cd1361","session_id":"27188d7e-37d9-4a60-b001-465a6950b24b","hook_event_name":"postToolUse","cursor_version":"2026.09.23-86fc751","workspace_roots":["/p"],"user_email":"ada@example.com","transcript_path":"/x.jsonl"}`
	cursorStop     = `{"conversation_id":"27188d7e-37d9-4a60-b001-465a6950b24b","generation_id":"1d0e1a2a","model":"m","status":"completed","loop_count":1,"session_id":"27188d7e-37d9-4a60-b001-465a6950b24b","hook_event_name":"stop","cursor_version":"2026.09.23-86fc751","workspace_roots":["/p"],"transcript_path":"/x.jsonl"}`
)

func cursorOutput(text string) string {
	b, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": false})
	return strings.Replace(cursorPostTool, "OUTPUT", jsonText(string(b)), 1)
}

func TestCursor(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := specHook(t, "cursor", ws, p)
	conv := "27188d7e-37d9-4a60-b001-465a6950b24b"
	// A beacon line naming no conversation is refused (only stripped): any
	// tool's text (a web page, an issue comment) could carry one an
	// attacker minted.
	if out := runAs(h, "", cursorOutput("joined #general\n\n"+BeaconText(unbound(t, ws)))); strings.Contains(out, "connected") || h.Links.Load(conv) != "" {
		t.Fatalf("unbound text beacon: %s", out)
	}
	out := runAs(h, "", cursorOutput("joined #general\n\n"+BeaconText(ws.beacon(t, conv, nil))))
	var r struct {
		AdditionalContext    string `json:"additional_context"`
		UpdatedMCPToolOutput string `json:"updated_mcp_tool_output"`
	}
	if json.Unmarshal([]byte(out), &r) != nil || !strings.Contains(r.AdditionalContext, "connected") || r.UpdatedMCPToolOutput != "joined #general" || h.Links.Load(conv) == "" {
		t.Fatalf("post: %s", out)
	}
	if out := runAs(h, "", cursorOutput("nothing here")); out != "" {
		t.Fatalf("plain: %s", out)
	}
	// Not an MCP tool: ignored.
	if out := runAs(h, "", strings.Replace(cursorOutput(BeaconText(unbound(t, ws))), `"MCP:probe"`, `"Shell"`, 1)); out != "" {
		t.Fatalf("shell: %s", out)
	}
	p.add(h.Links.Load(conv), msg("d1", "hi cursor"))
	var stop struct {
		FollowupMessage string `json:"followup_message"`
	}
	if json.Unmarshal([]byte(runAs(h, "", cursorStop)), &stop) != nil || !strings.Contains(stop.FollowupMessage, "hi cursor") {
		t.Fatalf("stop: %+v", stop)
	}
}

func TestCursorInstall(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	// Without "version" (as some tools write it): Cursor ignores the file;
	// the install adds it and says the other hooks will now run.
	os.WriteFile(path, []byte(`{"hooks":{"stop":[{"command":"notify.sh"}],"afterFileEdit":[{"command":"fmt.sh"}]}}`), 0o600)
	spec, _ := Lookup("cursor")
	var out bytes.Buffer
	const bin = "/Users/ada/.wakeshift/bin/wakeshift"
	if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !has(string(b), `"version": 1`, `"notify.sh"`, `"fmt.sh"`, `"loop_limit": null`, `"matcher": "^MCP:"`) || !has(out.String(), "its 2 other hooks") {
		t.Fatalf("%s\n%s", out.String(), b)
	}
	// A binary installed elsewhere replaces ours in place; nothing doubles.
	spec.Install("/opt/wakeshift/wakeshift", "https://app.wakeshift.ai", &out)
	b, _ = os.ReadFile(path)
	var f struct {
		Hooks map[string][]map[string]any
	}
	json.Unmarshal(b, &f)
	if len(f.Hooks["stop"]) != 2 || f.Hooks["stop"][1]["command"] != "/opt/wakeshift/wakeshift hook --harness cursor" || len(f.Hooks["postToolUse"]) != 1 {
		t.Fatalf("moved: %s", b)
	}
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "--harness") || !has(string(b), `"notify.sh"`, `"fmt.sh"`) {
		t.Fatalf("after: %s", b)
	}
	// CURSOR_CONFIG_DIR doesn't move user hooks.
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(home, "elsewhere"))
	os.Remove(path)
	spec.Install(bin, "https://app.wakeshift.ai", &out)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
