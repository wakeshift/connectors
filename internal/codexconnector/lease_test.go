package codexconnector

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A Codex thread subscribed, then sat idle. An hour later its connector
// stopped ("nobody used the thread for the lease"), and a later event woke
// nothing until the user typed again, where Claude Code's waiter would
// still have been waiting. While the
// connector holds the thread's stream (Wakeshift ends it once the session
// has no live subscription), the lease is renewed, up to Hold.

// idleThread runs a connector on an idle thread with stream as its link,
// and returns stopped (Run's error, nil if still running after wait).
func idleThread(t *testing.T, d *codexDaemon, stream string, cfg Config) (stopped func(wait time.Duration) error) {
	t.Helper()
	sock := d.serve(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg.Stream, cfg.Thread, cfg.Socket = stream, thread, sock
	cfg = trusting(cfg)
	cfg.Log = slog.New(slog.DiscardHandler)
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() { cancel(); <-done })
	return func(wait time.Duration) error {
		select {
		case err := <-done:
			done <- err
			return err
		case <-time.After(wait):
			return nil
		}
	}
}

// An idle thread whose stream the connector holds (still subscribed)
// outlives the lease many times over.
func TestSubscribedThreadOutlivesTheLease(t *testing.T) {
	d := &codexDaemon{status: "idle", turns: make(chan struct{}, 1)}
	stopped := idleThread(t, d, wakeshiftStream(t, 0), Config{Lease: 100 * time.Millisecond, Hold: time.Minute})
	if err := stopped(time.Second); err != nil { // ten leases
		t.Fatalf("stopped with the thread still subscribed: %v", err)
	}
}

// Without Hold (the connector before the fix), the same thread stops at the
// lease: what the pre-flight saw.
func TestWithoutHoldTheLeaseStillEnds(t *testing.T) {
	d := &codexDaemon{status: "idle", turns: make(chan struct{}, 1)}
	stopped := idleThread(t, d, wakeshiftStream(t, 0), Config{Lease: 100 * time.Millisecond})
	if err := stopped(5 * time.Second); !errors.Is(err, ErrLease) {
		t.Fatalf("returned %v, want ErrLease", err)
	}
}

// A connector that can't hold the thread's stream (no live link: Wakeshift
// keeps refusing it) stops at the lease, as before.
func TestUnheldStreamStopsAtTheLease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	d := &codexDaemon{status: "idle", turns: make(chan struct{}, 1)}
	start := time.Now()
	stopped := idleThread(t, d, "ws"+strings.TrimPrefix(srv.URL, "http"), Config{Lease: 300 * time.Millisecond, Hold: time.Minute})
	if err := stopped(5 * time.Second); !errors.Is(err, ErrLease) {
		t.Fatalf("returned %v, want ErrLease", err)
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Fatalf("stopped after %v, before the lease", took)
	}
}

