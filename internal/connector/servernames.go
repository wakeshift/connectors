package connector

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Per-server conversation names, for harnesses where it's Wakeshift's hook,
// not the harness, that tells MCP servers the conversation (Copilot CLI's
// preMcpToolCall). The hook can't tell which servers are Wakeshift apps, so
// it tells every server the conversation under a name of that server's own:
// an HMAC of the server's name and the harness's conversation ID under a key
// kept here (0600). A server learns nothing it could name the conversation
// with elsewhere, and a beacon names the server it was minted for: one
// planted in another server's result, or in a tool's text, doesn't name that
// result's server, so it isn't acted on. The pins (Pins) still tie each
// server to one app.

// AliasFormat is a Format whose MCP servers are told the conversation by the
// hook (PreCall), each under its own name (ServerConversation): a beacon is
// acted on only from a result whose server the hook knows (Server), bound to
// that server's name for the conversation.
type AliasFormat interface {
	Aliased() bool
}

const (
	aliasKeyFile = "conversation-names.key"
	callsDir     = "calls"
	// callTTL: a call's record (its server, for attributing its result) is
	// kept this long.
	callTTL = 10 * time.Minute
	// maxCalls a conversation's records keep.
	maxCalls = 50
)

// aliased says whether this hook's harness gives servers per-server names.
func (h *Hook) aliased() bool {
	af, ok := h.Format.(AliasFormat)
	return ok && af.Aliased()
}

// serverConversation is server's name for the conversation ("" if the key
// can't be read or made).
func (h *Hook) serverConversation(server, conversation string) string {
	if server == "" || conversation == "" {
		return ""
	}
	key, err := h.aliasKey()
	if err != nil {
		return ""
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("wakeshift conversation\x00" + server + "\x00" + conversation))
	return "wsn_" + hex.EncodeToString(m.Sum(nil)[:16])
}

// aliasKey is the key the names are made with, made the first time.
func (h *Hook) aliasKey() ([]byte, error) {
	dir := h.Verifier.Settings.Dir
	if !filepath.IsAbs(dir) {
		return nil, errors.New("connector: the settings need an absolute path")
	}
	path := filepath.Join(dir, aliasKeyFile)
	if b, err := os.ReadFile(path); err == nil && len(b) == 64 {
		return hex.DecodeString(string(b))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if b, err := os.ReadFile(path); err == nil && len(b) == 64 {
		return hex.DecodeString(string(b))
	}
	key := make([]byte, 32)
	rand.Read(key)
	tmp, err := os.CreateTemp(dir, "."+aliasKeyFile+".*.tmp") // 0600
	if err != nil {
		return nil, err
	}
	_, err = tmp.WriteString(hex.EncodeToString(key))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	return key, nil
}

// mcpCall is a PreCall's record: which server a call went to, so its
// result (whose hook input may not say) can be attributed.
type mcpCall struct {
	At     time.Time `json:"at"`
	Call   string    `json:"call,omitempty"`
	Server string    `json:"server"`
	Tool   string    `json:"tool"`
	URL    string    `json:"url,omitempty"` // the server's, if the harness said (Cursor)
}

func (h *Hook) callsPath(conversation string) string {
	return filepath.Join(h.Verifier.Settings.Dir, callsDir, filepath.Base(h.Links.path(conversation)))
}

// recordCall keeps a PreCall's server and tool for its result.
func (h *Hook) recordCall(in Input) {
	p := h.callsPath(in.Conversation)
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	unlock, err := lockFile(p + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	list := liveCalls(p)
	list = append(list, mcpCall{At: time.Now(), Call: in.ToolCallID, Server: in.Server, Tool: in.ToolName, URL: in.ServerURL})
	if len(list) > maxCalls {
		list = list[len(list)-maxCalls:]
	}
	b, _ := json.Marshal(list)
	os.WriteFile(p, b, 0o600)
}

func liveCalls(path string) []mcpCall {
	var list, live []mcpCall
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &list)
	}
	for _, c := range list {
		if time.Since(c.At) < callTTL {
			live = append(live, c)
		}
	}
	return live
}

// calledServer is the MCP server (and its URL) the recorded call with the
// tool result's call ID went to, for harnesses whose CallDone hook names
// it (CallServerFormat): "" without exactly that call.
func (h *Hook) calledServer(in Input) (server, url string) {
	if in.ToolCallID == "" {
		return "", ""
	}
	p := h.callsPath(in.Conversation)
	unlock, err := lockFile(p + ".lock")
	if err != nil {
		return "", ""
	}
	defer unlock()
	for _, c := range liveCalls(p) {
		if c.Call == in.ToolCallID {
			return c.Server, c.URL
		}
	}
	return "", ""
}

// ServerNamer is an AliasFormat that says which recorded call a tool
// result answers (Copilot: the call ID if its input has one, else the
// result's tool name, "<server>-<tool>").
type ServerNamer interface {
	ResultOf(in Input, c mcpCall) bool
}

// serverOf is the MCP server a tool result came from, and its tool, from
// the calls the conversation's PreCalls recorded: "" unless exactly one
// server matches (a built-in tool's result, or a name two servers' tools
// could make, has none), and the tool "" unless one tool of it does.
func (h *Hook) serverOf(in Input) (server, tool string) {
	sn, ok := h.Format.(ServerNamer)
	if !ok {
		return "", ""
	}
	p := h.callsPath(in.Conversation)
	unlock, err := lockFile(p + ".lock")
	if err != nil {
		return "", ""
	}
	defer unlock()
	for _, c := range liveCalls(p) {
		if !sn.ResultOf(in, c) {
			continue
		}
		if server != "" && server != c.Server {
			return "", ""
		}
		if server == "" {
			tool = c.Tool
		} else if tool != c.Tool {
			tool = ""
		}
		server = c.Server
	}
	return server, tool
}
