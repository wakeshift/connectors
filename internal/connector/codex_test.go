package connector

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexOtherHooks = `{
  "hooks": {
    "PostToolUse": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "notify.sh"}]}
    ],
    "Stop": [{"hooks": [{"type": "command", "command": "notify.sh"}]}]
  }
}`

func TestCodexInstall(t *testing.T) {
	home := setHome(t)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	path := filepath.Join(home, ".codex", "hooks.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(codexOtherHooks), 0o600)
	spec, _ := Lookup("codex")
	installRoundTrip(t, spec, path, []string{`"notify.sh"`, `"Stop"`}, []string{`"matcher": "^mcp__"`, `"SessionStart"`, `"UserPromptSubmit"`})
	if s := LoadSettings("codex"); len(s.Issuers) != 0 {
		t.Fatalf("uninstall left the trust: %+v", s)
	}
	spec.Install("/bin/wakeshift", "https://app.wakeshift.ai/", &bytes.Buffer{})
	if s := LoadSettings("codex"); len(s.Issuers) != 1 || s.Issuers[0] != "https://app.wakeshift.ai" {
		t.Fatalf("settings %+v", s)
	}
}

// A hooks.json Codex can't read either is left alone; with none, one is
// made (a path with a space quoted); a linked one stays linked, keeping its
// permissions.
func TestCodexInstallFiles(t *testing.T) {
	home := setHome(t)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	path := filepath.Join(home, ".codex", "hooks.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("{not json"), 0o600)
	spec, _ := Lookup("codex")
	if err := spec.Install("/bin/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{}); err == nil {
		t.Fatal("installed over invalid JSON")
	}
	if b, _ := os.ReadFile(path); string(b) != "{not json" {
		t.Fatalf("changed: %s", b)
	}
	os.Remove(path)
	if err := spec.Install("/bin/my tools/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var f struct {
		Hooks struct {
			PostToolUse []struct {
				Hooks []struct{ Command string }
			}
		}
	}
	json.Unmarshal(b, &f)
	if got := f.Hooks.PostToolUse[0].Hooks[0].Command; got != "'/bin/my tools/wakeshift' hook --harness codex" {
		t.Fatalf("command %q", got)
	}
	// Uninstall finds it, quoted path and all.
	spec.Uninstall("/elsewhere/wakeshift", &bytes.Buffer{})
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "--harness") {
		t.Fatalf("%s", b)
	}

	real := filepath.Join(t.TempDir(), "hooks.json")
	os.WriteFile(real, []byte(codexOtherHooks), 0o644)
	os.Remove(path)
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if err := spec.Install("/bin/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(path); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("not a link any more: %v", err)
	}
	if st, _ := os.Stat(real); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "hook --harness codex") {
		t.Fatalf("linked file: %s", b)
	}
}

// Only Wakeshift's own commands count as ours.
func TestOurs(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/u/.wakeshift/bin/wakeshift hook --harness cursor":                true,
		"/other/wakeshift hook --harness cursor":                           true,
		"'/my tools/wakeshift' hook --harness cursor --event stop":         true,
		"/u/.wakeshift/bin/wakeshift hook --harness cursorx":               false,
		"/u/.wakeshift/bin/wakeshift hook --harness codex":                 false,
		"/u/bin/notwakeshift hook --harness cursor":                        false,
		"wakeshift-hook --harness cursor":                                  false,
		"echo /u/.wakeshift/bin/wakeshift hook --harness cursor":           false,
		"/u/.wakeshift/bin/wakeshift-codex hook":                           false, // Codex's, not Cursor's
		"/u/.wakeshift/bin/wakeshift hook --harness cursor; rm -rf /tmp/x": false,
	} {
		if got := ours(cmd, "/u/.wakeshift/bin/wakeshift", "cursor"); got != want {
			t.Fatalf("%q: %v", cmd, got)
		}
	}
	if !ours("/u/.wakeshift/bin/wakeshift-codex hook", "/u/.wakeshift/bin/wakeshift", "codex") {
		t.Fatal("the old Codex hook")
	}
}
