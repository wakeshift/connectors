package connector

import (
	"encoding/json"
	"strings"

	"github.com/wakeshift/connectors/internal/beacon"
)

// Carried is where in a tool result a beacon was found. It matters for a
// beacon that doesn't name its conversation: text any tool returns (a web
// page, an issue comment) can hold a beacon line, and a JSON document a
// tool fetched can hold a _meta, but only an MCP server sets its own
// result's _meta.
type Carried int

const (
	NotCarried Carried = iota
	// InResultMeta: the result's own _meta (the CallToolResult's, or a
	// JSON string of it).
	InResultMeta
	// InNestedMeta: a _meta inside the result (under result, output…):
	// possibly a document a tool returned.
	InNestedMeta
	// InText: a beacon.Text line ending the result's text (beacon.Trailer):
	// where Wakeshift puts it, after everything the app wrote. A beacon
	// line anywhere else in the text (a chat message a read tool relays,
	// JSON it returns) isn't found at all.
	InText
)

// FindBeacon finds the beacon a tool result carries, saying where: in its
// own _meta, ending its text (resultText), or in a _meta a few levels down
// (last: a document a tool returned can hold one, and mustn't hide the
// result's own).
func FindBeacon(resp json.RawMessage) (string, Carried) {
	obj := resp
	if len(resp) > 0 && resp[0] == '"' {
		var s string
		obj = nil
		if json.Unmarshal(resp, &s) == nil && strings.HasPrefix(strings.TrimSpace(s), "{") {
			obj = json.RawMessage(s)
		}
	}
	tok, top := metaBeacon(obj)
	if tok != "" && top {
		return tok, InResultMeta
	}
	if t := beacon.Trailer(resultText(resp, 0)); t != "" {
		return t, InText
	}
	if tok != "" {
		return tok, InNestedMeta
	}
	return "", NotCarried
}

// resultText is a tool result's text, as the hook sees it: the result
// itself if the harness passes text (a JSON string), else its last text
// content item ({"content": [...]}, or such a result as a JSON string).
// The SDKs put Wakeshift's text, beacon last, at the end of the last item.
func resultText(resp json.RawMessage, depth int) string {
	if len(resp) == 0 || depth > 2 {
		return ""
	}
	if resp[0] == '"' {
		var s string
		if json.Unmarshal(resp, &s) != nil {
			return ""
		}
		if t := strings.TrimSpace(s); strings.HasPrefix(t, "{") {
			if inner := resultText(json.RawMessage(t), depth+1); inner != "" {
				return inner
			}
		}
		return s
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(resp, &r) != nil {
		return ""
	}
	for i := len(r.Content) - 1; i >= 0; i-- {
		if r.Content[i].Type == "text" {
			return r.Content[i].Text
		}
	}
	return ""
}

// HasBeaconText says whether a result has a beacon line anywhere in its
// text: the hooks take every one out of what the model sees, acted on or
// not.
func HasBeaconText(resp json.RawMessage) bool { return beacon.InText(string(resp)) != "" }

// metaBeacon is the beacon in resp's own _meta (top), else one a few levels
// down (beaconIn).
func metaBeacon(resp json.RawMessage) (string, bool) {
	var r map[string]json.RawMessage
	if json.Unmarshal(resp, &r) != nil {
		return "", false
	}
	if m, ok := r["_meta"]; ok {
		var meta map[string]any
		json.Unmarshal(m, &meta)
		if s, _ := meta[beacon.MetaKey].(string); s != "" {
			return s, true
		}
	}
	return beaconIn(resp, 1), false
}

// ToolOf says whether the result in is tool's (a beacon's Tool claim): the
// bare MCP tool name the harness gave (Input.MCPTool) is tool, or, where
// the harness joins server and tool with a separator that either can
// contain (opencode's <server>_<tool>: Input.ToolSep), its tool name ends
// with the separator and tool.
func ToolOf(in Input, tool string) bool {
	if in.MCPTool != "" {
		return beacon.ToolMatches(tool, in.MCPTool)
	}
	if in.ToolSep == "" || tool == "" || in.ToolName == "" {
		return false
	}
	name, suffix := beacon.NormalTool(in.ToolName), in.ToolSep+beacon.NormalTool(tool)
	return len(name) > len(suffix) && strings.HasSuffix(name, suffix)
}

// TextOnlyFormat is a Format whose hooks get a tool result's text only,
// never its _meta (Copilot CLI's textResultForLlm, Cursor's tool_output,
// Antigravity's transcript step). A JSON object with a top-level _meta in
// that text, or one further in, is the tool's words (a chat message a read
// tool relays as its whole text), not the result's own _meta, so no beacon
// is acted on from it: only from the line Wakeshift puts at the end of the
// text (InText), these harnesses' carrier (security review, October 28,
// 2026).
type TextOnlyFormat interface {
	TextOnly() bool
}

// carriedFor is where a beacon was carried, as far as h's harness can
// tell: never a _meta for a TextOnlyFormat.
func (h *Hook) carriedFor(c Carried) Carried {
	if tf, ok := h.Format.(TextOnlyFormat); ok && tf.TextOnly() && (c == InResultMeta || c == InNestedMeta) {
		return NotCarried
	}
	return c
}

// acceptable says whether a verified beacon may connect the conversation
// from where it was carried, in the result in describes: one that names the
// conversation, from the result's own _meta or its text's last line; one
// bound to the call, from _meta; one bound to
// nothing (the harness tells servers neither), only from the result's own
// _meta. And only from the tool it names (beacon.ToolMatches): a beacon
// naming no tool (an app on an SDK from before Tool) only from the
// result's own _meta, which no text a tool relays can set.
func acceptable(c beacon.Claims, carried Carried, in Input) bool {
	if c.Tool == "" {
		if carried != InResultMeta {
			return false
		}
	} else if !ToolOf(in, c.Tool) {
		return false
	}
	switch {
	case c.Thread != "":
		// Not from a _meta inside the result: that can be a document or
		// message the tool relays.
		return carried == InResultMeta || carried == InText
	case c.Call != "":
		return carried == InResultMeta || carried == InNestedMeta
	}
	return carried == InResultMeta
}
