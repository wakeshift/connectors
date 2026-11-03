package connector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Antigravity's plugin (github.com/wakeshift/antigravity-plugin) replaces a
// curl install's "wakeshift" entry in ~/.gemini/config/hooks.json: moving
// over takes that entry out, keeping the user's own hooks, so agy doesn't
// run Wakeshift's hooks twice.
func TestInstallPluginRemovesAntigravityHooks(t *testing.T) {
	home := setHome(t)
	hooks := filepath.Join(home, ".gemini", "config", "hooks.json")
	os.MkdirAll(filepath.Dir(hooks), 0o700)
	os.WriteFile(hooks, []byte(`{"lint":{"Stop":[{"type":"command","command":"./lint.sh"}]}}`), 0o600)
	agy, _ := Lookup("antigravity")
	install(t, agy, testBin)
	if b, _ := os.ReadFile(hooks); !strings.Contains(string(b), `"wakeshift"`) {
		t.Fatalf("curl install: %s", b)
	}
	out := installPlugin(t, agy, testBin, "https://app.wakeshift.ai")
	if !strings.Contains(out, "Removed the hooks an earlier Wakeshift install wrote to "+hooks+" (the previous file is at ") {
		t.Fatalf("antigravity: %s", out)
	}
	b, _ := os.ReadFile(hooks)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil || len(top) != 1 || top["lint"] == nil {
		t.Fatalf("hooks.json: %s %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".wakeshift", "antigravity", PluginIssuerFile)); err != nil || string(b) != "https://app.wakeshift.ai\n" {
		t.Fatalf("plugin-issuer %q %v", b, err)
	}
	if out := installPlugin(t, agy, testBin, "https://app.wakeshift.ai"); strings.Contains(out, "Removed") {
		t.Fatalf("twice: %s", out)
	}
}

// A hooks.json the curl install created, holding only Wakeshift's entry,
// goes with it.
func TestInstallPluginRemovesCreatedAntigravityFile(t *testing.T) {
	home := setHome(t)
	agy, _ := Lookup("antigravity")
	install(t, agy, testBin)
	hooks := filepath.Join(home, ".gemini", "config", "hooks.json")
	out := installPlugin(t, agy, testBin, "https://app.wakeshift.ai")
	if _, err := os.Stat(hooks); err == nil || !strings.Contains(out, "Removed the hooks an earlier Wakeshift install wrote to "+hooks) {
		t.Fatalf("hooks.json left: %s", out)
	}
}

// The plugin's hooks.json (plugins/agent-plugins/standalone/antigravity)
// runs, through its launcher, the same hooks with the same timeouts as the
// entry Install writes, so both routes behave alike.
func TestAntigravityPluginHooksMatch(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "plugins", "agent-plugins", "standalone", "antigravity", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plugin map[string]json.RawMessage
	if err := json.Unmarshal(b, &plugin); err != nil {
		t.Fatal(err)
	}
	const launcher = "sh ./bin/wakeshift"
	norm := func(raw json.RawMessage, prefix string) map[string]any {
		var events map[string]any
		json.Unmarshal(raw, &events)
		delete(events, "SessionStart") // the plugin's alone: it fetches the connector early
		s, _ := json.Marshal(events)
		return decode(strings.ReplaceAll(string(s), prefix, "BIN"))
	}
	got := norm(plugin["wakeshift"], launcher)
	want := norm(antigravityInstaller{}.ours(testBin), ShellQuote(testBin))
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("plugin hooks\n%s\ndiffer from the installer's\n%s", g, w)
	}
	var start struct {
		SessionStart []struct{ Command string }
	}
	json.Unmarshal(plugin["wakeshift"], &start)
	if len(start.SessionStart) != 1 || start.SessionStart[0].Command != launcher+" prepare" {
		t.Fatalf("SessionStart: %+v", start)
	}
}

func decode(s string) map[string]any {
	var m map[string]any
	json.Unmarshal([]byte(s), &m)
	return m
}
