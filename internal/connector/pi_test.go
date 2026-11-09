package connector

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/jws"
)

// The names pi gives MCP tools, from pi 1.5.0's own createMcpToolName
// (dist/extensions/mcp/tools.js), each with its hashed form (a name too
// long, or taken by another tool).
func TestPiToolName(t *testing.T) {
	for _, c := range [][4]string{
		{"wakebench", "watch", "mcp__wakebench__watch", "mcp__wakebench__watch_92ed81e1"},
		{"my-app", "watch_build", "mcp__my_app__watch_build", "mcp__my_app__watch_build_8725039e"},
		{"café", "wätch", "mcp__caf___w_tch", "mcp__caf___w_tch_aef73e29"},
		{"srv", "t😀", "mcp__srv__t__", "mcp__srv__t___f1381947"},
		{"a-very-long-server-name-for-testing", "an_extremely_long_tool_name_that_goes_on", "", "mcp__a_very_long_server_name_for_testing__an_extremely__b59d2930"},
	} {
		if c[2] != "" && !PiToolName(c[0], c[1], c[2]) {
			t.Errorf("%q %q: %q not its name", c[0], c[1], c[2])
		}
		if !PiToolName(c[0], c[1], c[3]) {
			t.Errorf("%q %q: %q not its hashed name", c[0], c[1], c[3])
		}
	}
	// Another server's or tool's name, a name without the prefix, and the
	// untruncated form of a name too long are never it.
	for _, c := range [][3]string{
		{"wakebench", "watch", "mcp__other__watch"}, {"wakebench", "watch", "mcp__wakebench__history"}, {"wakebench", "watch", "watch"},
		{"wakebench", "watch", "wakebench_watch"}, {"wakebench", "watch", "mcp__wakebench__watch_00000000"},
		{"a-very-long-server-name-for-testing", "an_extremely_long_tool_name_that_goes_on", "mcp__a_very_long_server_name_for_testing__an_extremely_long_tool_name_that_goes_on"},
	} {
		if PiToolName(c[0], c[1], c[2]) {
			t.Errorf("%q %q: %q taken for its name", c[0], c[1], c[2])
		}
	}
}

// What the extension sends the bridge for an MCP tool's result: pi 1.5.0's
// tool_result (toolName, toolCallId, details {server, tool}) with the
// result's content blocks; pi has dropped the result's _meta.
func piLine(session, toolName, server, tool, text string) string {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": session, "call_id": "call_Kp2", "tool_name": toolName,
		"server": server, "mcp_tool": tool, "tool_response": map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}}})
	return string(b) + "\n"
}

const piSession = "019a5c3e-7d2f-7c11-9a41-5e0f3b2d8c6a"

func piHook(t *testing.T, ws *fakeWakeshift, p *fakePoller) *Hook {
	t.Helper()
	spec, _ := Lookup("pi")
	set := Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}
	return &Hook{Harness: "pi", Format: spec.Format, Verifier: &Verifier{Settings: set}, Poller: p, Links: LinksFor(set), When: spec.When,
		Unbound: spec.Unbound, Repoll: 50 * time.Millisecond}
}

// firstLine runs a bridge on one line and returns its answer.
func firstLine(t *testing.T, h *Hook, line string) (string, bool) {
	t.Helper()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	(&Bridge{Hook: h}).Run(ctx, strings.NewReader(line), &out)
	var first struct {
		AdditionalContext string `json:"additionalContext"`
		KeepAlive         bool   `json:"keep_alive"`
	}
	sc := bufio.NewScanner(&out)
	sc.Scan()
	json.Unmarshal(sc.Bytes(), &first)
	return first.AdditionalContext, first.KeepAlive
}

