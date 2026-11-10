package connector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PromiseFormat is a Format that can read a conversation's own record
// (Claude Code's transcript) for the newest result of an app's tools that
// told the agent Wakeshift's plugin (or hooks) would connect the
// conversation (harnesses.PluginReadyLead).
type PromiseFormat interface {
	LastPromise(in Input) (Promise, bool)
}

// Promise is such a result: the tool call's ID, and when it was written.
type Promise struct {
	Call string
	At   time.Time
}

// promiseWindow: a promise older than this is from a conversation the user
// has moved on from; the waiter doesn't speak up for it.
const promiseWindow = time.Hour

// NoteUnconnected is what a waiter wakes a conversation with when a result
// told the agent Wakeshift's plugin would connect it and, by the end of the
// waiter's wait for the link (LinkGrace), it hadn't: the agent may have told
// the user they'd hear when something arrives. As for a conversation the
// core can't wake (harnesses.CantWake), one neutral fact, so the agent stops
// promising; never to call a tool again (that would repeat whatever the
// tool does).
const NoteUnconnected = "Wakeshift: this conversation isn't connected, so nothing will notify you here when something arrives, whatever an earlier result of an app's tools said about Wakeshift's plugin connecting it. If you told the user you'd let them know, tell them now that you won't be notified here."

// unconnected, for a waiter whose conversation still has no link once its
// wait for one is over, wakes the conversation with NoteUnconnected (after
// any note the hooks saved for it, which says why: a pin that refused the
// beacon, say) if its record has a result from the last hour that promised
// the plugin connects it. Once per such result (a marker in unconnected/
// holds its call), so the next turn's waiter stays quiet.
func (h *Hook) unconnected(ctx context.Context, in Input) (Output, bool) {
	pf, ok := h.Format.(PromiseFormat)
	if !ok || h.Verifier == nil || ctx.Err() != nil || h.stale != nil && h.stale() {
		return Output{}, false // stopped, or replaced: the wait wasn't over
	}
	p, ok := pf.LastPromise(in)
	if !ok || p.Call == "" || time.Since(p.At) > promiseWindow || h.Links.Load(in.Conversation) != "" {
		return Output{}, false
	}
	marker := filepath.Join(h.Verifier.Settings.Dir, "unconnected", filepath.Base(h.Links.path(in.Conversation)))
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == p.Call {
		return Output{}, false // said already, for this result
	}
	// Once: if the marker's directory can't be made, say nothing rather
	// than at every stop. The marker itself is written once the wake is
	// handed over (Run), so a wake that never reached the harness comes
	// again from the next waiter.
	if os.MkdirAll(filepath.Dir(marker), 0o700) != nil {
		return Output{}, false
	}
	text := NoteUnconnected
	notePath := h.notePath(in.Conversation)
	note, _ := os.ReadFile(notePath) // taken once it's handed over
	if len(note) > 0 {
		text = string(note) + "\n\n" + text
	}
	out, ok := h.Format.Continue(in, text)
	if !ok {
		return Output{}, false
	}
	h.why = "not connected after a result said the plugin would: told the agent"
	h.handed = func() {
		os.WriteFile(marker, []byte(p.Call), 0o600)
		if len(note) > 0 {
			os.Remove(notePath)
		}
	}
	return out, true
}
