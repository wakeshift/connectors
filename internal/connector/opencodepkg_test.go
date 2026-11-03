package connector

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var updatePackage = flag.Bool("update", false, "rewrite plugins/opencode/index.js from OpencodePackage")

// opencodePackageFile is the npm package's plugin, beside its package.json.
var opencodePackageFile = filepath.Join("..", "..", "plugins", "opencode", "index.js")

// The npm package's index.js is OpencodePackage, so a change to the plugin
// the installer writes reaches the package too.
func TestOpencodePackage(t *testing.T) {
	want := OpencodePackage()
	if *updatePackage {
		if err := os.WriteFile(opencodePackageFile, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(opencodePackageFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s isn't OpencodePackage(): run go test ./internal/connector -run TestOpencodePackage -update", opencodePackageFile)
	}
	if !strings.Contains(want, opencodePackageServer) || strings.Contains(want, opencodeServer) {
		t.Error("the package's server line wasn't replaced")
	}
	if strings.Count(want, OpencodeDefaultIssuers) != 1 {
		t.Error("plugins/pack-npm.mjs replaces the default issuers' line: it must be there once")
	}
	if strings.Contains(want, "__WAKESHIFT_BIN__") {
		t.Error("the package has the installer's placeholder for the binary")
	}
}

// npmPlatform is this machine's platform as npm names it.
func npmPlatform() string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	return runtime.GOOS + "-" + arch
}

// packageDir lays the package out as npm installs it: index.js, and this
// platform's connector (the fake bridge, not executable, as opencode's
// install leaves it) in node_modules.
func packageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pkg := filepath.Join(dir, "node_modules", "@wakeshift", "connect-"+npmPlatform())
	if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@wakeshift/connect-`+npmPlatform()+`","version":"0.0.0"}`), 0o644)
	os.WriteFile(filepath.Join(pkg, "bin", "wakeshift"), []byte(fakeBridge), 0o644)
	os.WriteFile(filepath.Join(dir, "wakeshift.mjs"), []byte(OpencodePackage()), 0o644)
	os.WriteFile(filepath.Join(dir, "plan"), []byte("ok\nok"), 0o600)
	os.WriteFile(filepath.Join(dir, "n"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "log"), nil, 0o600)
	return dir
}

// The package runs the connector from its platform package, pins the
// default Wakeshift where the bridge reads it, and removes the plugin file
// an earlier install wrote (only that one).
func TestOpencodePackageRuns(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	for _, tc := range []struct {
		name, old string
		removed   bool
	}{
		{"moves over from the installer's file", strings.Replace(opencodePlugin, "__WAKESHIFT_BIN__", `"/x/wakeshift"`, 1), true},
		{"leaves someone else's file", "// my own plugin\nexport default {}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := packageDir(t)
			xdg := filepath.Join(dir, "xdg")
			old := filepath.Join(xdg, "opencode", "plugins", "wakeshift.ts")
			os.MkdirAll(filepath.Dir(old), 0o700)
			os.WriteFile(old, []byte(tc.old), 0o600)
			os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(pluginDriver), 0o600)
			home := filepath.Join(dir, "home")
			cmd := exec.Command(node, "driver.mjs", "beacon")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "FAKE_BRIDGE_DIR="+dir, "WAKESHIFT_HOME="+home, "XDG_CONFIG_HOME="+xdg)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			var got struct {
				Starts int
				Note   string
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if got.Starts != 1 || got.Note != "Wakeshift: connected" {
				t.Errorf("started %d bridges with note %q, want the platform package's bridge, connected", got.Starts, got.Note)
			}
			s := LoadSettings(opencodeName)
			s.Dir = filepath.Join(home, "opencode")
			b, err := os.ReadFile(filepath.Join(s.Dir, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}
			if strings.Join(s.Issuers, ",") != "https://app.wakeshift.ai" || s.InstalledAt.IsZero() {
				t.Errorf("settings %s: want the default issuer and installed_at", b)
			}
			if _, err := os.Stat(old); os.IsNotExist(err) != tc.removed {
				t.Errorf("%s removed: %v, want %v", old, !tc.removed, tc.removed)
			}
		})
	}
}

// The plugin's "issuers" option in opencode.json replaces the default; an
// address that isn't one is ignored.
func TestOpencodePackageIssuersOption(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := packageDir(t)
	home := filepath.Join(dir, "home")
	driver := `import plugin from "./wakeshift.mjs"
await plugin.server({ client: { session: {} }, directory: "/tmp" }, { issuers: ["http://localhost:8370/", "https://x.test/path", 7] })
process.exit(0)
`
	os.WriteFile(filepath.Join(dir, "issuers.mjs"), []byte(driver), 0o600)
	cmd := exec.Command(node, "issuers.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "WAKESHIFT_HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(dir, "xdg"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var s Settings
	b, _ := os.ReadFile(filepath.Join(home, "opencode", "config.json"))
	json.Unmarshal(b, &s)
	if strings.Join(s.Issuers, ",") != "http://localhost:8370" {
		t.Errorf("issuers %v, want only http://localhost:8370", s.Issuers)
	}
}

// With the installer's file and the package both loaded (the run in which
// a user moves over), only the first starts bridges.
func TestOpencodeOnePluginPerProject(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := packageDir(t)
	bin := filepath.Join(dir, "node_modules", "@wakeshift", "connect-"+npmPlatform(), "bin", "wakeshift")
	os.WriteFile(filepath.Join(dir, "file.mjs"), []byte(strings.Replace(opencodePlugin, "__WAKESHIFT_BIN__", jsString(bin), 1)), 0o600)
	driver := `import a from "./file.mjs"
import b from "./wakeshift.mjs"
const client = { session: { get: async () => ({ data: {} }) } }
const ha = await a.server({ client, directory: "/p" })
const hb = await b.server({ client, directory: "/p" })
const hc = await b.server({ client, directory: "/q" }) // another project: its own
console.log(JSON.stringify({ a: Object.keys(ha).length > 0, b: Object.keys(hb).length > 0, c: Object.keys(hc).length > 0 }))
await ha.dispose()
const hd = await b.server({ client, directory: "/p" }) // after the first is disposed
console.log(JSON.stringify({ d: Object.keys(hd).length > 0 }))
await hc.dispose(); await hd.dispose()
process.exit(0)
`
	os.WriteFile(filepath.Join(dir, "both.mjs"), []byte(driver), 0o600)
	cmd := exec.Command(node, "both.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "WAKESHIFT_HOME="+filepath.Join(dir, "home"), "XDG_CONFIG_HOME="+filepath.Join(dir, "xdg"), "WAKESHIFT_OPENCODE_REARM_MS=60000")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != `{"a":true,"b":false,"c":true}`+"\n"+`{"d":true}` {
		t.Errorf("hooks per load: %s", got)
	}
}
