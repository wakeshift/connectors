package harnesses

import (
	"strings"
	"testing"
)

// With a version set, opencode's offers give opencode's own plugin command
// with the npm package at that version, and say what it changes; without
// one, Wakeshift's installer as before.
func TestPackageOffer(t *testing.T) {
	t.Cleanup(func() { SetPackageVersion("opencode", "") })
	h, _ := Get("opencode")
	base := "https://app.wakeshift.ai"
	if c := OfferCommand(h, base); !strings.HasPrefix(c, "curl -fsSL ") {
		t.Fatalf("no version set: offer %q, want the installer", c)
	}

	SetPackageVersion("opencode", "1.5.0")
	want := "opencode plugin @wakeshift/opencode@1.5.0 --global --force"
	if c := OfferCommand(h, base); c != want {
		t.Errorf("offer %q, want %q", c, want)
	}
	if c := UpdateCommand(h, base); c != want {
		t.Errorf("update %q, want %q", c, want)
	}
	n := InstallNote(h, base)
	for _, s := range []string{"`" + want + "`", "the npm package @wakeshift/opencode", "opencode's own plugin command", "Quit and restart opencode"} {
		if !strings.Contains(n, s) {
			t.Errorf("install note doesn't say %q: %s", s, n)
		}
	}
	if strings.Contains(n, "writes one plugin file") {
		t.Errorf("install note says what the installer changes: %s", n)
	}
	if n := RepeatNote(h, base); !strings.Contains(n, "the npm package @wakeshift/opencode") || !strings.Contains(n, want) {
		t.Errorf("repeat note: %s", n)
	}
	u := UpdateNote(h, base)
	for _, s := range []string{"`" + want + "`", "the new version of Wakeshift's plugin, the npm package @wakeshift/opencode", "so it loads the new version"} {
		if !strings.Contains(u, s) {
			t.Errorf("update note doesn't say %q: %s", s, u)
		}
	}
	if r := UpdateRequired(h, base); !strings.Contains(r, want) {
		t.Errorf("update required: %s", r)
	}

	// Without a version set for theirs, the others' offers are unchanged.
	for _, id := range []string{"codex", "cursor", "copilot", "antigravity", "claude-code", "hermes", "openclaw"} {
		o, _ := Get(id)
		if PackageSpec(o) != "" || strings.Contains(OfferCommand(o, base), "opencode plugin") {
			t.Errorf("%s offers a package", id)
		}
	}

	// A version that isn't one (it would go into a shell command) is refused.
	for _, bad := range []string{"1.5", "1.5.0; rm -rf ~", "latest", "01.2.3", "1.2.3+build", "$(id)"} {
		SetPackageVersion("opencode", bad)
		if c := OfferCommand(h, base); !strings.HasPrefix(c, "curl -fsSL ") {
			t.Errorf("version %q: offer %q, want the installer", bad, c)
		}
	}
	SetPackageVersion("opencode", "1.6.0-staging.3")
	if c := OfferCommand(h, base); c != "opencode plugin @wakeshift/opencode@1.6.0-staging.3 --global --force" {
		t.Errorf("prerelease: offer %q", c)
	}
	SetPackageVersion("opencode", "")
	if c := OfferCommand(h, base); !strings.HasPrefix(c, "curl -fsSL ") {
		t.Errorf("cleared: offer %q, want the installer", c)
	}
}

// opencode can't remove a plugin itself: the uninstall note says how,
// for either way it was installed.
func TestPackageUninstallNote(t *testing.T) {
	h, _ := Get("opencode")
	n := UninstallNote(h)
	for _, s := range []string{`"@wakeshift/opencode@…"`, `"plugin" list in ~/.config/opencode/opencode.json`, "wakeshift uninstall --harness opencode", "restart opencode"} {
		if !strings.Contains(n, s) {
			t.Errorf("uninstall note doesn't say %q: %s", s, n)
		}
	}
	c, _ := Get("cursor")
	if n := UninstallNote(c); !strings.Contains(n, "wakeshift uninstall --harness cursor") || strings.Contains(n, "opencode") {
		t.Errorf("cursor's uninstall note: %s", n)
	}
}

// Once a published version of @wakeshift/connect is known, Cursor's offers
// run it with npx at that version, say so, and pass an app's tools the user
// agreed to; without one, Wakeshift's installer as before.
func TestConnectOffer(t *testing.T) {
	t.Cleanup(func() { SetPackageVersion("cursor", "") })
	h, _ := Get("cursor")
	base := "https://app.wakeshift.ai"
	if c := OfferCommand(h, base); !strings.HasPrefix(c, "curl -fsSL ") {
		t.Fatalf("no version set: offer %q, want the installer", c)
	}

	SetPackageVersion("cursor", "1.5.0")
	want := "npx -y @wakeshift/connect@1.5.0 cursor"
	if c := OfferCommand(h, base); c != want {
		t.Errorf("offer %q, want %q", c, want)
	}
	if c := UpdateCommand(h, base); c != want {
		t.Errorf("update %q, want %q", c, want)
	}
	n := BackgroundUpgradeNote(h, base)
	for _, s := range []string{"`" + want + "`", "adds three hooks to ~/.cursor/hooks.json, keeping a backup, with Wakeshift's npm package @wakeshift/connect", "Restart cursor-agent"} {
		if !strings.Contains(n, s) {
			t.Errorf("upgrade note doesn't say %q: %s", s, n)
		}
	}
	u := UpdateNote(h, base)
	for _, s := range []string{"`" + want + "`", "Wakeshift's npm package @wakeshift/connect at its new version", "the hooks run the new version"} {
		if !strings.Contains(u, s) {
			t.Errorf("update note doesn't say %q: %s", s, u)
		}
	}
	if strings.Contains(u, "own plugin command") {
		t.Errorf("update note calls npx Cursor's plugin command: %s", u)
	}
	if r := UpdateRequired(h, base); !strings.Contains(r, want) {
		t.Errorf("update required: %s", r)
	}
	if n := UninstallNote(h); !strings.Contains(n, "wakeshift uninstall --harness cursor") || strings.Contains(n, "opencode") {
		t.Errorf("uninstall note: %s", n)
	}

	// An app's tools: named in what it changes, and passed to the installer
	// the package runs.
	w := WithTools(h, ToolsOffer{App: "agentchat", AppName: "Agent Chat", Tools: []string{"post"}})
	n = BackgroundUpgradeNote(w, base)
	if !strings.Contains(n, "with Wakeshift's npm package @wakeshift/connect, which also puts Wakeshift's connector in ~/.wakeshift/bin; it also lets Cursor run Agent Chat's post tool without asking each time (no other tools)") ||
		!strings.Contains(n, "It runs `WAKESHIFT_ALLOW_TOOLS=agentchat:post "+want+"`") {
		t.Errorf("with tools: %s", n)
	}

	// Only Cursor offers @wakeshift/connect; opencode keeps its own offer.
	o, _ := Get("opencode")
	if PackageSpec(o) != "" || strings.Contains(OfferCommand(o, base), "npx") {
		t.Errorf("opencode offers %q", OfferCommand(o, base))
	}
	SetPackageVersion("cursor", "")
	if c := OfferCommand(h, base); !strings.HasPrefix(c, "curl -fsSL ") {
		t.Errorf("cleared: offer %q, want the installer", c)
	}
}
