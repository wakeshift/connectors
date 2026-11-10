package harnesses

import (
	"strings"
	"testing"
)

// Every install offer asks a real yes/no question with the exact command in
// it, presents its options neutrally, names what it installs as it is (a
// plugin, Cursor's package, or hooks), and on a yes runs the command as
// given (wakebench, November 2, 2026: Codex's question left the command
// out, OpenClaw's described it, Copilot's only offered to give it, two
// models marked "No" as recommended, and every offer said "hooks" for a
// plugin).
func TestOfferAsksWithTheCommand(t *testing.T) {
	SetPackageVersion("opencode", "1.2.3")
	SetPackageVersion("openclaw", "1.2.3")
	SetPackageVersion("cursor", "1.2.3")
	t.Cleanup(func() {
		SetPackageVersion("opencode", "")
		SetPackageVersion("openclaw", "")
		SetPackageVersion("cursor", "")
	})
	const base = "https://app.wakeshift.ai"
	kits := map[string]string{
		"claude-code": "Wakeshift's plugin for Claude Code", "codex": "Wakeshift's plugin for Codex", "copilot": "Wakeshift's plugin for GitHub Copilot CLI",
		"antigravity": "Wakeshift's plugin for Antigravity CLI", "opencode": "Wakeshift's plugin for opencode", "openclaw": "Wakeshift's plugin for OpenClaw",
		"hermes": "Wakeshift's plugin for Hermes Agent", "cursor": "Wakeshift's package for Cursor", "pi": "Wakeshift's extension for pi",
	}
	for _, h := range All() {
		if !h.Installable || h.ReplacedBy != "" {
			continue
		}
		want, ok := kits[h.ID]
		if !ok {
			t.Fatalf("%s: no expected kit", h.ID)
		}
		if got := KitFor(h, base); got != want {
			t.Errorf("%s: kit %q, want %q", h.ID, got, want)
		}
		install := agreed(Route(h, base), OfferCommand(h, base))
		offers := map[string]string{"subscribe": SubscribeNote(h, base), "repeat": RepeatNote(h, base)}
		if h.ZeroInstall == Background {
			offers["background"] = BackgroundUpgradeNote(h, base)
		}
		if h.ZeroInstall == Monitor {
			offers["upgrade"] = PluginFirstNote(h, base, "App", false, "FALLBACK.")
		}
		for name, n := range offers {
			// Claude Code's plugin-first question offers the install as one of three
			// ways to be notified, the exact command in that option.
			threeWay := strings.Contains(n, `"`+AskEvery+"\" (description: runs `"+install+"`")
			if !strings.Contains(n, "Install "+want+"? It runs `"+install+"`.") && !threeWay {
				t.Errorf("%s %s: no yes/no question quoting `%s`: %s", h.ID, name, install, n)
			}
			if !strings.Contains(n, "mark neither as recommended") && !strings.Contains(n, "don't mark either as recommended") && !strings.Contains(n, "none marked as recommended") {
				t.Errorf("%s %s: options not neutral: %s", h.ID, name, n)
			}
			if !strings.Contains(n, "don't read docs or search for it first") && !strings.Contains(n, "don't look anything up first") {
				t.Errorf("%s %s: may look the command up first: %s", h.ID, name, n)
			}
			for _, bad := range []string{"Optional:", "Say if you want", "Mention it to the user", "hooks for " + h.Name} {
				if strings.Contains(n, bad) && !strings.Contains(want, "hooks") {
					t.Errorf("%s %s: says %q: %s", h.ID, name, bad, n)
				}
			}
		}
	}
	// A plain hooks install keeps saying hooks (Wakeshift's installer, where
	// no plugin route or package is offered).
	SetPackageVersion("cursor", "")
	c, _ := Get("cursor")
	if got := KitFor(c, base); got != "Wakeshift's hooks for Cursor" {
		t.Errorf("cursor's installer: %s", got)
	}
	if n := RepeatNote(c, base); !strings.Contains(n, "If Wakeshift's hooks for Cursor are installed, they connect") {
		t.Errorf("plural: %s", n)
	}
}

// Where nothing else wakes the conversation, a "no" is relayed as it is:
// messages wait for the agent's next call to the app (wakebench, November
// 2, 2026: opencode's agent said it would "keep polling").
func TestOfferDeclined(t *testing.T) {
	for _, id := range []string{"opencode", "openclaw", "hermes", "codex"} {
		h, _ := Get(id)
		if n := SubscribeNote(h, "https://app.wakeshift.ai"); !strings.Contains(n, declined) {
			t.Errorf("%s: %s", id, n)
		}
	}
	for _, id := range []string{"claude-code", "cursor", "copilot", "antigravity"} {
		h, _ := Get(id)
		if n := SubscribeNote(h, "https://app.wakeshift.ai"); strings.Contains(n, declined) {
			t.Errorf("%s: says nothing wakes it: %s", id, n)
		}
	}
}

