package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
)

// connect redeems a beacon for the conversation and keeps its link,
// returning the note for the model ("" if the beacon isn't one to act on:
// not from a Wakeshift trusted here, or not for this conversation or call;
// its text can't be trusted to repeat, so nothing is said).
//
// A beacon proves Wakeshift minted it, not that the result it came in was
// its app's: one an attacker minted for this conversation through some app
// can be planted in another server's result, or in text a tool returns. So
// it must also come from where its app's beacons can (acceptable: a
// beacon that doesn't name the conversation, only from the result's own
// _meta) and, where the harness says which MCP server the result came from,
// from the server pinned to its app (Pins). Where it doesn't, a beacon
// never takes over a conversation another app's beacon connected.
func (h *Hook) connect(ctx context.Context, in Input, token string, carried Carried) string {
	if token == "" {
		return ""
	}
	conv := in.Conversation
	if h.aliased() {
		// Only a result from a server the hook told, bound to that
		// server's name for the conversation: a beacon planted in another
		// server's result, or in a built-in tool's, names another.
		if in.Server == "" || in.ServerConversation == "" {
			return ""
		}
		conv = in.ServerConversation
	}
	bind := beacon.Binding{Conversation: conv, Call: in.ToolCallID, Unbound: h.Unbound}
	claims, err := h.Verifier.VerifyBound(ctx, token, bind)
	if err != nil || !acceptable(claims, carried) {
		return ""
	}
	if in.Server != "" {
		var pinned *ErrPinned
		var unreadable *ErrPinFile
		if err := h.pins().Allow(in.Server, claims.App); errors.As(err, &pinned) {
			return fmt.Sprintf(NotePinned, pinned.Error(), h.pins().Path)
		} else if errors.As(err, &unreadable) {
			return fmt.Sprintf(NotePinFile, unreadable.Path)
		} else if err != nil {
			return fmt.Sprintf(NoteFailed, "its server couldn't be recorded")
		}
	} else if app := h.Links.AppOf(in.Conversation); app != "" && app != claims.App {
		return fmt.Sprintf(NoteOtherApp, app, claims.App)
	}
	call := ""
	if claims.Thread == "" { // bound to the call or to nothing: Wakeshift binds the conversation
		call = in.ToolCallID
	}
	link, err := RedeemCall(ctx, h.HTTP, h.Harness, claims, conv, call)
	var outdated *UpdateRequired
	if errors.As(err, &outdated) {
		return fmt.Sprintf(NoteOutdated, outdated.Message)
	}
	if err != nil {
		if errors.Is(err, ErrUnreachable) && h.OnTransient != nil {
			h.OnTransient()
		}
		return fmt.Sprintf(NoteFailed, err)
	}
	if err := h.Links.SaveFor(in.Conversation, link, claims.App); err != nil {
		return fmt.Sprintf(NoteFailed, "its link couldn't be saved")
	}
	return fmt.Sprintf(NoteConnected, h.When)
}

// DeferredFormat is a Format whose tool results can only be read after the
// hook returns (Antigravity writes the result's step to its transcript
// then): Resolve reads one later, from what Parse put in Input.deferred
// (nil while it still isn't there).
type DeferredFormat interface {
	Resolve(deferred []byte) json.RawMessage
}

// deferredTTL: a result not readable within this long is dropped (its
// beacon has expired anyway).
const deferredTTL = 2 * time.Minute

type deferredResult struct {
	At       time.Time       `json:"at"`
	Call     string          `json:"call,omitempty"`
	Server   string          `json:"server,omitempty"`
	Deferred json.RawMessage `json:"deferred"`
}

func (h *Hook) pendingPath(conversation string) string {
	return filepath.Join(h.Verifier.Settings.Dir, "pending", filepath.Base(h.Links.path(conversation)))
}

// deferResult keeps a tool result to read at the conversation's next hook.
func (h *Hook) deferResult(in Input) {
	p := h.pendingPath(in.Conversation)
	var list []deferredResult
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &list)
	}
	list = append(list, deferredResult{At: time.Now(), Call: in.ToolCallID, Server: in.Server, Deferred: in.deferred})
	if len(list) > 20 {
		list = list[len(list)-20:]
	}
	b, _ := json.Marshal(list)
	if os.MkdirAll(filepath.Dir(p), 0o700) == nil {
		os.WriteFile(p, b, 0o600)
	}
}

// resolveDeferred reads the conversation's deferred tool results that are
// readable now and connects it from a beacon in them; the note waits for
// the next place the harness can show it (saveNote).
func (h *Hook) resolveDeferred(ctx context.Context, in Input) {
	h.resolvePending(ctx, in)
}

// resolvePending is resolveDeferred, saying whether results are still
// waiting to be written. A resolver and the conversation's next hook can
// run at once: the lock keeps a result from being read, and its beacon
// redeemed, twice.
func (h *Hook) resolvePending(ctx context.Context, in Input) (waiting bool) {
	df, ok := h.Format.(DeferredFormat)
	if !ok {
		return false
	}
	p := h.pendingPath(in.Conversation)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	unlock, err := lockFile(p + ".lock")
	if err != nil {
		return false
	}
	defer unlock()
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var list, kept []deferredResult
	json.Unmarshal(b, &list)
	for _, d := range list {
		if time.Since(d.At) > deferredTTL {
			continue
		}
		res := df.Resolve(d.Deferred)
		if res == nil {
			kept = append(kept, d)
			continue
		}
		at := in
		at.ToolCallID = d.Call
		at.Server = d.Server
		tok, carried := FindBeacon(res)
		if n := h.connect(ctx, at, tok, carried); n != "" {
			h.saveNote(in.Conversation, n)
		}
	}
	if len(kept) == 0 {
		os.Remove(p)
		return false
	}
	b, _ = json.Marshal(kept)
	os.WriteFile(p, b, 0o600)
	return true
}

// resolveWait bounds a resolver: the harness writes a result within
// moments of its hook returning.
const resolveWait = 15 * time.Second

// resolveSoon is a resolver (ResolveSoon): it reads the conversation's
// deferred results as soon as they're written, redeeming their beacons,
// for up to resolveWait.
func (h *Hook) resolveSoon(ctx context.Context, in Input) {
	ctx, cancel := context.WithTimeout(ctx, resolveWait)
	defer cancel()
	for h.resolvePending(ctx, in) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}
