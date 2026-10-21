package connector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Event is what a hook call is about.
type Event int

const (
	OtherEvent Event = iota
	// ToolResult: after a tool call, with its result (where beacons are).
	ToolResult
	// Stop: the agent finished its turn; the hook may continue it.
	Stop
	// Rewake: an async hook that may wait as long as it likes and wakes
	// the idle session when it exits (Claude Code's asyncRewake).
	Rewake
	// TurnStart: before a turn (Antigravity's PreInvocation): context can
	// be added there when it can't after a tool call.
	TurnStart
	// PreCall: before an MCP tool call, where the hook can tell the server
	// the conversation (Copilot's preMcpToolCall).
	PreCall
)

// PreCallFormat is a Format that answers PreCall.
type PreCallFormat interface {
	PreCall(in Input) (Output, bool)
}

// StripFormat is a Format that can take a beacon out of the result the
// model sees.
type StripFormat interface {
	Strip(in Input) (Output, bool)
}

// EventFormat is a Format whose input doesn't name its event: the hook
// command does (--event).
type EventFormat interface {
	ParseEvent(event string, stdin []byte) (Input, error)
}

// Input is a hook call, read from the harness's own format.
type Input struct {
	Event        Event
	Conversation string          // the harness's conversation (session) ID
	ToolName     string          // for ToolResult
	ToolCallID   string          // for ToolResult, if the harness gives one
	ToolResult   json.RawMessage // the tool's result, as the harness passes it
	// Server is the MCP server the result came from, for harnesses whose
	// hooks say ("" for the others): beacons from it are checked against
	// the app it's pinned to (Pins).
	Server string
	// Continued: the harness says this stop already follows a stop hook's
	// continuation (stop_hook_active, loop_count > 0).
	Continued bool

	raw      []byte // the input as the harness sent it, for its format
	deferred []byte // for a ToolResult that can't be read yet (DeferredFormat)
}

// Output is what the hook hands back, in the harness's own format.
type Output struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Format reads and writes one harness's hook JSON.
type Format interface {
	// Parse reads a hook call.
	Parse(stdin []byte) (Input, error)
	// Context is the output that adds text for the model after a tool call
	// (ok false: the harness can't).
	Context(in Input, text string) (Output, bool)
	// Continue is the output that continues the session with text after a
	// stop (or, for Rewake, wakes it).
	Continue(in Input, text string) (Output, bool)
}

// Notes for the model. NoteConnected starts with "Wakeshift: connected"
// (harnesses.Connected), what agents are told to look for; nothing else
// Wakeshift says starts with it.
const (
	NoteConnected = "Wakeshift: connected. New messages for this conversation will reach it %s."
	NoteFailed    = "Wakeshift: couldn't connect this conversation (%s), so new messages won't reach it yet. To retry, call the app's tool that started watching again."
	// NoteOtherApp: a beacon for another app than the one whose beacon
	// connected the conversation, where the hook can't tell which server
	// a result came from (it's never replaced without the user).
	NoteOtherApp = "Wakeshift: this conversation stays connected to %q; a result asked to connect it to %q instead, and Wakeshift's hooks here don't switch apps by themselves. To switch, start a new conversation for it."
)

// Hook is one harness's hook entry point.
type Hook struct {
	Harness  string
	Format   Format
	Verifier *Verifier
	Poller   Poller
	Links    Links
	HTTP     *http.Client
	// When says, in NoteConnected, when messages arrive (e.g. "when the
	// agent finishes a turn").
	When string
	// StopWait is how long a stop hook listens before letting the session
	// stop (keep it under the harness's hook timeout).
	StopWait time.Duration
	// RewakeWait bounds an async rewake hook's wait (0: until the harness
	// stops it).
	RewakeWait time.Duration
	// Unbound: accept beacons that name neither the conversation nor the
	// call (harnesses that tell MCP servers neither; beacon.Binding).
	Unbound bool
	// Repoll is the longest single poll (the server may cap a wait).
	Repoll time.Duration

	// EventName is the event the command line names (--event), for
	// formats whose input doesn't.
	EventName string

	// Rewake: this call is an async waiter (run with --rewake): a Stop or
	// SessionStart becomes a Rewake.
	Rewake bool
	// WaiterKey, for a waiter: one waiter per key (Claude Code's process,
	// CLAUDE_PID); a newer one makes an older one stop.
	WaiterKey string
	stale     func() bool
	// Alive, if set, says whether the harness still runs (HarnessWatch):
	// a listener stops when it doesn't, and hands nothing over.
	Alive func() bool
	// Pins: which app's beacons each MCP server carries (default: in the
	// harness's settings directory).
	Pins Pins

	// patient: this call listens as a waiter (Rewake), so its polls wait
	// out the agent's own wait command rather than stopping for it.
	patient bool
	// why is why the last listen or take stopped, for a waiter's log
	// (logExit).
	why string
	// handed confirms what this call's output hands over: Run calls it
	// once the output is written (at least once: what the harness wasn't
	// handed comes again).
	handed func()
}