// A beacon bound to the session (the extension set conversation_id), ending
// the result of the tool it names, from the server pi says, connects the
// session; the bridge hands over a message and acks it on the extension's ack.
func TestPiBridge(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := piHook(t, ws, p)
	tok := minted(t, ws, piSession, "watch", "wst_ada")

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- (&Bridge{Hook: h}).Run(context.Background(), inR, outW); outW.Close() }()
	go io.WriteString(inW, piLine(piSession, "mcp__wakebench__watch", "wakebench", "watch", "Watching.\n\n"+beacon.Guard+"\n"+BeaconText(tok)))
	lines := bufio.NewScanner(outR)
	lines.Scan()
	var first struct {
		AdditionalContext string `json:"additionalContext"`
		KeepAlive         bool   `json:"keep_alive"`
	}
	json.Unmarshal(lines.Bytes(), &first)
	if !first.KeepAlive || !strings.Contains(first.AdditionalContext, "Wakeshift: connected") || !strings.Contains(first.AdditionalContext, "or once its turn ends if it's busy") {
		t.Fatalf("first line %s", lines.Bytes())
	}
	if r := ws.redemptions(); len(r) != 1 || r[0] != "wst_ada@"+piSession {
		t.Fatalf("redeemed %v", r)
	}
	// The server is pinned to the beacon's app.
	if b, err := os.ReadFile(h.pins().Path); err != nil || !strings.Contains(string(b), "wakebench") {
		t.Fatalf("pins: %v %s", err, b)
	}
	p.add(h.Links.Load(piSession), msg("d1", "hello pi"))
	lines.Scan()
	var wake struct{ Wake struct{ ID, Text string } }
	json.Unmarshal(lines.Bytes(), &wake)
	if wake.Wake.ID == "" || !strings.Contains(wake.Wake.Text, "hello pi") {
		t.Fatalf("wake %s", lines.Bytes())
	}
	time.Sleep(50 * time.Millisecond)
	if a := p.ackedNow(); len(a) != 0 {
		t.Fatalf("acked %v before the extension did", a)
	}
	io.WriteString(inW, `{"ack":"`+wake.Wake.ID+`"}`+"\n")
	deadline := time.Now().Add(2 * time.Second)
	for !slices.Equal(p.ackedNow(), []string{"d1"}) {
		if time.Now().After(deadline) {
			t.Fatalf("acked %v, want d1", p.ackedNow())
		}
		time.Sleep(10 * time.Millisecond)
	}
	inW.Close() // pi quit
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the bridge outlived pi")
	}

	// pi started again (or /reload): the session listens again with no beacon.
	if _, keep := firstLine(t, h, `{"hook_event_name":"SessionIdle","session_id":"`+piSession+`"}`+"\n"); !keep {
		t.Fatal("not listening again after a restart")
	}
}

