package connector

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/wakeshift-plugins/internal/version"
	"github.com/clawvisor/wakeshift-plugins/plugins/hermes"
)

// Installing for Hermes writes Wakeshift's plugin into $HERMES_HOME/plugins,
// pins the issuer in its config.json and enables it with Hermes's own
// command; uninstalling disables and removes it and its state, and neither
// touches a "wakeshift" plugin that isn't Wakeshift's.
func TestHermesInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	var ran [][]string
	old := HermesCommand
	HermesCommand = func(out io.Writer, args ...string) error { ran = append(ran, args); return nil }
	t.Cleanup(func() { HermesCommand = old })

	spec, ok := Lookup("hermes")
	if !ok || !spec.Unbound {
		t.Fatalf("spec: %+v", spec)
	}
	var out strings.Builder
	if err := spec.Install("/bin/wakeshift", "https://app.wakeshift.test/", &out); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "plugins", "wakeshift")
	for _, f := range []string{"__init__.py", "plugin.py", "stream.py", "wire.py", "plugin.yaml"} {
		want, _ := hermes.Files.ReadFile("wakeshift/" + f)
		if got, err := os.ReadFile(filepath.Join(dir, f)); err != nil || string(got) != string(want) || len(want) == 0 {
			t.Fatalf("%s: %v", f, err)
		}
	}
	// Its version, which it names in its requests: this binary's.
	if got, _ := os.ReadFile(filepath.Join(dir, hermes.VersionFileName)); string(got) != string(hermes.VersionFile(version.Version)) ||
		!strings.Contains(string(got), `VERSION = "`+version.Version+`"`) {
		t.Fatalf("%s: %q", hermes.VersionFileName, got)
	}
	var cfg struct{ Issuers []string }
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if json.Unmarshal(b, &cfg) != nil || len(cfg.Issuers) != 1 || cfg.Issuers[0] != "https://app.wakeshift.test" {
		t.Fatalf("config: %s", b)
	}
	if len(ran) != 1 || strings.Join(ran[0], " ") != "plugins enable wakeshift" || !strings.Contains(out.String(), "Enabled it in Hermes") {
		t.Fatalf("ran %v: %s", ran, out.String())
	}
	// Again: the same files, no error.
	if err := spec.Install("/bin/wakeshift", "https://app.wakeshift.test", io.Discard); err != nil {
		t.Fatal(err)
	}
	// Its state, as the plugin keeps it.
	state := filepath.Join(home, "wakeshift", "links")
	os.MkdirAll(state, 0o700)
	os.WriteFile(filepath.Join(state, "x.json"), []byte(`{}`), 0o600)

	ran = nil
	if err := spec.Uninstall("/bin/wakeshift", &out); err != nil {
		t.Fatal(err)
	}
	if exists(dir) || exists(filepath.Join(home, "wakeshift")) || len(ran) != 1 || strings.Join(ran[0], " ") != "plugins disable wakeshift" {
		t.Fatalf("after uninstall: %v %v", ran, out.String())
	}
	if !exists(filepath.Join(home, "plugins")) {
		t.Fatal("removed Hermes's plugins directory")
	}

	// Without the plugin, its state (if any) is left alone: it may be
	// another Hermes's, or a plugin installed some other way.
	os.MkdirAll(state, 0o700)
	if err := spec.Uninstall("/bin/wakeshift", io.Discard); err != nil || !exists(state) {
		t.Fatalf("uninstall without the plugin: %v, state kept %v", err, exists(state))
	}
	os.RemoveAll(filepath.Join(home, "wakeshift"))

	// Someone else's "wakeshift" plugin is left alone.
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "__init__.py"), []byte("def register(ctx): pass\n"), 0o644)
	if err := spec.Install("/bin/wakeshift", "https://app.wakeshift.test", io.Discard); err == nil || !strings.Contains(err.Error(), "isn't Wakeshift's") {
		t.Fatalf("install over another plugin: %v", err)
	}
	if err := spec.Uninstall("/bin/wakeshift", io.Discard); err == nil {
		t.Fatal("uninstalled another plugin")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "__init__.py")); string(b) != "def register(ctx): pass\n" {
		t.Fatal("changed another plugin")
	}
}

// A reinstall replaces the plugin's code but keeps its config.json: the
// user's values stay, and only an issuer trusted now is added.
func TestHermesReinstallKeepsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	old := HermesCommand
	HermesCommand = func(io.Writer, ...string) error { return nil }
	t.Cleanup(func() { HermesCommand = old })
	spec, _ := Lookup("hermes")
	if err := spec.Install("/bin/wakeshift", "https://a.wakeshift.test", io.Discard); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "plugins", "wakeshift")
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"issuers": ["https://a.wakeshift.test", "https://mine.test"], "note": "mine"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "plugin.py"), []byte("# an old version\n"), 0o644)

	var out strings.Builder
	if err := spec.Install("/bin/wakeshift", "https://b.wakeshift.test", &out); err != nil {
		t.Fatal(err)
	}
	want, _ := hermes.Files.ReadFile("wakeshift/plugin.py")
	if got, _ := os.ReadFile(filepath.Join(dir, "plugin.py")); string(got) != string(want) {
		t.Fatal("the code wasn't replaced")
	}
	var cfg struct {
		Issuers []string
		Note    string
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	json.Unmarshal(b, &cfg)
	if strings.Join(cfg.Issuers, " ") != "https://a.wakeshift.test https://mine.test https://b.wakeshift.test" || cfg.Note != "mine" {
		t.Fatalf("config: %s", b)
	}
	if !strings.Contains(out.String(), "added https://b.wakeshift.test to the Wakeshifts it trusts") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := spec.Install("/bin/wakeshift", "https://b.wakeshift.test", &out); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "config.json")); string(again) != string(b) || !strings.Contains(out.String(), "as they were") {
		t.Fatalf("changed on a plain reinstall: %s %s", again, out.String())
	}
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`not json`), 0o644)
	if err := spec.Install("/bin/wakeshift", "https://b.wakeshift.test", io.Discard); err == nil {
		t.Fatal("overwrote a config.json it couldn't read")
	}
}

// Without hermes on PATH (Hermes in a container, the installer outside it)
// the plugin is written and the user is told how to enable it.
func TestHermesInstallWithoutHermes(t *testing.T) {
	t.Setenv("HERMES_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	spec, _ := Lookup("hermes")
	var out strings.Builder
	if err := spec.Install("/bin/wakeshift", "https://app.wakeshift.test", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "run `hermes plugins enable wakeshift`") {
		t.Fatal(out.String())
	}
}

// The plugin's "connected" note says what the spec (and the core's install
// note) promise.
func TestHermesPluginNote(t *testing.T) {
	spec, _ := Lookup("hermes")
	b, _ := hermes.Files.ReadFile("wakeshift/plugin.py")
	if !strings.Contains(string(b), `WHEN = "`+spec.When+`"`) || !strings.Contains(string(b), `"Wakeshift: connected. New messages for this conversation will reach it "`) {
		t.Fatal("the plugin's note differs from the spec's When")
	}
	init, _ := hermes.Files.ReadFile("wakeshift/__init__.py")
	if !strings.Contains(string(init), hermes.Marker) {
		t.Fatal("no marker")
	}
}
