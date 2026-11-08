package agentplugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// launcherCalls is every launcher command line (its arguments) a rendered
// hooks.json runs: Claude Code's "args" after the launcher's path, the
// others' command strings after it.
func launcherCalls(t *testing.T, hooksJSON string) [][]string {
	t.Helper()
	b, err := os.ReadFile(hooksJSON)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for _, x := range v {
				walk(x)
			}
		case []any:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok && strings.HasSuffix(s, "/bin/wakeshift") {
					var args []string
					for _, a := range v[1:] {
						args = append(args, a.(string))
					}
					calls = append(calls, args)
					return
				}
			}
			for _, x := range v {
				walk(x)
			}
		case string:
			f := strings.Fields(v)
			for i, w := range f {
				if strings.HasSuffix(strings.Trim(w, `"`), "/bin/wakeshift") {
					calls = append(calls, f[i+1:])
					break
				}
			}
		}
	}
	walk(doc)
	if len(calls) == 0 {
		t.Fatalf("%s runs no launcher", hooksJSON)
	}
	return calls
}

// WAKESHIFT_DISABLED=1 (or true, in any case) turns every hook of every
// plugin off in the launcher: before a download, the lock or the
// connector, so a first run fetches nothing and a warm one runs nothing,
// and either prints nothing at all and exits 0. Unset, 0 or false, the same
// hooks fetch (a first run) and run the connector (a warm one), so this
// test fails if the check goes.
func TestLauncherDisabled(t *testing.T) {
	name := platform(t)
	const version = "2026.11.02.1912+abc1234"
	served := fakeConnector(version)
	eachShell(t, func(t *testing.T, shell string) {
		t.Parallel()
		r := newRelease(t)
		r.put(name, served)
		sums := map[string]string{}
		for _, n := range Platforms {
			sums[n] = strings.Repeat("0", 64)
		}
		sums[name] = hexSum(served)
		pin := Pin{Version: version, URL: r.srv.URL, Sums: sums}
		dir := t.TempDir()
		pins := map[string]Pin{}
		for _, h := range Harnesses() {
			pins[h] = pin
		}
		if _, err := Render(dir, "https://app.wakeshift.ai", pins); err != nil {
			t.Fatal(err)
		}
		plugins := map[string]string{} // plugin root → its hooks.json
		for _, h := range Harnesses() {
			root := filepath.Join(dir, "plugins", h)
			hooks := filepath.Join(root, "hooks", "hooks.json")
			if h == "copilot" {
				hooks = filepath.Join(root, "com.github.copilot", "hooks", "hooks.json")
			}
			plugins[root] = hooks
		}
		for _, h := range Standalone() {
			root := filepath.Join(t.TempDir(), h)
			if _, err := RenderStandalone(root, h, "https://app.wakeshift.ai", &pin); err != nil {
				t.Fatal(err)
			}
			plugins[root] = filepath.Join(root, "hooks.json")
		}
		if len(plugins) < 5 {
			t.Fatalf("plugins: %v", plugins)
		}
		for root, hooks := range plugins {
			for _, args := range launcherCalls(t, hooks) {
				for _, v := range []string{"1", "true", "TRUE", "", "0", "false"} {
					off := v == "1" || strings.EqualFold(v, "true")
					what := filepath.Base(root) + " " + strings.Join(args, " ") + " WAKESHIFT_DISABLED=" + v
					for _, warm := range []bool{false, true} {
						home := filepath.Join(t.TempDir(), ".wakeshift")
						l := &lane{t: t, shell: shell, plugin: root, home: home, log: filepath.Join(t.TempDir(), "log")}
						l.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(home), "WAKESHIFT_HOME=" + home, "LOG=" + l.log}
						if warm {
							if out, errOut := l.run("", "prepare"); out != "" || errOut != "" {
								t.Fatalf("%s: setting up: %q %q", what, out, errOut)
							}
							os.Remove(l.log)
						}
						l.env = append(l.env, "WAKESHIFT_DISABLED="+v)
						r.hits.Store(0)
						out, errOut := l.run(`{"hook":1}`, args...)
						ran := l.logged() != ""
						if !off {
							// The control: the same hook does its work when it's on.
							switch {
							case !warm && args[0] != "--if-ready" && r.hits.Load() == 0:
								t.Fatalf("%s: a first run fetched nothing: the test proves nothing", what)
							case warm && args[0] != "prepare" && !ran:
								t.Fatalf("%s: a warm run ran no connector: the test proves nothing", what)
							}
							continue
						}
						if n := r.hits.Load(); n != 0 {
							t.Fatalf("%s (warm %v): %d requests", what, warm, n)
						}
						if out != "" || errOut != "" {
							t.Fatalf("%s (warm %v): stdout %q stderr %q", what, warm, out, errOut)
						}
						if ran {
							t.Fatalf("%s (warm %v): ran the connector: %s", what, warm, l.logged())
						}
						if !warm {
							if _, err := os.Stat(home); err == nil {
								t.Fatalf("%s: made %s", what, home)
							}
						}
					}
				}
			}
		}
	})
}
