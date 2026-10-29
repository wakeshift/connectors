package connector

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A server the user re-pins to another app (they removed its line from
// servers.json, as NotePinned says to, and the other app's beacon pinned
// it) carries the other app's tools now: the first app's rules for it go,
// and aren't written for it again, so a tool of the same name there isn't
// pre-approved without the user's yes to that app.
func TestToolRulesFollowPins(t *testing.T) {
	home := setHome(t)
	ws := newFakeWakeshift(t)
	ws.app = &RedeemedApp{ID: "chat", Name: "Chat", PreapproveTools: []string{"probe"}}
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{}`)
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSONWithProbe)
	spec, _ := Lookup("claude-code")
	var out bytes.Buffer
	if err := spec.AllowTools("chat", []string{"probe"}, &out); err != nil {
		t.Fatal(err)
	}
	h := toolHook(t, "claude-code", ws)
	probeResult(t, h, ws)
	if got := allowList(t, settings); !slices.Equal(got, []string{"mcp__wsprobe__probe"}) {
		t.Fatalf("chat's rule: %v", got)
	}

	// The user re-adds wsprobe for another app and removes its pin.
	pins := filepath.Join(home, ".wakeshift", "claude-code", pinsFile)
	b, _ := json.Marshal(map[string]any{"servers": map[string]string{}})
	writeFile(t, pins, string(b))
	ws.app = &RedeemedApp{ID: "other", Name: "Other", PreapproveTools: []string{"history"}}
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	conv := "c95dca59-8b63-4ebb-a70e-8d69fde72b92"
	writeFile(t, tr, strings.Replace(claudeTranscriptLine, "BEACON", ws.beaconFor(t, conv, "other"), 1)+"\n")
	got := runAs(h, "", strings.Replace(claudePostToolUse, "TRANSCRIPT", tr, 1))
	if !has(got, "Wakeshift: connected", "Removed Wakeshift's rule mcp__wsprobe__probe") {
		t.Fatalf("other's connect: %s", got)
	}
	if !has(readFile(t, pins), `"other"`) {
		t.Fatalf("pins: %s", readFile(t, pins))
	}
	if got := allowList(t, settings); slices.Contains(got, "mcp__wsprobe__probe") {
		t.Errorf("chat's rule is left on wsprobe, which now carries other's beacons: %v", got)
	}

	// Chat's next yes doesn't write it for wsprobe again.
	out.Reset()
	if err := spec.AllowTools("chat", []string{"probe"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := allowList(t, settings); slices.Contains(got, "mcp__wsprobe__probe") {
		t.Errorf("chat's yes wrote its rule for other's server: %v", got)
	}
}

// Pins that can't be read don't take away rules the user agreed to: nothing
// is added meanwhile, and a no still removes the app's rules.
func TestToolRulesUnreadablePins(t *testing.T) {
	home := setHome(t)
	ws := newFakeWakeshift(t)
	ws.app = &RedeemedApp{ID: "chat", Name: "Chat", PreapproveTools: []string{"probe", "history"}}
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{}`)
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSONWithProbe)
	spec, _ := Lookup("claude-code")
	var out bytes.Buffer
	if err := spec.AllowTools("chat", []string{"probe"}, &out); err != nil {
		t.Fatal(err)
	}
	probeResult(t, toolHook(t, "claude-code", ws), ws)
	if got := allowList(t, settings); !slices.Equal(got, []string{"mcp__wsprobe__probe"}) {
		t.Fatalf("chat's rule: %v", got)
	}

	writeFile(t, filepath.Join(home, ".wakeshift", "claude-code", pinsFile), "{not json")
	out.Reset()
	if err := spec.AllowTools("chat", []string{"history"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := allowList(t, settings); !slices.Equal(got, []string{"mcp__wsprobe__probe"}) || !has(out.String(), "can't read") {
		t.Fatalf("with unreadable pins: %v %s", got, out.String())
	}
	out.Reset()
	if err := spec.DeclineTools("chat", &out); err != nil {
		t.Fatal(err)
	}
	if got := allowList(t, settings); len(got) != 0 {
		t.Fatalf("a no with unreadable pins left %v", got)
	}
}
