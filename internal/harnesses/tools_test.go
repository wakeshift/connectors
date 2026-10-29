package harnesses

import (
	"strings"
	"testing"
)

// An app's install offer to Claude Code or Cursor asks, in the same
// question, to allow exactly the app's tools its manifest names, by name,
// and its command passes them to the installer.
func TestWithTools(t *testing.T) {
	const base = "https://app.wakeshift.ai"
	offer := ToolsOffer{App: "agentchat", AppName: "Agent Chat", Tools: []string{"list_channels", "history", "post"}}
	const cmd = "curl -fsSL https://app.wakeshift.ai/install/claude-code.sh | WAKESHIFT_ALLOW_TOOLS=agentchat:list_channels,history,post sh"
	cc, _ := Get("claude-code")
	h := WithTools(cc, offer)
	const consent = "; it also lets Claude Code run Agent Chat's list_channels, history and post tools without asking each time (no other tools)"
	if InstallCommand(h, base) != cmd || !strings.HasSuffix(h.Changes, consent) {
		t.Fatalf("%s\n%s", InstallCommand(h, base), h.Changes)
	}
	// In the picker's yes option, next to the command it runs, on every
	// path that offers the install, and the ! fallback runs the same.
	for name, note := range map[string]string{"subscribe": SubscribeNote(h, base), "upgrade": UpgradeNote(h, base), "repeat": RepeatNote(h, base)} {
		for _, want := range []string{`"Yes, install Wakeshift's hooks for Claude Code" (description: runs ` + "`" + cmd + "`, adds three hooks",
			consent + ")", "by typing `! " + cmd + "`"} {
			if !strings.Contains(note, want) {
				t.Errorf("%s: missing %q in %s", name, want, note)
			}
		}
	}

	cursor, _ := Get("cursor")
	h = WithTools(cursor, ToolsOffer{App: "agentchat", AppName: "Agent Chat", Tools: []string{"post"}})
	n := BackgroundUpgradeNote(h, base)
	if !strings.Contains(n, "it also lets Cursor run Agent Chat's post tool without asking each time (no other tools)") ||
		!strings.Contains(n, "run `curl -fsSL https://app.wakeshift.ai/install/cursor.sh | WAKESHIFT_ALLOW_TOOLS=agentchat:post sh`") {
		t.Fatalf("cursor: %s", n)
	}

	// Unchanged: a harness without exact per-tool rules, an app that names
	// no tools, and names a rule (or a shell) can't hold.
	for _, id := range []string{"codex", "copilot", "antigravity", "opencode", "hermes", "openclaw"} {
		h, _ := Get(id)
		if got := WithTools(h, offer); got.Changes != h.Changes || InstallCommand(got, base) != InstallCommand(h, base) {
			t.Errorf("%s offered tools: %s", id, got.Changes)
		}
	}
	for name, o := range map[string]ToolsOffer{
		"no tools":     {App: "agentchat", AppName: "Agent Chat"},
		"a glob":       {App: "agentchat", Tools: []string{"post", "*"}},
		"shell":        {App: "agentchat", Tools: []string{"post;rm -rf ~"}},
		"__":           {App: "agentchat", Tools: []string{"a__b"}},
		"a bad app ID": {App: "Agent Chat", Tools: []string{"post"}},
	} {
		if got := WithTools(cc, o); got.Changes != cc.Changes || InstallCommand(got, base) != InstallCommand(cc, base) {
			t.Errorf("%s: %s / %s", name, got.Changes, InstallCommand(got, base))
		}
	}
	// The app's ID stands in for a missing name.
	if h := WithTools(cc, ToolsOffer{App: "agentchat", Tools: []string{"post"}}); !strings.Contains(h.Changes, "run agentchat's post tool") {
		t.Fatal(h.Changes)
	}
	// With an AgreeEnv too, both variables are set for the installer.
	oc, _ := Get("openclaw")
	oc.ToolRules, oc.allowTools = true, AllowToolsEnv+"=agentchat:post"
	if got := agreed(oc, InstallCommand(oc, base)); got != "curl -fsSL https://app.wakeshift.ai/install/openclaw.sh | WAKESHIFT_ALLOW_TOOLS=agentchat:post WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1 sh" {
		t.Fatal(got)
	}
	if ToolList([]string{"a"}) != "a" || ToolList([]string{"a", "b"}) != "a and b" || ToolList([]string{"a", "b", "c"}) != "a, b and c" {
		t.Fatal("ToolList")
	}
}
