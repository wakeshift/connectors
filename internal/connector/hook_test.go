package connector

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/wakeshift-plugins/internal/beacon"
	"github.com/clawvisor/wakeshift-plugins/internal/jws"
)

// testFormat is a minimal harness format for the hook's own logic.
type testFormat struct{}

func (testFormat) Parse(b []byte) (Input, error) {
	var c struct {
		Event, Conversation, Call string
		Result                    json.RawMessage
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Input{}, err
	}
	in := Input{Conversation: c.Conversation, ToolCallID: c.Call, ToolResult: c.Result}
	switch c.Event {
	case "tool":
		in.Event = ToolResult
	case "stop":
		in.Event = Stop
	}
	return in, nil
}
func (testFormat) Context(_ Input, t string) (Output, bool) {
	return Output{Stdout: []byte("context:" + t)}, true
}
func (testFormat) Continue(_ Input, t string) (Output, bool) {
	return Output{Stdout: []byte("continue:" + t), ExitCode: 0}, true
}

func testHook(t *testing.T, ws *fakeWakeshift, p *fakePoller) *Hook {
	t.Helper()
	set := Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}
	return &Hook{Harness: "test", Format: testFormat{}, Verifier: &Verifier{Settings: set}, Poller: p, Links: LinksFor(set),
		When: "when it stops", StopWait: 2 * time.Second}
}

func call(h *Hook, in map[string]any) (string, int) {
	b, _ := json.Marshal(in)
	var out, errb bytes.Buffer
	code := h.Run(context.Background(), bytes.NewReader(b), &out, &errb)
	return out.String() + errb.String(), code
}

func result(token string) json.RawMessage {
	r := map[string]any{"content": []any{map[string]string{"type": "text", "text": "ok"}}}
	if token != "" {
		r["_meta"] = map[string]any{beacon.MetaKey: token}
	}
	b, _ := json.Marshal(r)
	return b
}

// A beacon from a trusted Wakeshift for this conversation connects it (the
// link kept, a note for the model); anything else is ignored silently.
func TestHookConnects(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := testHook(t, ws, p)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	for name, res := range map[string]json.RawMessage{
		"no beacon":                  result(""),
		"another conversation's":     result(ws.beacon(t, "conv-2", nil)),
		"signed by another key":      result(ws.beacon(t, "conv-1", other)),
		"garbage":                    result("a.b.c"),
		"not even a result":          json.RawMessage(`"nope"`),
		"an unbound one, not wanted": result(unbound(t, ws)),
	} {
		if out, _ := call(h, map[string]any{"event": "tool", "conversation": "conv-1", "result": res}); out != "" || len(ws.redemptions()) != 0 {
			t.Fatalf("%s: %q %v", name, out, ws.redemptions())
		}
	}
	out, _ := call(h, map[string]any{"event": "tool", "conversation": "conv-1", "result": result(ws.beacon(t, "conv-1", nil))})
	if !has(out, "context:Wakeshift: connected", "when it stops") || h.Links.Load("conv-1") == "" {
		t.Fatalf("connect: %q", out)
	}
	if r := ws.redemptions(); len(r) != 1 || r[0] != "wst_t@conv-1" {
		t.Fatalf("redeemed %v", r)
	}
	// A refused redeem says why.
	ws.status, ws.code = 404, "unknown_ticket"
	out, _ = call(h, map[string]any{"event": "tool", "conversation": "conv-1", "result": result(ws.beacon(t, "conv-1", nil))})
	if !has(out, "couldn't connect", "expired or was used") {
		t.Fatalf("refused: %q", out)
	}
}

// Beacons bound to a call need the hook's call; unbound ones are accepted
// only by harnesses that can't do better.
func TestHookBindings(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := testHook(t, ws, newFakePoller())
	byCall := callBeacon(t, ws, "toolu_1")
	if out, _ := call(h, map[string]any{"event": "tool", "conversation": "s1", "call": "toolu_2", "result": result(byCall)}); out != "" {
		t.Fatalf("another call's: %q", out)
	}
	if out, _ := call(h, map[string]any{"event": "tool", "conversation": "s1", "call": "toolu_1", "result": result(byCall)}); !has(out, "connected") {
		t.Fatalf("its call's: %q", out)
	}
	h.Unbound = true
	if out, _ := call(h, map[string]any{"event": "tool", "conversation": "s2", "result": result(unbound(t, ws))}); !has(out, "connected") {
		t.Fatalf("unbound: %q", out)
	}
	if out, _ := call(h, map[string]any{"event": "tool", "conversation": "s3", "result": result(ws.beacon(t, "s4", nil))}); out != "" {
		t.Fatalf("unbound harness, beacon for another conversation: %q", out)
	}
}

