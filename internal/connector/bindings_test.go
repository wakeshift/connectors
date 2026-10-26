package connector

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/jws"
)

// Beacons are bound to the tool whose result carries them: anyone can have
// an app mint a beacon naming a conversation whose ID they know (here, for
// a session of their own), and post its text where that conversation reads
// it, such as a chat message a read tool relays. The hooks act on a beacon
// only from the result of the tool it names, and in text only as the line
// that ends the result, where Wakeshift puts it after everything the app
// wrote. These run each harness's recorded hook input shapes.

// minted is a beacon the fake Wakeshift signed: for conversation (or ""),
// naming tool ("" for none) and carrying ticket.
func minted(t *testing.T, ws *fakeWakeshift, conversation, tool, ticket string) string {
	t.Helper()
	tok, err := jws.Sign(ws.key, "k1", beacon.Type, beacon.Claims{Issuer: ws.URL, Audience: beacon.Audience, Thread: conversation, App: "chat", Tool: tool,
		Redeem: ws.URL + "/v1/connector/redeem", Ticket: ticket, IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// structured is a CallToolResult as Agent Chat's history returns it: its
// structured content as JSON text (a message's text JSON-escaped in it).
func structured(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// cursorResultOf is Cursor's recorded postToolUse input for tool's result
// with text as its one text block.
func cursorResultOf(tool, text string) string {
	return strings.Replace(cursorOutput(text), `"MCP:probe"`, `"MCP:`+tool+`"`, 1)
}

func TestCursorBeaconsBoundToTheirTool(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "cursor", ws, newFakePoller())
	conv := "27188d7e-37d9-4a60-b001-465a6950b24b"
	// Mallory had the app mint a beacon for Ada's conversation (her own
	// session's), from its watch tool, and posted it in a channel.
	planted := minted(t, ws, conv, "watch", "wst_mallory")
	posted := "hi all\n\n" + BeaconText(planted)

	refused := func(name, in string) {
		t.Helper()
		out := runAs(h, "", in)
		if strings.Contains(out, "connected") || len(ws.redemptions()) != 0 || h.Links.Load(conv) != "" {
			t.Fatalf("%s: acted on (%s)", name, out)
		}
		var r struct {
			Updated string `json:"updated_mcp_tool_output"`
		}
		if json.Unmarshal([]byte(out), &r) != nil || strings.Contains(r.Updated, "wakeshift-beacon") {
			t.Fatalf("%s: the beacon line left for the model (%s)", name, out)
		}
	}
	// Ada reads the channel: the message comes back in history's result,
	// JSON-escaped in its structured content.
	refused("relayed in a read tool's structured result", cursorResultOf("history",
		structured(map[string]any{"messages": []any{map[string]string{"from": "mallory", "text": posted}}})))
	// A read tool that returns plain text, ending with the message: the
	// line ends the result, but the result is history's, not watch's.
	refused("ending another tool's text", cursorResultOf("history", "mallory: "+posted))
	// In the watch tool's own result, but inside the app's text (a message
	// the result relays), not ending it.
	refused("inside the named tool's text", cursorResultOf("watch", "Recent:\n"+posted+"\n\nWatching #general."))
	// A beacon minted on history's own result, relayed in history's result
	// (inside it): not the line that ends it.
	refused("its own tool, relayed", cursorResultOf("history",
		structured(map[string]any{"messages": []any{map[string]string{"text": "x\n" + BeaconText(minted(t, ws, conv, "history", "wst_mallory"))}}})))
	// A beacon naming no tool (an app on an SDK from before beacons did):
	// never from text.
	refused("naming no tool", cursorResultOf("watch", "Watching.\n\n"+BeaconText(minted(t, ws, conv, "", "wst_old"))))

	// Ada's own watch result: what it relays first, then her beacon, last.
	// Hers connects, not Mallory's.
	out := runAs(h, "", cursorResultOf("watch", "Waiting for you:\n"+posted+"\n\nWatching #general.\n\n"+BeaconText(minted(t, ws, conv, "watch", "wst_ada"))))
	if !strings.Contains(out, "Wakeshift: connected") || h.Links.Load(conv) == "" {
		t.Fatalf("Ada's own beacon: %s", out)
	}
	if r := ws.redemptions(); len(r) != 1 || r[0] != "wst_ada@"+conv {
		t.Fatalf("redeemed %v", r)
	}
}

// Copilot: the tool is the one the conversation's preMcpToolCall recorded
// for the result (its server and tool).
func TestCopilotBeaconsBoundToTheirTool(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "copilot", ws, newFakePoller())
	conv := "1b47c0de-2f6e-4a8e-9c1d-6a1e2b3c4d5e"
	chat := copilotCall(t, h, "chat", "watch")
	copilotCall(t, h, "chat", "history")
	// A beacon for chat's watch tool, ending chat's history result.
	out := runAs(h, "postToolUse", copilotResult("chat-history", "mallory: hi\n\n"+BeaconText(minted(t, ws, chat, "watch", "wst_mallory"))))
	if strings.Contains(out, "connected") || len(ws.redemptions()) != 0 || !strings.Contains(out, `"textResultForLlm":"mallory: hi"`) {
		t.Fatalf("watch's beacon in history's result: %s", out)
	}
	out = runAs(h, "postToolUse", copilotResult("chat-watch", "Watching.\n\n"+BeaconText(minted(t, ws, chat, "watch", "wst_ada"))))
	if !strings.Contains(out, "connected") || h.Links.Load(conv) == "" {
		t.Fatalf("watch's own: %s", out)
	}
}

// Antigravity: the tool is the call's ToolName; the result is read from
// the transcript once agy writes it.
func TestAntigravityBeaconsBoundToTheirTool(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "antigravity", ws, newFakePoller())
	conv := "2b9d08bb-7acc-4459-9956-69064f8d5869"
	post := func(tool, text string) {
		t.Helper()
		in := strings.Replace(strings.Replace(agyPostTool, "TRANSCRIPT", agyTranscript(t, text), 1), `"ToolName":"probe"`, `"ToolName":"`+tool+`"`, 1)
		runAs(h, "post-tool", in)
	}
	post("history", "mallory: hi\n\n"+BeaconText(minted(t, ws, conv, "watch", "wst_mallory")))
	post("probe", `{"messages":[{"text":"hi\n\n`+BeaconText(minted(t, ws, conv, "probe", "wst_mallory"))+`"}]}`)
	if len(ws.redemptions()) != 0 || h.Links.Load(conv) != "" {
		t.Fatalf("acted on a relayed beacon: %v", ws.redemptions())
	}
	post("probe", "Watching.\n\n"+BeaconText(minted(t, ws, conv, "probe", "wst_ada")))
	if r := ws.redemptions(); len(r) != 1 || r[0] != "wst_ada@"+conv {
		t.Fatalf("redeemed %v", r)
	}
}

// Claude Code and Codex name tools mcp__<server>__<tool>; opencode
// <server>_<tool>. Their beacons come in _meta, which no relayed text can
// set, but a beacon naming a tool still counts only from that tool.
func TestMetaBeaconsBoundToTheirTool(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "claude-code", ws, newFakePoller())
	conv := "c95dca59-8b63-4ebb-a70e-8d69fde72b92"
	claudeOf := func(tool, token string) string {
		return strings.Replace(claudeResult(conv, "chat", token), `"mcp__chat__watch"`, `"mcp__chat__`+tool+`"`, 1)
	}
	if out := runAs(h, "", claudeOf("history", minted(t, ws, conv, "watch", "wst_1"))); out != "" || len(ws.redemptions()) != 0 {
		t.Fatalf("watch's beacon in history's _meta: %s", out)
	}
	if out := runAs(h, "", claudeOf("watch", minted(t, ws, conv, "watch", "wst_2"))); !strings.Contains(out, "connected") {
		t.Fatalf("watch's own: %s", out)
	}

	oc := specHook(t, "opencode", ws, newFakePoller())
	line := func(tool, token string) string {
		return strings.Replace(opencodeLine("ses_1", token), `"chat_watch"`, `"`+tool+`"`, 1)
	}
	first := func(in string) string {
		var out bytes.Buffer
		(&Bridge{Hook: oc}).Run(t.Context(), strings.NewReader(in), &out)
		return out.String()
	}
	n := len(ws.redemptions())
	if out := first(line("chat_history", minted(t, ws, "", "watch", "wst_3"))); strings.Contains(out, "connected") || len(ws.redemptions()) != n {
		t.Fatalf("opencode, watch's beacon in history's result: %s", out)
	}
	if out := first(line("watch", minted(t, ws, "", "watch", "wst_4"))); strings.Contains(out, "connected") {
		t.Fatalf("opencode, a tool name without its server: %s", out)
	}
	if !ToolOf(Input{ToolName: "chat_watch", ToolSep: "_"}, "watch") || !ToolOf(Input{ToolName: "my_chat_subscribe_events", ToolSep: "_"}, "subscribe_events") ||
		ToolOf(Input{ToolName: "chat_rewatch", ToolSep: "_"}, "watch") {
		t.Fatal("ToolOf, opencode")
	}
	if MCPToolOf("mcp__chat__subscribe_events") != "subscribe_events" || MCPToolOf("mcp__x") != "" || MCPToolOf("Shell") != "" {
		t.Fatal("MCPToolOf")
	}
}

