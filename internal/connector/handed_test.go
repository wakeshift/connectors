package connector

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sdkConv = "c95dca59-8b63-4ebb-a70e-8d69fde72b92" // claudeStop's session

// sdkWaiter is a Claude Code waiter in an unattended session with
// streaming input (ClaudeStreaming), as cli.go sets one up.
func sdkWaiter(t *testing.T, p *fakePoller, links Links, dir string) *Hook {
	t.Helper()
	ws := newFakeWakeshift(t)
	h := testHook(t, ws, p)
	h.Format, h.Harness, h.Headline = claudeFormat{}, claudeName, true
	h.Verifier.Settings.Dir = dir
	h.Links = links
	h.Rewake, h.WaiterKey, h.ConfirmAtTurnEnd = true, "4242", true
	h.Repoll, h.RewakeWait = 100*time.Millisecond, 3*time.Second
	return h
}

// runWaiter runs the hook on a Claude Code hook input (event: Stop or
// SessionStart), returning its exit code and stderr.
func runWaiter(h *Hook, event string) (int, string) {
	in := strings.Replace(claudeStop, `"hook_event_name":"Stop"`, `"hook_event_name":"`+event+`"`, 1)
	var out, errb bytes.Buffer
	code := h.Run(context.Background(), strings.NewReader(in), &out, &errb)
	return code, errb.String()
}

// #1272: in an unattended session with streaming input, a handover is
// confirmed only once the woken turn has run: at that turn's Stop hook,
// not when it's handed over. A one-shot Agent SDK query exits after the
// handover with no turn: what it was handed stays pending and comes again.
func TestHandoverConfirmedAtTheWokenTurnsStop(t *testing.T) {
	dir := t.TempDir()
	p := newFakePoller()
	set := Settings{Dir: dir}
	links := LinksFor(set)
	link := "wss://wakeshift.test/v1/stream?link=a"
	links.Save(sdkConv, link)
	p.add(link, msg("d1", "deploy failed"))

	// The waiter hands it over (exit 2 wakes the session), unconfirmed.
	h := sdkWaiter(t, p, links, dir)
	code, text := runWaiter(h, "Stop")
	if code != 2 || !strings.Contains(text, "deploy failed") {
		t.Fatalf("the handover: exit %d %q (%s)", code, text, h.why)
	}
	if len(p.ackedNow()) != 0 {
		t.Fatalf("confirmed at the handover: %v", p.ackedNow())
	}
	if ids := h.handedIDs(sdkConv); len(ids) != 1 || ids[0] != "d1" {
		t.Fatalf("the record: %v", ids)
	}
	if st, err := os.Stat(h.handedPath(sdkConv)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the record's file: %v %v", st, err)
	}

	// A one-shot query's process exits here, with no turn and no Stop: it's
	// never confirmed, and the stream hands it out again (the next session
	// that listens gets it: a duplicate at worst, never a loss).
	if b, _ := p.Poll(context.Background(), link, 0, false); len(b.Deliveries) != 1 || b.Deliveries[0].ID != "d1" {
		t.Fatalf("after a one-shot query's exit: %+v", b)
	}

	// A streaming session: the woken turn ran, and its own Stop hook
	// confirms d1 before it polls, so it doesn't hand d1 over again, and
	// listens for what's next.
	next := sdkWaiter(t, p, links, dir)
	go func() {
		time.Sleep(300 * time.Millisecond)
		p.add(link, msg("d2", "deploy fixed"))
	}()
	code, text = runWaiter(next, "Stop")
	if acked := p.ackedNow(); len(acked) != 1 || acked[0] != "d1" {
		t.Fatalf("the woken turn's Stop confirmed %v", acked)
	}
	if code != 2 || strings.Contains(text, "deploy failed") || !strings.Contains(text, "deploy fixed") {
		t.Fatalf("the next wake: exit %d %q", code, text)
	}
	if ids := next.handedIDs(sdkConv); len(ids) != 1 || ids[0] != "d2" {
		t.Fatalf("the record now: %v", ids)
	}
}