func unbound(t *testing.T, ws *fakeWakeshift) string { return callBeacon(t, ws, "") }

func callBeacon(t *testing.T, ws *fakeWakeshift, callID string) string {
	t.Helper()
	tok, err := jws.Sign(ws.key, "k1", beacon.Type, beacon.Claims{Issuer: ws.URL, Audience: beacon.Audience, Call: callID, App: "chat",
		Redeem: ws.URL + "/v1/connector/redeem", Ticket: "wst_t", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A stop hook continues the session with what's waiting, and acks it only
// then; with nothing, it lets the session stop after its wait.
func TestHookStop(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := testHook(t, ws, p)
	h.StopWait = 300 * time.Millisecond
	if out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"}); out != "" || len(p.polls) != 0 {
		t.Fatalf("not connected: %q %v", out, p.polls)
	}
	link := "wss://wakeshift.test/v1/stream?link=a"
	h.Links.Save("c", link)
	start := time.Now() // listens most of StopWait (the rest is room for the answer)
	if out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"}); out != "" || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("nothing waiting: %q after %v", out, time.Since(start))
	}
	p.add(link, msg("d1", "ping"), msg("d2", "pong"))
	out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"})
	if !has(out, "continue:", "2 messages from outside", "ping", "pong") || strings.Join(p.acked, ",") != "d1,d2" {
		t.Fatalf("waiting: %q acked %v", out, p.acked)
	}
	// A tool call hands over what's waiting as context, at once.
	p.add(link, msg("d3", "mid-turn"))
	out, _ = call(h, map[string]any{"event": "tool", "conversation": "c", "result": result("")})
	if !has(out, "context:", "mid-turn") || p.acked[len(p.acked)-1] != "d3" || p.polls[len(p.polls)-1] != 0 {
		t.Fatalf("tool: %q %v %v", out, p.acked, p.polls)
	}
}

// A revoked link is forgotten; a rotated one replaces it.
func TestHookLinkChanges(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := testHook(t, ws, p)
	h.StopWait = 100 * time.Millisecond
	h.Links.Save("c", "wss://wakeshift.test/v1/stream?link=a")
	p.rotate = "wss://wakeshift.test/v1/stream?link=b"
	call(h, map[string]any{"event": "stop", "conversation": "c"})
	if h.Links.Load("c") != "wss://wakeshift.test/v1/stream?link=b" {
		t.Fatalf("rotated: %q", h.Links.Load("c"))
	}
	p.rotate = "wss://elsewhere.test/v1/stream?link=c"
	call(h, map[string]any{"event": "stop", "conversation": "c"})
	if h.Links.Load("c") != "wss://wakeshift.test/v1/stream?link=b" {
		t.Fatalf("a rotation to another host: %q", h.Links.Load("c"))
	}
	p.rotate, p.err = "", ErrLinkGone
	call(h, map[string]any{"event": "stop", "conversation": "c"})
	if h.Links.Load("c") != "" {
		t.Fatal("a revoked link kept")
	}
}

// deadlinePoller records how long each poll asked the server to wait, and
// how long its context had left.
type deadlinePoller struct {
	*fakePoller
	asked, left []time.Duration
}

func (p *deadlinePoller) Poll(ctx context.Context, link string, wait time.Duration, patient bool) (Batch, error) {
	d, _ := ctx.Deadline()
	p.asked, p.left = append(p.asked, wait), append(p.left, time.Until(d))
	return Batch{}, nil
}

// A stop hook's poll asks the server to stop waiting before the hook's own
// time runs out, so the answer (deliveries, a rotated link) isn't cut off.
func TestHookPollLeavesRoomForTheAnswer(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := &deadlinePoller{fakePoller: newFakePoller()}
	h := testHook(t, ws, nil)
	h.Poller, h.StopWait = p, time.Second
	h.Links.Save("c", "wss://wakeshift.test/v1/stream?link=a")
	call(h, map[string]any{"event": "stop", "conversation": "c"})
	if len(p.asked) == 0 || p.left[0]-p.asked[0] < 150*time.Millisecond {
		t.Fatalf("asked to wait %v with %v left", p.asked, p.left)
	}
}

// One waiter per key: a newer one makes the older stop.
func TestHookWaiterReplaced(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := testHook(t, ws, p)
	h.Rewake, h.WaiterKey, h.Repoll = true, "4242", 50*time.Millisecond
	h.Links.Save("c", "wss://wakeshift.test/v1/stream?link=a")
	h2 := *h
	done := make(chan string)
	go func() { out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"}); done <- out }()
	time.Sleep(100 * time.Millisecond)
	h2.claimWaiter()
	select {
	case out := <-done:
		if out != "" {
			t.Fatalf("old waiter said %q", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the old waiter kept waiting")
	}
}
