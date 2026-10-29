package codexconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wakeshift/connectors/internal/connector"
)

func eventInput(thread, event string) *bytes.Reader {
	b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": event, "cwd": "/tmp"})
	return bytes.NewReader(b)
}

// A connector that stopped while its thread sat idle (no tool call comes to
// start it again) is started again by the hook at the thread's next
// SessionStart or UserPromptSubmit, with the link the beacon's redeem gave,
// unless one is running. No link, no start; and the hook says nothing to
// the model at either.
func TestHookRearms(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	ws := newFakeWakeshift(t)
	var started []string
	running := false
	h := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}, Socket: fakeAppServer(t, thread),
		Start: func(th, link, why string, _ bool) error {
			started = append(started, th+" "+link+" ("+why+")")
			return nil
		},
		Running: func(string) bool { return running }}
	run := func(in *bytes.Reader) string {
		var out bytes.Buffer
		h.Run(context.Background(), in, &out)
		return out.String()
	}

	// Nothing kept yet: nothing to start.
	if out := run(eventInput(thread, EventSessionStart)); out != "" || len(started) != 0 {
		t.Fatalf("no link: %q %v", out, started)
	}
	// The beacon connects it, and the link is kept.
	if got := noteOf(t, run(hookInput(thread, ws.beacon(t, thread, nil)))); got != NoteConnected || len(started) != 1 {
		t.Fatalf("connect: %q %v", got, started)
	}
	if l := h.links().Load(thread); l != "ws://link-for-"+thread {
		t.Fatalf("kept %q", l)
	}
	// Running: nothing more at either event.
	running = true
	run(eventInput(thread, EventUserPromptSubmit))
	run(eventInput(thread, EventSessionStart))
	if len(started) != 1 {
		t.Fatalf("started over a running connector: %v", started)
	}
	// Stopped (the window went quiet, the lease ended, it was killed): the
	// user's next prompt, or opening the thread again, starts it.
	running = false
	for _, ev := range []string{EventUserPromptSubmit, EventSessionStart} {
		if out := run(eventInput(thread, ev)); out != "" {
			t.Fatalf("%s said %q", ev, out)
		}
		if want := thread + " ws://link-for-" + thread + " (re-armed at " + ev + ")"; started[len(started)-1] != want {
			t.Fatalf("%s: started %v", ev, started)
		}
	}
	// Another thread's events, and something that isn't a thread ID, start nothing.
	n := len(started)
	run(eventInput("01a14bd7-0000-7b70-a723-0abb9f38118e", EventUserPromptSubmit))
	run(eventInput("../../etc", EventUserPromptSubmit))
	if len(started) != n || len(ws.redeemed) != 1 {
		t.Fatalf("started %v, redeemed %v", started, ws.redeemed)
	}
}

// Running is the run lock: held while a connector runs, free once it stops.
func TestRunLock(t *testing.T) {
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	lock := RunLock(thread)
	if RunLock("../x") != "" || Running("") {
		t.Fatal("a lock for something that isn't a thread")
	}
	if Running(lock) {
		t.Fatal("running before anything ran")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { holdLock(ctx, lock, nil); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !Running(lock) {
		if time.Now().After(deadline) {
			t.Fatal("never held")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A second connector (a replacement) waits for it, then takes it.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { holdLock(ctx2, lock, nil); close(done2) }()
	cancel()
	<-done
	time.Sleep(1500 * time.Millisecond)
	if !Running(lock) {
		t.Fatal("the replacement didn't take the lock")
	}
	cancel2()
	<-done2
	if Running(lock) {
		t.Fatal("still held after it stopped")
	}
}

// The connector keeps the link current: a rotation saves the next one; a
// revoked link (unwatch) is forgotten, so the hook doesn't re-arm with it;
// a newer link, kept by a connector that replaced this one, stays.
func TestConnectorKeepsTheLink(t *testing.T) {
	links := connector.Links{Dir: filepath.Join(t.TempDir(), "links")}
	var mu sync.Mutex
	n := 0
	release := make(chan struct{}) // the rotated link is revoked once the test has seen it kept
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if k == 1 {
			b, _ := json.Marshal(map[string]any{"type": "hub.rotate", "url": "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/stream?link=next"})
			c.Write(r.Context(), websocket.MessageText, b)
			c.Close(websocket.StatusNormalClosure, "rotate")
			return
		}
		if k == 2 {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		c.Close(4001, "revoked")
	}))
	defer srv.Close()
	first := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/stream?link=first"
	next := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/stream?link=next"
	links.SaveFor(thread, first, "chat")

	stream := func() error {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		c := &conn{cfg: Config{Stream: first, Thread: thread, Links: &links, Log: slog.New(slog.DiscardHandler)}, cancel: cancel,
			seen: map[string]uint64{}, unconfirmed: map[string]bool{}}
		done := make(chan struct{})
		go func() { c.stream(ctx); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the stream went on")
		}
		return context.Cause(ctx)
	}
	// Rotated to next (kept, with its app), then next revoked: forgotten.
	saved := make(chan string, 1)
	go func() {
		defer close(release)
		for range 1000 {
			if l := links.Load(thread); l == next {
				saved <- links.AppOf(thread)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		saved <- ""
	}()
	if err := stream(); !errors.Is(err, ErrRevoked) {
		t.Fatalf("stopped with %v", err)
	}
	if app := <-saved; app != "chat" {
		t.Fatalf("the rotated link wasn't kept with its app (%q)", app)
	}
	if l := links.Load(thread); l != "" {
		t.Fatalf("a revoked link was kept: %q", l)
	}

	// A newer link (another connector's) isn't forgotten when this one's is revoked.
	mu.Lock()
	n = 2 // the next connection is closed as revoked at once
	mu.Unlock()
	links.SaveFor(thread, "ws://newer", "chat")
	if err := stream(); !errors.Is(err, ErrRevoked) {
		t.Fatalf("stopped with %v", err)
	}
	if l := links.Load(thread); l != "ws://newer" {
		t.Fatalf("forgot the newer link: %q", l)
	}
}

// A re-arm (Exclusive) starts only if no connector holds the thread's lock:
// re-arms racing each other, or a running connector, leave one connector.
func TestRearmIsExclusive(t *testing.T) {
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	lock := RunLock(thread)
	d := &codexDaemon{status: "idle", turns: make(chan struct{}, 1)}
	sock := d.serve(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() {
		first <- Run(ctx, Config{Stream: wakeshiftStream(t, 0), Thread: thread, Socket: sock, Lock: lock, Exclusive: true, Log: slog.New(slog.DiscardHandler)})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !Running(lock) {
		if time.Now().After(deadline) {
			t.Fatal("the first never held the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	err := Run(context.Background(), Config{Stream: wakeshiftStream(t, 0), Thread: thread, Socket: sock, Lock: lock, Exclusive: true, Log: slog.New(slog.DiscardHandler)})
	if !errors.Is(err, ErrRunning) {
		t.Fatalf("a second re-arm: %v", err)
	}
	select {
	case err := <-first:
		t.Fatalf("the first stopped: %v", err)
	default:
	}
	cancel()
	<-first
}
