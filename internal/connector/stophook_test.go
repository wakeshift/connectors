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

// installRoundTrip: install keeps what was there and adds ours (with a
// backup), twice changes nothing, uninstall leaves what was there.
func installRoundTrip(t *testing.T, spec Spec, path string, kept, added []string) {
	t.Helper()
	before, _ := os.ReadFile(path)
	const bin = "/Users/ada/.wakeshift/bin/wakeshift"
	var out bytes.Buffer
	if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !has(string(b), append(append(kept, added...), bin+" hook --harness "+spec.Name)...) {
		t.Fatalf("installed: %s", b)
	}
	if len(before) > 0 && !has(out.String(), ".before-wakeshift-") {
		t.Fatalf("no backup: %s", out.String())
	}
	out.Reset()
	spec.Install(bin, "https://app.wakeshift.ai", &out)
	if b2, _ := os.ReadFile(path); !bytes.Equal(b, b2) || !has(out.String(), "already") {
		t.Fatalf("twice: %s\n%s", out.String(), b2)
	}
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "--harness") || !has(string(b), kept...) {
		t.Fatalf("after uninstall: %s", b)
	}
	if len(before) > 0 && !jsonEqual(before, b) {
		t.Fatalf("not as it was:\n%s\n%s", before, b)
	}
}

// Antigravity's real hook inputs (agy 1.4.0): no event name, camelCase, and
// no result in PostToolUse.
const (
	agyPostTool = `{"artifactDirectoryPath":"/tmp/brain/2b9d08bb","conversationId":"2b9d08bb-7acc-4459-9956-69064f8d5869","error":"","modelName":"gemini-3.8-flash-low","stepIdx":4,"toolCall":{"args":{"Arguments":{"note":"probing"},"ServerName":"mcplog","ToolName":"probe","toolAction":"Calling probe tool","toolSummary":"Call probe tool"},"name":"call_mcp_tool"},"transcriptPath":"TRANSCRIPT","workspacePaths":["/tmp/proj"]}`
	agyStop     = `{"artifactDirectoryPath":"/tmp/brain/2b9d08bb","conversationId":"2b9d08bb-7acc-4459-9956-69064f8d5869","error":"","executionNum":1,"fullyIdle":true,"modelName":"m","terminationReason":"NO_TOOL_CALL","transcriptPath":"/tmp/t.jsonl","workspacePaths":["/tmp/proj"]}`
	agyPreInv   = `{"artifactDirectoryPath":"/tmp/brain/2b9d08bb","conversationId":"2b9d08bb-7acc-4459-9956-69064f8d5869","initialNumSteps":1,"invocationNum":0,"modelName":"m","transcriptPath":"/tmp/t.jsonl","workspacePaths":["/tmp/proj"]}`
)

func agyTranscript(t *testing.T, text string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "logs")
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "transcript_full.jsonl")
	step := func(i int, src, typ, content string) string {
		b, _ := json.Marshal(map[string]any{"step_index": i, "source": src, "type": typ, "status": "DONE", "content": content})
		return string(b) + "\n"
	}
	os.WriteFile(p, []byte(step(0, "USER_EXPLICIT", "USER_INPUT", "call probe "+BeaconText("x"))+
		step(3, "MODEL", "PLANNER_RESPONSE", "")+step(4, "MODEL", "GENERIC", "Created At: t\nCompleted At: t\nprobe ok\n"+text)), 0o600)
	return p
}

