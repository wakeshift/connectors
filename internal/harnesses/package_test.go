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

	// Only opencode has a package: the others' offers are unchanged.
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
