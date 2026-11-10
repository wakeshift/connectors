package codexconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/connector"
	"github.com/wakeshift/connectors/internal/pluginmark"
	"github.com/wakeshift/connectors/internal/version"
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
// A thread with no link there is connected from the beacon in the result
// that offered the install, read from the rollout (late.go).
// Codex has no hook that fires while a thread sits idle, so a connector
// that stops then stays stopped until one of those, or a tool call, comes.

// Notes for the model. The watch result tells it what no note means, and
// that one starting "Wakeshift: connected" (harnesses.Connected) means it's
// connected; the others don't start that way.
const (
	NoteConnected = "Wakeshift: connected. New messages will wake this thread as soon as they arrive, or once its turn ends if it's busy, while it's open in Codex: so end your turn when you're done rather than waiting for them."
	NoteInProcess = "Wakeshift: not connected. This Codex session runs without Codex's background server (it was started with --no-daemon or -c options), " +
		"so new messages can't wake it; you'll see them when you next use the app's tools. To be woken, the user can start codex without those options."
	NoteFailed = "Wakeshift: couldn't connect this thread (%s), so new messages won't wake it yet. It tries again from the next result of the app's tools."
	// NoteOutdated: Wakeshift refused this version (426 update_required); its message follows.
	NoteOutdated = "Wakeshift: couldn't connect this thread. %s"
)

// HookInput is what Codex sends a PostToolUse hook (the fields used here).
type HookInput struct {
	SessionID     string          `json:"session_id"` // the thread
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolResponse  json.RawMessage `json:"tool_response"`
	// TranscriptPath is the thread's rollout (every event), for the late
	// redeem at SessionStart and UserPromptSubmit (lateRedeem).
	TranscriptPath string `json:"transcript_path"`
}

// harness is Codex's ID (internal/harnesses), for ClientHeader.
const harness = "codex"