// A beacon naming the conversation, in the step's text, connects it; the
// note waits for the next turn's start (PostToolUse can't add context).
func TestAntigravityConnects(t *testing.T) {
	ws := newFakeWakeshift(t)
	spec, _ := Lookup("antigravity")
	set := Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}
	p := newFakePoller()
	h := &Hook{Format: spec.Format, Verifier: &Verifier{Settings: set}, Poller: p, Links: LinksFor(set), When: spec.When}
	conv := "2b9d08bb-7acc-4459-9956-69064f8d5869"
	run := func(event, in string) string {
		h.EventName = event
		var out bytes.Buffer
		h.Run(t.Context(), strings.NewReader(in), &out, &out)
		return out.String()
	}
	tr := agyTranscript(t, BeaconText(ws.beacon(t, conv, nil)))
	if out := run("post-tool", strings.Replace(agyPostTool, "TRANSCRIPT", tr, 1)); out != "" || h.Links.Load(conv) == "" {
		t.Fatalf("post-tool: %q, link %q", out, h.Links.Load(conv))
	}
	out := run("pre-invocation", agyPreInv)
	if !has(out, `"injectSteps"`, `"systemMessage"`, "Wakeshift: connected") {
		t.Fatalf("pre-invocation: %s", out)
	}
	if out := run("pre-invocation", agyPreInv); out != "" {
		t.Fatalf("the note twice: %s", out)
	}
	// Another conversation's beacon (in the same text) is ignored.
	tr = agyTranscript(t, BeaconText(ws.beacon(t, "someone-else", nil)))
	n := len(ws.redemptions())
	run("post-tool", strings.Replace(agyPostTool, "TRANSCRIPT", tr, 1))
	if len(ws.redemptions()) != n {
		t.Fatal("redeemed another conversation's beacon")
	}
	// A beacon the user typed (step 0) isn't the result's.
	in, _ := antigravityFormat{}.ParseEvent("post-tool", []byte(strings.Replace(strings.Replace(agyPostTool, "TRANSCRIPT", tr, 1), `"stepIdx":4`, `"stepIdx":0`, 1)))
	if BeaconIn(in.ToolResult) != "" {
		t.Fatal("read the user's step")
	}
	// Stop continues with what's waiting.
	p.add(h.Links.Load(conv), msg("d1", "hello agy"))
	out = run("stop", agyStop)
	var c struct{ Decision, Reason string }
	if json.Unmarshal([]byte(out), &c) != nil || c.Decision != "continue" || !strings.Contains(c.Reason, "hello agy") {
		t.Fatalf("stop: %s", out)
	}
	// Non-MCP tools are ignored.
	in, _ = antigravityFormat{}.ParseEvent("post-tool", []byte(`{"conversationId":"c","stepIdx":2,"toolCall":{"name":"view_file"}}`))
	if in.Event != OtherEvent {
		t.Fatalf("%+v", in)
	}
}

func TestAntigravityInstall(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, ".gemini", "config", "hooks.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"mine":{"Stop":[{"type":"command","command":"notify.sh","timeout":5}]}}`), 0o600)
	spec, _ := Lookup("antigravity")
	installRoundTrip(t, spec, path, []string{`"mine"`, `"notify.sh"`}, []string{`"wakeshift"`, `"matcher": "call_mcp_tool"`, `--event stop`})
	// Ours is flat for Stop (a nested Stop entry makes agy drop the file).
	spec.Install("/bin/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{})
	b, _ := os.ReadFile(path)
	var f map[string]map[string][]map[string]any
	json.Unmarshal(b, &f)
	if f["wakeshift"]["Stop"][0]["command"] == nil || f["wakeshift"]["PreInvocation"][0]["command"] == nil {
		t.Fatalf("%s", b)
	}
	// A file agy already ignores isn't added to.
	os.WriteFile(path, []byte(`{"bad":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]}}`), 0o600)
	if err := spec.Install("/bin/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "without a command") {
		t.Fatalf("%v", err)
	}
}

// Claude Code shows an async PostToolUse hook's context only at the next
// user prompt, so the "connected" note also comes with the next wake (once).
func TestConnectedNoteComesWithTheNextWake(t *testing.T) {
	ws := newFakeWakeshift(t)
	spec, _ := Lookup("claude-code")
	set := Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}
	p := newFakePoller()
	h := &Hook{Format: spec.Format, Verifier: &Verifier{Settings: set}, Poller: p, Links: LinksFor(set), When: spec.When}
	conv := "6b1d4c1e-0f0e-4a43-9c55-0c3b7b1f2d11"
	res, _ := json.Marshal(map[string]any{"_meta": map[string]any{"wakeshift/connector": ws.beacon(t, conv, nil)}})
	out, ok := h.toolResult(t.Context(), Input{Event: ToolResult, Conversation: conv, ToolCallID: "toolu_1", ToolResult: res})
	if !ok || !strings.Contains(string(out.Stdout), "Wakeshift: connected") || h.Links.Load(conv) == "" {
		t.Fatalf("post-tool: %q %v", out.Stdout, ok)
	}
	p.add(h.Links.Load(conv), msg("d1", "first"))
	out, ok = h.listen(t.Context(), Input{Event: Stop, Conversation: conv}, 2*time.Second)
	if !ok || !strings.Contains(string(out.Stderr), "Wakeshift: connected") || !strings.Contains(string(out.Stderr), "first") {
		t.Fatalf("the first wake: %q", out.Stderr)
	}
	p.add(h.Links.Load(conv), msg("d2", "second"))
	out, _ = h.listen(t.Context(), Input{Event: Stop, Conversation: conv}, 2*time.Second)
	if strings.Contains(string(out.Stderr), "Wakeshift: connected") || !strings.Contains(string(out.Stderr), "second") {
		t.Fatalf("the second wake: %q", out.Stderr)
	}
}