func (h *Hook) pins() Pins {
	if h.Pins.Path == "" {
		return PinsFor(h.Verifier.Settings)
	}
	return h.Pins
}

// Run handles one hook call. It never fails the harness's call: anything it
// can't do, it leaves alone.
func (h *Hook) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	b, err := io.ReadAll(io.LimitReader(stdin, 16<<20))
	if err != nil {
		return 0
	}
	var in Input
	if ef, ok := h.Format.(EventFormat); ok && h.EventName != "" {
		in, err = ef.ParseEvent(h.EventName, b)
	} else {
		in, err = h.Format.Parse(b)
	}
	if err != nil || in.Conversation == "" {
		return 0
	}
	if in.Event == TurnStart || in.Event == Stop || in.Event == Rewake {
		h.resolveDeferred(ctx, in)
	}
	var out Output
	var ok bool
	switch in.Event {
	case ToolResult:
		out, ok = h.toolResult(ctx, in)
	case TurnStart:
		out, ok = h.turnStart(ctx, in)
	case PreCall:
		if pf, is := h.Format.(PreCallFormat); is {
			out, ok = pf.PreCall(in)
		}
	case Stop, Rewake:
		if h.Rewake || in.Event == Rewake {
			started := time.Now()
			h.claimWaiter()
			h.patient = true
			out, ok = h.listen(ctx, in, h.RewakeWait)
			h.logExit(in.Conversation, started)
		} else {
			out, ok = h.listen(ctx, in, h.StopWait)
		}
	}
	if !ok {
		return 0
	}
	_, err1 := stdout.Write(out.Stdout)
	_, err2 := stderr.Write(out.Stderr)
	// Confirmed only once it's handed over: written where the harness
	// reads it (a write that failed hands over nothing).
	handed := h.handed
	h.handed = nil
	if err1 != nil || err2 != nil {
		return 0 // nothing reached the harness: don't wake it with nothing
	}
	if handed != nil {
		handed()
	}
	return out.ExitCode
}

// toolResult connects the conversation on a beacon for it, and hands over
// anything already waiting.
func (h *Hook) toolResult(ctx context.Context, in Input) (Output, bool) {
	if in.ToolResult == nil && in.deferred != nil {
		// The result isn't readable yet (Antigravity writes it after this
		// hook returns): the conversation's next hook reads it.
		h.deferResult(in)
		return Output{}, false
	}
	var notes []string
	token, carried := FindBeacon(in.ToolResult)
	if n := h.connect(ctx, in, token, carried); n != "" {
		notes = append(notes, n)
	}
	late, _ := h.Format.(LateContextFormat)
	// A harness that shows this hook's context only later (Claude Code: at
	// the next user prompt) isn't handed messages here: they'd count as
	// delivered and not reach the model. Nor does this hook poll the
	// stream there: it would take it from the conversation's waiter, which
	// delivers them.
	if late == nil || !late.LateContext() {
		if text, ack := h.take(ctx, in.Conversation, 0, true); text != "" {
			out, ok := h.Format.Context(in, strings.Join(append(notes, text), "\n\n"))
			if ok {
				h.handed = ack
				return out, true
			}
		}
	}
	if len(notes) == 0 {
		if sf, is := h.Format.(StripFormat); is && token != "" {
			return sf.Strip(in)
		}
		return Output{}, false
	}
	out, ok := h.Format.Context(in, strings.Join(notes, "\n\n"))
	if !ok || late != nil && late.LateContext() {
		// Said at the next turn's start, or with the next wake, whichever
		// comes first.
		h.saveNote(in.Conversation, strings.Join(notes, "\n\n"))
	}
	return out, ok
}

// LateContextFormat is a Format whose tool-result hooks' context reaches
// the model late (Claude Code shows an async hook's additionalContext only
// at the next user prompt): a note it adds is also kept for the next wake.
type LateContextFormat interface {
	LateContext() bool
}

