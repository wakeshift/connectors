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
	hf, ok := h.Format.(HistoryFormat)
	if !ok || h.Verifier == nil {
		return false
	}
	installed := h.Verifier.Settings.InstalledAt
	if installed.IsZero() {
		return false
	}
	marker := h.nudgedPath(in.Conversation)
	if _, err := os.Stat(marker); err == nil {
		return false
	}
	found, ok := hf.LastBeacon(in)
	if !ok || found.Token == "" || found.Call == "" {
		return false
	}
	iat := peekIssuedAt(found.Token)
	if iat == 0 || !time.Unix(iat, 0).Before(installed) || time.Since(time.Unix(iat, 0)) > installedSinceWindow {
		return false
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
	if os.MkdirAll(filepath.Dir(marker), 0o700) != nil || os.WriteFile(marker, nil, 0o600) != nil {
		return false
	}
	note := h.connectVerified(ctx, call, claims, in.Conversation, true)
	if h.Links.Load(in.Conversation) == "" {
		h.why = "couldn't connect from the beacon before the install: " + note
		return false
	}
	// Said with the first wake (listen puts a saved note before it).
	h.saveNote(in.Conversation, note)
	h.why = "connected from the result before the install"
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
