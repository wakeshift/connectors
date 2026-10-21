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

// Notes for the model. The watch result tells it what no note means, and
// that one starting "Wakeshift: connected" (harnesses.Connected) means it's
// connected; the others don't start that way.
const (
	NoteConnected = "Wakeshift: connected. New messages will wake this thread as soon as they arrive, while it's open in Codex."
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
	// detached from the hook.
	Start func(thread, link string) error
	Now   func() time.Time
	// Pins: which app's beacons each MCP server carries (default: in the
	// settings directory).
	Pins connector.Pins
}

// Settings is the connector's settings on this machine (~/.wakeshift/codex).
type Settings = connector.Settings

// Run handles one hook call.
func (h *Hook) Run(ctx context.Context, stdin io.Reader, stdout io.Writer) {
	var in HookInput
	if json.NewDecoder(io.LimitReader(stdin, 8<<20)).Decode(&in) != nil || in.SessionID == "" {
		return
	}
	token := connector.BeaconIn(in.ToolResponse)
	server := connector.MCPServerOf(in.ToolName)
	if token == "" || server == "" {
		return
	}
	v := &connector.Verifier{Settings: h.Settings, HTTP: h.HTTP, Now: h.now}
	claims, err := v.Verify(ctx, token, in.SessionID)
	if err != nil {
		// Not from a Wakeshift trusted here, or not for this thread: say
		// nothing (its text can't be trusted to repeat).
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
	if err := h.Start(in.SessionID, link); err != nil {
		note(fmt.Sprintf(NoteFailed, "the connector didn't start"))
		return
	}
	note(NoteConnected)
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