// Hermes: the user is shown `hermes …` (with the Docker form), the agent
// runs its fallback only where hermes isn't on its PATH, the installer's
// unreviewed-source warning is relayed in one line, and no restart is asked
// for when the install says the running gateway loaded the plugin.
func TestHermesText(t *testing.T) {
	h, _ := Get("hermes")
	n := SubscribeNote(h, "https://app.staging.wakeshift.ai")
	for _, want := range []string{
		"quote the command as it is, starting with `hermes`", "docker exec -u hermes <container>",
		"first check `command -v hermes`", "`\"$HERMES_PYTHON\" -m hermes_cli.main`",
		"every command you give the user starts with `hermes`, so `hermes chat --continue`",
		"custom (unreviewed) source", "in one line",
		"Gateway reloaded plugins", "skip the restart and resume below",
		// The gateway's reload doesn't reach a running hermes chat, so a
		// CLI or TUI chat still resumes first (QA pass, November 9, 2026:
		// a classic chat was told only to send a message).
		"for a chat through the gateway (Hermes's API server, or a Telegram or Discord direct message)",
		"if this chat is in Hermes's CLI or TUI, still give them the resume below, then a message",
		"in the classic `hermes chat`, exit and resume this chat with the command Hermes prints as it exits (`hermes --resume <session>`)", "in Hermes's TUI, exit and run `hermes chat --tui --continue`",
		"`hermes config set plugins.entries.wakeshift.settings.issuers '[\"https://app.staging.wakeshift.ai\"]' && hermes plugins install wakeshift/hermes-plugin --enable && ",
	} {
		if !strings.Contains(n, want) {
			t.Errorf("missing %q: %s", want, n)
		}
	}
	if strings.Contains(n, "$HERMES_PYTHON\" -m hermes_cli.main chat") {
		t.Errorf("a resume step in the fallback form: %s", n)
	}
}

// Codex: the offer says the install is Wakeshift's, not the app's, and that
// a blocked command is asked to run outside the sandbox, never only printed
// (QA pass, November 9, 2026: the agent said "The Webhooks tool requires
// this one-time setup", and printed the command when the sandbox blocked
// it). Other harnesses' offers don't change.
func TestCodexOfferCreditsWakeshiftAndEscalates(t *testing.T) {
	codex, _ := Get("codex")
	for _, base := range []string{ProductionIssuer, "http://localhost:8370"} {
		n := SubscribeNote(codex, base)
		for _, want := range []string{
			"The install is Wakeshift's, not this app's: when you ask, say it's for Wakeshift",
			"run it with escalated permissions, so the user approves it",
			"If the sandbox blocks it anyway, ask to run it outside the sandbox rather than only printing the command",
		} {
			if !strings.Contains(n, want) {
				t.Errorf("%s: missing %q: %s", base, want, n)
			}
		}
	}
	for _, h := range all {
		if h.ID != "codex" && strings.Contains(SubscribeNote(h, ProductionIssuer), "The install is Wakeshift's") {
			t.Errorf("%s's offer changed too", h.ID)
		}
	}
}

// Claude Code: the session-only answer is described in the user's terms,
// without the wait tool's name (QA pass, November 9, 2026), and the first
// wake's note isn't given as words the user will see.
func TestClaudeCodeOfferInUserTerms(t *testing.T) {
	claude, _ := Get("claude-code")
	for _, tasks := range []bool{true, false} {
		n := PluginFirstNote(claude, ProductionIssuer, "Webhooks", tasks, "FALLBACK")
		desc := n[strings.Index(n, `"`+AskSession+`" (description: `):]
		desc = desc[:strings.Index(desc, ")")]
		if strings.Contains(desc, WaitTool) || strings.Contains(desc, "wakeshift_") {
			t.Errorf("tasks=%v: the session-only answer names a tool: %s", tasks, desc)
		}
		if !strings.Contains(n, `note starting "Wakeshift: connected" (for you, not words the user sees: don't quote it to them)`) || strings.Contains(n, `first wake starts with "Wakeshift: connected"`) {
			t.Errorf("tasks=%v: the first wake is predicted as the user's words: %s", tasks, n)
		}
	}
}
