package harnesses

import (
	"strings"
	"testing"
)

// Claude Code's offers give its own plugin command, from Wakeshift's
// marketplace (its staging branch for staging). The install still sets the
// issuer option an older copy of the plugin had (a stale marketplace clone
// may install it); the update, which ends on the current plugin, has none:
// it refreshes the marketplace first, after an install that moves a user of
// Wakeshift's installer over.
func TestClaudeCodeMarketplace(t *testing.T) {
	h, _ := Get("claude-code")
	h = WithClient(h, newClaudeUA, "", "")
	for base, want := range map[string]string{
		"https://app.wakeshift.ai/":        "claude plugin install wakeshift --marketplace wakeshift/agent-plugins",
		"https://app.staging.wakeshift.ai": "claude plugin install wakeshift --marketplace wakeshift/agent-plugins#staging",
	} {
		if got := OfferCommand(h, base); got != want {
			t.Errorf("%s: %s", base, got)
		}
		if got := UpdateCommand(h, base); got != want+" && claude plugin marketplace update wakeshift && claude plugin update wakeshift@wakeshift" || strings.Contains(got, "--config") {
			t.Errorf("%s update: %s", base, got)
		}
	}
	// A local core: Wakeshift's installer, unless a marketplace is set for it.
	if got := OfferCommand(h, "http://localhost:8370"); !strings.HasPrefix(got, "curl ") {
		t.Errorf("local: %s", got)
	}
	SetAgentPlugins("/tmp/wb/marketplace")
	defer SetAgentPlugins("")
	if got := OfferCommand(h, "http://localhost:8370"); got != "claude plugin install wakeshift --marketplace /tmp/wb/marketplace" {
		t.Errorf("configured: %s", got)
	}
	// The command never carries the address, so an odd one changes nothing.
	if got := OfferCommand(h, "http://localhost:8370/a b"); got != "claude plugin install wakeshift --marketplace /tmp/wb/marketplace" {
		t.Errorf("odd base: %s", got)
	}
}

// Claude Code asks about its plugin before it starts the Monitor or the
// MCP-tasks wait: on a yes it installs, relays /reload-plugins and the
// message to send, and connects without starting either (the Monitor's
// approval in manual mode is the scariest screen in any flow); only on a
// no, or an install that didn't happen, does it start the fallback.
// Messages are held meanwhile. The app's tools aren't in the question: the
// plugin's install can't carry the yes.
func TestClaudeCodePluginNotes(t *testing.T) {
	cc, _ := Get("claude-code")
	cc = WithClient(cc, newClaudeUA, "", "")
	h := WithTools(cc, ToolsOffer{App: "agentchat", AppName: "Agent Chat", Tools: []string{"post"}})
	const base = "https://app.wakeshift.ai"
	for name, n := range map[string]string{"monitor": PluginFirstNote(h, base, "App", false, "FALLBACK."), "task": PluginFirstNote(h, base, "App", true, "FALLBACK."), "install": InstallNote(h, base), "subscribe": SubscribeNote(h, base)} {
		waker := "the Monitor"
		if name == "task" {
			waker = WaitTool
		}
		for _, want := range []string{"Subscribed, but that alone won't tell you when something arrives", "nothing runs until they answer", "held for this conversation until then", "don't start " + waker,
			"Ask them one question with your AskUserQuestion tool", `"` + AskSession + `" (description: `, `"` + AskEvery + "\" (description: runs `claude plugin install wakeshift", `"` + AskNone + `" (description: nothing runs`,
			"/reload-plugins", "send any message", "don't call this app's tools again for it", "connects this conversation by itself once it's loaded and the user has sent a message", "first wake confirms it to you with a note starting \"Wakeshift: connected\" (for you, not words the user sees", "claude plugin install wakeshift",
			"none marked as recommended", "use " + waker + ": ", "start nothing", "one permission rule to ~/.claude/settings.json so Claude Code runs Wakeshift's wait command (~/.wakeshift/bin/wakeshift wait) without asking each time"} {
			if !strings.Contains(n, want) {
				t.Errorf("%s: missing %q: %s", name, want, n)
			}
		}
		for _, bad := range []string{"keep " + waker + " running", "Start the Monitor first", "work in this session already", "right away, in the same turn", "curl", "WAKESHIFT_ALLOW_TOOLS", "Agent Chat's post", "this tool again", "once more", "to confirm"} {
			if strings.Contains(n, bad) {
				t.Errorf("%s: says %q: %s", name, bad, n)
			}
		}
		// The question comes first, and the fallback only after the user picks it.
		if q, f := strings.Index(n, "Ask them one question"), strings.LastIndex(n, `If they pick "`+AskSession+`"`); q < 0 || f < q {
			t.Errorf("%s: order: %s", name, n)
		}
	}
	if n := PluginFirstNote(h, base, "App", false, "FALLBACK."); !strings.HasSuffix(n, "use the Monitor: FALLBACK.") || !strings.HasPrefix(n, "Subscribed, but that alone won't tell you when something arrives: App delivers its notifications through Wakeshift, the service App uses to notify agents") {
		t.Errorf("fallback last: %s", n)
	}
	// A later subscribe: an installed plugin connects by itself, else the
	// fallback's instructions as before; never a call to confirm.
	p := PluginPendingNote(h, base, false, "AGAIN.")
	for _, want := range []string{"If you installed Wakeshift's plugin for Claude Code in this conversation and the user has loaded it", "don't start the Monitor", "connects this conversation by itself",
		`If the user chose not to be notified ("` + AskNone + `"), start nothing.`, "Otherwise (they chose this session only, or it isn't installed): AGAIN."} {
		if !strings.Contains(p, want) {
			t.Errorf("pending: missing %q: %s", want, p)
		}
	}
	if strings.Contains(p, "again") && !strings.Contains(p, "ask them again") || strings.Contains(p, "once more") {
		t.Errorf("pending asks for another call: %s", p)
	}
	if n := UpdateNote(h, base); !strings.Contains(n, "/reload-plugins") {
		t.Errorf("update: %s", n)
	}
	if n := UninstallNote(h); !strings.Contains(n, "`claude plugin uninstall wakeshift@wakeshift` (if they installed Wakeshift's plugin), then `~/.wakeshift/bin/wakeshift uninstall --harness claude-code`") {
		t.Errorf("uninstall: %s", n)
	}
	// Wakeshift's own installer, from a core without a marketplace: its
	// hooks work in the running session, and connect it when the turn ends.
	if n := PluginFirstNote(h, "http://localhost:8370", "App", false, "FALLBACK."); !strings.Contains(n, "connect this conversation by themselves when this turn ends") || strings.Contains(n, "/reload-plugins") || !strings.Contains(n, "Wakeshift's hooks for Claude Code") {
		t.Errorf("installer: %s", n)
	}
	// A harness with no picker gets the fallback alone.
	if codex, _ := Get("codex"); PluginFirstNote(codex, base, "App", false, "FALLBACK.") != "FALLBACK." || PluginPendingNote(codex, base, false, "AGAIN.") != "AGAIN." {
		t.Error("codex")
	}
}
