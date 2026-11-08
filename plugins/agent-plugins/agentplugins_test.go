package agentplugins

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wakeshift/connectors/internal/connector"
	"github.com/wakeshift/connectors/internal/connectorversion"
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
		// Claude Code's is semver (TestSemver); the others' the connector's
		// version and a hash of their files.
		if man.Name != "wakeshift" || h == "claude-code" && man.Version != "1.0.0" || h != "claude-code" && !strings.HasPrefix(man.Version, "2026.11.02.1912+") {
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

// Claude Code's plugin is versioned MAJOR.MINOR.PATCH from the version
// already published: the same while nothing changes, PATCH+1 for a new pin
// alone, MINOR+1 for any other change; the source's version is the first
// and the floor, and sets MAJOR.
func TestSemver(t *testing.T) {
	dir := t.TempDir()
	version := func() string {
		t.Helper()
		v, err := manifestVersion([]byte(read(t, dir, "plugins/claude-code/.claude-plugin/plugin.json")))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	render := func(pins map[string]Pin, want string) {
		t.Helper()
		if _, err := Render(dir, "https://app.wakeshift.ai", pins); err != nil {
			t.Fatal(err)
		}
		if v := version(); v != want {
			t.Fatalf("version %s, want %s", v, want)
		}
	}
	cc := func(p Pin) map[string]Pin { return map[string]Pin{"claude-code": p} }
	render(cc(testPin("2026.11.02.1912+abc1234", "a")), "1.0.0")
	render(nil, "1.0.0")                                         // nothing changed
	render(cc(testPin("2026.11.02.1912+abc1234", "a")), "1.0.0") // the same pin
	render(cc(testPin("2026.11.09.0800+def5678", "b")), "1.0.1") // a new pin
	render(map[string]Pin{"codex": testPin("2026.11.10.0800", "c")}, "1.0.1")
	// Another of its own files changed (here: the published copy differs
	// from the source, as when the source's hooks or launcher change).
	hooks := filepath.Join(dir, "plugins/claude-code/hooks/hooks.json")
	os.WriteFile(hooks, []byte("{}"), 0o644)
	render(nil, "1.1.0")
	os.WriteFile(filepath.Join(dir, "plugins/claude-code/stray"), []byte("x"), 0o644)
	render(nil, "1.2.0") // a file the source doesn't have, removed
	if _, err := os.Stat(filepath.Join(dir, "plugins/claude-code/stray")); err == nil {
		t.Fatal("stray file kept")
	}
	os.WriteFile(hooks, []byte("{}"), 0o644)
	render(cc(testPin("2026.11.16.0800", "d")), "1.3.0") // both: MINOR

	// Published before semver (the connector's version and a hash): the
	// source's version.
	man := filepath.Join(dir, "plugins/claude-code/.claude-plugin/plugin.json")
	b, _ := setVersion([]byte(read(t, dir, "plugins/claude-code/.claude-plugin/plugin.json")), "2026.11.07.1745+962889d84040")
	os.WriteFile(man, b, 0o644)
	render(nil, "1.0.0")
	// A published MAJOR above the source's is refused.
	b, _ = setVersion(b, "2.0.0")
	os.WriteFile(man, b, 0o644)
	if _, err := Render(dir, "https://app.wakeshift.ai", nil); err == nil || !strings.Contains(err.Error(), "higher MAJOR") {
		t.Fatalf("err %v", err)
	}

	src := func(v string) map[string][]byte {
		return map[string][]byte{"m.json": []byte("{\n  \"version\": \"" + v + "\"\n}\n"), "a": []byte("a"), "wakeshift.env": []byte("e")}
	}
	with := func(m map[string][]byte, k, v string) map[string][]byte { m[k] = []byte(v); return m }
	for _, c := range []struct {
		next, prev map[string][]byte
		want       string
	}{
		{src("1.0.0"), nil, "1.0.0"},
		{src("1.0.0"), src("1.4.2"), "1.4.2"},
		{src("1.0.0"), with(src("1.4.2"), "wakeshift.env", "old"), "1.4.3"},
		{src("1.0.0"), with(src("1.4.2"), "a", "old"), "1.5.0"},
		{src("1.6.0"), src("1.4.2"), "1.6.0"},           // the source's floor
		{src("2.0.0"), src("1.4.2"), "2.0.0"},           // a new MAJOR
		{src("2.0.0"), src("2026.11.07.1745"), "2.0.0"}, // not semver
	} {
		if v, err := Semver(c.next, c.prev, "m.json"); err != nil || v != c.want {
			t.Errorf("%s over %s: %s %v, want %s", c.next["m.json"], c.prev["m.json"], v, err, c.want)
		}
	}
	if _, err := Semver(src("1.0"), nil, "m.json"); err == nil {
		t.Error("a source version that isn't semver")
	}
}

// Claude Code's plugin as rendered meets the rules of Anthropic's plugin
// directory that a file check can see (docs/claude-plugin-submission.md):
// every hook runs a file in the plugin, through ${CLAUDE_PLUGIN_ROOT} and no
// other variable, with no shell, substitution, wildcard or path out of the
// plugin; no symlinks or .DS_Store; nothing over 5 MiB, and nothing but
// images over 256 KiB; a README of at least 40 words; a license; a square
// icon; and the manifest's directory fields.
func TestClaudeCodePluginForDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := Render(dir, "https://app.wakeshift.ai", map[string]Pin{"claude-code": testPin("2026.11.02.1912+abc1234", "a")}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "plugins", "claude-code")
	var hooks struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []map[string]any
		}
	}
	if err := json.Unmarshal([]byte(read(t, root, "hooks/hooks.json")), &hooks); err != nil {
		t.Fatal(err)
	}
	n := 0
	for ev, groups := range hooks.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				n++
				args, _ := h["args"].([]any)
				if h["type"] != "command" || h["command"] != "/bin/sh" || len(args) < 2 {
					t.Fatalf("%s: not the launcher in exec form: %v", ev, h)
				}
				for i, a := range args {
					a, _ := a.(string)
					if strings.Contains(a, "${") && (i != 0 || strings.Count(a, "${") != 1) || strings.ContainsAny(a, "`*?|;&<>\n") || strings.Contains(a, "$(") || strings.Contains(a, "..") {
						t.Fatalf("%s: argument %q", ev, a)
					}
				}
				file, ok := strings.CutPrefix(args[0].(string), "${CLAUDE_PLUGIN_ROOT}/")
				if !ok {
					t.Fatalf("%s: runs %q, not a file in the plugin", ev, args[0])
				}
				if st, err := os.Stat(filepath.Join(root, file)); err != nil || st.Mode().Perm()&0o111 == 0 {
					t.Fatalf("%s: %s isn't an executable file in the plugin: %v", ev, file, err)
				}
			}
		}
	}
	if n != 4 {
		t.Fatalf("%d hooks", n)
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		info, _ := d.Info()
		rel, _ := filepath.Rel(root, p)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			t.Errorf("%s is a symlink", rel)
		case d.Name() == ".DS_Store":
			t.Errorf("%s", rel)
		case !d.IsDir() && info.Size() > 5<<20:
			t.Errorf("%s is over 5 MiB", rel)
		case !d.IsDir() && info.Size() > 256<<10 && !strings.HasSuffix(p, ".png"):
			t.Errorf("%s is over 256 KiB", rel)
		}
		return nil
	})
	if words := len(strings.Fields(read(t, root, "README.md"))); words < 40 {
		t.Errorf("README: %d words", words)
	}
	var man struct {
		Name, DisplayName, Version, Description, Homepage, Repository, License, Icon string
		DocumentationURL, SupportURL, PrivacyPolicyURL, TermsOfServiceURL            string
		Author                                                                       struct{ Name, Email, URL string }
		Keywords                                                                     []string
		UserConfig                                                                   any
	}
	if err := json.Unmarshal([]byte(read(t, root, ".claude-plugin/plugin.json")), &man); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"displayName": man.DisplayName, "description": man.Description, "homepage": man.Homepage, "repository": man.Repository,
		"license": man.License, "author.name": man.Author.Name, "author.email": man.Author.Email, "documentationUrl": man.DocumentationURL,
		"supportUrl": man.SupportURL, "privacyPolicyUrl": man.PrivacyPolicyURL, "termsOfServiceUrl": man.TermsOfServiceURL} {
		if v == "" {
			t.Errorf("manifest: no %s", k)
		}
	}
	if man.UserConfig != nil || len(man.Keywords) == 0 || man.License != "Apache-2.0" {
		t.Errorf("manifest: %+v", man)
	}
	// The description says what the hooks fetch and where they talk to.
	for _, want := range []string{"downloads", "SHA-256", "app.wakeshift.ai"} {
		if !strings.Contains(man.Description, want) {
			t.Errorf("description doesn't say %q", want)
		}
	}
	f, err := os.Open(filepath.Join(root, man.Icon))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.DecodeConfig(f)
	if err != nil || img.Width != img.Height || img.Width < 512 || img.Width > 2048 {
		t.Errorf("icon: %+v %v", img, err)
	}
}

