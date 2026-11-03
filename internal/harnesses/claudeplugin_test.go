package harnesses

import (
	"strings"
	"testing"
)

// Claude Code's offers give its own plugin command, from Wakeshift's
// marketplace (its staging branch for staging), always setting the
// plugin's issuer; the update refreshes the marketplace first, after an
// install that moves a user of Wakeshift's installer over.
func TestClaudeCodeMarketplace(t *testing.T) {
	h, _ := Get("claude-code")
	for base, want := range map[string]string{
		"https://app.wakeshift.ai/":        "claude plugin install wakeshift --marketplace wakeshift/agent-plugins --config issuer=https://app.wakeshift.ai",
		"https://app.staging.wakeshift.ai": "claude plugin install wakeshift --marketplace wakeshift/agent-plugins#staging --config issuer=https://app.staging.wakeshift.ai",
	} {
		if got := OfferCommand(h, base); got != want {
			t.Errorf("%s: %s", base, got)
		}
		if got := UpdateCommand(h, base); got != want+" && claude plugin marketplace update wakeshift && claude plugin update wakeshift@wakeshift" {
			t.Errorf("%s update: %s", base, got)
		}
	}
	// A local core: Wakeshift's installer, unless a marketplace is set for it.
	if got := OfferCommand(h, "http://localhost:8370"); !strings.HasPrefix(got, "curl ") {
		t.Errorf("local: %s", got)
	}
	SetAgentPlugins("/tmp/wb/marketplace")
	defer SetAgentPlugins("")
	if got := OfferCommand(h, "http://localhost:8370"); got != "claude plugin install wakeshift --marketplace /tmp/wb/marketplace --config issuer=http://localhost:8370" {
		t.Errorf("configured: %s", got)
	}
	// An address that can't go into the command as it is: the installer.
	if got := OfferCommand(h, "http://localhost:8370/a b"); strings.Contains(got, "claude plugin") {
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
	h := WithTools(cc, ToolsOffer{App: "agentchat", AppName: "Agent Chat", Tools: []string{"post"}})
	const base = "https://app.wakeshift.ai"
	for name, n := range map[string]string{"monitor": PluginFirstNote(h, base, false, "FALLBACK."), "task": PluginFirstNote(h, base, true, "FALLBACK."), "install": InstallNote(h, base), "subscribe": SubscribeNote(h, base)} {
		waker := "the Monitor"
		if name == "task" {
			waker = WaitTool
		}
		for _, want := range []string{"Ask them one question before anything runs", "held for this conversation until then", "don't start " + waker,
			"/reload-plugins", "send any message", "Once they say they've loaded it, call this tool again", "call this tool once more, to confirm", "claude plugin install wakeshift",
			"\"Yes, install Wakeshift's plugin for Claude Code\"", "mark neither as recommended", "use " + waker + " instead"} {
			if !strings.Contains(n, want) {
				t.Errorf("%s: missing %q: %s", name, want, n)
			}
		}
		for _, bad := range []string{"keep " + waker + " running", "Start the Monitor first", "work in this session already", "right away, in the same turn", "curl", "WAKESHIFT_ALLOW_TOOLS", "Agent Chat's post"} {
			if strings.Contains(n, bad) {
				t.Errorf("%s: says %q: %s", name, bad, n)
			}
		}
		// The question comes first, and the fallback only after a no.
		if q, f := strings.Index(n, "Ask them one question"), strings.LastIndex(n, "If they pick no"); q < 0 || f < q {
			t.Errorf("%s: order: %s", name, n)
		}
	}
	if n := PluginFirstNote(h, base, false, "FALLBACK."); !strings.HasSuffix(n, "use the Monitor instead: FALLBACK.") {
		t.Errorf("fallback last: %s", n)
	}
	// A later result: this one connects an installed plugin, else the
	// fallback's instructions as before.
	p := PluginPendingNote(h, base, false, "AGAIN.")
	for _, want := range []string{"If you installed Wakeshift's plugin for Claude Code in this conversation and the user has loaded it", "don't start the Monitor", "connects this conversation from this result",
		"call this tool once more, to confirm", "Otherwise (the user said no, or it isn't installed): AGAIN."} {
		if !strings.Contains(p, want) {
			t.Errorf("pending: missing %q: %s", want, p)
		}
	}
	if n := UpdateNote(h, base); !strings.Contains(n, "/reload-plugins") {
		t.Errorf("update: %s", n)
	}
	if n := UninstallNote(h); !strings.Contains(n, "`claude plugin uninstall wakeshift@wakeshift` (if they installed Wakeshift's plugin), then `~/.wakeshift/bin/wakeshift uninstall --harness claude-code`") {
		t.Errorf("uninstall: %s", n)
	}
	// Wakeshift's own installer, from a core without a marketplace: its
	// hooks work in the running session, so the agent calls again at once.
	if n := PluginFirstNote(h, "http://localhost:8370", false, "FALLBACK."); !strings.Contains(n, "work in this session already") || strings.Contains(n, "/reload-plugins") || !strings.Contains(n, "Wakeshift's hooks for Claude Code") {
		t.Errorf("installer: %s", n)
	}
	// A harness with no picker gets the fallback alone.
	if codex, _ := Get("codex"); PluginFirstNote(codex, base, false, "FALLBACK.") != "FALLBACK." || PluginPendingNote(codex, base, false, "AGAIN.") != "AGAIN." {
		t.Error("codex")
	}
}
