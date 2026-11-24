package connector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"
)

// Handovers confirmed at the woken turn's end (Hook.ConfirmAtTurnEnd), and
// the waiter's watch on its harness's process (Hook.Parent).
//
// A waiter that hands messages over in such a session doesn't acknowledge
// them: it records their delivery IDs in <dir>/handed/<CLAUDE_PID>-<conversation hash>.json.
// The next waiter a turn's end starts in the same process and conversation
// (Claude Code's Stop hook) acknowledges them before it polls: that turn
// ran, so they reached the model. A process that exits instead (a one-shot
// SDK query) never acknowledges them, and Wakeshift hands them out again
// to the conversation's next listener: a duplicate at worst, never a loss.
// The record holds delivery IDs only, never a link.

// ParentEvery is how often a waiter checks that its harness's process
// (Hook.Parent) still runs.
var ParentEvery = 2 * time.Second

// watchParent returns ctx, ended as soon as h.Parent is gone (checked every
// ParentEvery) or the harness's input has closed (h.InputClosed, checked
// every InputEvery), the func that stops the watch, and one that says
// whether it ended ctx because the input closed.
func (h *Hook) watchParent(ctx context.Context) (context.Context, func(), func() bool) {
	ctx, cancel := context.WithCancel(ctx)
	var ended atomic.Bool
	// Read before the goroutine starts: the intervals are package
	// variables tests change.
	parentEvery, every := ParentEvery, ParentEvery
	if h.InputClosed != nil {
		every = min(every, InputEvery)
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		lastParent := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if h.inputClosed() {
					ended.Store(true)
					cancel()
					return
				}
				if h.Parent > 0 && time.Since(lastParent) >= parentEvery-every/2 {
					lastParent = time.Now()
					if !alive(h.Parent) {
						cancel()
						return
					}
				}
			}
		}
	}()
	return ctx, cancel, ended.Load
}

// inputClosed: h.InputClosed, false without one.
func (h *Hook) inputClosed() bool {
	return h.InputClosed != nil && h.InputClosed()
}

type handedRecord struct {
	IDs []string  `json:"delivery_ids"`
	At  time.Time `json:"at"`
}

// handedTTL: records older than this are removed. What they name was
// handed out again long since (a poll's lease is minutes).
const handedTTL = 25 * time.Hour

// handedPath is the record's file for this waiter's process and the
// conversation, "" without a usable key (the process: WaiterKey).
func (h *Hook) handedPath(conversation string) string {
	if h.WaiterKey == "" || !waiterKeyRE.MatchString(h.WaiterKey) || conversation == "" {
		return ""
	}
	return filepath.Join(h.Verifier.Settings.Dir, "handed", h.WaiterKey+"-"+hashName(conversation)+".json")
}

// saveHanded records ids as handed over in the conversation, unconfirmed,
// with any the record holds already (a second handover before a turn
// ended).
func (h *Hook) saveHanded(conversation string, ids []string) {
	p := h.handedPath(conversation)
	if p == "" || len(ids) == 0 || os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	unlock, err := lockFile(filepath.Join(filepath.Dir(p), ".lock"))
	if err != nil {
		return
	}
	defer unlock()
	if entries, err := os.ReadDir(filepath.Dir(p)); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && e.Name() != ".lock" && time.Since(info.ModTime()) > handedTTL {
				os.Remove(filepath.Join(filepath.Dir(p), e.Name()))
			}
		}
	}
	var rec handedRecord
	if b, err := readOwn(p); err == nil {
		json.Unmarshal(b, &rec)
	}
	for _, id := range ids {
		if !slices.Contains(rec.IDs, id) {
			rec.IDs = append(rec.IDs, id)
		}
	}
	rec.At = time.Now()
	b, _ := json.Marshal(rec)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

// handedIDs is what the record holds for the conversation (nil: none).
func (h *Hook) handedIDs(conversation string) []string {
	p := h.handedPath(conversation)
	if p == "" {
		return nil
	}
	b, err := readOwn(p)
	if err != nil {
		return nil
	}
	var rec handedRecord
	if json.Unmarshal(b, &rec) != nil {
		return nil
	}
	return rec.IDs
}

// confirmHanded acknowledges what this process handed over in the
// conversation and hasn't confirmed, on each of the conversation's links
// (an ack only counts on the link of the session it was delivered to:
// the others record nothing), and removes the record. If no ack got an
// answer, the record stays for the next turn's end, and the poll that
// follows may hand the same messages over again (at least once). An
// answer counts whatever it lists as recorded: an ID it can't record (a
// delivery finished another way) would otherwise keep the record, and a
// SessionStart waiter waiting on it (awaitTurnEnd), for good. One it
// should have recorded and didn't comes again, recorded again: a
// duplicate, never a loss.
func (h *Hook) confirmHanded(ctx context.Context, conversation string) {
	p := h.handedPath(conversation)
	ids := h.handedIDs(conversation)
	if len(ids) == 0 {
		if p != "" {
			os.Remove(p)
		}
		return
	}
	confirmed := false
	for _, l := range h.Links.All(conversation) {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if h.Poller.Ack(actx, l.Link, ids) == nil {
			confirmed = true
		}
		cancel()
	}
	if !confirmed {
		return
	}
	unlock, err := lockFile(filepath.Join(filepath.Dir(p), ".lock"))
	if err != nil {
		return
	}
	defer unlock()
	// Only what was acknowledged: a handover recorded meanwhile stays.
	var left []string
	for _, id := range h.handedIDs(conversation) {
		if !slices.Contains(ids, id) {
			left = append(left, id)
		}
	}
	if len(left) == 0 {
		os.Remove(p)
		return
	}
	b, _ := json.Marshal(handedRecord{IDs: left, At: time.Now()})
	os.WriteFile(p, b, 0o600)
}

// awaitTurnEnd: a waiter not started by a turn's end (SessionStart: a
// compaction mid-turn) while this process's last handover waits for its
// turn to end doesn't poll, or it would get that handover again and hand
// it over twice: it waits until the record is gone (the turn's Stop
// confirmed it), a newer waiter takes over (that Stop's), or it's stopped.
// It reports whether to go on listening.
func (h *Hook) awaitTurnEnd(ctx context.Context, conversation string) bool {
	for len(h.handedIDs(conversation)) > 0 {
		if h.stale != nil && h.stale() {
			h.why = "a newer waiter took over"
			return false
		}
		select {
		case <-ctx.Done():
			h.why = "stopped by the harness"
			return false
		case <-time.After(time.Second):
		}
	}
	return true
}

// deliveryIDs are the deliveries' IDs.
func deliveryIDs(ds []Delivery) []string {
	ids := make([]string, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
	}
	return ids
}
