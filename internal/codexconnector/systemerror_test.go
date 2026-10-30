package codexconnector

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// modelDown makes d's turns fail as Codex 0.163's do when the model
// answers 503 (at capacity): active, then systemError, where the thread
// stays. While up, they go active, then idle. It records when each turn
// started.
type modelDown struct {
	down atomic.Bool
	mu   sync.Mutex
	at   []time.Time
}

func (m *modelDown) install(d *codexDaemon) {
	d.onTurn = func(send func(string, any)) {
		m.mu.Lock()
		m.at = append(m.at, time.Now())
		m.mu.Unlock()
		end := "idle"
		if m.down.Load() {
			end = "systemError"
		}
		d.set(func(d *codexDaemon) { d.status = end })
		send("thread/status/changed", statusNote("active"))
		send("thread/status/changed", statusNote(end))
	}
}

func (m *modelDown) starts() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Time(nil), m.at...)
}

// runScripted runs the connector on d's thread with a scripted stream, and
// returns the frames to send, the acks it makes and Run's result.
func runScripted(t *testing.T, d *codexDaemon, cfg Config) (frames chan map[string]any, acks chan string, done chan error) {
	t.Helper()
	sock := d.serve(t)
	ctx, cancel := context.WithCancel(context.Background())
	frames, acks, done = make(chan map[string]any, 50), make(chan string, 50), make(chan error, 1)
	cfg.Stream, cfg.Thread, cfg.Socket = scriptedStream(t, frames, acks), thread, sock
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() { cancel(); <-done })
	return frames, acks, done
}

// nextTurn waits for d's next turn, failing the test if none starts.
func nextTurn(t *testing.T, d *codexDaemon, within time.Duration) {
	t.Helper()
	select {
	case <-d.turns:
	case <-time.After(within):
		t.Fatalf("no turn within %v", within)
	}
}

// noTurn fails the test if a turn starts, or anything is acked, within wait.
func noTurn(t *testing.T, d *codexDaemon, acks <-chan string, wait time.Duration, why string) {
	t.Helper()
	select {
	case <-d.turns:
		t.Fatalf("a turn started %s", why)
	case id := <-acks:
		t.Fatalf("acked %s %s", id, why)
	case <-time.After(wait):
	}
}

// ackedOnce collects acks until want are all in, and then a moment more,
// failing on any delivery acked twice or not wanted.
func ackedOnce(t *testing.T, acks <-chan string, want ...string) {
	t.Helper()
	got := map[string]int{}
	deadline := time.After(5 * time.Second)
	for len(got) < len(want) {
		select {
		case id := <-acks:
			got[id]++
		case <-deadline:
			t.Fatalf("acked %v, want %v", got, want)
		}
	}
	quiet := time.After(300 * time.Millisecond)
	for done := false; !done; {
		select {
		case id := <-acks:
			got[id]++
		case <-quiet:
			done = true
		}
	}
	for _, id := range want {
		if got[id] != 1 {
			t.Fatalf("acked %v, want each of %v once", got, want)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("acked %v, want %v", got, want)
	}
}

// S12 on October 29: the model was at capacity (503), Codex's thread went
// to systemError and stayed there, and the connector, waiting for idle,
// held 8 mentions and a DM until the user's next turn. Now a thread in
// systemError takes a turn, acknowledged only once it succeeds: a turn
// that fails keeps its events, tried again after a backoff that doubles,
// and the turn that succeeds acknowledges each once.
func TestSystemErrorRetriedUntilTheTurnSucceeds(t *testing.T) {
	d := &codexDaemon{status: "systemError", turns: make(chan struct{}, 10)}
	var m modelDown
	m.down.Store(true)
	m.install(d)
	const first = 400 * time.Millisecond
	frames, acks, _ := runScripted(t, d, Config{RetryAfter: first, RetryMax: time.Minute})

	frames <- event(0)
	nextTurn(t, d, 5*time.Second) // at once: systemError takes a turn
	noTurn(t, d, acks, first/2, "during the backoff, or the failed turn was acked")
	frames <- event(1) // joins the held one
	nextTurn(t, d, 5*time.Second)
	noTurn(t, d, acks, first, "before the doubled backoff, or the failed turn was acked")
	m.down.Store(false)
	nextTurn(t, d, 5*time.Second)
	ackedOnce(t, acks, "d0", "d1")

	at := m.starts()
	if len(at) != 3 {
		t.Fatalf("%d turns", len(at))
	}
	if gap := at[1].Sub(at[0]); gap < first {
		t.Fatalf("second turn %v after the first, want the backoff (%v)", gap, first)
	}
	if gap := at[2].Sub(at[1]); gap < 2*first {
		t.Fatalf("third turn %v after the second, want the doubled backoff (%v)", gap, 2*first)
	}
	d.mu.Lock()
	outputs := append([]string(nil), d.outputs...)
	d.mu.Unlock()
	if !strings.Contains(outputs[0], "hi 0") || strings.Contains(outputs[0], "hi 1") {
		t.Fatalf("first turn: %s", outputs[0])
	}
	for _, out := range outputs[1:] {
		if strings.Count(out, "hi 0") != 1 || strings.Count(out, "hi 1") != 1 {
			t.Fatalf("a retry lacks a held event, or repeats one: %s", out)
		}
	}

	// Then a turn from idle again: confirmed when it starts, as before.
	frames <- event(2)
	nextTurn(t, d, 5*time.Second)
	ackedOnce(t, acks, "d2")
}

// A turn started from idle is confirmed when it starts; if the model then
// fails it, its events come again in the next turn (they were never
// answered), without being acknowledged twice.
func TestFailedTurnFromIdleIsRetried(t *testing.T) {
	d := &codexDaemon{status: "idle", turns: make(chan struct{}, 10)}
	var m modelDown
	m.down.Store(true)
	m.install(d)
	frames, acks, _ := runScripted(t, d, Config{RetryAfter: 300 * time.Millisecond})
	frames <- event(0)
	nextTurn(t, d, 5*time.Second)
	ackedOnce(t, acks, "d0") // the turn started with it
	m.down.Store(false)
	nextTurn(t, d, 5*time.Second)
	noTurn(t, d, acks, 500*time.Millisecond, "after the retry succeeded")
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.outputs) != 2 || !strings.Contains(d.outputs[1], "hi 0") {
		t.Fatalf("turns %q", d.outputs)
	}
}

