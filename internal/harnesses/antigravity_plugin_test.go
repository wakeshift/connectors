package harnesses

import (
	"strings"
	"testing"
)

// Antigravity CLI is offered agy's own plugin installer, from the plugin's
// own repository: production's Wakeshift its default branch, staging's its
// staging branch. Install and update are the same command. Any other
// Wakeshift has no published plugin, so it offers its installer.
func TestAntigravityPluginOffer(t *testing.T) {
	h, _ := Get("antigravity")
	const prod = "agy plugin install https://github.com/wakeshift/antigravity-plugin"
	for base, want := range map[string]string{
		"https://app.wakeshift.ai":         prod,
		"https://app.wakeshift.ai/":        prod,
		"https://app.staging.wakeshift.ai": prod + "/tree/staging",
		"http://localhost:8370":            "curl -fsSL http://localhost:8370/install/antigravity.sh | sh",
		"https://wakeshift.example.com":    "curl -fsSL https://wakeshift.example.com/install/antigravity.sh | sh",
	} {
		if got := OfferCommand(h, base); got != want {
			t.Errorf("%s: offer %q, want %q", base, got, want)
		}
		if got := UpdateCommand(h, base); got != want {
			t.Errorf("%s: update %q, want %q", base, got, want)
		}
	}

	// What the user is told names agy's installer and the repository, and
	// that a curl install's entry comes out.
	for name, n := range map[string]string{
		"subscribe":  SubscribeNote(h, "https://app.wakeshift.ai"),
		"repeat":     RepeatNote(h, "https://app.wakeshift.ai"),
		"background": BackgroundUpgradeNote(h, "https://app.wakeshift.ai"),
	} {
		for _, want := range []string{"`" + prod + "`", "agy's own plugin installer, from github.com/wakeshift/antigravity-plugin,", "SHA-256", "~/.gemini/config/hooks.json"} {
			if !strings.Contains(n, want) {
				t.Errorf("%s lacks %q: %s", name, want, n)
			}
		}
		if strings.Contains(n, "curl") {
			t.Errorf("%s offers curl: %s", name, n)
		}
	}
	if n := UpdateNote(h, "https://app.wakeshift.ai"); !strings.Contains(n, "updates Wakeshift's plugin for Antigravity CLI from github.com/wakeshift/antigravity-plugin") || !strings.Contains(n, "`"+prod+"`") {
		t.Errorf("update: %s", n)
	}
	// Elsewhere, the installer's own text.
	if n := SubscribeNote(h, "http://localhost:8370"); !strings.Contains(n, h.Changes) || strings.Contains(n, "agy plugin") {
		t.Errorf("local: %s", n)
	}
	if n := UninstallNote(h); !strings.Contains(n, "`agy plugin uninstall wakeshift` (if they installed Wakeshift's plugin)") || !strings.Contains(n, "wakeshift uninstall --harness antigravity") {
		t.Errorf("uninstall: %s", n)
	}
	// Codex's marketplace override doesn't reach it.
	SetAgentPlugins("/tmp/agent-plugins")
	t.Cleanup(func() { SetAgentPlugins("") })
	if got := OfferCommand(h, "http://localhost:8370"); strings.Contains(got, "agy plugin") {
		t.Errorf("agent-plugins override: %s", got)
	}
}

// SetPluginRepo points a Wakeshift whose plugin isn't published (the
// bench's) at a local checkout, takes only what can go into a shell command
// as it is, and never changes production's or staging's offer.
func TestSetPluginRepo(t *testing.T) {
	h, _ := Get("antigravity")
	t.Cleanup(func() { SetPluginRepo("antigravity", "") })
	SetPluginRepo("antigravity", "/tmp/run/antigravity-plugin")
	if got := OfferCommand(h, "http://localhost:8370"); got != "agy plugin install /tmp/run/antigravity-plugin" {
		t.Fatalf("local source: %s", got)
	}
	if got := OfferCommand(h, "https://app.wakeshift.ai"); got != "agy plugin install https://github.com/wakeshift/antigravity-plugin" {
		t.Fatalf("production: %s", got)
	}
	// Another harness isn't affected.
	if c, _ := Get("cursor"); strings.Contains(OfferCommand(c, "http://localhost:8370"), "plugin install") {
		t.Fatal("cursor changed")
	}
	for _, bad := range []string{"relative/path", "/tmp/x; rm -rf ~", "/tmp/$(id)", "https://example.com/x/y", "https://github.com/a/b c", "/tmp/../etc", "file:///tmp/x"} {
		if ValidPluginRepo(bad) {
			t.Errorf("%q valid", bad)
		}
		SetPluginRepo("antigravity", bad)
		if got := OfferCommand(h, "http://localhost:8370"); strings.Contains(got, "agy plugin") {
			t.Errorf("%q taken: %s", bad, got)
		}
	}
	if !ValidPluginRepo("https://github.com/someone/fork/tree/my-branch") {
		t.Fatal("a fork's branch")
	}
	SetPluginRepo("antigravity", "")
	if got := OfferCommand(h, "http://localhost:8370"); strings.Contains(got, "agy plugin") {
		t.Fatalf("reset: %s", got)
	}
}