// Hook is the hook's setup.
type Hook struct {
	Settings Settings
	HTTP     *http.Client
	Socket   string
	// Start starts the connector for a thread with one app's stream link,
	// detached from the hook (why: for its log). A thread watching several
	// apps has a connector for each.
	// rearm: started again without a beacon (Config.Exclusive).
	Start func(thread, link, app, why string, rearm bool) error
	// Running reports whether the connector for a thread's app is running
	// (default: its AppRunLock is held).
	Running func(thread, app string) bool
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
		var notes []string
		// The daemon is asked at most once in this run (the mark, the
		// late redeem).
		var daemon *bool
		onDaemon := func() bool {
			if daemon == nil {
				ok := h.onDaemon(ctx, in.SessionID)
				daemon = &ok
			}
			return *daemon
		}
		if in.HookEventName == EventSessionStart {
			// The mark to pass (internal/pluginmark), so the first watch
			// result of the thread offers no install.
			if m := h.markNote(onDaemon); m != "" {
				notes = append(notes, m)
			}
		}
		// A thread with no link yet may have had the install offered in a
		// result no hook saw: connected from that result's beacon (late),
		// without a call of the app's tool, which may have side effects.
		if note := h.lateRedeem(ctx, in, onDaemon); note != "" {
			notes = append(notes, note)
		} else {
			h.rearm(in.SessionID, in.HookEventName)
		}
		if len(notes) > 0 {
			json.NewEncoder(stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": in.HookEventName, "additionalContext": strings.Join(notes, "\n\n")}})
		}
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
	// The thread's link for this app goes along (held), read, redeemed with
	// and replaced one redeem at a time (LockRedeems): two results of one
	// turn whose calls came before either result gave the thread a
	// session_key carry beacons for two sessions, and the second redeem,
	// handing back the first's link, keeps the thread one session.
	unlock := h.links().LockRedeems()
	held := h.links().LoadFor(in.SessionID, claims.App)
	link, err := h.redeem(ctx, claims, in.SessionID, held)
	var outdated *connector.UpdateRequired
	if errors.As(err, &outdated) {
		unlock()
		note(fmt.Sprintf(NoteOutdated, outdated.Message))
		return
	}
	if errors.Is(err, connector.ErrTaken) {
		unlock()
		note(connector.NoteTaken)
		return
	}
	if err != nil {
		unlock()
		note(fmt.Sprintf(NoteFailed, err))
		return
	}
	// Kept for re-arming (below); a link the hook can't keep only means
	// the next start needs a tool call, as before. Per app: another app's
	// link, and its connector, stay as they are.
	h.links().SaveFor(in.SessionID, link, claims.App)
	unlock()
	if err := h.Start(in.SessionID, link, claims.App, "a beacon in "+in.ToolName+"'s result", false); err != nil {
		note(fmt.Sprintf(NoteFailed, "the connector didn't start"))
		return
	}
	note(NoteConnected)
}

// CodexMarkNote is the context the hook adds at SessionStart, with the
// mark (%s) to pass. Codex runs SessionStart when the thread's first turn
// starts (and a resumed thread's), before the model's first call, and
// shows a hook's additionalContext to the model as a developer message.
const CodexMarkNote = "Wakeshift's plugin is installed in this Codex session, so apps that notify through Wakeshift can wake this thread by themselves, even while it's idle. " +
	"When you call an app's tool that takes a `harness` argument, pass harness=%q (exactly this): it tells the app the plugin is here, so its result won't ask you to set notifications up."

// CodexHooksMarkNote is CodexMarkNote for the hooks Wakeshift's installer
// wrote to ~/.codex/hooks.json (no plugin).
const CodexHooksMarkNote = "Wakeshift's hooks are installed in this Codex session, so apps that notify through Wakeshift can wake this thread by themselves, even while it's idle. " +
	"When you call an app's tool that takes a `harness` argument, pass harness=%q (exactly this): it tells the app the hooks are here, so its result won't ask you to set notifications up."

// markNote is the mark's context for a thread, or "" when there's nothing
// to mark with (no trusted Wakeshift) or nothing could wake the thread (it
// runs in-process, off Codex's daemon: codex exec, --no-daemon), so its
// results keep saying so. Like Claude Code's (connector.SessionContext),
// the plugin's install leaves its issuer's file and the installer's hooks
// don't. That this runs at all is the evidence the hooks are installed and
// trusted: Codex runs SessionStart only for hooks the user trusted, and the
// same install added the PostToolUse hook that redeems the beacon (an
// install from before October 27 has no SessionStart hook, so no mark).
// The plugin's hooks.json can't be edited without Codex asking again; the
// installer's can, so for those the PostToolUse entry must still be there.
func (h *Hook) markNote(onDaemon func() bool) string {
	set := h.Settings
	hooks := set.Dir != "" && !fileExists(filepath.Join(set.Dir, connector.PluginIssuerFile))
	mark, note := pluginmark.Value(harness, version.Version, set.Issuers), CodexMarkNote
	if hooks {
		mark, note = pluginmark.HooksValue(harness, version.Version, set.Issuers), CodexHooksMarkNote
	}
	if hooks && !connector.CodexToolHookInstalled() {
		return "" // no hook to redeem the beacon: the offer stays
	}
	if mark == "" || !onDaemon() {
		return ""
	}
	return fmt.Sprintf(note, mark)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// rearm starts the thread's connectors again with their kept links (one
// per app it watches), each that isn't running. It says nothing to the
// model: a connector the link no longer works for (unwatched, revoked, out
// of date) stops at once and forgets the link.
func (h *Hook) rearm(thread, event string) {
	if !uuidRE.MatchString(thread) {
		return
	}
	running := h.Running
	if running == nil {
		running = func(thread, app string) bool { return Running(AppRunLock(thread, app)) }
	}
	for _, k := range h.links().All(thread) {
		if !running(thread, k.App) {
			h.Start(thread, k.Link, k.App, "re-armed at "+event, true)
		}
	}
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

// redeem exchanges the beacon's ticket for the thread's stream link,
// saying which link the thread holds for the app now (held), if any.
func (h *Hook) redeem(ctx context.Context, c beacon.Claims, thread, held string) (string, error) {
	return connector.RedeemHeld(ctx, h.HTTP, harness, c, thread, "", held, "")
}

func (h *Hook) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
