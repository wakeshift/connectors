package connector

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countingPoller answers at once with nothing, however long it's asked to
// wait (a server that doesn't hold polls), counting the polls.
type countingPoller struct{ n atomic.Int64 }

func (p *countingPoller) Poll(ctx context.Context, link string, wait time.Duration, patient bool) (Batch, error) {
	p.n.Add(1)
	return Batch{}, nil
}
func (p *countingPoller) Ack(context.Context, string, []string) error { return nil }

// A stop hook's last second isn't a flood of zero-second polls (found end
// to end with Copilot), and a server that answers at once doesn't make it
// spin.
func TestStopDoesntSpin(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := testHook(t, ws, p)
	h.Links.Save("c", "wss://wakeshift.test/v1/stream?link=a")
	h.StopWait = 2500 * time.Millisecond
	call(h, map[string]any{"event": "stop", "conversation": "c"})
	if len(p.polls) > 2 {
		t.Fatalf("%d polls: %v", len(p.polls), p.polls)
	}
	for _, w := range p.polls[1:] {
		if w == 0 {
			t.Fatalf("a zero wait after the first: %v", p.polls)
		}
	}
	cp := &countingPoller{}
	h.Poller, h.StopWait = cp, 3*time.Second
	call(h, map[string]any{"event": "stop", "conversation": "c"})
	if n := cp.n.Load(); n > 4 {
		t.Fatalf("%d polls against a server that doesn't wait", n)
	}
}
