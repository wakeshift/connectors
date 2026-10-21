package connector

import (
	"strings"
	"testing"
	"time"
)

// A waiter that starts before its conversation is connected (Claude Code
// runs Stop while the async PostToolUse is still redeeming) waits for the
// link, then delivers; a plain stop hook doesn't wait.
func TestWaiterWaitsForLink(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := testHook(t, ws, p)
	h.Repoll = 50 * time.Millisecond
	if out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"}); out != "" {
		t.Fatalf("plain stop: %q", out)
	}
	h.Rewake = true
	link := "wss://wakeshift.test/v1/stream?link=a"
	done := make(chan string)
	go func() { out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"}); done <- out }()
	time.Sleep(300 * time.Millisecond) // the redeem lands after the waiter started
	p.add(link, msg("d1", "first message"))
	h.Links.Save("c", link)
	select {
	case out := <-done:
		if !strings.Contains(out, "first message") {
			t.Fatalf("%q", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiter never saw the link")
	}
	// With no link within the grace, it gives up.
	old := LinkGrace
	LinkGrace = 200 * time.Millisecond
	defer func() { LinkGrace = old }()
	start := time.Now()
	if out, _ := call(h, map[string]any{"event": "stop", "conversation": "never"}); out != "" || time.Since(start) > 2*time.Second {
		t.Fatalf("%q after %v", out, time.Since(start))
	}
}