// The backoff doubles from RetryAfter and stops at RetryMax: 15s, 30s,
// 1m, 2m, 4m, then 5m by default.
func TestRetryBackoff(t *testing.T) {
	c := &conn{}
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := c.backoff(i + 1); got != w {
			t.Fatalf("failure %d: %v, want %v", i+1, got, w)
		}
	}
	if got := c.backoff(1000); got != retryMax {
		t.Fatalf("failure 1000: %v", got)
	}
}

// While the connector backs off, the user's turn comes: if it fails too
// (the model's still down) the backoff stands; once one succeeds (the
// thread idle) the held events' turn starts at once.
func TestUserTurnDuringBackoff(t *testing.T) {
	d := &codexDaemon{status: "systemError", turns: make(chan struct{}, 10)}
	var m modelDown
	m.down.Store(true)
	m.install(d)
	frames, acks, _ := runScripted(t, d, Config{RetryAfter: time.Minute})
	frames <- event(0)
	nextTurn(t, d, 5*time.Second)
	time.Sleep(200 * time.Millisecond) // its failure is seen

	userTurn := func(end string) {
		d.set(func(d *codexDaemon) { d.status = "active" })
		d.broadcast("thread/status/changed", statusNote("active"))
		time.Sleep(100 * time.Millisecond)
		d.set(func(d *codexDaemon) { d.status = end })
		d.broadcast("thread/status/changed", statusNote(end))
	}
	userTurn("systemError")
	noTurn(t, d, acks, 500*time.Millisecond, "after the user's turn failed too")

	m.down.Store(false)
	start := time.Now()
	userTurn("idle")
	nextTurn(t, d, 5*time.Second)
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("waited %v", waited)
	}
	ackedOnce(t, acks, "d0")
}

// A thread archived or deleted while the connector holds events for a
// model that's down still ends the connector, at once (#583), with
// nothing acknowledged.
func TestEndedDuringBackoff(t *testing.T) {
	for _, method := range []string{"thread/archived", "thread/deleted"} {
		t.Run(method, func(t *testing.T) {
			d := &codexDaemon{status: "systemError", turns: make(chan struct{}, 10)}
			var m modelDown
			m.down.Store(true)
			m.install(d)
			frames, acks, done := runScripted(t, d, Config{RetryAfter: time.Minute})
			frames <- event(0)
			nextTurn(t, d, 5*time.Second)
			time.Sleep(200 * time.Millisecond)
			if method == "thread/archived" {
				d.broadcast("thread/status/changed", statusNote("notLoaded"))
			}
			d.broadcast(method, map[string]any{"threadId": thread})
			select {
			case err := <-done:
				done <- err
				if !errors.Is(err, ErrThreadEnded) {
					t.Fatalf("stopped with %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("didn't stop")
			}
			select {
			case id := <-acks:
				t.Fatalf("acked %s", id)
			default:
			}
		})
	}
}

// What an app's wait takes while the connector backs off isn't in the
// retry, and isn't acknowledged by the connector.
func TestTakenDuringBackoff(t *testing.T) {
	d := &codexDaemon{status: "systemError", turns: make(chan struct{}, 10)}
	var m modelDown
	m.down.Store(true)
	m.install(d)
	frames, acks, _ := runScripted(t, d, Config{RetryAfter: 500 * time.Millisecond})
	frames <- event(0)
	frames <- event(1)
	nextTurn(t, d, 5*time.Second)
	frames <- taken(0)
	m.down.Store(false)
	nextTurn(t, d, 5*time.Second)
	ackedOnce(t, acks, "d1")
	d.mu.Lock()
	defer d.mu.Unlock()
	if last := d.outputs[len(d.outputs)-1]; strings.Contains(last, "hi 0") || !strings.Contains(last, "hi 1") {
		t.Fatalf("the retry: %s", last)
	}
}