// Below names the plugins whose pin is older than the core's minimum for
// their harness, or that pin nothing.
func TestBelow(t *testing.T) {
	dir := t.TempDir()
	if _, err := Render(dir, "https://app.wakeshift.ai", map[string]Pin{
		"claude-code": testPin("2026.11.07.0303+f1923a8", "a"), "codex": testPin("2026.11.03.1553+cc2fc62", "a"), "copilot": testPin("2026.11.07.0303+f1923a8", "a"),
	}); err != nil {
		t.Fatal(err)
	}
	for minimums, want := range map[string]string{
		"":                                  "cursor",
		"wakeshift=2026.10.30.0132":         "cursor",
		"wakeshift=2026.11.07.0303":         "codex cursor",
		"wakeshift:claude-code=2026.11.08":  "claude-code cursor",
		"wakeshift-hermes=2027.1.1":         "cursor",
		"wakeshift=2026.11.03.1553+cc2fc62": "cursor",
	} {
		m, err := connectorversion.ParseMinimums(minimums)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := Below(dir, m); err != nil || strings.Join(got, " ") != want {
			t.Errorf("%q: %v %v, want %s", minimums, got, err, want)
		}
	}
	// A plugin the published marketplace doesn't have yet isn't below.
	os.RemoveAll(filepath.Join(dir, "plugins", "cursor"))
	if got, err := Below(dir, nil); err != nil || len(got) != 0 {
		t.Errorf("without cursor's: %v %v", got, err)
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

// Claude Code's plugin: its hooks run the launcher in exec form, with the
// issuer wakeshift.env's (the Wakeshift the plugin was rendered for), so
// neither a shell, the environment (which a project's settings can set) nor
// a plugin that depends on it is involved.
func TestClaudeCodePlugin(t *testing.T) {
	dir := t.TempDir()
	if _, err := Render(dir, "https://app.wakeshift.ai", nil); err != nil {
		t.Fatal(err)
	}
	// No options: Claude Code applies no option's default and runs no hook
	// whose arguments name an unset one, and a plugin installed as another
	// plugin's dependency gets none set. The issuer is wakeshift.env's.
	var man map[string]any
	if err := json.Unmarshal([]byte(read(t, dir, "plugins/claude-code/.claude-plugin/plugin.json")), &man); err != nil {
		t.Fatal(err)
	}
	if _, ok := man["userConfig"]; ok {
		t.Fatalf("userConfig: %v", man["userConfig"])
	}
	if strings.Contains(read(t, dir, "plugins/claude-code/hooks/hooks.json"), "user_config") {
		t.Fatal("the hooks name a plugin option")
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
	// SessionStart's first entry tells the model the plugin's mark
	// (internal/pluginmark): a plain hook, run once per session; the waiter
	// is the second.
	start := hooks.Hooks["SessionStart"]
	if len(start) != 2 || len(start[0].Hooks) != 1 || strings.Join(start[0].Hooks[0].Args, " ") != "${CLAUDE_PLUGIN_ROOT}/bin/wakeshift hook --harness claude-code --event "+connector.ClaudeSessionContextEvent ||
		start[0].Hooks[0].Async || start[0].Hooks[0].AsyncRewake || start[0].Hooks[0].Timeout != 60 {
		t.Fatalf("SessionStart: %+v", start)
	}
	hooks.Hooks["SessionStart"] = start[1:]
	for ev, tail := range want {
		g := hooks.Hooks[ev]
		if len(g) != 1 || len(g[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", ev, g)
		}
		h := g[0].Hooks[0]
		args := strings.Join(h.Args, " ")
		if h.Command != "/bin/sh" || args != "${CLAUDE_PLUGIN_ROOT}/bin/wakeshift "+tail {
			t.Fatalf("%s: %+v", ev, h)
		}
		switch ev {
		case "PostToolUse":
			if g[0].Matcher != "mcp__.*" || !h.Async || h.Timeout != 30 {
				t.Fatalf("%s: %+v", ev, g[0])
			}
		default:
			if !h.AsyncRewake || h.Timeout != 86400 {
				t.Fatalf("%s: %+v", ev, g[0])
			}
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

// Claude Code's description names the Wakeshift the plugin trusts: the
// issuer's host, so staging's branch names staging's. Rendering it again
// changes nothing, and production's is the source's, word for word.
func TestDescriptionNamesTheIssuersHost(t *testing.T) {
	description := func(dir string) string {
		var m struct{ Description string }
		if err := json.Unmarshal([]byte(read(t, dir, "plugins/claude-code/.claude-plugin/plugin.json")), &m); err != nil {
			t.Fatal(err)
		}
		return m.Description
	}
	src, err := files.ReadFile("marketplace/plugins/claude-code/.claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var source struct{ Description string }
	if err := json.Unmarshal(src, &source); err != nil || !strings.Contains(source.Description, "(app.wakeshift.ai)") {
		t.Fatalf("the source's description: %q (%v)", source.Description, err)
	}
	for issuer, host := range map[string]string{
		"https://app.wakeshift.ai":         "app.wakeshift.ai",
		"https://app.staging.wakeshift.ai": "app.staging.wakeshift.ai",
		"http://localhost:8370":            "localhost:8370",
	} {
		dir := t.TempDir()
		if _, err := Render(dir, issuer, nil); err != nil {
			t.Fatal(err)
		}
		got := description(dir)
		if want := strings.ReplaceAll(source.Description, "app.wakeshift.ai", host); got != want {
			t.Errorf("%s: %q", issuer, got)
		}
		v, _ := manifestVersion([]byte(read(t, dir, "plugins/claude-code/.claude-plugin/plugin.json")))
		if changed, err := Render(dir, issuer, nil); err != nil || len(changed) != 0 {
			t.Errorf("%s again: changed %v (%v)", issuer, changed, err)
		}
		if again, _ := manifestVersion([]byte(read(t, dir, "plugins/claude-code/.claude-plugin/plugin.json"))); again != v {
			t.Errorf("%s again: version %s, was %s", issuer, again, v)
		}
	}
}

// A description describeHost can't rewrite (not on its own line) is an
// error, never a staging plugin naming production.
func TestDescribeHostRefusesWhatItCantRewrite(t *testing.T) {
	folded := []byte("{\n  \"name\": \"wakeshift\",\n  \"description\":\n    \"Talks only to Wakeshift (app.wakeshift.ai).\"\n}\n")
	if _, err := describeHost(folded, "https://app.staging.wakeshift.ai"); err == nil {
		t.Fatal("a folded description was let through")
	}
	if out, err := describeHost(folded, "https://app.wakeshift.ai"); err != nil || !bytes.Equal(out, folded) {
		t.Fatalf("production's: %s (%v)", out, err)
	}
}
