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
		"hermes": "Wakeshift's plugin for Hermes Agent", "cursor": "Wakeshift's package for Cursor",
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
			offers["upgrade"] = PluginFirstNote(h, base, false, "FALLBACK.")
		}
		for name, n := range offers {
			if !strings.Contains(n, "Install "+want+"? It runs `"+install+"`.") {
				t.Errorf("%s %s: no yes/no question quoting `%s`: %s", h.ID, name, install, n)
			}
			if !strings.Contains(n, "mark neither as recommended") && !strings.Contains(n, "don't mark either as recommended") {
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
