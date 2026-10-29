package connector

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/jws"
)

const sinceSession = "c95dca59-8b63-4ebb-a70e-8d69fde72b92"

// sinceBeacon is a Claude Code beacon (bound to a call) minted at iat.
func sinceBeacon(t *testing.T, ws *fakeWakeshift, key ed25519.PrivateKey, call, tool string, iat time.Time) string {
	t.Helper()
	if key == nil {
		key = ws.key
	}
	tok, err := jws.Sign(key, "k1", beacon.Type, beacon.Claims{Issuer: ws.URL, Audience: beacon.Audience, Call: call, App: "chat", Tool: tool,
		Redeem: ws.URL + "/v1/connector/redeem", Ticket: "wst_t", IssuedAt: iat.Unix(), Expires: iat.Add(beacon.TTL).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// sinceTranscript writes a transcript in which the agent called
// mcp__wsprobe__probe (call toolu_1) and its result's _meta carried token.
func sinceTranscript(t *testing.T, token string) string {
	t.Helper()
	use := fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__wsprobe__probe","input":{}}]},"sessionId":%q}`, sinceSession)
	res := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"ok"}]},"mcpMeta":{"_meta":{%q:%q}},"sessionId":%q}`,
		beacon.MetaKey, token, sinceSession)
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(`{"type":"summary"}`+"\n"+use+"\n"+res+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sinceHook(t *testing.T, ws *fakeWakeshift, installed time.Time) *Hook {
	t.Helper()
	set := Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}, InstalledAt: installed}
	return &Hook{Harness: claudeName, Format: claudeFormat{}, Verifier: &Verifier{Settings: set}, Poller: newFakePoller(), Links: LinksFor(set),
		When: "as soon as they arrive", Rewake: true, RewakeWait: time.Second}
}

func sinceStop(h *Hook, transcript string) (string, int) {
	return call(h, map[string]any{"session_id": sinceSession, "transcript_path": transcript, "hook_event_name": "Stop", "stop_hook_active": false})
}

// Hooks installed after the conversation's last beacon (the agent asked,
// the user said yes, the agent ran the installer and stopped): the first
// waiter wakes the agent, once, to call the tool again, since nothing else
// would connect the conversation.
func TestWaiterInstalledSince(t *testing.T) {
	old := LinkGrace
	LinkGrace = 100 * time.Millisecond
	defer func() { LinkGrace = old }()
	ws := newFakeWakeshift(t)
	minted := time.Now().Add(-2 * time.Minute) // expired: it's evidence, not redeemed
	tr := sinceTranscript(t, sinceBeacon(t, ws, nil, "toolu_1", "probe", minted))

	h := sinceHook(t, ws, minted.Add(30*time.Second))
	out, code := sinceStop(h, tr)
	if code != 2 || !strings.Contains(out, "Call mcp__wsprobe__probe again now") || !strings.HasPrefix(out, "Wakeshift: ") {
		t.Fatalf("%d %q", code, out)
	}
	if len(ws.redemptions()) != 0 {
		t.Fatal("redeemed an expired beacon")
	}
	// Once per conversation.
	if out, code := sinceStop(h, tr); code != 0 || out != "" {
		t.Fatalf("again: %d %q", code, out)
	}

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	for name, c := range map[string]struct {
		token     string
		installed time.Time
	}{
		"installed before the beacon": {sinceBeacon(t, ws, nil, "toolu_1", "probe", minted), minted.Add(-time.Minute)},
		"installs from before":        {sinceBeacon(t, ws, nil, "toolu_1", "probe", minted), time.Time{}},
		"another call's beacon":       {sinceBeacon(t, ws, nil, "toolu_2", "probe", minted), minted.Add(time.Minute)},
		"another tool's beacon":       {sinceBeacon(t, ws, nil, "toolu_1", "other", minted), minted.Add(time.Minute)},
		"not signed by Wakeshift":     {sinceBeacon(t, ws, other, "toolu_1", "probe", minted), minted.Add(time.Minute)},
		"over an hour old":            {sinceBeacon(t, ws, nil, "toolu_1", "probe", time.Now().Add(-2*time.Hour)), time.Now().Add(-time.Hour)},
	} {
		h := sinceHook(t, ws, c.installed)
		if out, code := sinceStop(h, sinceTranscript(t, c.token)); code != 0 || out != "" {
			t.Fatalf("%s: %d %q", name, code, out)
		}
	}

	// A conversation with a link listens as before.
	h = sinceHook(t, ws, minted.Add(30*time.Second))
	link := "wss://wakeshift.test/v1/stream?link=a"
	h.Links.Save(sinceSession, link)
	h.Poller.(*fakePoller).add(link, msg("d1", "first message"))
	if out, code := sinceStop(h, tr); code != 2 || !strings.Contains(out, "first message") || strings.Contains(out, "again now") {
		t.Fatalf("connected: %d %q", code, out)
	}
}

// LastBeacon reads the newest result's beacon and its call's tool, from
// this session's lines only.
func TestClaudeLastBeacon(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"mcp__a__x"}]},"sessionId":%q}`, sinceSession),
		fmt.Sprintf(`{"type":"user","message":{"content":[{"tool_use_id":"toolu_1","type":"tool_result"}]},"mcpMeta":{"_meta":{%q:"FIRST"}},"sessionId":%q}`, beacon.MetaKey, sinceSession),
		fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"mcp__a__y"}]},"sessionId":%q}`, sinceSession),
		fmt.Sprintf(`{"type":"user","message":{"content":[{"tool_use_id":"toolu_2","type":"tool_result"}]},"mcpMeta":{"_meta":{%q:"SECOND"}},"sessionId":%q}`, beacon.MetaKey, sinceSession),
		// Another session's, and a beacon nested further in, don't count.
		fmt.Sprintf(`{"type":"user","message":{"content":[{"tool_use_id":"toolu_3","type":"tool_result"}]},"mcpMeta":{"_meta":{%q:"OTHER"}},"sessionId":"someone-else"}`, beacon.MetaKey),
		fmt.Sprintf(`{"type":"user","message":{"content":[{"tool_use_id":"toolu_2","type":"tool_result"}]},"mcpMeta":{"structuredContent":{"_meta":{%q:"NESTED"}}},"sessionId":%q}`, beacon.MetaKey, sinceSession),
	}
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	got, ok := claudeFormat{}.LastBeacon(Input{Conversation: sinceSession, transcript: p})
	if !ok || got != (FoundBeacon{Token: "SECOND", Call: "toolu_2", ToolName: "mcp__a__y"}) {
		t.Fatalf("%v %+v", ok, got)
	}
	if _, ok := (claudeFormat{}).LastBeacon(Input{Conversation: sinceSession, transcript: "s.jsonl"}); ok {
		t.Fatal("read a relative path")
	}
}
