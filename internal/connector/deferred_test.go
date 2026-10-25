package connector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Antigravity writes a tool result's step to its transcript only after
// PostToolUse returns (found end to end): the hook keeps the step, and the
// conversation's next PreInvocation reads it, connects, and says so.
func TestAntigravityDeferred(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "antigravity", ws, newFakePoller())
	conv := "2b9d08bb-7acc-4459-9956-69064f8d5869"
	dir := filepath.Join(t.TempDir(), "logs")
	os.MkdirAll(dir, 0o700)
	tr := filepath.Join(dir, "transcript_full.jsonl")
	step := func(i int, content string) string {
		b, _ := json.Marshal(map[string]any{"step_index": i, "source": "MODEL", "type": "GENERIC", "content": content})
		return string(b) + "\n"
	}
	os.WriteFile(tr, []byte(step(3, "")), 0o600) // as agy has it while PostToolUse runs
	if out := runAs(h, "post-tool", strings.Replace(agyPostTool, "TRANSCRIPT", tr, 1)); out != "" || h.Links.Load(conv) != "" {
		t.Fatalf("post-tool: %q", out)
	}
	// Not written yet at the next hook either: kept for later.
	if out := runAs(h, "pre-invocation", agyPreInv); out != "" {
		t.Fatalf("too early: %q", out)
	}
	f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(step(4, "probe ok\n"+BeaconText(ws.beacon(t, conv, nil))))
	f.Close()
	out := runAs(h, "pre-invocation", agyPreInv)
	if !has(out, `"injectSteps"`, "Wakeshift: connected") || h.Links.Load(conv) == "" {
		t.Fatalf("pre-invocation: %q", out)
	}
	if r := ws.redemptions(); len(r) != 1 {
		t.Fatalf("redeemed %v", r)
	}
	// Done with: not read again.
	if out := runAs(h, "pre-invocation", agyPreInv); out != "" || len(ws.redemptions()) != 1 {
		t.Fatalf("again: %q", out)
	}
}

// Antigravity's model sees a tool result's text, beacon and all, as soon as
// agy writes it: the post-tool hook starts a resolver that redeems the
// beacon the moment the step is written, not at the next turn, so it's used
// up before the model could repeat it. The next hook doesn't redeem it
// again.
func TestAntigravityRedeemsAtPostTool(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := specHook(t, "antigravity", ws, newFakePoller())
	conv := "2b9d08bb-7acc-4459-9956-69064f8d5869"
	dir := filepath.Join(t.TempDir(), "logs")
	os.MkdirAll(dir, 0o700)
	tr := filepath.Join(dir, "transcript_full.jsonl")
	step := func(i int, content string) string {
		b, _ := json.Marshal(map[string]any{"step_index": i, "source": "MODEL", "type": "GENERIC", "content": content})
		return string(b) + "\n"
	}
	os.WriteFile(tr, []byte(step(3, "")), 0o600)
	// The resolver runs as its own process: here, a second hook.
	resolver := specHook(t, "antigravity", ws, newFakePoller())
	resolver.Verifier, resolver.Links = h.Verifier, h.Links
	done := make(chan string, 1)
	h.ResolveSoon = func(c string) {
		go func() {
			b, _ := json.Marshal(map[string]string{"conversationId": c})
			done <- runAs(resolver, "resolve", string(b))
		}()
	}
	if out := runAs(h, "post-tool", strings.Replace(agyPostTool, "TRANSCRIPT", tr, 1)); out != "" {
		t.Fatalf("post-tool: %q", out)
	}
	// agy writes the step once the hook has returned.
	time.Sleep(100 * time.Millisecond)
	f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(step(4, "probe ok\n"+BeaconText(ws.beacon(t, conv, nil))))
	f.Close()
	select {
	case out := <-done:
		if out != "" {
			t.Fatalf("resolver said %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the resolver didn't finish")
	}
	if r := ws.redemptions(); len(r) != 1 || h.Links.Load(conv) == "" {
		t.Fatalf("not redeemed by the resolver: %v", r)
	}
	// The note still comes at the next turn's start; nothing is redeemed twice.
	if out := runAs(h, "pre-invocation", agyPreInv); !has(out, "Wakeshift: connected") || len(ws.redemptions()) != 1 {
		t.Fatalf("pre-invocation: %q, %v", out, ws.redemptions())
	}
}
