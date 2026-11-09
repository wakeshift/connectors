package codexconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/connector"
	"github.com/wakeshift/connectors/internal/jws"
)

// lateBeacon is a beacon for thread minted at iat (an hour's late redeem
// verifies it as of then), naming tool.
func (f *fakeWakeshift) lateBeacon(t *testing.T, thread, tool string, iat time.Time) string {
	t.Helper()
	tok, err := jws.Sign(f.key, "k1", beacon.Type, beacon.Claims{Issuer: f.URL, Audience: beacon.Audience, Thread: thread, App: "chat", Tool: tool,
		Redeem: f.URL + "/v1/connector/redeem", Ticket: "wst_late", IssuedAt: iat.Unix(), Expires: iat.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// rolloutLine is one line of a Codex 0.166 rollout: Codex's record of a
// completed MCP tool call, with its whole result.
func rolloutLine(thread, server, tool string, result any) map[string]any {
	return map[string]any{"timestamp": "2026-11-06T01:11:48.000Z", "type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": thread, "turn_id": "t",
		"item": map[string]any{"type": "McpToolCall", "id": "exec-1", "server": server, "tool": tool, "arguments": map[string]any{}, "status": "completed", "result": result}}}
}

func metaResult(token string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": "Install Wakeshift's plugin for Codex?"}}, "_meta": map[string]any{beacon.MetaKey: token}}
}

func writeRollout(t *testing.T, dir, thread string, lines ...map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "rollout-2026-11-05T17-11-38-"+thread+".jsonl")
	var b bytes.Buffer
	b.WriteString(`{"type":"session_meta","payload":{"id":"` + thread + `"}}` + "\n")
	for _, l := range lines {
		json.NewEncoder(&b).Encode(l)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Only Codex's own record of an MCP call of this thread, and only the
// result's own _meta: a beacon in text, structured content or a nested
// _meta (the app may relay someone else's), another thread's call, or a
// call that didn't complete isn't one. The newest counts.
func TestLastRolloutBeacon(t *testing.T) {
	dir := t.TempDir()
	other := "01a14bd7-0000-7b70-a723-0abb9f38118e"
	nested := map[string]any{"content": []any{}, "structuredContent": map[string]any{"result": map[string]any{"_meta": map[string]any{beacon.MetaKey: "n.n.n"}}}}
	text := map[string]any{"content": []any{map[string]any{"type": "text", "text": beacon.Text("t.t.t")}}}
	failed := rolloutLine(thread, "chat", "watch", metaResult("f.f.f"))
	failed["payload"].(map[string]any)["item"].(map[string]any)["status"] = "failed"
	modelSaw := map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "c", "output": []any{
		map[string]any{"type": "input_text", "text": `{"_meta":{"` + beacon.MetaKey + `":"m.m.m"}}`}}}}

	path := writeRollout(t, dir, thread,
		rolloutLine(thread, "chat", "watch", metaResult("a.a.a")),
		rolloutLine(thread, "chat", "history", nested),
		rolloutLine(thread, "chat", "history", text),
		rolloutLine(other, "chat", "watch", metaResult("o.o.o")),
		failed, modelSaw)
	got, ok := lastRolloutBeacon(path, thread)
	if !ok || got.Token != "a.a.a" || got.Server != "chat" || got.Tool != "watch" {
		t.Fatalf("got %+v %v", got, ok)
	}
	path = writeRollout(t, dir, thread, rolloutLine(thread, "chat", "watch", metaResult("a.a.a")), rolloutLine(thread, "chat.v2", "status", metaResult("b.b.b")))
	if got, _ := lastRolloutBeacon(path, thread); got.Token != "b.b.b" || got.Server != "chat_v2" {
		t.Fatalf("the newest, its server as Codex names it in tool names: %+v", got)
	}
	// Only a rollout named for the thread, by an absolute path.
	if _, ok := lastRolloutBeacon(path, other); ok {
		t.Fatal("another thread's rollout")
	}
	rel, _ := filepath.Rel(mustGetwd(t), path)
	if _, ok := lastRolloutBeacon(rel, thread); ok {
		t.Fatal("a relative path")
	}
	if _, ok := lastRolloutBeacon(filepath.Join(dir, "notes-"+thread+".jsonl"), thread); ok {
		t.Fatal("not a rollout")
	}
}

func mustGetwd(t *testing.T) string {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// The late redeem: at SessionStart (or UserPromptSubmit) for a thread with
// no link, the beacon of the result that offered the install, found in the
// rollout, is checked as PostToolUse checks one but as of when it was
// minted, and redeemed late, once; the "connected" note goes with that
// hook's output.
func TestHookLateRedeem(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	ws := newFakeWakeshift(t)
	minted := time.Now().Add(-10 * time.Minute)
	type lane struct {
		h       *Hook
		started []string
	}
	newHook := func(installed time.Time) *lane {
		l := &lane{}
		l.h = &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}, InstalledAt: installed}, Socket: fakeAppServer(t, thread),
			Start: func(th, link, _, _ string, _ bool) error { l.started = append(l.started, th+" "+link); return nil }}
		return l
	}
	run := func(l *lane, event, rollout string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": event, "transcript_path": rollout, "source": "resume"})
		var out bytes.Buffer
		l.h.Run(context.Background(), bytes.NewReader(b), &out)
		if out.Len() == 0 {
			return ""
		}
		var m struct {
			HookSpecificOutput struct{ HookEventName, AdditionalContext string }
		}
		if err := json.Unmarshal(out.Bytes(), &m); err != nil || m.HookSpecificOutput.HookEventName != event {
			t.Fatalf("hook output %q", out.String())
		}
		// The mark's note comes at every SessionStart (TestHookMark).
		return withoutMark(m.HookSpecificOutput.AdditionalContext)
	}
	offer := writeRollout(t, t.TempDir(), thread, rolloutLine(thread, "chat", "watch", metaResult(ws.lateBeacon(t, thread, "watch", minted))))

	l := newHook(time.Now().Add(-time.Minute))
	if got := run(l, EventSessionStart, offer); got != NoteConnected || len(l.started) != 1 || l.started[0] != thread+" "+ws.link(thread) {
		t.Fatalf("late: %q %v", got, l.started)
	}
	if len(ws.late) != 1 || !ws.late[0] || ws.redeemed[0] != "wst_late@"+thread {
		t.Fatalf("redeemed %v late %v", ws.redeemed, ws.late)
	}
	// UserPromptSubmit, run with it: the thread has its link (re-armed, not
	// redeemed again).
	if got := run(l, EventUserPromptSubmit, offer); got != "" || len(ws.redeemed) != 1 {
		t.Fatalf("again: %q %v", got, ws.redeemed)
	}

	// Nothing is redeemed for any of these.
	for name, c := range map[string]struct {
		installed time.Time
		rollout   string
	}{
		"hooks installed before the beacon (one saw it)": {minted.Add(-time.Minute), offer},
		"an install from before installed_at":            {time.Time{}, offer},
		"over an hour old": {time.Now(), writeRollout(t, t.TempDir(), thread,
			rolloutLine(thread, "chat", "watch", metaResult(ws.lateBeacon(t, thread, "watch", time.Now().Add(-61*time.Minute)))))},
		"another thread's beacon": {time.Now(), writeRollout(t, t.TempDir(), thread,
			rolloutLine(thread, "chat", "watch", metaResult(ws.lateBeacon(t, "01a14bd7-0000-7b70-a723-0abb9f38118e", "watch", minted))))},
		"another tool's result": {time.Now(), writeRollout(t, t.TempDir(), thread,
			rolloutLine(thread, "chat", "history", metaResult(ws.lateBeacon(t, thread, "watch", minted))))},
		"no rollout":   {time.Now(), ""},
		"in text only": {time.Now(), writeRollout(t, t.TempDir(), thread, rolloutLine(thread, "chat", "watch", map[string]any{"content": []any{map[string]any{"type": "text", "text": beacon.Text(ws.lateBeacon(t, thread, "watch", minted))}}}))},
	} {
		n := len(ws.redeemed)
		l := newHook(c.installed)
		if got := run(l, EventSessionStart, c.rollout); got != "" || len(ws.redeemed) != n || len(l.started) != 0 {
			t.Fatalf("%s: %q %v %v", name, got, ws.redeemed, l.started)
		}
	}

	// An issuer not trusted here.
	n := len(ws.redeemed)
	untrusted := newHook(time.Now())
	untrusted.h.Settings.Issuers = []string{"https://app.wakeshift.ai"}
	if got := run(untrusted, EventSessionStart, offer); got != "" || len(ws.redeemed) != n {
		t.Fatalf("untrusted: %q %v", got, ws.redeemed)
	}

	// From a server other than the one pinned to the app.
	pinned := newHook(time.Now())
	pins := connector.PinsFor(pinned.h.Settings)
	if err := pins.Allow("chat", "chat"); err != nil {
		t.Fatal(err)
	}
	evil := writeRollout(t, t.TempDir(), thread, rolloutLine(thread, "evil", "watch", metaResult(ws.lateBeacon(t, thread, "watch", minted))))
	if got := run(pinned, EventSessionStart, evil); got != "" || len(ws.redeemed) != n {
		t.Fatalf("another server: %q %v", got, ws.redeemed)
	}

	// A thread with a link already: re-armed, never redeemed late.
	linked := newHook(time.Now())
	connector.LinksFor(linked.h.Settings).SaveFor(thread, "ws://kept", "chat")
	linked.h.Running = func(string, string) bool { return false }
	if got := run(linked, EventSessionStart, offer); got != "" || len(ws.redeemed) != n || len(linked.started) != 1 || linked.started[0] != thread+" ws://kept" {
		t.Fatalf("linked: %q %v %v", got, ws.redeemed, linked.started)
	}

	// Refused (used, expired past the hour): tried once, then left to the
	// next result of the app's tools.
	ws.status = 404
	refused := newHook(time.Now())
	if got := run(refused, EventSessionStart, offer); got != "" || len(ws.redeemed) != n+1 || len(refused.started) != 0 {
		t.Fatalf("refused: %q %v", got, ws.redeemed)
	}
	if got := run(refused, EventUserPromptSubmit, offer); got != "" || len(ws.redeemed) != n+1 {
		t.Fatalf("refused, tried again: %q %v", got, ws.redeemed)
	}
}
