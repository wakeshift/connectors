package connector

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installPlugin(t *testing.T, spec Spec, bin, issuer string) string {
	t.Helper()
	var out bytes.Buffer
	if err := spec.InstallPlugin(bin, issuer, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// A plugin install trusts the issuer, stamps installed_at and records the
// plugin's issuer, but writes no hooks: the plugin carries them.
func TestInstallPluginWritesNoHooks(t *testing.T) {
	home := setHome(t)
	for _, name := range []string{"codex", "copilot", "claude-code"} {
		spec, _ := Lookup(name)
		out := installPlugin(t, spec, testBin, "https://app.staging.wakeshift.ai/")
		if strings.Contains(out, "Removed") || !strings.Contains(out, "trusts beacons from https://app.staging.wakeshift.ai") {
			t.Fatalf("%s: %s", name, out)
		}
		set := LoadSettings(name)
		if len(set.Issuers) != 1 || set.Issuers[0] != "https://app.staging.wakeshift.ai" || set.InstalledAt.IsZero() {
			t.Fatalf("%s: settings %+v", name, set)
		}
		if b, err := os.ReadFile(filepath.Join(set.Dir, PluginIssuerFile)); err != nil || string(b) != "https://app.staging.wakeshift.ai\n" {
			t.Fatalf("%s: plugin-issuer %q %v", name, b, err)
		}
		if f, ok := spec.Installer.(hookFiler); ok {
			if b, err := os.ReadFile(f.path()); err == nil && strings.Contains(string(b), "--harness") {
				t.Fatalf("%s: hooks written to %s: %s", name, f.path(), b)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); err == nil {
		t.Fatal("codex: hooks.json made")
	}
	spec, _ := Lookup("codex")
	if err := spec.InstallPlugin(testBin, "http://example.com", &bytes.Buffer{}); err == nil {
		t.Fatal("plain http issuer trusted")
	}
}

// Moving from a curl install to the plugin removes the hooks that install
// wrote, keeping the user's own, so the harness doesn't run both.
func TestInstallPluginRemovesCurlHooks(t *testing.T) {
	home := setHome(t)

	// Codex: its hook comes out of hooks.json; the user's hook stays.
	hooks := filepath.Join(home, ".codex", "hooks.json")
	os.MkdirAll(filepath.Dir(hooks), 0o700)
	os.WriteFile(hooks, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"notify.sh"}]}]}}`), 0o600)
	codex, _ := Lookup("codex")
	install(t, codex, testBin)
	out := installPlugin(t, codex, testBin, "https://app.wakeshift.ai")
	if !strings.Contains(out, "Removed the hooks an earlier Wakeshift install wrote to "+hooks+" (the previous file is at ") {
		t.Fatalf("codex: %s", out)
	}
	if b, _ := os.ReadFile(hooks); strings.Contains(string(b), "--harness") || !strings.Contains(string(b), "notify.sh") {
		t.Fatalf("codex hooks.json: %s", b)
	}
	// Again: nothing more to remove, and nothing said about it.
	if out := installPlugin(t, codex, testBin, "https://app.wakeshift.ai"); strings.Contains(out, "Removed") {
		t.Fatalf("codex twice: %s", out)
	}

	// Copilot: Wakeshift's own hooks file goes.
	copilot, _ := Lookup("copilot")
	install(t, copilot, testBin)
	file := filepath.Join(home, ".copilot", "hooks", "wakeshift.json")
	if out := installPlugin(t, copilot, testBin, "https://app.wakeshift.ai"); !strings.Contains(out, "Removed the hooks an earlier Wakeshift install wrote to "+file+": ") {
		t.Fatalf("copilot: %s", out)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("copilot: hooks file left")
	}
	// A file of that name that isn't Wakeshift's is left alone, with a note.
	os.MkdirAll(filepath.Dir(file), 0o700)
	os.WriteFile(file, []byte(`{"version":1,"hooks":{}}`), 0o600)
	if out := installPlugin(t, copilot, testBin, "https://app.wakeshift.ai"); !strings.Contains(out, "Left the hooks in "+file) {
		t.Fatalf("copilot, not ours: %s", out)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("copilot: the user's file removed")
	}
}

// A file the curl install created, holding nothing but its hooks, goes
// with them (and the backup the removal made of it).
func TestInstallPluginRemovesCreatedFile(t *testing.T) {
	home := setHome(t)
	codex, _ := Lookup("codex")
	install(t, codex, testBin)
	hooks := filepath.Join(home, ".codex", "hooks.json")
	out := installPlugin(t, codex, testBin, "https://app.wakeshift.ai")
	if _, err := os.Stat(hooks); err == nil {
		t.Fatalf("hooks.json left: %s", out)
	}
	if strings.Contains(out, "the previous file is at") || len(backupsOf(hooks)) != 0 {
		t.Fatalf("backup mentioned or left: %s %v", out, backupsOf(hooks))
	}
}

// Claude Code's plugin can't carry the wait command's rule, so the plugin
// install writes it, and keeps it while it takes out a curl install's hooks.
func TestInstallPluginClaudeRule(t *testing.T) {
	home := setHome(t)
	bin := realBin(t)
	spec, _ := Lookup("claude-code")
	settings := filepath.Join(home, ".claude", "settings.json")
	install(t, spec, bin)
	out := installPlugin(t, spec, bin, "https://app.wakeshift.ai")
	b, _ := os.ReadFile(settings)
	if strings.Contains(string(b), "hook --harness") || !strings.Contains(string(b), "Bash("+bin+" wait)") {
		t.Fatalf("settings: %s\n%s", b, out)
	}
	// Uninstalling takes the rule and the harness's directory away.
	uninstall(t, spec, bin)
	if b, _ := os.ReadFile(settings); strings.Contains(string(b), bin) {
		t.Fatalf("after uninstall: %s", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".wakeshift", "claude-code", PluginIssuerFile)); err == nil {
		t.Fatal("plugin-issuer left")
	}
}

// An install from npx or the script over Wakeshift's plugin (Cursor's from
// its Marketplace) forgets the plugin's issuer, so the plugin's launcher sets
// it up again on its next hook, which takes the new hooks out again: the
// harness never keeps running both.
func TestInstallOverPlugin(t *testing.T) {
	home := setHome(t)
	spec, _ := Lookup("cursor")
	installPlugin(t, spec, testBin, "https://app.wakeshift.ai")
	stamp := filepath.Join(LoadSettings("cursor").Dir, PluginIssuerFile)
	if _, err := os.Stat(stamp); err != nil {
		t.Fatal(err)
	}
	install(t, spec, testBin)
	if _, err := os.Stat(stamp); err == nil {
		t.Fatal("plugin-issuer kept: the plugin's launcher wouldn't take the hooks out")
	}
	hooks := filepath.Join(home, ".cursor", "hooks.json")
	if b, _ := os.ReadFile(hooks); !strings.Contains(string(b), "--harness cursor") {
		t.Fatalf("hooks.json: %s", b)
	}
	// What the launcher then runs.
	if out := installPlugin(t, spec, testBin, "https://app.wakeshift.ai"); !strings.Contains(out, "Removed the hooks an earlier Wakeshift install wrote to "+hooks) {
		t.Fatalf("plugin again: %s", out)
	}
	if b, _ := os.ReadFile(hooks); strings.Contains(string(b), "--harness cursor") {
		t.Fatalf("hooks.json after the plugin: %s", b)
	}
	// Without a plugin, the install works as ever.
	install(t, spec, testBin)
}