// turnStart hands over a note saved for the conversation and anything
// waiting, before a turn.
func (h *Hook) turnStart(ctx context.Context, in Input) (Output, bool) {
	var parts []string
	if n := h.takeNote(in.Conversation); n != "" {
		parts = append(parts, n)
	}
	text, ack := h.take(ctx, in.Conversation, 0, true)
	if text != "" {
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return Output{}, false
	}
	out, ok := h.Format.Context(in, strings.Join(parts, "\n\n"))
	if ok && text != "" {
		h.handed = ack
	}
	return out, ok
}

func (h *Hook) notePath(conversation string) string {
	return filepath.Join(h.Verifier.Settings.Dir, "notes", filepath.Base(h.Links.path(conversation)))
}

func (h *Hook) saveNote(conversation, note string) {
	p := h.notePath(conversation)
	if os.MkdirAll(filepath.Dir(p), 0o700) == nil {
		os.WriteFile(p, []byte(note), 0o600)
	}
}

func (h *Hook) takeNote(conversation string) string {
	p := h.notePath(conversation)
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	os.Remove(p)
	return string(b)
}

// listen waits up to wait (0: until ctx ends) for the conversation's
// messages and continues the session with them.
func (h *Hook) listen(ctx context.Context, in Input, wait time.Duration) (Output, bool) {
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	if h.Links.Load(in.Conversation) == "" {
		// A waiter can start before the turn's beacon is redeemed (Claude
		// Code runs the Stop hook while the async PostToolUse that
		// connects the conversation is still running): give the link a
		// little while to appear.
		h.why = "no link for the conversation"
		if !h.Rewake && in.Event != Rewake || !h.waitForLink(ctx, in.Conversation) {
			return Output{}, false
		}
	}
	text, ack := h.take(ctx, in.Conversation, wait, false)
	if text == "" {
		return Output{}, false
	}
	if h.Alive != nil && !h.Alive() {
		h.why = "the harness quit before the messages could be handed over"
		return Output{}, false // not acknowledged: Wakeshift hands it out again
	}
	h.why = "handed messages over"
	// A note saved for the next turn's start (the conversation connected
	// in a hook that couldn't add context) comes with this wake instead.
	note := h.takeNote(in.Conversation)
	if note != "" {
		text = note + "\n\n" + text
	}
	out, ok := h.Format.Continue(in, text)
	if ok {
		h.handed = ack // once it's written (Run)
	} else {
		h.why = "the harness's format can't continue the session"
		if note != "" {
			h.saveNote(in.Conversation, note)
		}
	}
	return out, ok
}