// A beacon naming the conversation, in a _meta inside the result (a
// document or message the tool relays), isn't the result's: refused, and it
// doesn't hide the beacon ending the result's text.
func TestNestedMetaDoesntCarryAConversationsBeacon(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := testHook(t, ws, newFakePoller())
	nested, _ := json.Marshal(map[string]any{"content": []any{}, "structuredContent": map[string]any{"result": map[string]any{"_meta": map[string]any{beacon.MetaKey: minted(t, ws, "conv-1", "probe", "wst_mallory")}}}})
	if out, _ := call(h, map[string]any{"event": "tool", "conversation": "conv-1", "result": json.RawMessage(nested)}); out != "" || len(ws.redemptions()) != 0 {
		t.Fatalf("nested: %q %v", out, ws.redemptions())
	}
	both, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": "ok\n\n" + BeaconText(minted(t, ws, "conv-1", "probe", "wst_ada"))}},
		"structuredContent": map[string]any{"result": map[string]any{"_meta": map[string]any{beacon.MetaKey: minted(t, ws, "conv-1", "probe", "wst_mallory")}}}})
	call(h, map[string]any{"event": "tool", "conversation": "conv-1", "result": json.RawMessage(both)})
	if r := ws.redemptions(); len(r) != 1 || r[0] != "wst_ada@conv-1" {
		t.Fatalf("redeemed %v", r)
	}
}

