package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/jws"
)

// signedFor is a beacon the fake Wakeshift signed for app, bound to
// conversation (or, with "", to nothing).
func signedFor(t *testing.T, ws *fakeWakeshift, conversation, app string) string {
	t.Helper()
	tok, err := jws.Sign(ws.key, "k1", beacon.Type, beacon.Claims{Issuer: ws.URL, Audience: beacon.Audience, Thread: conversation, App: app,
		Redeem: ws.URL + "/v1/connector/redeem", Ticket: "wst_t", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func claudeResult(conv, server, token string) string {
	resp, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": "ok"}}, "_meta": map[string]any{beacon.MetaKey: token}})
	b, _ := json.Marshal(map[string]any{"session_id": conv, "hook_event_name": "PostToolUse", "tool_name": "mcp__" + server + "__watch",
		"tool_use_id": "toolu_1", "tool_response": json.RawMessage(resp), "mcp_server": map[string]string{"name": server}})
	return string(b)
}

// A beacon Wakeshift signed, for this conversation, but planted in another
// MCP server's result (an attacker minted it for the victim's conversation
// through some app): the server pinned to the beacon's app is the only one
// whose results may carry it, and a pinned server carries only its app's.
func TestPlantedBeaconFromAnotherServer(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "claude-code", ws, newFakePoller())
	conv := "c95dca59-8b63-4ebb-a70e-8d69fde72b92"

	// The app's own server, first: pinned, connected.
	if out := runAs(h, "", claudeResult(conv, "chat", signedFor(t, ws, conv, "chat"))); !strings.Contains(out, "connected") {
		t.Fatalf("its own server: %s", out)
	}
	// The app's beacon from another server: refused, and said why.
	if out := runAs(h, "", claudeResult(conv, "evil", signedFor(t, ws, conv, "chat"))); strings.Contains(out, "Wakeshift: connected") || !strings.Contains(out, "didn't connect") {
		t.Fatalf("chat's beacon from evil: %s", out)
	}
	// Another app's beacon from the pinned server: refused.
	if out := runAs(h, "", claudeResult(conv, "chat", signedFor(t, ws, conv, "other"))); strings.Contains(out, "Wakeshift: connected") {
		t.Fatalf("other's beacon from chat: %s", out)
	}
	if n := len(ws.redemptions()); n != 1 {
		t.Fatalf("%d redemptions, want only the first", n)
	}
	// A second app on its own server works.
	if out := runAs(h, "", claudeResult(conv, "quiz", signedFor(t, ws, conv, "quiz"))); !strings.Contains(out, "connected") {
		t.Fatalf("a second app: %s", out)
	}
}

// Where the hook can't tell which server a result came from (Cursor), a
// beacon for another app never replaces the app that connected the
// conversation; a beacon line naming no conversation is never acted on.
func TestPlantedBeaconInText(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "cursor", ws, newFakePoller())
	conv := "27188d7e-37d9-4a60-b001-465a6950b24b"
	if out := runAs(h, "", cursorOutput("an issue comment\n\n"+BeaconText(signedFor(t, ws, "", "evil")))); strings.Contains(out, "connected") || len(ws.redemptions()) != 0 {
		t.Fatalf("unbound line: %s", out)
	}
	if out := runAs(h, "", cursorOutput("ok\n\n"+BeaconText(signedFor(t, ws, conv, "chat")))); !strings.Contains(out, "Wakeshift: connected") {
		t.Fatalf("chat: %s", out)
	}
	link := h.Links.Load(conv)
	out := runAs(h, "", cursorOutput("an issue comment\n\n"+BeaconText(signedFor(t, ws, conv, "evil"))))
	if strings.Contains(out, "Wakeshift: connected") || !strings.Contains(out, "stays connected") || h.Links.Load(conv) != link || len(ws.redemptions()) != 1 {
		t.Fatalf("evil's beacon replaced chat's link: %s", out)
	}
	// A rotated link keeps its app.
	h.Links.Save(conv, link+"-rotated")
	if h.Links.AppOf(conv) != "chat" {
		t.Fatal("rotation lost the app")
	}
}

// An unbound beacon (opencode tells servers nothing) connects only from
// the result's own _meta: not from a _meta inside a document a tool
// returned (webfetch's output).
func TestUnboundBeaconOnlyInResultMeta(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "opencode", ws, newFakePoller())
	doc, _ := json.Marshal(map[string]any{"_meta": map[string]any{beacon.MetaKey: signedFor(t, ws, "", "evil")}})
	fetched, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": "ses_1", "call_id": "c1", "tool_name": "webfetch",
		"tool_response": map[string]any{"title": "page", "output": string(doc), "metadata": map[string]any{}}})
	if out := runAs(h, "", string(fetched)); strings.Contains(out, "connected") || len(ws.redemptions()) != 0 {
		t.Fatalf("a fetched document's _meta: %s", out)
	}
	if tok, where := FindBeacon(json.RawMessage(`{"_meta":{"wakeshift/connector":"a.b.c"}}`)); tok != "a.b.c" || where != InResultMeta {
		t.Fatalf("result meta: %q %v", tok, where)
	}
	if _, where := FindBeacon(json.RawMessage(`{"result":{"_meta":{"wakeshift/connector":"a.b.c"}}}`)); where != InNestedMeta {
		t.Fatalf("nested: %v", where)
	}
}