// take polls the conversation's link once (immediate), or until there are
// deliveries or wait runs out (0: until ctx ends), and returns them framed,
// with a func that acknowledges them (call it once they're handed over).
func (h *Hook) take(ctx context.Context, conversation string, wait time.Duration, immediate bool) (string, func()) {
	link := h.Links.Load(conversation)
	if link == "" {
		h.why = "no link for the conversation"
		return "", nil
	}
	deadline := time.Time{}
	if wait > 0 {
		deadline = time.Now().Add(wait)
	}
	// The poll's answer has to arrive before ctx ends (listen's ctx ends
	// at the same moment as wait): asked to wait right up to it, the
	// server's answer, with any deliveries and the link's rotation, would
	// be cut off. Leave it room.
	if d, ok := ctx.Deadline(); ok {
		reserve := min(time.Until(d)/5, answerReserve)
		if end := d.Add(-reserve); deadline.IsZero() || end.Before(deadline) {
			deadline = end
		}
	}
	repoll := h.Repoll
	if repoll <= 0 {
		repoll = 25 * time.Second
	}
	for backoff, first := time.Second, true; ; first = false {
		w := repoll
		if !deadline.IsZero() {
			w = min(w, time.Until(deadline).Truncate(time.Millisecond))
			if w < minPoll && !first { // the first poll always looks once
				h.why = "listened as long as it may"
				return "", nil
			}
		}
		if immediate || w < 0 {
			w = 0
		}
		started := time.Now()
		patient := h.patient && !immediate
		b, err := h.Poller.Poll(ctx, link, w, patient)
		var outdated *UpdateRequired
		if errors.As(err, &outdated) {
			// Kept: the updated hooks use the same link. The app's tool
			// results ask the agent for the update meanwhile.
			h.why = "Wakeshift's hooks are out of date"
			return "", nil
		}
		switch err {
		case ErrLinkGone:
			h.Links.Forget(conversation)
			h.why = "the link was revoked or expired"
			return "", nil
		case ErrReplaced:
			switch {
			case h.stale != nil && h.stale():
				h.why = "a newer waiter took over"
				return "", nil
			case !h.patient || h.stale == nil || h.Alive != nil && !h.Alive():
				// Another hook of this conversation is listening.
				h.why = "another poll took the stream (a newer hook)"
				return "", nil
			}
			// The newest waiter: another hook polled for a moment (a turn's
			// tool hook); take the stream back after a pause, so the
			// conversation isn't left with nothing listening.
		}
		if b.Link != "" && b.Link != link && sameOrigin(b.Link, link) {
			link = b.Link
			h.Links.Save(conversation, link)
		}
		if err == nil && b.Background && !patient {
			// The agent's own wait command wakes it. A waiter keeps
			// listening: the server waited, and takes the stream for it as
			// soon as that command stops counting as live.
			h.why = "the agent's wait command holds the stream"
			return "", nil
		}
		if err == nil && len(b.Deliveries) > 0 {
			ids := make([]string, len(b.Deliveries))
			for i, d := range b.Deliveries {
				ids[i] = d.ID
			}
			l := link
			return Prompt(b.Deliveries), func() {
				actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer cancel()
				h.Poller.Ack(actx, l, ids) // unacknowledged, they come back: at least once
			}
		}
		switch {
		case immediate:
			h.why = "nothing waiting"
			return "", nil
		case ctx.Err() != nil:
			h.why = "stopped by the harness"
			return "", nil
		case !deadline.IsZero() && !time.Now().Before(deadline):
			h.why = "listened as long as it may"
			return "", nil
		case h.stale != nil && h.stale():
			h.why = "a newer waiter took over"
			return "", nil
		case h.Alive != nil && !h.Alive():
			h.why = "the harness quit"
			return "", nil
		}
		// Wakeshift unreachable, or answering at once with nothing when asked
		// to wait: don't spin.
		if err != nil || w > 0 && time.Since(started) < w/2 {
			pause := backoff
			var busy *Busy
			if errors.As(err, &busy) {
				pause = max(pause, busy.After) // at capacity: come back when it says
				// Past the deadline once paused (or with too little left to
				// poll): stop now rather than wait for nothing.
				if !deadline.IsZero() && time.Until(deadline)-pause < minPoll {
					h.why = "Wakeshift was busy until the end of its listening time"
					return "", nil
				}
			}
			select {
			case <-ctx.Done():
				h.why = "stopped by the harness"
				return "", nil
			case <-time.After(pause):
			}
			backoff = min(backoff*2, 30*time.Second)
		} else {
			backoff = time.Second
		}
	}
}

// minPoll is the shortest wait worth polling for once the deadline nears.
const minPoll = 100 * time.Millisecond

// answerReserve is the most of a hook's time left for a poll's answer to
// arrive after the server stops waiting (a fifth of it, for a short one).
const answerReserve = 2 * time.Second

func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Scheme == ub.Scheme && ua.Host == ub.Host
}

// claimWaiter makes this the waiter for its key: it writes a fresh token to
// <dir>/waiters/<key>, and stops listening once the file holds another's.
func (h *Hook) claimWaiter() {
	if h.WaiterKey == "" || !waiterKeyRE.MatchString(h.WaiterKey) {
		return
	}
	dir := filepath.Join(h.Verifier.Settings.Dir, "waiters")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	if entries, err := os.ReadDir(dir); err == nil { // waiters outlive no hook timeout (a day)
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 25*time.Hour {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	path := filepath.Join(dir, h.WaiterKey)
	tok := make([]byte, 16)
	rand.Read(tok)
	mine := hex.EncodeToString(tok)
	if os.WriteFile(path, []byte(mine), 0o600) != nil {
		return
	}
	h.stale = func() bool {
		b, err := os.ReadFile(path)
		return err == nil && string(b) != mine
	}
}

var waiterKeyRE = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

// LinkGrace is how long a waiter waits for its conversation's link to
// appear before giving up (the next turn's waiter tries again).
var LinkGrace = 2 * time.Minute

func (h *Hook) waitForLink(ctx context.Context, conversation string) bool {
	deadline := time.After(LinkGrace)
	for h.Links.Load(conversation) == "" {
		if h.stale != nil && h.stale() {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return true
}
