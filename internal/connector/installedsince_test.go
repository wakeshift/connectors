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
// waiter connects the conversation from that beacon itself, as a late
// redeem, and listens; the "connected" note comes with the first wake.
// Nothing asks the agent to call the tool again (it may have side effects).
func TestWaiterInstalledSince(t *testing.T) {
	old := LinkGrace
	LinkGrace = 100 * time.Millisecond
	defer func() { LinkGrace = old }()
	ws := newFakeWakeshift(t)
	minted := time.Now().Add(-2 * time.Minute) // expired: redeemed late
	tr := sinceTranscript(t, sinceBeacon(t, ws, nil, "toolu_1", "probe", minted))

	h := sinceHook(t, ws, minted.Add(30*time.Second))
	link := "ws://" + strings.TrimPrefix(ws.URL, "http://") + "/v1/stream?link=tok-" + sinceSession
	h.Poller.(*fakePoller).add(link, msg("d1", "first message"))
	out, code := sinceStop(h, tr)
	if code != 2 || !strings.HasPrefix(out, "Wakeshift: connected") || !strings.Contains(out, "first message") || strings.Contains(out, "again") {
		t.Fatalf("%d %q", code, out)
	}
	if r := ws.redemptions(); len(r) != 1 || r[0] != "wst_t@"+sinceSession || !ws.late[0] {
		t.Fatalf("redeemed %v late %v", r, ws.late)
	}
	if h.Links.Load(sinceSession) != link {
		t.Fatal("the link wasn't kept")
	}

	// Refused (the core's window passed, or a connector connected it
	// since): tried once per conversation, and nothing asks for a call.
	refused := newFakeWakeshift(t)
	refused.status, refused.code = 404, "unknown_ticket"
	tr2 := sinceTranscript(t, sinceBeacon(t, refused, nil, "toolu_1", "probe", minted))
	h = sinceHook(t, refused, minted.Add(30*time.Second))
	for range 2 {
		if out, code := sinceStop(h, tr2); code != 0 || out != "" {
			t.Fatalf("refused: %d %q", code, out)
		}
	}
	if len(refused.redemptions()) != 1 {
		t.Fatalf("tried %d times", len(refused.redemptions()))
	}

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	for name, c := range map[string]struct {
		token     string
		installed time.Time
	}{
		"installs from before":    {sinceBeacon(t, ws, nil, "toolu_1", "probe", minted), time.Time{}},
		"another call's beacon":   {sinceBeacon(t, ws, nil, "toolu_2", "probe", minted), minted.Add(time.Minute)},
		"another tool's beacon":   {sinceBeacon(t, ws, nil, "toolu_1", "other", minted), minted.Add(time.Minute)},
		"not signed by Wakeshift": {sinceBeacon(t, ws, other, "toolu_1", "probe", minted), minted.Add(time.Minute)},
		"over an hour old":        {sinceBeacon(t, ws, nil, "toolu_1", "probe", time.Now().Add(-2*time.Hour)), time.Now().Add(-time.Hour)},
	} {
		h := sinceHook(t, ws, c.installed)
		if out, code := sinceStop(h, sinceTranscript(t, c.token)); code != 0 || out != "" {
			t.Fatalf("%s: %d %q", name, code, out)
		}
	}
	if len(ws.redemptions()) != 1 {
		t.Fatalf("redeemed %v", ws.redemptions())
	}

	// A conversation with a link listens as before.
	h = sinceHook(t, ws, minted.Add(30*time.Second))
	own := "wss://wakeshift.test/v1/stream?link=a"
	h.Links.Save(sinceSession, own)
	h.Poller.(*fakePoller).add(own, msg("d1", "first message"))
	if out, code := sinceStop(h, tr); code != 2 || !strings.Contains(out, "first message") || strings.Contains(out, "Wakeshift: connected") {
		t.Fatalf("connected: %d %q", code, out)
	}
	if len(ws.redemptions()) != 1 {
		t.Fatalf("redeemed again: %v", ws.redemptions())
	}
}