// Another process's handover (a resumed SDK session runs a new Claude
// process: another CLAUDE_PID), or another conversation's, isn't confirmed
// by this one's Stop: it may never have reached a turn.
func TestHandoverConfirmedOnlyInItsProcess(t *testing.T) {
	dir := t.TempDir()
	p := newFakePoller()
	links := LinksFor(Settings{Dir: dir})
	link := "wss://wakeshift.test/v1/stream?link=a"
	links.Save(sdkConv, link)
	h := sdkWaiter(t, p, links, dir)
	h.saveHanded(sdkConv, []string{"d1"})
	h.saveHanded("another-conversation", []string{"d9"})

	other := sdkWaiter(t, p, links, dir)
	other.WaiterKey = "5151"
	other.RewakeWait = 300 * time.Millisecond
	runWaiter(other, "Stop")
	if acked := p.ackedNow(); len(acked) != 0 {
		t.Fatalf("another process confirmed %v", acked)
	}
	if len(h.handedIDs(sdkConv)) != 1 || len(h.handedIDs("another-conversation")) != 1 {
		t.Fatal("another process's Stop removed the record")
	}
}

// A waiter not started by a turn's end (SessionStart: a compaction while
// the woken turn runs) doesn't poll while a handover waits for its turn's
// Stop: the poll would hand it over twice. The Stop's waiter replaces it.
func TestSessionStartWaiterLeavesAPendingHandover(t *testing.T) {
	dir := t.TempDir()
	p := newFakePoller()
	links := LinksFor(Settings{Dir: dir})
	link := "wss://wakeshift.test/v1/stream?link=a"
	links.Save(sdkConv, link)
	p.add(link, msg("d1", "deploy failed"))
	h := sdkWaiter(t, p, links, dir)
	h.saveHanded(sdkConv, []string{"d1"})

	ss := sdkWaiter(t, p, links, dir)
	done := make(chan string, 1)
	go func() { _, text := runWaiter(ss, "SessionStart"); done <- text }()
	time.Sleep(500 * time.Millisecond)
	p.mu.Lock()
	polls := len(p.polls)
	p.mu.Unlock()
	if polls != 0 {
		t.Fatalf("the SessionStart waiter polled %d times with a handover pending", polls)
	}
	// The woken turn ends: its Stop's waiter takes over, confirms d1, and
	// the SessionStart waiter stops, having handed nothing over.
	stop := sdkWaiter(t, p, links, dir)
	stop.RewakeWait = 500 * time.Millisecond
	runWaiter(stop, "Stop")
	select {
	case text := <-done:
		if text != "" || ss.why != "a newer waiter took over" {
			t.Fatalf("the SessionStart waiter: %q (%s)", text, ss.why)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the SessionStart waiter didn't stop")
	}
	if acked := p.ackedNow(); len(acked) != 1 || acked[0] != "d1" {
		t.Fatalf("confirmed %v", acked)
	}
}

// Attended (the terminal), a handover is confirmed as it's handed over,
// as before.
func TestAttendedHandoverConfirmedAtOnce(t *testing.T) {
	dir := t.TempDir()
	p := newFakePoller()
	links := LinksFor(Settings{Dir: dir})
	link := "wss://wakeshift.test/v1/stream?link=a"
	links.Save(sdkConv, link)
	p.add(link, msg("d1", "deploy failed"))
	h := sdkWaiter(t, p, links, dir)
	h.ConfirmAtTurnEnd = false
	if code, _ := runWaiter(h, "Stop"); code != 2 || strings.Join(p.ackedNow(), ",") != "d1" || h.handedIDs(sdkConv) != nil {
		t.Fatalf("exit %d, acked %v, record %v", code, p.ackedNow(), h.handedIDs(sdkConv))
	}
}

// The waiter watches Claude's process (CLAUDE_PID) while it waits: once
// it's gone, the waiter stops within ParentEvery, taking nothing, so it
// can't hold the stream (or take a message) for a process that exited.
func TestWaiterStopsWhenClaudeIsGone(t *testing.T) {
	old := ParentEvery
	ParentEvery = 100 * time.Millisecond
	t.Cleanup(func() { ParentEvery = old })
	claude := exec.Command("sleep", "30")
	if err := claude.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := newFakePoller()
	links := LinksFor(Settings{Dir: dir})
	link := "wss://wakeshift.test/v1/stream?link=a"
	links.Save(sdkConv, link)
	h := sdkWaiter(t, p, links, dir)
	h.Parent, h.RewakeWait, h.Repoll = claude.Process.Pid, time.Minute, 30*time.Second
	done := make(chan int, 1)
	go func() { code, _ := runWaiter(h, "Stop"); done <- code }()
	time.Sleep(300 * time.Millisecond)
	claude.Process.Kill()
	claude.Wait()
	start := time.Now()
	select {
	case code := <-done:
		if code != 0 || time.Since(start) > 2*time.Second {
			t.Fatalf("exit %d after %v", code, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter outlived Claude")
	}
	// What arrives after it's gone stays for the next listener.
	p.add(link, msg("d1", "after Claude exited"))
	if len(p.ackedNow()) != 0 || h.handedIDs(sdkConv) != nil {
		t.Fatalf("taken after Claude exited: %v", p.ackedNow())
	}
	// And a waiter whose Claude is already gone hands nothing over, even
	// when a message is waiting.
	late := sdkWaiter(t, p, links, dir)
	late.Parent = claude.Process.Pid
	if code, text := runWaiter(late, "Stop"); code != 0 || text != "" || len(p.ackedNow()) != 0 {
		t.Fatalf("a waiter for a gone Claude: exit %d %q", code, text)
	}
}

// In a streaming session the waiter watches Claude's input too
// (InputClosed): once a one-shot query's input has closed it stops within
// InputEvery, taking nothing, so Claude Code doesn't hold its exit 30 s
// for it; a waiter that starts after the input closed doesn't listen at
// all; and one that took messages as the input closed hands nothing over
// (what it took comes again).
func TestWaiterStopsWhenClaudesInputCloses(t *testing.T) {
	old := InputEvery
	InputEvery = 50 * time.Millisecond
	t.Cleanup(func() { InputEvery = old })
	var closed atomic.Bool
	dir := t.TempDir()
	p := newFakePoller()
	links := LinksFor(Settings{Dir: dir})
	link := "wss://wakeshift.test/v1/stream?link=a"
	links.Save(sdkConv, link)
	h := sdkWaiter(t, p, links, dir)
	h.InputClosed = closed.Load
	h.RewakeWait, h.Repoll = time.Minute, 30*time.Second
	done := make(chan int, 1)
	go func() { code, _ := runWaiter(h, "Stop"); done <- code }()
	time.Sleep(200 * time.Millisecond)
	select {
	case code := <-done:
		t.Fatalf("stopped (exit %d) while the input was open", code)
	default:
	}
	closed.Store(true)
	start := time.Now()
	select {
	case code := <-done:
		if code != 0 || time.Since(start) > time.Second {
			t.Fatalf("exit %d after %v", code, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter outlived Claude's input")
	}
	// A waiter that starts with the input closed hands nothing over, even
	// with a message waiting, and leaves it pending.
	p.add(link, msg("d1", "after the input closed"))
	late := sdkWaiter(t, p, links, dir)
	late.InputClosed = closed.Load
	if code, text := runWaiter(late, "Stop"); code != 0 || text != "" || len(p.ackedNow()) != 0 || late.handedIDs(sdkConv) != nil {
		t.Fatalf("a waiter after the input closed: exit %d %q, acked %v", code, text, p.ackedNow())
	}
	// Input that closes between the take and the handover: nothing is
	// handed over or acknowledged.
	var checks atomic.Int32
	racing := sdkWaiter(t, p, links, dir)
	racing.InputClosed = func() bool { return checks.Add(1) > 1 } // open at the start, closed by the handover
	InputEvery = time.Hour
	if code, text := runWaiter(racing, "Stop"); code != 0 || text != "" || len(p.ackedNow()) != 0 || racing.handedIDs(sdkConv) != nil {
		t.Fatalf("input closed at the handover: exit %d %q, acked %v", code, text, p.ackedNow())
	}
	if !strings.Contains(racing.why, "before the messages could be handed over") {
		t.Fatalf("why = %q: the take didn't reach the handover", racing.why)
	}
}
