package connector

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/wakeshift-plugins/internal/beacon"
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
	// InText: a beacon.Text line.
	InText
)

// FindBeacon is BeaconIn, saying where the beacon was.
func FindBeacon(resp json.RawMessage) (string, Carried) {
	if len(resp) > 0 && resp[0] == '"' {
		var s string
		if json.Unmarshal(resp, &s) == nil && strings.HasPrefix(strings.TrimSpace(s), "{") {
			if tok, top := metaBeacon(json.RawMessage(s)); tok != "" {
				return tok, carriedAt(top)
			}
		}
	} else if tok, top := metaBeacon(resp); tok != "" {
		return tok, carriedAt(top)
	}
	if tok := beacon.InText(string(resp)); tok != "" {
		return tok, InText
	}
	return "", NotCarried
}

func carriedAt(top bool) Carried {
	if top {
		return InResultMeta
	}
	return InNestedMeta
}

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

// acceptable says whether a verified beacon may connect the conversation
// from where it was carried: one that names the conversation, anywhere;
// one bound to the call, from _meta; one bound to nothing (the harness
// tells servers neither), only from the result's own _meta.
func acceptable(c beacon.Claims, carried Carried) bool {
	switch {
	case c.Thread != "":
		return carried != NotCarried
	case c.Call != "":
		return carried == InResultMeta || carried == InNestedMeta
	}
	return carried == InResultMeta
}