// A thread whose last subscription ends (Wakeshift closes its stream with
// 4001, as StreamEnding does) stops at once, not at the hold.
func TestUnsubscribedThreadStopsAtOnce(t *testing.T) {
	end := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		select {
		case <-end:
			c.Close(4001, "unsubscribed")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	d := &codexDaemon{status: "idle", turns: make(chan struct{}, 1)}
	stopped := idleThread(t, d, "ws"+strings.TrimPrefix(srv.URL, "http"), Config{Lease: time.Minute, Hold: time.Hour})
	if err := stopped(300 * time.Millisecond); err != nil {
		t.Fatalf("stopped while subscribed: %v", err)
	}
	close(end)
	if err := stopped(5 * time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("returned %v, want ErrRevoked", err)
	}
}

// Even subscribed, a thread nobody uses stops at Hold: a connector can't
// live forever on a thread nobody looks at.
func TestHoldBoundsASubscribedThread(t *testing.T) {
	d := &codexDaemon{status: "idle", turns: make(chan struct{}, 1)}
	stopped := idleThread(t, d, wakeshiftStream(t, 0), Config{Lease: 50 * time.Millisecond, Hold: 400 * time.Millisecond})
	if err := stopped(5 * time.Second); !errors.Is(err, ErrHold) {
		t.Fatalf("returned %v, want ErrHold", err)
	}
}

// A wake the connector delivers renews the lease: the thread is in use.
// Without Hold, so only the wake can renew it. Each turn takes about 450ms
// here (Gather, then the daemon's answer), so the second wake is delivered
// about 1.3s in, and the connector must still run well past the first
// wake's lease (about 2s in).
func TestWakeRenewsTheLease(t *testing.T) {
	d := &codexDaemon{status: "idle", turns: make(chan struct{}, 2)}
	turnsThenIdle(d)
	const lease = 1500 * time.Millisecond
	turn, stopped, next := runThread(t, d, 2, Config{Lease: lease})
	turn(0) // the first event, at once
	time.Sleep(lease / 4)
	next <- struct{}{}
	turn(1) // renews it
	if err := stopped(time.Second); err != nil {
		t.Fatalf("stopped despite the wake: %v", err)
	}
	if err := stopped(5 * time.Second); !errors.Is(err, ErrLease) {
		t.Fatalf("returned %v, want ErrLease", err)
	}
}

// A wake renews Hold too, as Claude Code's waiter starts again after each
// wake.
func TestWakeRenewsTheHold(t *testing.T) {
	d := &codexDaemon{status: "idle", turns: make(chan struct{}, 2)}
	turnsThenIdle(d)
	const hold = 1500 * time.Millisecond
	turn, stopped, next := runThread(t, d, 2, Config{Lease: 50 * time.Millisecond, Hold: hold})
	turn(0)
	time.Sleep(hold / 4)
	next <- struct{}{}
	turn(1) // renews it (see TestWakeRenewsTheLease for the timing)
	if err := stopped(time.Second); err != nil {
		t.Fatalf("stopped despite the wake: %v", err)
	}
	if err := stopped(5 * time.Second); !errors.Is(err, ErrHold) {
		t.Fatalf("returned %v, want ErrHold", err)
	}
}

// A dead thread still stops promptly with the thread subscribed: deleted,
// archived, or Codex's app-server gone.
func TestDeadThreadStopsWhileSubscribed(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		d := &codexDaemon{status: "idle", turns: make(chan struct{}, 1)}
		stopped := idleThread(t, d, wakeshiftStream(t, 0), Config{Lease: time.Hour, Hold: 23 * time.Hour})
		d.connected(t, 0)
		d.broadcast("thread/deleted", map[string]any{"threadId": thread})
		if err := stopped(5 * time.Second); !errors.Is(err, ErrThreadEnded) {
			t.Fatalf("returned %v, want ErrThreadEnded", err)
		}
	})
	t.Run("archived", func(t *testing.T) {
		d := &codexDaemon{status: "idle", turns: make(chan struct{}, 1)}
		stopped := idleThread(t, d, wakeshiftStream(t, 0), Config{Lease: time.Hour, Hold: 23 * time.Hour})
		d.connected(t, 0)
		d.broadcast("thread/archived", map[string]any{"threadId": thread})
		if err := stopped(5 * time.Second); !errors.Is(err, ErrThreadEnded) {
			t.Fatalf("returned %v, want ErrThreadEnded", err)
		}
	})
	t.Run("codex gone", func(t *testing.T) {
		d := &codexDaemon{status: "idle", turns: make(chan struct{}, 1)}
		stopped := idleThread(t, d, wakeshiftStream(t, 0), Config{Lease: time.Hour, Hold: 23 * time.Hour})
		d.connected(t, 0)
		d.drop() // no CodexGrace: stop at once
		if err := stopped(5 * time.Second); !errors.Is(err, ErrCodexGone) {
			t.Fatalf("returned %v, want ErrCodexGone", err)
		}
	})
}

// leaseOver's arithmetic, without clocks running.
func TestLeaseOver(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name        string
		lease, hold time.Duration
		used, held  time.Duration // ago; held -1: never
		connected   bool
		want        error
	}{
		{"no lease", 0, 0, 100 * time.Hour, -1, false, nil},
		{"used lately", time.Hour, 23 * time.Hour, 30 * time.Minute, -1, false, nil},
		{"unheld past the lease", time.Hour, 23 * time.Hour, 61 * time.Minute, -1, false, ErrLease},
		{"connected past the lease", time.Hour, 23 * time.Hour, 5 * time.Hour, -1, true, nil},
		{"disconnected lately", time.Hour, 23 * time.Hour, 5 * time.Hour, 30 * time.Minute, false, nil},
		{"disconnected a lease ago", time.Hour, 23 * time.Hour, 5 * time.Hour, 61 * time.Minute, false, ErrLease},
		{"connected past the hold", time.Hour, 23 * time.Hour, 23*time.Hour + time.Minute, -1, true, ErrHold},
		{"no hold: connected doesn't count", time.Hour, 0, 61 * time.Minute, -1, true, ErrLease},
		{"hold under the lease is the lease", time.Hour, time.Minute, 30 * time.Minute, -1, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &conn{cfg: Config{Lease: tc.lease, Hold: tc.hold}, lastUsed: now.Add(-tc.used)}
			if tc.held >= 0 {
				c.heldAt = now.Add(-tc.held)
			}
			if tc.connected {
				c.ws = &websocket.Conn{}
			}
			if got := c.leaseOver(now); !errors.Is(got, tc.want) || (got == nil) != (tc.want == nil) {
				t.Fatalf("leaseOver = %v, want %v", got, tc.want)
			}
		})
	}
}
