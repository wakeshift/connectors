package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// NoteInstalledSince wakes an agent whose hooks were installed after the
// last result that could have connected its conversation: nothing will
// connect it until the agent calls the tool again. In wakebench S11 an
// agent that installed mid-conversation took "nothing to approve" as done,
// never called another tool, and missed its turns.
const NoteInstalledSince = "Wakeshift: its hooks were installed after your last result from %s, so they haven't connected this conversation and can't wake it yet. " +
	"Call %s again now, without waiting for the user: its result connects them."

// installedSinceWindow: a beacon older than this is from a conversation
// the user has moved on from; resuming it doesn't wake the agent.
const installedSinceWindow = time.Hour

// installedSince, for a waiter whose conversation has no link, wakes the
// agent once to call the tool again when the conversation's newest beacon
// came before the hooks were installed (so no hook saw it), within the
// last hour. The beacon is checked as a hook would (signed by an issuer
// trusted here, bound to its call, from the tool it names), except for its
// expiry: it's evidence the conversation asked to be woken, not redeemed.
// Once per conversation, so an agent that doesn't connect isn't woken
// again and again.
func (h *Hook) installedSince(ctx context.Context, in Input) (Output, bool) {
	hf, ok := h.Format.(HistoryFormat)
	if !ok || h.Verifier == nil {
		return Output{}, false
	}
	installed := h.Verifier.Settings.InstalledAt
	if installed.IsZero() {
		return Output{}, false
	}
	marker := h.nudgedPath(in.Conversation)
	if _, err := os.Stat(marker); err == nil {
		return Output{}, false
	}
	found, ok := hf.LastBeacon(in)
	if !ok || found.Token == "" || found.Call == "" {
		return Output{}, false
	}
	iat := peekIssuedAt(found.Token)
	if iat == 0 || !time.Unix(iat, 0).Before(installed) || time.Since(time.Unix(iat, 0)) > installedSinceWindow {
		return Output{}, false
	}
	v := *h.Verifier
	v.Now = func() time.Time { return time.Unix(iat, 0) }
	claims, err := v.VerifyBound(ctx, found.Token, beacon.Binding{Conversation: in.Conversation, Call: found.Call, Unbound: h.Unbound})
	if err != nil {
		return Output{}, false
	}
	call := Input{Conversation: in.Conversation, ToolName: found.ToolName, MCPTool: MCPToolOf(found.ToolName), ToolCallID: found.Call}
	if !acceptable(claims, InResultMeta, call) {
		return Output{}, false
	}
	tool := safeToolName(found.ToolName)
	out, ok := h.Format.Continue(in, fmt.Sprintf(NoteInstalledSince, tool, tool))
	if !ok {
		return Output{}, false
	}
	// Said once: if the marker can't be written, say nothing rather than
	// risk waking the agent on every stop.
	if os.MkdirAll(filepath.Dir(marker), 0o700) != nil || os.WriteFile(marker, nil, 0o600) != nil {
		return Output{}, false
	}
	h.why = "told the agent to call its tool again: the hooks were installed after its last beacon"
	return out, true
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

// safeToolName is a tool name fit to quote to the model: letters, digits,
// _ and - only (Claude Code's own tool names are), at most 128 characters.
func safeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
		if b.Len() >= 128 {
			break
		}
	}
	if b.Len() == 0 {
		return "the app's tool"
	}
	return b.String()
}
