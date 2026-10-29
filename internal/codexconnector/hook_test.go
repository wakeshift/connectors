package codexconnector

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/jws"
)

// fakeAppServer answers initialize and thread/read (for known threads) on a
// unix socket, as Codex's daemon does.
func fakeAppServer(t *testing.T, threads ...string) string {
	t.Helper()
	dir, _ := os.MkdirTemp("", "cx") // short: unix socket paths are limited
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var m struct {
				ID     *int64
				Method string
				Params struct {
					ThreadID string `json:"threadId"`
				}
			}
			json.Unmarshal(b, &m)
			if m.ID == nil {
				continue
			}
			reply := map[string]any{"id": *m.ID, "result": map[string]any{}}
			if m.Method == "thread/read" {
				known := false
				for _, th := range threads {
					known = known || th == m.Params.ThreadID
				}
				if known {
					reply["result"] = map[string]any{"thread": map[string]any{"id": m.Params.ThreadID, "status": map[string]any{"type": "idle"}}}
				} else {
					delete(reply, "result")
					reply["error"] = map[string]any{"code": -32600, "message": "thread not found"}
				}
			}
			out, _ := json.Marshal(reply)
			c.Write(context.Background(), websocket.MessageText, out)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

type fakeWakeshift struct {
	*httptest.Server
	key      ed25519.PrivateKey
	redeemed []string
	status   int
}

func newFakeWakeshift(t *testing.T) *fakeWakeshift {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &fakeWakeshift{key: priv, status: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub)}}})
	})
	mux.HandleFunc("POST /v1/connector/redeem", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Ticket, Thread string }
		json.NewDecoder(r.Body).Decode(&in)
		f.redeemed = append(f.redeemed, in.Ticket+"@"+in.Thread)
		w.WriteHeader(f.status)
		if f.status == http.StatusUpgradeRequired {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "update_required", "message": "Wakeshift's hooks for Codex are out of date. Ask the user."}})
			return
		}
		if f.status != 200 {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "unknown_ticket"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"url": "ws://link-for-" + in.Thread})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWakeshift) beacon(t *testing.T, thread string, key ed25519.PrivateKey) string {
	t.Helper()
	if key == nil {
		key = f.key
	}
	tok, err := jws.Sign(key, "k1", beacon.Type, beacon.Claims{Issuer: f.URL, Audience: beacon.Audience, Thread: thread, App: "chat",
		Redeem: f.URL + "/v1/connector/redeem", Ticket: "wst_t", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

const thread = "01a14bd7-bbbc-7b70-a723-0abb9f38118e"

func hookInput(thread, token string) *bytes.Reader {
	resp := map[string]any{"content": []any{map[string]string{"type": "text", "text": "ok"}}}
	if token != "" {
		resp["_meta"] = map[string]any{beacon.MetaKey: token}
	}
	b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": "PostToolUse", "tool_name": "mcp__chat__watch", "tool_response": resp})
	return bytes.NewReader(b)
}

func noteOf(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var m struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil || m.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Fatalf("hook output %q", out)
	}
	return m.HookSpecificOutput.AdditionalContext
}

// The hook acts only on a beacon signed by a trusted Wakeshift for its own
// thread, redeems it, starts the connector and says so; it says nothing
// about anything else.
func TestHook(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	ws := newFakeWakeshift(t)
	var started []string
	h := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}, Socket: fakeAppServer(t, thread),
		Start: func(th, link, _ string, _ bool) error { started = append(started, th+" "+link); return nil }}
	run := func(in *bytes.Reader) string {
		var out bytes.Buffer
		h.Run(context.Background(), in, &out)
		return noteOf(t, out.String())
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)

	for name, in := range map[string]*bytes.Reader{
		"no beacon":           hookInput(thread, ""),
		"another thread's":    hookInput("01a14bd7-0000-7b70-a723-0abb9f38118e", ws.beacon(t, thread, nil)),
		"another key's":       hookInput(thread, ws.beacon(t, thread, other)),
		"not JSON":            bytes.NewReader([]byte("nope")),
		"a beacon that's not": hookInput(thread, "a.b.c"),
	} {
		if got := run(in); got != "" || len(ws.redeemed) != 0 || len(started) != 0 {
			t.Fatalf("%s: note %q, redeemed %v, started %v", name, got, ws.redeemed, started)
		}
	}
	untrusted := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{"https://app.wakeshift.ai"}}, Socket: h.Socket, Start: h.Start}
	var out bytes.Buffer
	untrusted.Run(context.Background(), hookInput(thread, ws.beacon(t, thread, nil)), &out)
	if out.Len() != 0 || len(ws.redeemed) != 0 {
		t.Fatalf("an issuer not installed: %q %v", out.String(), ws.redeemed)
	}

	if got := run(hookInput(thread, ws.beacon(t, thread, nil))); got != NoteConnected || len(started) != 1 || started[0] != thread+" ws://link-for-"+thread {
		t.Fatalf("connect: note %q, started %v", got, started)
	}
	if ws.redeemed[0] != "wst_t@"+thread {
		t.Fatalf("redeemed %v", ws.redeemed)
	}
	ws.status = 404
	if got := run(hookInput(thread, ws.beacon(t, thread, nil))); !strings.Contains(got, "expired or was used") || len(started) != 1 {
		t.Fatalf("a used ticket: %q %v", got, started)
	}
	// Out of date: Wakeshift's words, and nothing started.
	ws.status = http.StatusUpgradeRequired
	if got := run(hookInput(thread, ws.beacon(t, thread, nil))); got != fmt.Sprintf(NoteOutdated, "Wakeshift's hooks for Codex are out of date. Ask the user.") || len(started) != 1 {
		t.Fatalf("out of date: %q %v", got, started)
	}

	// A thread the daemon doesn't have: an in-process session.
	inProcess := "01a14bd7-1111-7b70-a723-0abb9f38118e"
	ws.status = 200
	n := len(ws.redeemed)
	if got := run(hookInput(inProcess, ws.beacon(t, inProcess, nil))); got != NoteInProcess || len(ws.redeemed) != n {
		t.Fatalf("in-process: %q, redeemed %v", got, ws.redeemed)
	}
}

