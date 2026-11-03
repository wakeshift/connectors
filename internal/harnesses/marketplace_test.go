package harnesses

import (
	"strings"
	"testing"
)

// Codex's offers install Wakeshift's plugin from the marketplace with
// Codex's own plugin commands: production's from its default branch,
// staging's from its staging branch, anything else (a local core) through
// Wakeshift's installer, unless WAKESHIFT_AGENT_PLUGINS names a marketplace.
func TestCodexMarketplace(t *testing.T) {
	h, _ := Get("codex")
	const (
		install = "codex plugin marketplace add wakeshift/agent-plugins && codex plugin add wakeshift@wakeshift"
		update  = "codex plugin marketplace add wakeshift/agent-plugins && codex plugin marketplace upgrade wakeshift && codex plugin add wakeshift@wakeshift"
	)
	for base, want := range map[string][2]string{
		"https://app.wakeshift.ai":          {install, update},
		"https://app.wakeshift.ai/":         {install, update},
		"https://app.staging.wakeshift.ai":  {strings.ReplaceAll(install, "agent-plugins", "agent-plugins@staging"), strings.ReplaceAll(update, "agent-plugins", "agent-plugins@staging")},
		"http://localhost:8370":             {"curl -fsSL http://localhost:8370/install/codex.sh | sh", "curl -fsSL http://localhost:8370/install/codex.sh | sh"},
		"https://wakeshift.example.com/sub": {"curl -fsSL https://wakeshift.example.com/sub/install/codex.sh | sh", "curl -fsSL https://wakeshift.example.com/sub/install/codex.sh | sh"},
	} {
		if got := OfferCommand(h, base); got != want[0] {
			t.Errorf("%s: offer %s", base, got)
		}
		if got := UpdateCommand(h, base); got != want[1] {
			t.Errorf("%s: update %s", base, got)
		}
		plugin := strings.Contains(want[0], "codex plugin")
		if PluginRoute(h, base) != plugin {
			t.Errorf("%s: plugin route %v", base, !plugin)
		}
		// The texts follow the route: the plugin's for the plugin, the
		// hooks file's for the installer.
		n := InstallNote(h, base)
		if plugin != strings.Contains(n, "from github.com/wakeshift/agent-plugins") || plugin == strings.Contains(n, "hooks.json") {
			t.Errorf("%s: install note %s", base, n)
		}
		if u := UpdateNote(h, base); plugin != strings.Contains(u, "Codex asks to trust the plugin's hooks again after each update") {
			t.Errorf("%s: update note %s", base, u)
		}
	}

	// A marketplace on this machine, for a local core (the bench).
	SetAgentPlugins("/tmp/wb/agent-plugins")
	defer SetAgentPlugins("")
	if got := OfferCommand(h, "http://localhost:8370"); got != "codex plugin marketplace add /tmp/wb/agent-plugins && codex plugin add wakeshift@wakeshift" {
		t.Errorf("override: %s", got)
	}
	// Production's offers name production's marketplace whatever it says.
	if got := OfferCommand(h, "https://app.wakeshift.ai"); got != install {
		t.Errorf("production with an override: %s", got)
	}
	// Anything that isn't a plain path or owner/repo isn't put in a command.
	for _, bad := range []string{"/tmp/a b", "x; rm -rf ~", "wakeshift/agent-plugins$(id)", "relative/../x/y", ""} {
		SetAgentPlugins(bad)
		if got := OfferCommand(h, "http://localhost:8370"); !strings.HasPrefix(got, "curl ") {
			t.Errorf("%q: %s", bad, got)
		}
	}
	SetAgentPlugins("")

	// Uninstalling names the plugin's own command first.
	if n := UninstallNote(h); !strings.Contains(n, "`codex plugin remove wakeshift@wakeshift` (if they installed Wakeshift's plugin), then `~/.wakeshift/bin/wakeshift uninstall --harness codex`") {
		t.Errorf("uninstall: %s", n)
	}
	// Route leaves a harness without a marketplace as it is.
	cc, _ := Get("claude-code")
	if Route(cc, "https://app.wakeshift.ai").Changes != cc.Changes {
		t.Error("claude-code routed")
	}
}
