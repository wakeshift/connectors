package connector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// realConfigs are files of the person running the tests that no test may
// change: each harness's own settings and Wakeshift's connector.
var realConfigs = []string{
	".claude/settings.json", ".claude/plugins/known_marketplaces.json", ".claude/plugins/installed_plugins.json",
	".codex/config.toml", ".codex/hooks.json",
	".copilot/settings.json", ".copilot/config.json",
	".cursor/hooks.json", ".cursor/cli-config.json",
	".config/opencode/opencode.json", ".pi/agent/settings.json",
	".gemini/config/hooks.json", ".hermes/config.yaml",
	".wakeshift/bin/wakeshift",
}

// realConfigHash hashes those files under the real home ("" for one that
// isn't there).
func realConfigHash(t *testing.T, home string) string {
	t.Helper()
	h := sha256.New()
	for _, p := range realConfigs {
		b, err := os.ReadFile(filepath.Join(home, p))
		h.Write([]byte(p + "\x00"))
		if err == nil {
			s := sha256.Sum256(b)
			h.Write(s[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// isolatedHome points every harness and Wakeshift at a temporary home, and
// checks after the test that the real one's settings are unchanged.
func isolatedHome(t *testing.T) string {
	t.Helper()
	real, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	before := realConfigHash(t, real)
	t.Cleanup(func() {
		if after := realConfigHash(t, real); after != before {
			t.Errorf("the real home's settings changed during the test")
		}
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WAKESHIFT_HOME", filepath.Join(home, ".wakeshift"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("HERMES_HOME", filepath.Join(home, ".hermes"))
	t.Setenv("OPENCODE_CONFIG", filepath.Join(home, ".config", "opencode", "opencode.json"))
	t.Setenv("COPILOT_HOME", filepath.Join(home, ".copilot"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))
	for _, k := range []string{"XDG_DATA_HOME", "CURSOR_CONFIG_DIR", "OPENCODE_CONFIG_DIR"} {
		t.Setenv(k, "")
	}
	return home
}

// files lists every file under dir, relative to it.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var l []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			r, _ := filepath.Rel(dir, p)
			l = append(l, r)
		}
		return nil
	})
	slices.Sort(l)
	return l
}

func writeMode(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func procFile(t *testing.T, home, pid, harness string) string {
	t.Helper()
	p := filepath.Join(home, ".wakeshift", "procs", pid+".json")
	writeMode(t, p, `{"harness":"`+harness+`","conversation":"c-`+pid+`","start":"Mon Nov  9 10:00:00 2026"}`, 0o600)
	return p
}

// Uninstalling one harness removes its process records and keeps the
// connector while another harness uses it; uninstalling the last removes
// everything Wakeshift wrote to its home, and the home itself. The
// harnesses' own files are left as they are, but for the hooks Wakeshift
// added.
func TestUninstallLeftovers(t *testing.T) {
	home := isolatedHome(t)
	ws := filepath.Join(home, ".wakeshift")
	bin := filepath.Join(ws, "bin", "wakeshift")
	writeMode(t, bin, "#!/bin/sh\n", 0o755)
	writeMode(t, filepath.Join(ws, "bin", "wakeshift-codex"), "old", 0o755)
	writeMode(t, filepath.Join(ws, "bin", ".lock", "pid"), "1\n", 0o600)
	writeMode(t, filepath.Join(ws, "bin", ".wakeshift.Ab12Cd"), "partial", 0o600)

	// The harnesses' own files, which uninstall mustn't touch: Claude
	// Code's marketplace record and its clone, Codex's config.
	marketplaces := filepath.Join(home, ".claude", "plugins", "known_marketplaces.json")
	writeMode(t, marketplaces, `{"wakeshift":{"source":{"source":"github","repo":"wakeshift/agent-plugins"}}}`, 0o600)
	writeMode(t, filepath.Join(home, ".claude", "plugins", "marketplaces", "wakeshift", "README.md"), "clone", 0o600)
	codexConfig := filepath.Join(home, ".codex", "config.toml")
	writeMode(t, codexConfig, "[marketplaces.wakeshift]\nsource = \"wakeshift/agent-plugins\"\n", 0o600)
	theirs := map[string]string{}
	for _, p := range []string{marketplaces, codexConfig, filepath.Join(home, ".claude", "plugins", "marketplaces", "wakeshift", "README.md")} {
		b, _ := os.ReadFile(p)
		theirs[p] = string(b)
	}

	for _, h := range []string{"claude-code", "cursor"} {
		spec, _ := Lookup(h)
		var out bytes.Buffer
		if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
			t.Fatalf("%s: %v", h, err)
		}
	}
	claudeProc := procFile(t, home, "101", "claude-code")
	claudeTmp := filepath.Join(ws, "procs", "102.json.tmp")
	writeMode(t, claudeTmp, `{"harness":"claude-code","conversation":"c","start":"x"}`, 0o600)
	cursorProc := procFile(t, home, "201", "cursor")
	notOurs := filepath.Join(ws, "procs", "notes.txt")
	writeMode(t, notOurs, "mine", 0o600)

	// Claude Code first: Cursor still uses the connector.
	spec, _ := Lookup("claude-code")
	var out bytes.Buffer
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Kept "+bin+": Wakeshift is still installed for cursor.") {
		t.Fatalf("output: %s", out.String())
	}
	for _, p := range []string{filepath.Join(ws, "claude-code"), claudeProc, claudeTmp} {
		if exists(p) {
			t.Fatalf("left %s", p)
		}
	}
	for _, p := range []string{bin, cursorProc, filepath.Join(ws, "cursor")} {
		if !exists(p) {
			t.Fatalf("removed %s, which Cursor uses", p)
		}
	}

	// A file in procs that isn't a record stays, and so does the home it's in.
	spec, _ = Lookup("cursor")
	out.Reset()
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Removed "+bin+": no other agent here uses it.") || !strings.Contains(out.String(), "Kept "+ws+": other files are still in it.") {
		t.Fatalf("output: %s", out.String())
	}
	if got := files(t, ws); !slices.Equal(got, []string{filepath.Join("procs", "notes.txt")}) {
		t.Fatalf("left in %s: %v", ws, got)
	}
	for p, was := range theirs {
		if b, err := os.ReadFile(p); err != nil || string(b) != was {
			t.Fatalf("changed %s: %v %q", p, err, b)
		}
	}

	// Without it, the home goes too.
	if err := os.Remove(notOurs); err != nil {
		t.Fatal(err)
	}
	spec.Uninstall(bin, &out)
	if exists(ws) {
		t.Fatalf("left %s: %v", ws, files(t, ws))
	}
}

// A record that isn't a regular file of this user's, readable by no one
// else, isn't read, so it isn't removed: uninstall removes only what
// Wakeshift wrote.
func TestUninstallLeavesOthersProcs(t *testing.T) {
	home := isolatedHome(t)
	ws := filepath.Join(home, ".wakeshift")
	bin := filepath.Join(ws, "bin", "wakeshift")
	writeMode(t, bin, "#!/bin/sh\n", 0o755)
	spec, _ := Lookup("copilot")
	var out bytes.Buffer
	if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(ws, "procs", "301.json")
	writeMode(t, open, `{"harness":"copilot","conversation":"c","start":"x"}`, 0o644)
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	if !exists(open) || exists(bin) || exists(filepath.Join(ws, "copilot")) {
		t.Fatalf("left: %v", files(t, ws))
	}
}

// With no home (WAKESHIFT_HOME relative), uninstall removes nothing
// outside the harness's own hooks.
func TestUninstallNoHome(t *testing.T) {
	isolatedHome(t)
	t.Setenv("WAKESHIFT_HOME", "relative")
	var out bytes.Buffer
	if err := removeLeftovers("cursor", &out); err != nil || out.Len() != 0 {
		t.Fatalf("%v %q", err, out.String())
	}
}

// A bin or procs folder that's a link to somewhere else isn't followed:
// nothing in the place it points to is removed.
func TestUninstallFollowsNoLinks(t *testing.T) {
	home := isolatedHome(t)
	ws := filepath.Join(home, ".wakeshift")
	elsewhere := filepath.Join(home, "elsewhere")
	writeMode(t, filepath.Join(elsewhere, "wakeshift"), "someone else's", 0o755)
	writeMode(t, filepath.Join(elsewhere, "1.json"), `{"harness":"cursor","conversation":"c","start":"x"}`, 0o600)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bin", "procs"} {
		if err := os.Symlink(elsewhere, filepath.Join(ws, name)); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := removeLeftovers("cursor", &out); err != nil {
		t.Fatal(err)
	}
	if got := files(t, elsewhere); !slices.Equal(got, []string{"1.json", "wakeshift"}) {
		t.Fatalf("left in the link's target: %v", got)
	}
}
