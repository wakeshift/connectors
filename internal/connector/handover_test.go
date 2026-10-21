package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// takenPoller is a fakePoller whose first polls answer 409 replaced:
// another hook of the conversation polled the stream meanwhile.
type takenPoller struct {
	*fakePoller
	mu    sync.Mutex
	taken int
	polls int
}

func (p *takenPoller) Poll(ctx context.Context, link string, wait time.Duration, patient bool) (Batch, error) {
	p.mu.Lock()
	p.polls++
	if p.taken > 0 {
		p.taken--
		p.mu.Unlock()
		return Batch{}, ErrReplaced
	}
	p.mu.Unlock()
	return p.fakePoller.Poll(ctx, link, wait, patient)
}

// The bench's S8 (Claude Code): after each wake, the next waiter exited
// within a second or two because a tool hook's poll took the stream ("a
// newer hook"), and nothing listened. The newest waiter now takes the
// stream back and keeps listening; only a waiter that a newer one
// replaced stops.
func TestNewestWaiterKeepsListening(t *testing.T) {
	ws := newFakeWakeshift(t)
	fp := newFakePoller()
	p := &takenPoller{fakePoller: fp, taken: 2}
	h := testHook(t, ws, fp)
	h.Poller, h.Rewake, h.WaiterKey, h.Repoll, h.RewakeWait = p, true, "4242", 200*time.Millisecond, 10*time.Second
	link := "wss://wakeshift.test/v1/stream?link=a"
	h.Links.Save("c", link)
	go func() {
		time.Sleep(2500 * time.Millisecond) // after two pauses (1s, 2s)
		fp.add(link, msg("d1", "after the tool hook's poll"))
	}()
	out, code := call(h, map[string]any{"event": "stop", "conversation": "c"})
	if !has(out, "continue:", "after the tool hook's poll") || code != 0 || strings.Join(fp.acked, ",") != "d1" || p.polls < 3 {
		t.Fatalf("the newest waiter: %q acked %v after %d polls (%s)", out, fp.acked, p.polls, h.why)
	}

	// A waiter a newer one replaced stops at the 409, and says so.
	p.taken = 1
	old := testHook(t, ws, fp)
	old.Verifier, old.Links, old.Poller, old.Rewake, old.WaiterKey, old.RewakeWait = h.Verifier, h.Links, p, true, "4242", 10*time.Second
	old.stale = func() bool { return true }
	start := time.Now()
	old.patient = true
	if text, _ := old.take(context.Background(), "c", time.Second, false); text != "" || old.why != "a newer waiter took over" || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("the replaced waiter: %q %q after %v", text, old.why, time.Since(start))
	}
	// Without a waiter key (nothing says which waiter is newest), a 409
	// still ends it.
	p.taken = 1
	nokey := testHook(t, ws, fp)
	nokey.Links, nokey.Poller, nokey.patient = h.Links, p, true
	if text, _ := nokey.take(context.Background(), "c", time.Second, false); text != "" || nokey.why != "another poll took the stream (a newer hook)" {
		t.Fatalf("without a key: %q %q", text, nokey.why)
	}
}

// Claude Code shows a PostToolUse hook's context only at the next user
// prompt, so that hook mustn't take messages (they'd be confirmed and not
// reach the model: S8's acked_unseen), nor poll the stream (it would take
// it from the waiter). The waiter delivers them.
func TestClaudeToolHookLeavesMessagesForTheWaiter(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := testHook(t, ws, p)
	h.Format = claudeFormat{}
	link := "wss://wakeshift.test/v1/stream?link=a"
	const session = "c95dca59-8b63-4ebb-a70e-8d69fde72b92"
	h.Links.Save(session, link)
	p.add(link, msg("d1", "mid-turn"))
	in := strings.Replace(claudePostToolUse, "TRANSCRIPT", filepath.Join(t.TempDir(), "none.jsonl"), 1)
	var out, errb bytes.Buffer
	h.Run(context.Background(), strings.NewReader(in), &out, &errb)
	if strings.Contains(out.String(), "mid-turn") || len(p.acked) != 0 || len(p.polls) != 0 {
		t.Fatalf("the tool hook: %q, acked %v, polls %v", out.String(), p.acked, p.polls)
	}
	// The waiter (Stop, --rewake) hands it over.
	h.Rewake = true
	var stop map[string]any
	json.Unmarshal([]byte(claudeStop), &stop)
	stop["session_id"] = session
	b, _ := json.Marshal(stop)
	out.Reset()
	errb.Reset()
	if code := h.Run(context.Background(), bytes.NewReader(b), &out, &errb); code != 2 || !strings.Contains(errb.String(), "mid-turn") || strings.Join(p.acked, ",") != "d1" {
		t.Fatalf("the waiter: %d %q acked %v", code, errb.String(), p.acked)
	}
}

// failWriter fails every write: the harness isn't there to read it.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A delivery is confirmed only once the hook has handed it over: written
// where the harness reads it. One it couldn't write comes again.
func TestConfirmedOnlyAfterTheHandOver(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := testHook(t, ws, p)
	link := "wss://wakeshift.test/v1/stream?link=a"
	h.Links.Save("c", link)
	p.add(link, msg("d1", "ping"))
	b, _ := json.Marshal(map[string]any{"event": "stop", "conversation": "c"})
	var errb bytes.Buffer
	h.Format = claudeFormat{} // exit 2 wakes Claude Code: not with nothing
	stop := strings.Replace(claudeStop, "c95dca59-8b63-4ebb-a70e-8d69fde72b92", "c", 1)
	if code := h.Run(context.Background(), strings.NewReader(stop), &errb, failWriter{}); code != 0 || len(p.acked) != 0 {
		t.Fatalf("a failed write: exit %d, acked %v", code, p.acked)
	}
	h.Format = testFormat{}
	h.Run(context.Background(), bytes.NewReader(b), failWriter{}, &errb)
	if len(p.acked) != 0 {
		t.Fatalf("confirmed though it wasn't handed over: %v", p.acked)
	}
	// Confirmed after the write, not before: the ack comes once the
	// output is out.
	var order []string
	w := &recordWriter{onWrite: func() { order = append(order, "write:"+strings.Join(p.acked, ",")) }}
	h.Run(context.Background(), bytes.NewReader(b), w, &errb)
	if len(order) == 0 || order[0] != "write:" || strings.Join(p.acked, ",") != "d1" {
		t.Fatalf("order %v, acked %v", order, p.acked)
	}
}

type recordWriter struct {
	bytes.Buffer
	onWrite func()
}

func (w *recordWriter) Write(b []byte) (int, error) {
	w.onWrite()
	return w.Buffer.Write(b)
}