// A beacon minted after the install whose result's hook didn't redeem it
// (it gave up before Claude Code wrote the result's line, as an older hook
// often did): once LinkGrace passes with no link, the waiter redeems it
// itself, late, and listens; the "connected" note comes with the first
// wake.
func TestWaiterMissedRedeem(t *testing.T) {
	old := LinkGrace
	LinkGrace = 100 * time.Millisecond
	defer func() { LinkGrace = old }()
	ws := newFakeWakeshift(t)
	minted := time.Now().Add(-2 * time.Minute) // expired: redeemed late
	installed := minted.Add(-30 * time.Second)
	tr := sinceTranscript(t, sinceBeacon(t, ws, nil, "toolu_1", "probe", minted))

	h := sinceHook(t, ws, installed)
	link := "ws://" + strings.TrimPrefix(ws.URL, "http://") + "/v1/stream?link=tok-" + sinceSession
	h.Poller.(*fakePoller).add(link, msg("d1", "first message"))
	out, code := sinceStop(h, tr)
	if code != 2 || !strings.HasPrefix(out, "Wakeshift: connected") || !strings.Contains(out, "first message") {
		t.Fatalf("%d %q", code, out)
	}
	if r := ws.redemptions(); len(r) != 1 || r[0] != "wst_t@"+sinceSession || !ws.late[0] {
		t.Fatalf("redeemed %v late %v", r, ws.late)
	}
	if h.Links.Load(sinceSession) != link {
		t.Fatal("the link wasn't kept")
	}

	// Refused (the hook did redeem it, and the conversation was unwatched
	// since; or the core's window passed): tried once per beacon, and a
	// newer beacon is tried in its turn.
	refused := newFakeWakeshift(t)
	refused.status, refused.code = 404, "unknown_ticket"
	h = sinceHook(t, refused, installed)
	for range 2 {
		if out, code := sinceStop(h, sinceTranscript(t, sinceBeacon(t, refused, nil, "toolu_1", "probe", minted))); code != 0 || out != "" {
			t.Fatalf("refused: %d %q", code, out)
		}
	}
	if len(refused.redemptions()) != 1 {
		t.Fatalf("tried %d times", len(refused.redemptions()))
	}
	newer := sinceTranscript(t, sinceBeacon(t, refused, nil, "toolu_2", "probe", minted))
	b, _ := os.ReadFile(newer) // the call is toolu_2's
	os.WriteFile(newer, []byte(strings.ReplaceAll(string(b), "toolu_1", "toolu_2")), 0o600)
	if out, code := sinceStop(h, newer); code != 0 || out != "" {
		t.Fatalf("newer: %d %q", code, out)
	}
	if len(refused.redemptions()) != 2 {
		t.Fatalf("newer beacon tried %d times in all", len(refused.redemptions()))
	}

	// Not while the result's hook may still be redeeming it, nor from a
	// replaced waiter.
	ws2 := newFakeWakeshift(t)
	h = sinceHook(t, ws2, installed)
	if out, code := sinceStop(h, sinceTranscript(t, sinceBeacon(t, ws2, nil, "toolu_1", "probe", time.Now().Add(-10*time.Second)))); code != 0 || out != "" {
		t.Fatalf("young: %d %q", code, out)
	}
	h = sinceHook(t, ws2, installed)
	h.stale = func() bool { return true }
	if out, code := sinceStop(h, tr); code != 0 || out != "" {
		t.Fatalf("stale: %d %q", code, out)
	}
	if len(ws2.redemptions()) != 0 {
		t.Fatalf("redeemed %v", ws2.redemptions())
	}
}