// Beacons the extension must never act on (docs/harnesses.md, "Beacon
// binding"): each leaves the session unconnected, with nothing redeemed.
func TestPiBeaconRefusals(t *testing.T) {
	ws := newFakeWakeshift(t)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	text := func(tok string) string { return "Watching.\n\n" + beacon.Guard + "\n" + BeaconText(tok) }
	forged, err := jws.Sign(otherKey, "k1", beacon.Type, beacon.Claims{Issuer: "https://evil.example", Audience: beacon.Audience, Thread: piSession, App: "chat", Tool: "watch",
		Redeem: "https://evil.example/v1/connector/redeem", Ticket: "wst_evil", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	// The trusted issuer's name, another key.
	badKey, err := jws.Sign(otherKey, "k1", beacon.Type, beacon.Claims{Issuer: ws.URL, Audience: beacon.Audience, Thread: piSession, App: "chat", Tool: "watch",
		Redeem: ws.URL + "/v1/connector/redeem", Ticket: "wst_evil", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	ok := minted(t, ws, piSession, "watch", "wst_ada")
	contentMeta, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": piSession, "call_id": "c", "tool_name": "mcp__wakebench__watch",
		"server": "wakebench", "mcp_tool": "watch", "tool_response": map[string]any{"_meta": map[string]any{beacon.MetaKey: minted(t, ws, "", "watch", "wst_m")},
			"content": []any{map[string]string{"type": "text", "text": "Watching."}}}})
	for _, c := range []struct{ name, line string }{
		{"an issuer that isn't pinned", piLine(piSession, "mcp__wakebench__watch", "wakebench", "watch", text(forged))},
		{"the pinned issuer's name, signed by another key", piLine(piSession, "mcp__wakebench__watch", "wakebench", "watch", text(badKey))},
		{"another tool's result", piLine(piSession, "mcp__wakebench__history", "wakebench", "history", text(ok))},
		{"a built-in tool's result", piLine(piSession, "bash", "", "", text(ok))},
		{"details naming a tool pi didn't run", piLine(piSession, "mcp__wakebench__history", "wakebench", "watch", text(ok))},
		{"another session's beacon", piLine("019a0000-0000-7000-8000-000000000000", "mcp__wakebench__watch", "wakebench", "watch", text(ok))},
		{"bound to nothing, in text", piLine(piSession, "mcp__wakebench__watch", "wakebench", "watch", text(minted(t, ws, "", "watch", "wst_m")))},
		{"inside the result's text, not ending it", piLine(piSession, "mcp__wakebench__watch", "wakebench", "watch", "Recent: "+BeaconText(ok)+"\nWatching.")},
		{"a _meta in what the extension sent", string(contentMeta) + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := piHook(t, ws, newFakePoller())
			before := len(ws.redemptions())
			note, keep := firstLine(t, h, c.line)
			if keep || strings.Contains(note, "connected") || len(ws.redemptions()) != before || h.Links.Load(piSession) != "" {
				t.Fatalf("acted on it: %q %v %v", note, keep, ws.redemptions()[before:])
			}
		})
	}

	// A replay: Wakeshift refuses a ticket that's been redeemed (single use),
	// and nothing is kept.
	t.Run("a ticket already redeemed", func(t *testing.T) {
		h := piHook(t, ws, newFakePoller())
		ws.status, ws.code = 404, "unknown_ticket"
		defer func() { ws.status, ws.code = 200, "" }()
		note, keep := firstLine(t, h, piLine(piSession, "mcp__wakebench__watch", "wakebench", "watch", text(ok)))
		if keep || !strings.Contains(note, "couldn't connect") || h.Links.Load(piSession) != "" {
			t.Fatalf("replay: %q %v", note, keep)
		}
	})

	// A server pinned to another app: its beacon is refused.
	t.Run("a server pinned to another app", func(t *testing.T) {
		h := piHook(t, ws, newFakePoller())
		if err := h.pins().Allow("wakebench", "other-app"); err != nil {
			t.Fatal(err)
		}
		before := len(ws.redemptions())
		note, keep := firstLine(t, h, piLine(piSession, "mcp__wakebench__watch", "wakebench", "watch", text(ok)))
		if keep || strings.Contains(note, "Wakeshift: connected") || len(ws.redemptions()) != before {
			t.Fatalf("pinned: %q %v", note, keep)
		}
	})
}

func TestPiInstall(t *testing.T) {
	home := setHome(t)
	spec, _ := Lookup("pi")
	const bin = "/Users/ada/.wakeshift/bin/wakeshift"
	var out bytes.Buffer
	if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".pi", "agent", "extensions", "wakeshift.ts")
	b, err := os.ReadFile(path)
	if err != nil || !has(string(b), `const BIN = "/Users/ada/.wakeshift/bin/wakeshift"`, "export default function", `["bridge", "--harness", "pi"]`, piMarker) {
		t.Fatalf("%v %s", err, b)
	}
	if strings.Contains(string(b), "__WAKESHIFT_BIN__") {
		t.Fatal("the placeholder is left")
	}
	out.Reset()
	spec.Install(bin, "https://app.wakeshift.ai", &out)
	if !has(out.String(), "already") {
		t.Fatalf("twice: %s", out.String())
	}
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("still there")
	}
	// Someone else's file at that path isn't removed.
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("export default function () {}"), 0o600)
	if err := spec.Uninstall(bin, &out); err == nil {
		t.Fatal("removed a file that isn't ours")
	}
	// PI_CODING_AGENT_DIR moves pi's agent directory, ~ included.
	t.Setenv("PI_CODING_AGENT_DIR", "~/elsewhere/agent")
	if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "elsewhere", "agent", "extensions", "wakeshift.ts")); err != nil {
		t.Fatal("PI_CODING_AGENT_DIR ignored")
	}
}
