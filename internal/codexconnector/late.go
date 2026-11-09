package codexconnector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/connector"
)

// The late redeem (Codex's side of connector.installedSince). The result
// that offered the install carried a beacon, but no hook was there to read
// it, and asking the agent to call the app's tool again would repeat
// whatever that tool does. So at the thread's next SessionStart or
// UserPromptSubmit (the user's next message, after the restart the install
// asks for), a thread with no link here looks for that beacon in its own
// rollout, where Codex 0.166 keeps each MCP call's whole result, _meta
// included, and redeems it late.

// lateWindow: a beacon older than this is from a conversation the user has
// moved on from. The core accepts a late redeem for as long
// (appapi.LateRedeemWithin).
const lateWindow = time.Hour

// maxRollout is the most of a rollout the hook reads (from its end).
const maxRollout = 32 << 20

// rolloutBeacon is the newest beacon in an MCP call's result in the rollout.
type rolloutBeacon struct {
	Token  string
	Server string // the MCP server, as Codex names it in the call's tool name
	Tool   string // the tool, as the server lists it
}

// lastRolloutBeacon reads the thread's rollout (path: the hook's
// transcript_path) for the newest completed MCP tool call of this thread
// whose result's own _meta carries a beacon. Only Codex's record of the
// call (an item_completed McpToolCall event, which the model never sees),
// only the result's own _meta: never text, structured content or anything
// nested, which the app may relay from someone else. The path must be an
// absolute .jsonl named for the thread, as Codex names rollouts.
func lastRolloutBeacon(path, thread string) (rolloutBeacon, bool) {
	if path == "" || !filepath.IsAbs(path) || filepath.Ext(path) != ".jsonl" ||
		!strings.HasPrefix(filepath.Base(path), "rollout-") || !strings.HasSuffix(filepath.Base(path), "-"+thread+".jsonl") {
		return rolloutBeacon{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return rolloutBeacon{}, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return rolloutBeacon{}, false
	} else if st.Size() > maxRollout {
		f.Seek(st.Size()-maxRollout, 0)
	}
	var found rolloutBeacon
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, []byte(`"McpToolCall"`)) && bytes.Contains(line, []byte(beacon.MetaKey)) {
			var l struct {
				Type    string `json:"type"`
				Payload struct {
					Type     string `json:"type"`
					ThreadID string `json:"thread_id"`
					Item     struct {
						Type   string `json:"type"`
						Server string `json:"server"`
						Tool   string `json:"tool"`
						Status string `json:"status"`
						Result struct {
							Meta map[string]any `json:"_meta"`
						} `json:"result"`
					} `json:"item"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &l) == nil && l.Type == "event_msg" && l.Payload.Type == "item_completed" && l.Payload.ThreadID == thread &&
				l.Payload.Item.Type == "McpToolCall" && l.Payload.Item.Status == "completed" && l.Payload.Item.Server != "" && l.Payload.Item.Tool != "" {
				if tok, _ := l.Payload.Item.Result.Meta[beacon.MetaKey].(string); tok != "" {
					found = rolloutBeacon{Token: tok, Server: beacon.NormalTool(l.Payload.Item.Server), Tool: l.Payload.Item.Tool}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return found, found.Token != ""
}

// lateRedeem, at SessionStart or UserPromptSubmit for a thread with no
// link, connects the thread from the newest beacon in its rollout when that
// beacon came before the hooks were installed (so no hook saw it), within
// the last hour. The beacon is checked as the PostToolUse hook checks one
// (signed by an issuer trusted here, for this thread, from the tool it
// names, from the server pinned to its app, the thread on Codex's daemon),
// except for its expiry, and redeemed late, which the core accepts only
// for a ticket minted late-redeemable (a _meta beacon for the thread Codex
// itself named), for a session no connector has connected yet, within the
// hour, once. Tried once per thread (a marker in late/), so a refused late
// redeem isn't tried at every prompt: the thread then connects from the
// next result of the app's tools. Returns the note for the model ("" for
// none).
func (h *Hook) lateRedeem(ctx context.Context, in HookInput, onDaemon func() bool) string {
	thread := in.SessionID
	installed := h.Settings.InstalledAt
	if !uuidRE.MatchString(thread) || installed.IsZero() || h.Settings.Dir == "" || len(h.links().All(thread)) > 0 {
		return ""
	}
	marker := filepath.Join(h.Settings.Dir, "late", thread)
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	found, ok := lastRolloutBeacon(in.TranscriptPath, thread)
	if !ok {
		return ""
	}
	iat := peekIssuedAt(found.Token)
	if iat == 0 || !time.Unix(iat, 0).Before(installed) || h.now().Sub(time.Unix(iat, 0)) > lateWindow {
		return ""
	}
	v := &connector.Verifier{Settings: h.Settings, HTTP: h.HTTP, Now: func() time.Time { return time.Unix(iat, 0) }}
	claims, err := v.Verify(ctx, found.Token, thread)
	if err != nil || claims.Thread != thread {
		return ""
	}
	if claims.Tool != "" && !beacon.ToolMatches(claims.Tool, found.Tool) {
		return ""
	}
	pins := h.Pins
	if pins.Path == "" {
		pins = connector.PinsFor(h.Settings)
	}
	if pins.Allow(found.Server, claims.App) != nil {
		return ""
	}
	if !onDaemon() {
		return ""
	}
	// Tried once: SessionStart and UserPromptSubmit run together at the
	// first prompt, and only the hook that makes the marker goes on. If it
	// can't be made, don't try at all rather than at every prompt.
	if os.MkdirAll(filepath.Dir(marker), 0o700) != nil {
		return ""
	}
	mf, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ""
	}
	mf.Close()
	link, err := connector.RedeemLate(ctx, h.HTTP, harness, claims, thread)
	var outdated *connector.UpdateRequired
	if errors.As(err, &outdated) {
		return "" // the next result of the app's tools says so
	}
	if err != nil {
		return ""
	}
	h.links().SaveFor(thread, link, claims.App)
	if err := h.Start(thread, link, claims.App, "the beacon in "+found.Tool+"'s result before the install (late)", false); err != nil {
		return ""
	}
	return NoteConnected
}

// peekIssuedAt reads a beacon's iat without checking anything (0 if it
// can't), to verify it as of when it was minted.
func peekIssuedAt(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var c struct {
		IssuedAt int64 `json:"iat"`
	}
	if json.Unmarshal(b, &c) != nil {
		return 0
	}
	return c.IssuedAt
}