// LastBeacon reads the newest result's beacon and its call's tool, from
// this session's lines only.
// A subscribe made from a subagent whose hook missed the beacon: the
// waiter (whose input names the session's transcript, not the subagent's)
// finds it in <session>/subagents/agent-<id>.jsonl and redeems it.
func TestWaiterMissedRedeemSubagent(t *testing.T) {
	old := LinkGrace
	LinkGrace = 100 * time.Millisecond
	defer func() { LinkGrace = old }()
	ws := newFakeWakeshift(t)
	minted := time.Now().Add(-2 * time.Minute)
	// The subagent's lines go to its own file; the session's has none.
	in := sinceTranscript(t, sinceBeacon(t, ws, nil, "toolu_1", "probe", minted))
	dir := t.TempDir()
	tr := filepath.Join(dir, sinceSession+".jsonl")
	os.WriteFile(tr, []byte(`{"type":"summary"}`+"\n"), 0o600)
	sub := filepath.Join(dir, sinceSession, "subagents", "agent-a5998067ef883574a.jsonl")
	os.MkdirAll(filepath.Dir(sub), 0o700)
	b, _ := os.ReadFile(in)
	os.WriteFile(sub, b, 0o600)

	h := sinceHook(t, ws, minted.Add(-30*time.Second))
	link := "ws://" + strings.TrimPrefix(ws.URL, "http://") + "/v1/stream?link=tok-" + sinceSession
	h.Poller.(*fakePoller).add(link, msg("d1", "first message"))
	out, code := sinceStop(h, tr)
	if code != 2 || !strings.HasPrefix(out, "Wakeshift: connected") || !strings.Contains(out, "first message") {
		t.Fatalf("%d %q", code, out)
	}
	if r := ws.redemptions(); len(r) != 1 || r[0] != "wst_t@"+sinceSession {
		t.Fatalf("redeemed %v", r)
	}
}

// LastBeacon reads the subagents' transcripts too, and takes the beacon
// written last across them.
func TestClaudeLastBeaconSubagents(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, sinceSession+".jsonl")
	line := func(id, tool, beaconText, at string) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"content":[{"type":"tool_use","id":%q,"name":%q}]},"sessionId":%q}`, at, id, tool, sinceSession) + "\n" +
			fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":[{"tool_use_id":%q,"type":"tool_result"}]},"mcpMeta":{"_meta":{%q:%q}},"sessionId":%q}`, at, id, beacon.MetaKey, beaconText, sinceSession) + "\n"
	}
	os.WriteFile(tr, []byte(line("toolu_1", "mcp__a__x", "SESSION", "2026-11-11T10:00:00Z")), 0o600)
	subs := filepath.Join(dir, sinceSession, "subagents")
	os.MkdirAll(subs, 0o700)
	os.WriteFile(filepath.Join(subs, "agent-a1.jsonl"), []byte(line("toolu_2", "mcp__a__y", "NEWER", "2026-11-11T10:05:00Z")), 0o600)
	os.WriteFile(filepath.Join(subs, "agent-a2.jsonl"), []byte(line("toolu_3", "mcp__a__z", "OLDER", "2026-11-11T09:00:00Z")), 0o600)
	got, ok := claudeFormat{}.LastBeacon(Input{Conversation: sinceSession, transcript: tr})
	if !ok || got != (FoundBeacon{Token: "NEWER", Call: "toolu_2", ToolName: "mcp__a__y"}) {
		t.Fatalf("%v %+v", ok, got)
	}
	// A line without a timestamp keeps the reading order: read last, it's
	// the newest.
	os.WriteFile(filepath.Join(subs, "agent-a3.jsonl"), []byte(strings.ReplaceAll(line("toolu_4", "mcp__a__w", "UNDATED", ""), `"timestamp":"",`, "")), 0o600)
	if got, ok := (claudeFormat{}).LastBeacon(Input{Conversation: sinceSession, transcript: tr}); !ok || got.Token != "UNDATED" {
		t.Fatalf("undated: %v %+v", ok, got)
	}
	// Without subagents, the session's own.
	os.RemoveAll(subs)
	if got, ok := (claudeFormat{}).LastBeacon(Input{Conversation: sinceSession, transcript: tr}); !ok || got.Token != "SESSION" {
		t.Fatalf("%v %+v", ok, got)
	}
}

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
