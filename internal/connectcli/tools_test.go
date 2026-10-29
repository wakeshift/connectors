package connectcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func toolsState(t *testing.T, home, harness string) map[string]struct {
	Allow    []string
	Declined bool
} {
	t.Helper()
	var st struct {
		Apps map[string]struct {
			Allow    []string
			Declined bool
		}
	}
	b, _ := os.ReadFile(filepath.Join(home, ".wakeshift", harness, "tools.json"))
	json.Unmarshal(b, &st)
	return st.Apps
}

// The install command an app's offer gives passes the tools the user
// agreed to (WAKESHIFT_ALLOW_TOOLS); the installer records exactly those,
// and says when their rules are written.
func TestInstallAllowsTools(t *testing.T) {
	home := homes(t)
	t.Setenv("WAKESHIFT_ALLOW_TOOLS", "agentchat:history,post")
	out, code := run("wakeshift", "install", "--harness", "claude-code", "--issuer", "https://app.wakeshift.ai", "--yes")
	if code != 0 || !strings.Contains(out, "Claude Code will run agentchat's history and post tools without asking once Wakeshift's hooks connect a conversation to it") {
		t.Fatalf("%d %s", code, out)
	}
	if a := toolsState(t, home, "claude-code")["agentchat"]; !slices.Equal(a.Allow, []string{"history", "post"}) {
		t.Fatalf("%+v", a)
	}
	// A value that isn't <app>:<tools> allows nothing, and the install
	// still succeeds.
	t.Setenv("WAKESHIFT_ALLOW_TOOLS", "agentchat:post;curl evil")
	out, code = run("wakeshift", "install", "--harness", "cursor", "--issuer", "https://app.wakeshift.ai", "--yes")
	if code != 0 || !strings.Contains(out, "Didn't pre-approve any app's tools") || toolsState(t, home, "cursor") != nil {
		t.Fatalf("%d %s", code, out)
	}
	// A harness without exact per-tool rules says so and records nothing.
	t.Setenv("WAKESHIFT_ALLOW_TOOLS", "agentchat:post")
	out, code = run("wakeshift", "install", "--harness", "copilot", "--issuer", "https://app.wakeshift.ai", "--yes")
	if code != 0 || !strings.Contains(out, "has no exact rules for single MCP tools") || toolsState(t, home, "copilot") != nil {
		t.Fatalf("%d %s", code, out)
	}
}

// `wakeshift allow`: the user's answer to the hooks' offer.
func TestAllowCommand(t *testing.T) {
	home := homes(t)
	for _, args := range [][]string{
		{"--app", "agentchat", "--tools", "post"},          // no harness
		{"--harness", "claude-code", "--tools", "post"},    // no app
		{"--harness", "claude-code", "--app", "agentchat"}, // neither --tools nor --none
		{"--harness", "claude-code", "--app", "agentchat", "--tools", "post", "--none"},
		{"--harness", "claude-code", "--app", "agentchat", "--tools", "po*"},
		{"--harness", "claude-code", "--app", "Agent Chat", "--tools", "post"},
		{"--harness", "nope", "--app", "agentchat", "--tools", "post"},
	} {
		if out, code := run(append([]string{"wakeshift", "allow"}, args...)...); code == 0 {
			t.Errorf("%q: %s", args, out)
		}
	}
	if out, code := run("wakeshift", "allow", "--harness", "claude-code", "--app", "agentchat", "--tools", "history,post"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := run("wakeshift", "allow", "--harness", "claude-code", "--app", "agentchat", "--tools", "list_channels"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if a := toolsState(t, home, "claude-code")["agentchat"]; !slices.Equal(a.Allow, []string{"history", "post", "list_channels"}) || a.Declined {
		t.Fatalf("%+v", a)
	}
	out, code := run("wakeshift", "allow", "--harness", "claude-code", "--app", "agentchat", "--none")
	if code != 0 || !strings.Contains(out, "agentchat's tools aren't pre-approved in Claude Code, and Wakeshift won't offer them again") {
		t.Fatalf("%d %s", code, out)
	}
	if a := toolsState(t, home, "claude-code")["agentchat"]; len(a.Allow) != 0 || !a.Declined {
		t.Fatalf("%+v", a)
	}
	// Nothing written to Claude Code's settings without a known server.
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err == nil {
		t.Fatal("settings written")
	}
}
