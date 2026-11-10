package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/harnesses"
)

// promiseTranscript writes a transcript in which the agent called tool
// (call toolu_w1) at at, and its result said text, with a _meta holding
// only the server's serverInfo: what the Claude desktop app's Code tab
// wrote on November 9, 2026 for a watch through a claude.ai connector.
func promiseTranscript(t *testing.T, tool, text string, at time.Time) string {
	t.Helper()
	use := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_w1","name":%q,"input":{"harness":"claude-code+x"}}]},"sessionId":%q}`,
		at.UTC().Format(time.RFC3339Nano), tool, sinceSession)
	res := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"tool_use_id":"toolu_w1","type":"tool_result","content":[{"type":"text","text":%q}]}]},"mcpMeta":{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"webhooks","version":"0.1.0"}}},"sessionId":%q}`,
		at.UTC().Format(time.RFC3339Nano), text, sinceSession)
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(use+"\n"+res+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A result told the agent Wakeshift's plugin connects the conversation
// when the turn ends, and no beacon ever connected it (none reached the
// hooks): once its wait for the link is over, the waiter wakes the
// conversation, once, saying it isn't connected, so the agent stops
// promising the user a notification. It never asks for another call.
func TestWaiterUnconnectedPromise(t *testing.T) {
	old := LinkGrace
	LinkGrace = 100 * time.Millisecond
	defer func() { LinkGrace = old }()
	ws := newFakeWakeshift(t)
	cc, _ := harnesses.Get(claudeName)
	promise := "Watching your webhook.\n\n" + harnesses.PluginReadyNote(cc, "Webhooks", false) + " Pass session_key=\"k\" to this app's tools from now on."
	connectorTool := "mcp__5e4162e1-d466-42ec-82e7-0812dc6af25c__watch" // the desktop app's name for a claude.ai connector's tool

	tr := promiseTranscript(t, connectorTool, promise, time.Now().Add(-10*time.Second))
	h := sinceHook(t, ws, time.Time{})
	// A wake that never reached the harness (its write failed) isn't
	// counted: the next waiter says it.
	b, _ := json.Marshal(map[string]any{"session_id": sinceSession, "transcript_path": tr, "hook_event_name": "Stop"})
	if code := h.Run(context.Background(), bytes.NewReader(b), io.Discard, failWriter{}); code != 0 {
		t.Fatalf("a failed write: exit %d", code)
	}
	out, code := sinceStop(h, tr)
	if code != 2 || !strings.HasPrefix(out, NoteUnconnected) {
		t.Fatalf("%d %q", code, out)
	}
	for _, no := range []string{"call", "again", "Wakeshift: connected"} {
		if strings.Contains(out, no) {
			t.Fatalf("the note says %q: %s", no, out)
		}
	}
	// The next turn's waiter, for the same result: nothing.
	if out, code := sinceStop(h, tr); code != 0 || out != "" {
		t.Fatalf("again: %d %q", code, out)
	}

	// A note the hooks saved (the beacon was refused) comes first: it says why.
	h = sinceHook(t, ws, time.Time{})
	h.saveNote(sinceSession, "Wakeshift: didn't connect this conversation: a pin.")
	if out, code := sinceStop(h, tr); code != 2 || !strings.HasPrefix(out, "Wakeshift: didn't connect this conversation: a pin.\n\n"+NoteUnconnected) {
		t.Fatalf("with a saved note: %d %q", code, out)
	}

	// The hooks' own wording (the installer's) counts too.
	hooks := promiseTranscript(t, "mcp__webhooks__watch", harnesses.PluginReadyNote(cc, "Webhooks", true), time.Now())
	if out, code := sinceStop(sinceHook(t, ws, time.Time{}), hooks); code != 2 || !strings.HasPrefix(out, NoteUnconnected) {
		t.Fatalf("hooks: %d %q", code, out)
	}

	// A result written before its tool_use line still counts.
	lines := strings.Split(strings.TrimSpace(readFile(t, promiseTranscript(t, connectorTool, promise, time.Now()))), "\n")
	reordered := filepath.Join(t.TempDir(), "r.jsonl")
	if err := os.WriteFile(reordered, []byte(lines[1]+"\n"+lines[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := sinceStop(sinceHook(t, ws, time.Time{}), reordered); code != 2 || !strings.HasPrefix(out, NoteUnconnected) {
		t.Fatalf("result before its tool_use: %d %q", code, out)
	}

	for name, tr := range map[string]string{
		"no promise":            promiseTranscript(t, connectorTool, "Watching your webhook.", time.Now()),
		"over an hour old":      promiseTranscript(t, connectorTool, promise, time.Now().Add(-2*time.Hour)),
		"not an MCP tool's":     promiseTranscript(t, "WebFetch", promise, time.Now()),
		"quoted, not its start": promiseTranscript(t, connectorTool, "The docs say: "+harnesses.PluginReadyNote(cc, "Webhooks", false), time.Now()),
	} {
		if out, code := sinceStop(sinceHook(t, ws, time.Time{}), tr); code != 0 || out != "" {
			t.Fatalf("%s: %d %q", name, code, out)
		}
	}

	// A conversation the plugin connected listens as before.
	h = sinceHook(t, ws, time.Time{})
	own := "wss://wakeshift.test/v1/stream?link=a"
	h.Links.Save(sinceSession, own)
	h.Poller.(*fakePoller).add(own, msg("d1", "first post"))
	if out, code := sinceStop(h, tr); code != 2 || !strings.Contains(out, "first post") || strings.Contains(out, NoteUnconnected) {
		t.Fatalf("connected: %d %q", code, out)
	}
}
