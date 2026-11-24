package connector

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// revokingPoller is the long-poll contract with revocation: a poll on a
// revoked link answers ErrLinkGone, at once or as soon as it's revoked
// while the poll waits, as the core's does.
type revokingPoller struct {
	mu      sync.Mutex
	revoked map[string]bool
	queue   map[string][]Delivery
	polled  []string
	changed chan struct{} // closed (and replaced) on every change
}

func newRevokingPoller() *revokingPoller {
	return &revokingPoller{revoked: map[string]bool{}, queue: map[string][]Delivery{}, changed: make(chan struct{})}
}

func (p *revokingPoller) touch() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *revokingPoller) revoke(link string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked[link] = true
	p.touch()
}

func (p *revokingPoller) add(link string, ds ...Delivery) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue[link] = append(p.queue[link], ds...)
	p.touch()
}

func (p *revokingPoller) links() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.polled...)
}

func (p *revokingPoller) Poll(ctx context.Context, link string, wait time.Duration, patient bool) (Batch, error) {
	t := time.NewTimer(wait)
	defer t.Stop()
	p.mu.Lock()
	p.polled = append(p.polled, link)
	p.mu.Unlock()
	for {
		p.mu.Lock()
		revoked, q, changed := p.revoked[link], append([]Delivery(nil), p.queue[link]...), p.changed
		p.mu.Unlock()
		if revoked {
			return Batch{}, ErrLinkGone
		}
		if len(q) > 0 || wait <= 0 {
			return Batch{Deliveries: q}, nil
		}
		select {
		case <-ctx.Done():
			return Batch{}, ctx.Err()
		case <-t.C:
			return Batch{}, nil
		case <-changed:
		}
	}
}

func (p *revokingPoller) Ack(ctx context.Context, link string, ids []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var kept []Delivery
	for _, d := range p.queue[link] {
		if !contains(ids, d.ID) {
			kept = append(kept, d)
		}
	}
	p.queue[link] = kept
	return nil
}

// A resumed conversation that calls watch again: its waiter (Claude Code's
// SessionStart or Stop hook) listens on the link the conversation held,
// and the re-watch's redeem supersedes that link's session, revoking it,
// before its hook saves the new link. The waiter takes up the new link
// and is woken by what comes on it, rather than stopping with "the link
// was revoked" and leaving the conversation with nothing listening.
func TestWaiterTakesUpTheLinkThatReplacedItsRevokedOne(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newRevokingPoller()
	h := testHook(t, ws, nil)
	h.Poller = p
	h.StopWait = 10 * time.Second
	old := strings.Replace(ws.URL, "http://", "ws://", 1) + "/v1/stream?link=old"
	if err := h.Links.SaveFor("c", old, "chat"); err != nil {
		t.Fatal(err)
	}
	next := strings.Replace(ws.URL, "http://", "ws://", 1) + "/v1/stream?link=tok-c"
	ws.onRedeem = func(held string) {
		if held != old {
			t.Errorf("redeemed holding %q, not the conversation's link", held)
		}
		p.revoke(held)
		// The waiter hears of the revocation before the redeem's answer
		// reaches the hook, and its link is saved.
		time.Sleep(300 * time.Millisecond)
	}

	type exit struct {
		out  string
		code int
	}
	done := make(chan exit, 1)
	go func() {
		w := testHook(t, ws, nil)
		w.Verifier, w.Links, w.Poller, w.StopWait = h.Verifier, h.Links, p, h.StopWait
		out, code := call(w, map[string]any{"event": "stop", "conversation": "c"})
		done <- exit{out, code}
	}()
	// The waiter is listening on the old link.
	for deadline := time.Now().Add(5 * time.Second); len(p.links()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the waiter never polled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out, _ := call(h, map[string]any{"event": "tool", "conversation": "c", "result": result(ws.beaconFor(t, "c", "chat"))}); !has(out, "connected") {
		t.Fatalf("the re-watch didn't connect: %q", out)
	}
	if got := h.Links.LoadFor("c", "chat"); got != next {
		t.Fatalf("the conversation's link is %q, not the redeem's", got)
	}
	p.add(next, msg("d1", "after the re-watch"))
	select {
	case e := <-done:
		if !has(e.out, "continue:", "after the re-watch") {
			t.Fatalf("the waiter wasn't woken by the new link's message: %q (polled %v)", e.out, p.links())
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the waiter didn't hand the new link's message over")
	}
	if l := p.links(); l[0] != old || l[len(l)-1] != next {
		t.Fatalf("polled %v", l)
	}
}

// A link revoked with nothing to replace it (an unwatch, or a redeem
// elsewhere) still ends the waiter, and is forgotten.
func TestWaiterStopsOnARevokedLinkWithNoSuccessor(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newRevokingPoller()
	h := testHook(t, ws, nil)
	h.Poller = p
	h.StopWait = 5 * time.Second
	old := strings.Replace(ws.URL, "http://", "ws://", 1) + "/v1/stream?link=old"
	h.Links.SaveFor("c", old, "chat")
	go func() {
		for len(p.links()) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		p.revoke(old)
	}()
	start := time.Now()
	if out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"}); out != "" {
		t.Fatalf("handed over %q", out)
	}
	if time.Since(start) > 3*time.Second || h.why != "the link was revoked" {
		t.Fatalf("stopped after %s: %q", time.Since(start), h.why)
	}
	if h.Links.Load("c") != "" {
		t.Fatal("the revoked link was kept")
	}
}

// forgetRevoked forgets the revoked link and gives the conversation's newer
// link for its app, on its server; nothing for a link that names no app,
// another app's or another server's link, or a waiter a newer one replaced.
func TestForgetRevoked(t *testing.T) {
	ws := newFakeWakeshift(t)
	h := testHook(t, ws, nil)
	old := "wss://wakeshift.test/v1/stream?link=old"
	next := "wss://wakeshift.test/v1/stream?link=next"
	other := "wss://wakeshift.test/v1/stream?link=other"
	h.Links.SaveFor("c", other, "other")
	h.Links.SaveFor("c", old, "chat")
	if got := h.forgetRevoked("c", "chat", old); got != "" || h.Links.LoadFor("c", "chat") != "" {
		t.Fatalf("no newer link: %q, kept %q", got, h.Links.LoadFor("c", "chat"))
	}
	if h.Links.LoadFor("c", "other") != other {
		t.Fatal("another app's link was forgotten")
	}
	h.Links.SaveFor("c", next, "chat")
	if got := h.forgetRevoked("c", "chat", old); got != next {
		t.Fatalf("the app's newer link: %q", got)
	}
	if got := h.forgetRevoked("c", "", old); got != "" {
		t.Fatalf("a link naming no app: %q", got)
	}
	if got := h.forgetRevoked("c", "chat", "wss://elsewhere.test/v1/stream?link=old"); got != "" {
		t.Fatalf("another server's: %q", got)
	}
	h.stale = func() bool { return true }
	if got := h.forgetRevoked("c", "chat", old); got != "" {
		t.Fatalf("a replaced waiter: %q", got)
	}
	h.stale = nil
	if got := h.forgetRevoked("c", "chat", next); got != "" || h.Links.LoadFor("c", "chat") != "" {
		t.Fatalf("the revoked link itself: %q", got)
	}
}
