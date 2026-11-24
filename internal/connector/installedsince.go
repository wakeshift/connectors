package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
)

// HistoryFormat is a Format that can read a conversation's own record
// (Claude Code's transcript) for the newest tool result whose own _meta
// carries a Wakeshift beacon.
type HistoryFormat interface {
	LastBeacon(in Input) (FoundBeacon, bool)
}

// FoundBeacon is a beacon in a conversation's record, with the call whose
// result carried it.
type FoundBeacon struct {
	Token    string
	Call     string // the tool call's ID
	ToolName string // as the harness names the tool (mcp__<server>__<tool>)
}

// installedSinceWindow: a beacon older than this is from a conversation
// the user has moved on from; the waiter doesn't connect from it. The core
// accepts a late redeem for as long (appapi.LateRedeemWithin).
const installedSinceWindow = time.Hour

// installedSince, for a waiter whose conversation has no link, connects
// the conversation from the newest beacon in its own record (Claude Code's
// transcript) when that beacon came before the hooks were installed (so no
// hook saw it), within the last hour: the result that offered the install
// went by before there was a hook to read it, and asking the agent to call
// the tool again would repeat whatever that tool does (it may send a
// message or start a paid job). The beacon is checked as a hook would
// (signed by an issuer trusted here, bound to its call, from the tool it
// names, from the server pinned to its app), except for its expiry, and
// redeemed as a late redeem, which the core accepts only for a session no
// connector has connected yet, within the hour. The "connected" note then
// comes with the first wake. Once per conversation (a marker in nudged/),
// so a conversation whose late redeem is refused isn't tried at every
// stop; it connects from the next result of the app's tools instead.
// Reports whether it connected the conversation.
func (h *Hook) installedSince(ctx context.Context, in Input) bool {
	return h.fromRecord(ctx, in, false)
}

// missedHookAge: a beacon minted after the install is the tool-result
// hook's to redeem, which gives up within its timeout (Claude Code's: 30s);
// only one older than this is taken as missed (missedRedeem).
const missedHookAge = 45 * time.Second

// missedRedeem, for a waiter whose conversation still has no link after
// LinkGrace, connects the conversation from the newest beacon in its own
// record when that beacon came after the hooks were installed, so the
// tool-result hook should have redeemed it and didn't (it never saw the
// result's line in the transcript: claudeMetaWait). Checked and redeemed
// as installedSince does (a late redeem: the ticket has expired by now,
// and the core accepts it only while no connector has connected the
// session; it's single use, so a beacon a hook did redeem is refused).
// Once per beacon (the call's ID in nudged/). Reports whether it
// connected the conversation.
func (h *Hook) missedRedeem(ctx context.Context, in Input) bool {
	if ctx.Err() != nil || h.stale != nil && h.stale() {
		return false // stopped, or replaced: the newer waiter does it
	}
	return h.fromRecord(ctx, in, true)
}

// fromRecord is installedSince (missed false: a beacon minted before the
// install) and missedRedeem (missed true: after it, and older than
// missedHookAge).
func (h *Hook) fromRecord(ctx context.Context, in Input, missed bool) bool {
	hf, ok := h.Format.(HistoryFormat)
	if !ok || h.Verifier == nil {
		return false
	}
	installed := h.Verifier.Settings.InstalledAt
	if installed.IsZero() {
		return false
	}
	marker := h.nudgedPath(in.Conversation)
	tried, err := os.ReadFile(marker)
	if err == nil && !missed {
		return false // installedSince: once per conversation
	}
	found, ok := hf.LastBeacon(in)
	if !ok || found.Token == "" || found.Call == "" {
		return false
	}
	if err == nil && strings.TrimSpace(string(tried)) == found.Call {
		return false // tried this beacon already
	}
	iat := peekIssuedAt(found.Token)
	minted := time.Unix(iat, 0)
	if iat == 0 || minted.Before(installed) == missed || time.Since(minted) > installedSinceWindow {
		return false
	}
	if missed && time.Since(minted) < missedHookAge {
		return false // its hook may still be at it
	}
	v := *h.Verifier
	v.Now = func() time.Time { return time.Unix(iat, 0) }
	claims, err := v.VerifyBound(ctx, found.Token, beacon.Binding{Conversation: in.Conversation, Call: found.Call, Unbound: h.Unbound})
	if err != nil {
		return false
	}
	call := Input{Conversation: in.Conversation, ToolName: found.ToolName, MCPTool: MCPToolOf(found.ToolName), Server: MCPServerOf(found.ToolName), ToolCallID: found.Call}
	if !acceptable(claims, InResultMeta, call) {
		return false
	}
	// Tried once: if the marker can't be written, don't try at all rather
	// than at every stop.
	if os.MkdirAll(filepath.Dir(marker), 0o700) != nil || os.WriteFile(marker, []byte(found.Call), 0o600) != nil {
		return false
	}
	which := "the result before the install"
	if missed {
		which = "a result its hook missed"
	}
	note := h.connectVerified(ctx, call, claims, in.Conversation, true)
	if h.Links.Load(in.Conversation) == "" {
		h.why = "couldn't connect from " + which + ": " + note
		return false
	}
	// Said with the first wake (listen puts a saved note before it).
	h.saveNote(in.Conversation, note)
	h.why = "connected from " + which
	return true
}

func (h *Hook) nudgedPath(conversation string) string {
	return filepath.Join(h.Verifier.Settings.Dir, "nudged", filepath.Base(h.Links.path(conversation)))
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
