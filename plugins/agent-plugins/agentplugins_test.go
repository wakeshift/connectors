package agentplugins

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testPin(version, fill string) Pin {
	sums := map[string]string{}
	for _, n := range Platforms {
		sums[n] = strings.Repeat(fill, 64)
	}
	return Pin{Version: version, URL: "https://releases.example.com/download/v1.4.21", Sums: sums}
}

func read(t *testing.T, dir, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Each harness reads its own marketplace file, which names "wakeshift" at
// its own plugin directory; each plugin has its manifest, hooks that run
// the launcher, the launcher itself (executable) and its pin.
func TestRender(t *testing.T) {
	dir := t.TempDir()
	changed, err := Render(dir, "https://app.wakeshift.ai", map[string]Pin{"codex": testPin("2026.11.02.1912+abc1234", "a"), "copilot": testPin("2026.11.02.1912+abc1234", "a")})
	if err != nil || !slices.Equal(changed, []string{"codex", "copilot"}) {
		t.Fatalf("%v %v", changed, err)
	}
	for file, plugin := range map[string]string{".agents/plugins/marketplace.json": "./plugins/codex", ".github/plugin/marketplace.json": "./plugins/copilot"} {
		var m struct {
			Name    string
			Plugins []struct {
				Name   string
				Source json.RawMessage
			}
		}
		if err := json.Unmarshal([]byte(read(t, dir, file)), &m); err != nil {
			t.Fatal(err)
		}
		if m.Name != "wakeshift" || len(m.Plugins) != 1 || m.Plugins[0].Name != "wakeshift" || !strings.Contains(string(m.Plugins[0].Source), plugin) {
			t.Fatalf("%s: %+v", file, m)
		}
	}
	for h, hooks := range map[string]string{"codex": "hooks/hooks.json", "copilot": "com.github.copilot/hooks/hooks.json"} {
		root := "plugins/" + h + "/"
		var man struct{ Name, Version string }
		if err := json.Unmarshal([]byte(read(t, dir, root+manifests[h])), &man); err != nil {
			t.Fatal(err)
		}
		if man.Name != "wakeshift" || !strings.HasPrefix(man.Version, "2026.11.02.1912+") {
			t.Fatalf("%s manifest: %+v", h, man)
		}
		if hk := read(t, dir, root+hooks); !strings.Contains(hk, "/bin/wakeshift") || !strings.Contains(hk, " hook --harness "+h) {
			t.Fatalf("%s hooks: %s", h, hk)
		}
		if read(t, dir, root+"bin/wakeshift") != string(Launcher()) {
			t.Fatalf("%s: launcher differs", h)
		}
		if st, _ := os.Stat(filepath.Join(dir, root, "bin/wakeshift")); st.Mode().Perm() != 0o755 {
			t.Fatalf("%s: launcher mode %v", h, st.Mode())
		}
		env := read(t, dir, root+"wakeshift.env")
		for _, line := range []string{"harness=" + h, "issuer=https://app.wakeshift.ai", "connector_version=2026.11.02.1912+abc1234",
			"connector_url=https://releases.example.com/download/v1.4.21"} {
			if !strings.Contains(env, "\n"+line+"\n") {
				t.Fatalf("%s wakeshift.env lacks %s: %s", h, line, env)
			}
		}
		if p, ok, err := ReadPin(dir, h); !ok || err != nil || p.Sums["wakeshift-linux-arm64"] != strings.Repeat("a", 64) {
			t.Fatalf("%s pin: %+v %v %v", h, p, ok, err)
		}
	}
	// Again, the same: nothing changes.
	if changed, err := Render(dir, "https://app.wakeshift.ai", nil); err != nil || len(changed) != 0 {
		t.Fatalf("again: %v %v", changed, err)
	}
}

// A release that stamps one plugin leaves the other's files exactly as they
// were (Codex asks for trust again when any file of its plugin changes),
// and files that aren't the marketplace's (README, .git) are kept.
func TestRenderKeepsOtherPins(t *testing.T) {
	dir := t.TempDir()
	if _, err := Render(dir, "https://app.wakeshift.ai", map[string]Pin{"codex": testPin("2026.11.02.1912", "a"), "copilot": testPin("2026.11.02.1912", "a")}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "plugins", "codex", "stray"), []byte("x"), 0o644)
	before := snapshot(t, filepath.Join(dir, "plugins", "codex"))
	delete(before, "stray")
	// The stray file goes: the plugin's files are exactly the rendered ones.
	changed, err := Render(dir, "https://app.wakeshift.ai", map[string]Pin{"copilot": testPin("2026.11.09.0800", "b")})
	if err != nil || !slices.Equal(changed, []string{"codex", "copilot"}) {
		t.Fatalf("%v %v", changed, err)
	}
	if after := snapshot(t, filepath.Join(dir, "plugins", "codex")); !mapsEqual(before, after) {
		t.Fatal("codex's files changed")
	}
	if changed, _ := Render(dir, "https://app.wakeshift.ai", map[string]Pin{"copilot": testPin("2026.11.16.0800", "c")}); !slices.Equal(changed, []string{"copilot"}) {
		t.Fatalf("changed %v", changed)
	}
	if p, _, _ := ReadPin(dir, "copilot"); p.Version != "2026.11.16.0800" {
		t.Fatalf("copilot: %+v", p)
	}
	if read(t, dir, "README.md") != "hi" {
		t.Fatal("README replaced")
	}
	// Another issuer (another ref) changes every plugin.
	if changed, _ := Render(dir, "https://app.staging.wakeshift.ai", nil); len(changed) != 2 {
		t.Fatalf("changed %v", changed)
	}
}

func snapshot(t *testing.T, root string) map[string]string {
	m := map[string]string{}
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			m[rel] = info.Mode().String() + string(b)
		}
		return nil
	})
	return m
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestPinChecks(t *testing.T) {
	good := testPin("2026.11.02.1912+abc", "a")
	if err := good.Check(); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Pin{
		"version":    {Version: "latest", URL: good.URL, Sums: good.Sums},
		"plain http": {Version: good.Version, URL: "http://example.com/r", Sums: good.Sums},
		"quote":      {Version: good.Version, URL: "https://example.com/\"r", Sums: good.Sums},
		"slash":      {Version: good.Version, URL: "https://example.com/r/", Sums: good.Sums},
		"no sums":    {Version: good.Version, URL: good.URL},
	} {
		if p.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	local := good
	local.URL = "http://127.0.0.1:18399"
	if err := local.Check(); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(t.TempDir(), "http://example.com", nil); err == nil {
		t.Fatal("plain http issuer")
	}
	if _, err := Render(t.TempDir(), "https://app.wakeshift.ai/x", nil); err == nil {
		t.Fatal("issuer with a path")
	}
}

func TestParseSums(t *testing.T) {
	var b bytes.Buffer
	for i, n := range append(Platforms, "codex.sh", "openclaw.tgz") {
		b.WriteString(strings.Repeat(string(rune('a'+i)), 64) + "  " + n + "\n")
	}
	sums, err := ParseSums(b.Bytes())
	if err != nil || len(sums) != 4 || sums["wakeshift-darwin-amd64"] != strings.Repeat("a", 64) {
		t.Fatalf("%v %v", sums, err)
	}
	if _, err := ParseSums([]byte(strings.Repeat("a", 64) + "  wakeshift-darwin-amd64\n")); err == nil {
		t.Fatal("missing platforms accepted")
	}
}
