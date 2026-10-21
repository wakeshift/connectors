package connector

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBin = "/Users/ada/.wakeshift/bin/wakeshift"

func installedPath(t *testing.T, name string) (Spec, string) {
	t.Helper()
	spec, _ := Lookup(name)
	return spec, spec.Installer.(hookFiler).path()
}

func mustInstall(t *testing.T, spec Spec) {
	t.Helper()
	if err := spec.Install(testBin, "https://app.wakeshift.ai", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func mustUninstall(t *testing.T, spec Spec) string {
	t.Helper()
	var out bytes.Buffer
	if err := spec.Uninstall(testBin, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// What the install created for a harness that had nothing there, uninstall
// takes away: the file (Antigravity's would be {}, Cursor's
// {"version": 1}), its backups, and the directories it made.
func TestUninstallRemovesWhatInstallCreated(t *testing.T) {
	for name, dirs := range map[string][]string{
		"antigravity": {".gemini/config", ".gemini"},
		"cursor":      {".cursor"},
		"copilot":     {".copilot/hooks", ".copilot"},
		"opencode":    {".config/opencode/plugins", ".config/opencode", ".config"},
	} {
		t.Run(name, func(t *testing.T) {
			home := setHome(t)
			spec, path := installedPath(t, name)
			mustInstall(t, spec)
			if _, err := os.Stat(path); err != nil {
				t.Fatal("not installed")
			}
			mustInstall(t, spec) // twice: still the install's own file
			said := mustUninstall(t, spec)
			if _, err := os.Lstat(path); err == nil {
				b, _ := os.ReadFile(path)
				t.Fatalf("%s left: %s\n%s", path, b, said)
			}
			for _, d := range dirs {
				if _, err := os.Lstat(filepath.Join(home, d)); err == nil {
					l, _ := os.ReadDir(filepath.Join(home, d))
					t.Fatalf("%s left (%d entries)\n%s", d, len(l), said)
				}
			}
			if !strings.Contains(said, "Wakeshift's install created it") {
				t.Fatalf("doesn't say: %s", said)
			}
			// It doesn't point to a backup it just deleted.
			if strings.Contains(said, "before-wakeshift") {
				t.Fatalf("names a deleted backup: %s", said)
			}
			if _, err := os.Stat(filepath.Join(Home(), name, createdFile)); err == nil {
				t.Fatal("the record outlived the uninstall")
			}
		})
	}
}

// A file or directory the user had before installing is never removed,
// however little is left in it.
func TestUninstallKeepsWhatWasThere(t *testing.T) {
	for name, before := range map[string]string{"antigravity": `{}`, "cursor": `{"version": 1}`} {
		t.Run(name, func(t *testing.T) {
			setHome(t)
			spec, path := installedPath(t, name)
			os.MkdirAll(filepath.Dir(path), 0o700)
			os.WriteFile(path, []byte(before), 0o600)
			mustInstall(t, spec)
			mustUninstall(t, spec)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s removed", path)
			}
			if strings.Contains(string(b), "--harness") {
				t.Fatalf("%s", b)
			}
		})
	}
	for _, name := range []string{"copilot", "opencode"} {
		t.Run(name+" dir", func(t *testing.T) {
			setHome(t)
			spec, path := installedPath(t, name)
			os.MkdirAll(filepath.Dir(path), 0o700)
			mustInstall(t, spec)
			mustUninstall(t, spec)
			if _, err := os.Stat(path); err == nil {
				t.Fatal("the file stayed")
			}
			if st, err := os.Stat(filepath.Dir(path)); err != nil || !st.IsDir() {
				t.Fatalf("%s removed", filepath.Dir(path))
			}
		})
	}
}

// A file the install created but the user (or the harness) has since put
// something of theirs in stays, and so do the directories above it.
func TestUninstallKeepsWhatWasAddedSince(t *testing.T) {
	home := setHome(t)
	spec, path := installedPath(t, "antigravity")
	mustInstall(t, spec)
	top, _ := ReadJSONFile(path)
	top.Set("fmt", json.RawMessage(`{"Stop":[{"type":"command","command":"fmt.sh"}]}`))
	WriteJSONFile(path, top)
	mustUninstall(t, spec)
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "fmt.sh") || strings.Contains(string(b), "--harness") {
		t.Fatalf("%s %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config")); err != nil {
		t.Fatal(".gemini/config removed")
	}

	// Cursor: something besides "version": 1 keeps the file.
	spec, path = installedPath(t, "cursor")
	mustInstall(t, spec)
	top, _ = ReadJSONFile(path)
	top.Set("version", json.RawMessage(`2`))
	WriteJSONFile(path, top)
	mustUninstall(t, spec)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed a file with the user's version")
	}

	// Copilot: a file of the user's next to ours keeps the directory.
	spec, path = installedPath(t, "copilot")
	mustInstall(t, spec)
	mine := filepath.Join(filepath.Dir(path), "mine.json")
	os.WriteFile(mine, []byte(`{}`), 0o600)
	mustUninstall(t, spec)
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("removed the user's hooks file")
	}
}

// Only the installer's own file and the directories above it are ever
// candidates, whatever the record says.
func TestUninstallIgnoresOtherRecordedPaths(t *testing.T) {
	home := setHome(t)
	spec, _ := installedPath(t, "cursor")
	mustInstall(t, spec)
	other := filepath.Join(home, "Documents")
	os.MkdirAll(other, 0o700)
	otherFile := filepath.Join(home, "notes.json")
	os.WriteFile(otherFile, []byte(`{}`), 0o600)
	b, _ := json.Marshal([]string{other, otherFile, home})
	os.WriteFile(filepath.Join(Home(), "cursor", createdFile), b, 0o600)
	mustUninstall(t, spec)
	for _, p := range []string{other, otherFile, home} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed", p)
		}
	}
}

// A path that can't be checked (a directory the user can't search) isn't
// counted as missing: it may exist, so it's never recorded as the install's.
func TestMissingPathsOnlyWhatDoesNotExist(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	os.MkdirAll(filepath.Join(locked, "there"), 0o700)
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if l := missingPaths(filepath.Join(locked, "there", "hooks.json")); len(l) != 0 {
		t.Fatalf("counted as missing: %v", l)
	}
	if l := missingPaths(filepath.Join(dir, "new", "hooks.json")); len(l) != 2 {
		t.Fatalf("missing: %v", l)
	}
}

// A backup uninstall can't remove is said, not silently left.
func TestUninstallSaysWhatBackupItLeft(t *testing.T) {
	setHome(t)
	spec, path := installedPath(t, "cursor")
	mustInstall(t, spec)
	if err := spec.Installer.Uninstall(testBin, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	stuck := path + ".before-wakeshift-stuck"
	os.MkdirAll(filepath.Join(stuck, "x"), 0o700) // os.Remove fails on it
	var out bytes.Buffer
	removeCreated(LoadSettings("cursor"), spec.Installer, []string{stuck}, &out)
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("%s left", path)
	}
	if !strings.Contains(out.String(), "Couldn't remove "+stuck) {
		t.Fatalf("said: %s", out.String())
	}
}