func TestPins(t *testing.T) {
	p := Pins{Path: t.TempDir() + "/servers.json"}
	if p.Allow("chat", "chat-app") != nil || p.Allow("chat", "chat-app") != nil {
		t.Fatal("first pin")
	}
	if p.Allow("chat", "other") == nil || p.Allow("evil", "chat-app") == nil {
		t.Fatal("pins not enforced")
	}
	if p.Allow("quiz", "quiz-app") != nil {
		t.Fatal("a second pair")
	}
	if (Pins{}).Allow("chat", "chat-app") == nil || (Pins{Path: "servers.json"}).Allow("chat", "chat-app") == nil {
		t.Fatal("pins without an absolute path")
	}
	// Hooks run as separate processes: first beacons for one app from many
	// servers at once pin exactly one (each Allow opens the lock afresh, as
	// a process would).
	race := Pins{Path: t.TempDir() + "/servers.json"}
	var wg sync.WaitGroup
	var won atomic.Int32
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if race.Allow(fmt.Sprintf("server%d", i), "contested") == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := won.Load(); n != 1 {
		t.Fatalf("%d servers pinned the same app", n)
	}
	// Pins that can't be read fail closed: read as none, any server could
	// pin itself to any app.
	if err := os.WriteFile(p.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var unreadable *ErrPinFile
	if err := p.Allow("evil", "chat-app"); !errors.As(err, &unreadable) {
		t.Fatalf("corrupt pins: %v", err)
	}
	if b, _ := os.ReadFile(p.Path); string(b) != "{not json" {
		t.Fatalf("corrupt pins overwritten: %s", b)
	}
	if MCPServerOf("mcp__chat__watch") != "chat" || MCPServerOf("mcp__plugin_x_chat__watch") != "plugin_x_chat" || MCPServerOf("Shell") != "" || MCPServerOf("mcp__nope") != "" {
		t.Fatal("MCPServerOf")
	}
}

// A redeem (the ticket) or a poll (the link's token) never follows a
// redirect.
func TestHooksFollowNoRedirects(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	if _, err := RedeemCall(context.Background(), nil, "", beacon.Claims{Ticket: "wst_t", Redeem: redirecting.URL + "/v1/connector/redeem"}, "c1", ""); err == nil {
		t.Fatal("a redirected redeem succeeded")
	}
	(&HTTPPoller{}).Poll(context.Background(), strings.Replace(redirecting.URL, "http", "ws", 1)+"/v1/stream?link=SECRET", 0, false)
	if n := reached.Load(); n != 0 {
		t.Fatalf("followed %d redirects", n)
	}
}