// A beacon signed by Wakeshift for this thread, planted in another MCP
// server's result (Codex sends the thread ID to every server, so anyone
// running one can mint a beacon for it through some app): only the server
// pinned to the beacon's app may carry it, and a non-MCP tool never.
func TestHookRefusesAPlantedBeacon(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	ws := newFakeWakeshift(t)
	var started []string
	h := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}, Socket: fakeAppServer(t, thread),
		Start: func(th, link, _ string, _ bool) error { started = append(started, th); return nil }}
	from := func(tool, token string) string {
		resp := map[string]any{"content": []any{}, "_meta": map[string]any{beacon.MetaKey: token}}
		b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": "PostToolUse", "tool_name": tool, "tool_response": resp})
		var out bytes.Buffer
		h.Run(context.Background(), bytes.NewReader(b), &out)
		return noteOf(t, out.String())
	}
	if got := from("mcp__chat__watch", ws.beacon(t, thread, nil)); got != NoteConnected {
		t.Fatalf("the app's own server: %q", got)
	}
	if got := from("mcp__evil__lookup", ws.beacon(t, thread, nil)); got == NoteConnected || !strings.Contains(got, `"chat"`) {
		t.Fatalf("the app's beacon from another server: %q", got)
	}
	if got := from("shell", ws.beacon(t, thread, nil)); got != "" {
		t.Fatalf("not an MCP tool: %q", got)
	}
	if len(ws.redeemed) != 1 || len(started) != 1 {
		t.Fatalf("redeemed %v, started %v", ws.redeemed, started)
	}
}

// A beacon line in a result's text, from the app's own server: Codex's
// beacons come only in _meta, and an app's result can relay someone else's
// words (a chat message carrying a beacon they minted for this thread), so
// the hook ignores it.
func TestHookIgnoresABeaconInText(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	ws := newFakeWakeshift(t)
	var started []string
	h := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}, Socket: fakeAppServer(t, thread),
		Start: func(th, link, _ string, _ bool) error { started = append(started, th); return nil }}
	line := "@mallory: hi\n" + beacon.Text(ws.beacon(t, thread, nil))
	for name, resp := range map[string]any{
		"content":           map[string]any{"content": []any{map[string]string{"type": "text", "text": line}}},
		"structuredContent": map[string]any{"content": []any{}, "structuredContent": map[string]any{"messages": []string{line}}},
		// JSON with a _meta of its own, in the result or in a field around
		// it: not the result's _meta.
		"a wrapped _meta": map[string]any{"output": map[string]any{"_meta": map[string]any{beacon.MetaKey: ws.beacon(t, thread, nil)}}},
		"relayed JSON":    map[string]any{"content": []any{}, "structuredContent": map[string]any{"result": map[string]any{"_meta": map[string]any{beacon.MetaKey: ws.beacon(t, thread, nil)}}}},
		"a JSON string":   line,
	} {
		b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": "PostToolUse", "tool_name": "mcp__chat__history", "tool_response": resp})
		var out bytes.Buffer
		h.Run(context.Background(), bytes.NewReader(b), &out)
		if out.Len() != 0 || len(ws.redeemed) != 0 || len(started) != 0 {
			t.Fatalf("%s: note %q, redeemed %v, started %v", name, out.String(), ws.redeemed, started)
		}
	}
}

// A beacon naming a tool connects only from that tool's result (its own
// _meta): one minted on watch and returned in history's _meta (a relay, or
// an app putting it in the wrong place) is ignored.
func TestHookChecksTheBeaconsTool(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	ws := newFakeWakeshift(t)
	var started []string
	h := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}, Socket: fakeAppServer(t, thread),
		Start: func(th, link, _ string, _ bool) error { started = append(started, th); return nil }}
	tok, err := jws.Sign(ws.key, "k1", beacon.Type, beacon.Claims{Issuer: ws.URL, Audience: beacon.Audience, Thread: thread, App: "chat", Tool: "watch",
		Redeem: ws.URL + "/v1/connector/redeem", Ticket: "wst_t", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	in := func(tool string) *bytes.Reader {
		b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": "PostToolUse", "tool_name": tool,
			"tool_response": map[string]any{"content": []any{}, "_meta": map[string]any{beacon.MetaKey: tok}}})
		return bytes.NewReader(b)
	}
	var out bytes.Buffer
	h.Run(context.Background(), in("mcp__chat__history"), &out)
	if out.Len() != 0 || len(ws.redeemed) != 0 || len(started) != 0 {
		t.Fatalf("history's result: %q %v", out.String(), ws.redeemed)
	}
	h.Run(context.Background(), in("mcp__chat__watch"), &out)
	if !strings.Contains(noteOf(t, out.String()), "Wakeshift: connected") || len(started) != 1 {
		t.Fatalf("watch's result: %q", out.String())
	}
}

func TestRedact(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "ws://127.0.0.1:1/v1/stream?link=SECRET", Err: errors.New("connection refused")}
	if got := redact(err); strings.Contains(got, "SECRET") || !strings.Contains(got, "/v1/stream?…") {
		t.Fatalf("redact: %q", got)
	}
}
