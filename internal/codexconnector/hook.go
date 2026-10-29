package codexconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/connector"
)

// The hook: Codex runs it after MCP tool calls (PostToolUse) with the call
// and its result on stdin. A Wakeshift result for a Codex thread carries a
// beacon in _meta; the hook checks it, redeems its ticket for the thread's
// stream link, starts the connector for the thread, and adds a note for the
// model (additionalContext, which Codex adds as a developer message right
// after the result). Without a beacon it does nothing. It never fails the
// tool call.
//
// It keeps the link (connector.Links), so it can start the connector again
// without a tool call: Codex runs it at SessionStart (the thread opened or
// resumed in a window) and UserPromptSubmit (the user's next prompt), and if
// the thread has a link and no connector holds it (Running), it starts one.
// Codex has no hook that fires while a thread sits idle, so a connector
// that stops then stays stopped until one of those, or a tool call, comes.

// Notes for the model. The watch result tells it what no note means, and
// that one starting "Wakeshift: connected" (harnesses.Connected) means it's
// connected; the others don't start that way.
const (
	NoteConnected = "Wakeshift: connected. New messages will wake this thread as soon as they arrive, or once its turn ends if it's busy, while it's open in Codex: so end your turn when you're done rather than waiting for them."
	NoteInProcess = "Wakeshift: not connected. This Codex session runs without Codex's background server (it was started with --no-daemon or -c options), " +
		"so new messages can't wake it; you'll see them when you next use the app's tools. To be woken, the user can start codex without those options."
	NoteFailed = "Wakeshift: couldn't connect this thread (%s), so new messages won't wake it yet. To retry, call the app's tool that started watching again."
	// NoteOutdated: Wakeshift refused this version (426 update_required); its message follows.
	NoteOutdated = "Wakeshift: couldn't connect this thread. %s"
)

// HookInput is what Codex sends a PostToolUse hook (the fields used here).
type HookInput struct {
	SessionID     string          `json:"session_id"` // the thread
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolResponse  json.RawMessage `json:"tool_response"`
}

// harness is Codex's ID (internal/harnesses), for ClientHeader.
const harness = "codex"

// Hook is the hook's setup.
type Hook struct {
	Settings Settings
	HTTP     *http.Client
	Socket   string
	// Start starts the connector for a thread with its stream link,
	// detached from the hook (why: for its log).
	// rearm: started again without a beacon (Config.Exclusive).
	Start func(thread, link, why string, rearm bool) error
	// Running reports whether a thread's connector is running (default:
	// its RunLock is held).
	Running func(thread string) bool
	Now     func() time.Time
	// Pins: which app's beacons each MCP server carries (default: in the
	// settings directory).
	Pins connector.Pins
	// Links: each thread's stream link (default: in the settings directory).
	Links connector.Links
}

// Codex's hook events the hook re-arms on (no tool call, no beacon).
const (
	EventSessionStart     = "SessionStart"
	EventUserPromptSubmit = "UserPromptSubmit"
)

// Settings is the connector's settings on this machine (~/.wakeshift/codex).
type Settings = connector.Settings

// Run handles one hook call.
func (h *Hook) Run(ctx context.Context, stdin io.Reader, stdout io.Writer) {
	var in HookInput
	if json.NewDecoder(io.LimitReader(stdin, 8<<20)).Decode(&in) != nil || in.SessionID == "" {
		return
	}
	if in.HookEventName == EventSessionStart || in.HookEventName == EventUserPromptSubmit {
		h.rearm(in.SessionID, in.HookEventName)
		return
	}
	// Only from the result's own _meta (tool_response._meta): Wakeshift
	// always puts Codex's beacons there (harnesses.Meta). A beacon line in
	// the result's text, or a _meta in JSON inside it, can be anyone's words
	// or data the app relays (a chat message), naming this thread.
	token, carried := connector.FindBeacon(in.ToolResponse)
	server := connector.MCPServerOf(in.ToolName)
	if token == "" || carried != connector.InResultMeta || server == "" {
		return
	}
	v := &connector.Verifier{Settings: h.Settings, HTTP: h.HTTP, Now: h.now}
	claims, err := v.Verify(ctx, token, in.SessionID)
	if err != nil {
		// Not from a Wakeshift trusted here, or not for this thread: say
		// nothing (its text can't be trusted to repeat).
		return
	}
	// And from the tool it names (mcp__<server>__<tool>), where it names
	// one (apps on SDKs from before beacons did name none).
	if claims.Tool != "" && !connector.ToolOf(connector.Input{MCPTool: connector.MCPToolOf(in.ToolName)}, claims.Tool) {
		return
	}
	note := func(s string) {
		json.NewEncoder(stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PostToolUse", "additionalContext": s}})
	}
	// Signed by Wakeshift and for this thread, but any MCP server's result
	// could carry a beacon an attacker minted for this thread through some
	// app: only the server pinned to the beacon's app may (connector.Pins).
	pins := h.Pins
	if pins.Path == "" {
		pins = connector.PinsFor(h.Settings)
	}
	var pinned *connector.ErrPinned
	if err := pins.Allow(server, claims.App); errors.As(err, &pinned) {
		note(fmt.Sprintf(connector.NotePinned, pinned.Error(), pins.Path))
		return
	} else if err != nil {
		note(fmt.Sprintf(NoteFailed, "its server couldn't be recorded"))
		return
	}
	if !h.onDaemon(ctx, in.SessionID) {
		note(NoteInProcess)
		return
	}
	link, err := h.redeem(ctx, claims, in.SessionID)
	var outdated *connector.UpdateRequired
	if errors.As(err, &outdated) {
		note(fmt.Sprintf(NoteOutdated, outdated.Message))
		return
	}
	if err != nil {
		note(fmt.Sprintf(NoteFailed, err))
		return
	}
	// Kept for re-arming (below); a link the hook can't keep only means
	// the next start needs a tool call, as before.
	h.links().SaveFor(in.SessionID, link, claims.App)
	if err := h.Start(in.SessionID, link, "a beacon in "+in.ToolName+"'s result", false); err != nil {
		note(fmt.Sprintf(NoteFailed, "the connector didn't start"))
		return
	}
	note(NoteConnected)
}

// rearm starts the thread's connector again with its kept link, if it has
// one and no connector is running for it. It says nothing to the model: a
// connector the link no longer works for (unwatched, revoked, out of date)
// stops at once and forgets the link.
func (h *Hook) rearm(thread, event string) {
	if !uuidRE.MatchString(thread) {
		return
	}
	link := h.links().Load(thread)
	if link == "" {
		return
	}
	running := h.Running
	if running == nil {
		running = func(thread string) bool { return Running(RunLock(thread)) }
	}
	if running(thread) {
		return
	}
	h.Start(thread, link, "re-armed at "+event, true)
}

func (h *Hook) links() connector.Links {
	if h.Links.Dir != "" {
		return h.Links
	}
	return connector.LinksFor(h.Settings)
}

// onDaemon: the thread is on Codex's shared daemon (the connector can reach
// it), not in an in-process app-server.
func (h *Hook) onDaemon(ctx context.Context, thread string) bool {
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	app, err := dialAppServer(dctx, h.Socket)
	if err != nil {
		return false
	}
	defer app.close()
	_, err = app.call(dctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false})
	return err == nil
}

// redeem exchanges the beacon's ticket for the thread's stream link.
func (h *Hook) redeem(ctx context.Context, c beacon.Claims, thread string) (string, error) {
	return connector.Redeem(ctx, h.HTTP, harness, c, thread)
}

func (h *Hook) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
