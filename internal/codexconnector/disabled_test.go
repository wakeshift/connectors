package codexconnector

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A connector that's running when Wakeshift is turned off on the machine
// (`wakeshift disable`, connector.DisabledFile) starts no turn: it stops
// before the wake, leaving what arrived unacknowledged for Wakeshift to hand
// out again. With the switch off, the same events start a turn and are
// acknowledged, so the test fails if the check goes.
func TestSwitchedOffStartsNoTurn(t *testing.T) {
	for _, off := range []bool{false, true} {
		d := &codexDaemon{status: "idle", turns: make(chan struct{}, 4)}
		sock := d.serve(t)
		acks := make(chan string, 4)
		var asked atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, trusting(Config{Stream: pacedStream(t, 1, nil, acks), Thread: thread, Socket: sock,
				Disabled: func() bool { asked.Add(1); return off }}))
		}()
		if !off {
			select {
			case <-d.turns:
			case err := <-done:
				t.Fatalf("on: Run returned: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("on: no turn started: the test proves nothing")
			}
			select {
			case <-acks:
			case <-time.After(5 * time.Second):
				t.Fatal("on: the event wasn't acknowledged")
			}
			cancel()
			<-done
			continue
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrDisabled) {
				t.Fatalf("off: Run returned %v, want ErrDisabled", err)
			}
		case <-d.turns:
			t.Fatal("off: a turn started")
		case <-time.After(5 * time.Second):
			t.Fatal("off: Run didn't stop")
		}
		cancel()
		if asked.Load() == 0 {
			t.Fatal("off: the switch was never asked")
		}
		select {
		case id := <-acks:
			t.Fatalf("off: %s was acknowledged", id)
		case <-d.turns:
			t.Fatal("off: a turn started")
		default:
		}
	}
}
