package agentplugins

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wakeshift/connectors/internal/connector"
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
	pin := testPin("2026.11.02.1912+abc1234", "a")
	changed, err := Render(dir, "https://app.wakeshift.ai", map[string]Pin{"claude-code": pin, "codex": pin, "copilot": pin, "cursor": pin})
	if err != nil || !slices.Equal(changed, []string{"claude-code", "codex", "copilot", "cursor"}) {
		t.Fatalf("%v %v", changed, err)
	}
	for file, plugin := range map[string]string{".agents/plugins/marketplace.json": "./plugins/codex", ".github/plugin/marketplace.json": "./plugins/copilot", ".claude-plugin/marketplace.json": "./plugins/claude-code", ".cursor-plugin/marketplace.json": `"plugins/cursor"`} {
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
	for h, hooks := range map[string]string{"claude-code": "hooks/hooks.json", "codex": "hooks/hooks.json", "copilot": "com.github.copilot/hooks/hooks.json", "cursor": "hooks/hooks.json"} {
		root := "plugins/" + h + "/"
		var man struct{ Name, Version string }
		if err := json.Unmarshal([]byte(read(t, dir, root+manifests[h])), &man); err != nil {
			t.Fatal(err)
		}
		if man.Name != "wakeshift" || !strings.HasPrefix(man.Version, "2026.11.02.1912+") {
			t.Fatalf("%s manifest: %+v", h, man)
		}
		if hk := strings.ReplaceAll(read(t, dir, root+hooks), `", "`, " "); !strings.Contains(hk, "/bin/wakeshift") || !strings.Contains(hk, " hook --harness "+h) {
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
	if _, err := Render(dir, "https://app.wakeshift.ai", map[string]Pin{"claude-code": testPin("2026.11.02.1912", "a"), "codex": testPin("2026.11.02.1912", "a"), "copilot": testPin("2026.11.02.1912", "a")}); err != nil {
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
	if changed, _ := Render(dir, "https://app.staging.wakeshift.ai", nil); len(changed) != len(Harnesses()) {
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

// Cursor's plugin follows the Cursor Marketplace's checklist (a kebab-case
// name, relative paths without "..", a committed logo, a README), and its
// hooks are the ones Wakeshift's installer writes to ~/.cursor/hooks.json,
// each run through ${CURSOR_PLUGIN_ROOT}: Cursor runs a plugin's stop hook
// from the workspace, not the plugin's folder.
func TestCursorPlugin(t *testing.T) {
	dir := t.TempDir()
	if _, err := Render(dir, "https://app.wakeshift.ai", map[string]Pin{"cursor": testPin("2026.11.02.1912+abc1234", "a")}); err != nil {
		t.Fatal(err)
	}
	root := "plugins/cursor/"
	var man struct {
		Name, Version, Description, Logo, Hooks, Repository, License string
		Author                                                       struct{ Name string }
	}
	if err := json.Unmarshal([]byte(read(t, dir, root+".cursor-plugin/plugin.json")), &man); err != nil {
		t.Fatal(err)
	}
	if man.Name != "wakeshift" || man.Description == "" || man.Author.Name == "" || man.License != "Apache-2.0" || !strings.HasPrefix(man.Version, "2026.11.02.1912+") {
		t.Fatalf("manifest: %+v", man)
	}
	for _, p := range []string{man.Logo, man.Hooks, "README.md"} {
		if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			t.Fatalf("manifest path %q", p)
		}
		read(t, dir, root+p)
	}
	var hooks struct {
		Version int
		Hooks   map[string][]struct {
			Command string
			Matcher string
			Timeout int
		}
	}
	if err := json.Unmarshal([]byte(read(t, dir, root+man.Hooks)), &hooks); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"sessionStart": " prepare", "afterMCPExecution": " hook --harness cursor", "postToolUse": " hook --harness cursor", "stop": " hook --harness cursor"}
	if hooks.Version != 1 || len(hooks.Hooks) != len(want) {
		t.Fatalf("hooks: %+v", hooks)
	}
	for ev, suffix := range want {
		l := hooks.Hooks[ev]
		if len(l) != 1 || l[0].Command != `sh "${CURSOR_PLUGIN_ROOT}/bin/wakeshift"`+suffix || l[0].Timeout == 0 {
			t.Errorf("%s: %+v", ev, l)
		}
	}
	if m := hooks.Hooks["postToolUse"][0].Matcher; m != "^MCP:" {
		t.Errorf("postToolUse matcher %q", m)
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

// Claude Code's plugin: its hooks pass the issuer the user chose (the
// plugin's userConfig, production by default) to the launcher as an
// argument, in exec form, so neither a shell nor the environment (which a
// project's settings can set) is involved.
func TestClaudeCodePlugin(t *testing.T) {
	dir := t.TempDir()
	if _, err := Render(dir, "https://app.wakeshift.ai", nil); err != nil {
		t.Fatal(err)
	}
	var man struct {
		UserConfig map[string]struct {
			Type, Default string
			Required      bool
		}
	}
	if err := json.Unmarshal([]byte(read(t, dir, "plugins/claude-code/.claude-plugin/plugin.json")), &man); err != nil {
		t.Fatal(err)
	}
	if o := man.UserConfig["issuer"]; o.Type != "string" || o.Default != "https://app.wakeshift.ai" || !o.Required {
		t.Fatalf("userConfig: %+v", man.UserConfig)
	}
	var hooks struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct {
				Type, Command                string
				Args                         []string
				Async, AsyncRewake           bool
				Timeout                      int
				RewakeSummary, RewakeMessage string
			}
		}
	}
	if err := json.Unmarshal([]byte(read(t, dir, "plugins/claude-code/hooks/hooks.json")), &hooks); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PostToolUse": "hook --harness claude-code", "SessionStart": "hook --harness claude-code --rewake", "Stop": "hook --harness claude-code --rewake"}
	if len(hooks.Hooks) != len(want) {
		t.Fatalf("events: %v", hooks.Hooks)
	}
	for ev, tail := range want {
		g := hooks.Hooks[ev]
		if len(g) != 1 || len(g[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", ev, g)
		}
		h := g[0].Hooks[0]
		args := strings.Join(h.Args, " ")
		if h.Command != "/bin/sh" || args != "${CLAUDE_PLUGIN_ROOT}/bin/wakeshift --issuer ${user_config.issuer} "+tail {
			t.Fatalf("%s: %+v", ev, h)
		}
		if ev == "PostToolUse" && (g[0].Matcher != "mcp__.*" || !h.Async || h.Timeout != 30) || ev != "PostToolUse" && (!h.AsyncRewake || h.Timeout != 86400) {
			t.Fatalf("%s: %+v", ev, g[0])
		}
		// The waiters' wakes are presented as the installer's are.
		if ev != "PostToolUse" && (h.RewakeSummary != connector.ClaudeRewakeSummary || h.RewakeMessage != connector.ClaudeRewakeMessage) {
			t.Fatalf("%s: %+v", ev, g[0])
		}
	}
}

// Antigravity's plugin is its repository's root (agy installs only from a
// git URL whose root is the plugin): its manifest, hooks.json in agy's flat
// format with every hook run through the launcher from the plugin's folder
// (agy runs hooks with sh -c there), and the launcher and pin beside them.
func TestRenderStandalone(t *testing.T) {
	if !slices.Equal(Standalone(), []string{"antigravity"}) {
		t.Fatalf("standalone %v", Standalone())
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644)
	changed, err := RenderStandalone(dir, "antigravity", "https://app.wakeshift.ai", ptr(testPin("2026.11.02.1912+abc1234", "a")))
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	var man struct{ Name, Version, Schema string }
	if err := json.Unmarshal([]byte(read(t, dir, "plugin.json")), &man); err != nil {
		t.Fatal(err)
	}
	// No Agent Plugins $schema: agy loads it as a native plugin, hooks.json
	// at its root, whether or not its enable-agent-plugins-spec flag is on.
	if man.Name != "wakeshift" || !strings.HasPrefix(man.Version, "2026.11.02.1912+") || strings.Contains(read(t, dir, "plugin.json"), "$schema") {
		t.Fatalf("manifest: %+v", man)
	}
	var hooks map[string]map[string][]struct {
		Matcher, Command string
		Timeout          int
		Hooks            []struct {
			Command string
			Timeout int
		}
	}
	if err := json.Unmarshal([]byte(read(t, dir, "hooks.json")), &hooks); err != nil {
		t.Fatal(err)
	}
	ours := hooks["wakeshift"]
	if len(hooks) != 1 || len(ours) != 4 {
		t.Fatalf("hooks: %+v", hooks)
	}
	want := map[string]string{
		"SessionStart":  "sh ./bin/wakeshift prepare",
		"PreInvocation": "sh ./bin/wakeshift hook --harness antigravity --event pre-invocation",
		"Stop":          "sh ./bin/wakeshift hook --harness antigravity --event stop",
	}
	for ev, cmd := range want {
		if len(ours[ev]) != 1 || ours[ev][0].Command != cmd || ours[ev][0].Timeout == 0 {
			t.Fatalf("%s: %+v", ev, ours[ev])
		}
	}
	// The connector's own hooks file entry (connector.antigravityInstaller)
	// has the same events and timeouts.
	if ours["Stop"][0].Timeout != 90 {
		t.Fatalf("Stop timeout %d", ours["Stop"][0].Timeout)
	}
	post := ours["PostToolUse"]
	if len(post) != 1 || post[0].Matcher != "call_mcp_tool" || len(post[0].Hooks) != 1 ||
		post[0].Hooks[0].Command != "sh ./bin/wakeshift hook --harness antigravity --event post-tool" {
		t.Fatalf("PostToolUse: %+v", post)
	}
	if read(t, dir, "bin/wakeshift") != string(Launcher()) {
		t.Fatal("launcher differs")
	}
	if st, _ := os.Stat(filepath.Join(dir, "bin/wakeshift")); st.Mode().Perm() != 0o755 {
		t.Fatalf("launcher mode %v", st.Mode())
	}
	env := read(t, dir, "wakeshift.env")
	for _, line := range []string{"harness=antigravity", "issuer=https://app.wakeshift.ai", "connector_version=2026.11.02.1912+abc1234"} {
		if !strings.Contains(env, "\n"+line+"\n") {
			t.Fatalf("wakeshift.env lacks %s: %s", line, env)
		}
	}
	if read(t, dir, "README.md") != "hi" {
		t.Fatal("README replaced")
	}

	// Rendered again with no pin, it keeps the one it has: nothing changes.
	before := snapshot(t, dir)
	if changed, err := RenderStandalone(dir, "antigravity", "https://app.wakeshift.ai", nil); err != nil || changed {
		t.Fatalf("again: %v %v", changed, err)
	}
	if !mapsEqual(before, snapshot(t, dir)) {
		t.Fatal("files changed")
	}
	// Staging's branch: another issuer.
	if changed, _ := RenderStandalone(dir, "antigravity", "https://app.staging.wakeshift.ai", nil); !changed {
		t.Fatal("another issuer changed nothing")
	}
	if p, ok, err := ReadStandalonePin(dir, "antigravity"); !ok || err != nil || p.Version != "2026.11.02.1912+abc1234" {
		t.Fatalf("pin: %+v %v %v", p, ok, err)
	}

	// No pin at all: the launcher does nothing until a release pins one.
	empty := t.TempDir()
	if _, err := RenderStandalone(empty, "antigravity", "https://app.wakeshift.ai", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, empty, "wakeshift.env"), "\nconnector_version=none\n") {
		t.Fatal("unpinned plugin has a pin")
	}
	if _, err := os.Stat(filepath.Join(empty, "SHA256SUMS")); err == nil {
		t.Fatal("unpinned plugin has SHA256SUMS")
	}

	if _, err := RenderStandalone(t.TempDir(), "codex", "https://app.wakeshift.ai", nil); err == nil {
		t.Fatal("codex isn't standalone")
	}
	if _, err := RenderStandalone(t.TempDir(), "antigravity", "http://example.com", nil); err == nil {
		t.Fatal("plain http issuer")
	}
	bad := testPin("latest", "a")
	if _, err := RenderStandalone(t.TempDir(), "antigravity", "https://app.wakeshift.ai", &bad); err == nil {
		t.Fatal("bad pin")
	}
}

func ptr[T any](v T) *T { return &v }