// A redeem says which link the hook holds for the conversation, so
// Wakeshift can refuse to move it to another workspace's session; refused,
// the hook keeps its link and says so.
func TestRedeemSaysWhatItHolds(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "cursor", ws, newFakePoller())
	conv := "27188d7e-37d9-4a60-b001-465a6950b24b"
	runAs(h, "", cursorResultOf("probe", "ok\n\n"+BeaconText(minted(t, ws, conv, "probe", "wst_1"))))
	link := h.Links.Load(conv)
	if link == "" || ws.held[0] != "" {
		t.Fatalf("first: link %q, held %v", link, ws.held)
	}
	ws.status, ws.code = 409, "conversation_taken"
	out := runAs(h, "", cursorResultOf("probe", "ok\n\n"+BeaconText(minted(t, ws, conv, "probe", "wst_2"))))
	if ws.held[1] != link || !strings.Contains(out, "stays connected where it was") || h.Links.Load(conv) != link {
		t.Fatalf("taken: held %v, out %s", ws.held, out)
	}
	// A link on another host than the redeem's is never sent.
	if linkOnOrigin("wss://elsewhere.test/v1/stream?link=x", ws.URL+"/v1/connector/redeem") ||
		!linkOnOrigin(strings.Replace(ws.URL, "http", "ws", 1)+"/v1/stream?link=x", ws.URL+"/v1/connector/redeem") {
		t.Fatal("linkOnOrigin")
	}
}
