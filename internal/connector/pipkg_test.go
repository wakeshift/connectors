package connector

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// piPackageFile is the npm package's extension, beside its package.json.
var piPackageFile = filepath.Join("..", "..", "plugins", "pi", "index.js")

// The npm package's index.js is PiPackage, so a change to the extension the
// installer writes reaches the package too (go test ./internal/connector
// -run TestPiPackage -update writes it).
func TestPiPackage(t *testing.T) {
	want := PiPackage()
	if *updatePackage {
		if err := os.WriteFile(piPackageFile, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(piPackageFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s isn't PiPackage(): run go test ./internal/connector -run TestPiPackage -update", piPackageFile)
	}
	if strings.Count(want, PiDefaultIssuers) != 1 {
		t.Error("plugins/pack-npm.mjs replaces the default issuers' line: it must be there once")
	}
	if strings.Contains(want, "__WAKESHIFT_BIN__") || strings.Contains(want, "const ready = () => true") {
		t.Error("the package has the installer's head")
	}
	// pi loads the package's extension from its manifest.
	var pkg struct {
		Name     string
		Keywords []string
		Pi       struct{ Extensions []string }
		Files    []string
		Scripts  map[string]string
		Optional map[string]string `json:"optionalDependencies"`
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "plugins", "pi", "package.json"))
	if err != nil || json.Unmarshal(b, &pkg) != nil {
		t.Fatal(err)
	}
	if pkg.Name != "@wakeshift/pi" || len(pkg.Pi.Extensions) != 1 || pkg.Pi.Extensions[0] != "./index.js" || len(pkg.Scripts) != 0 || len(pkg.Optional) != 4 ||
		!strings.Contains(strings.Join(pkg.Keywords, ","), "pi-package") || strings.Join(pkg.Files, ",") != "index.js" {
		t.Fatalf("package.json: %s", b)
	}
}

// piPackageDriver loads the package with a fake pi and has it connect a
// session from a beacon, printing what the fake bridge was given.
const piPackageDriver = `
import ext from "./node_modules/@wakeshift/pi/index.js"
import { readFileSync, existsSync } from "node:fs"
const handlers = {}
const pi = { on: (n, h) => { handlers[n] = h; return () => {} }, sendMessage: () => {}, getAllTools: () => [] }
ext(pi)
// A second copy (the file an earlier install wrote, loaded too): it doesn't act.
const handlers2 = {}
ext({ on: (n, h) => { handlers2[n] = h; return () => {} }, sendMessage: () => {}, getAllTools: () => [] })
const ctx = { mode: "tui", isIdle: () => true, sessionManager: { getSessionId: () => "ses_pkg" } }
await handlers.session_start({ type: "session_start", reason: "startup" }, ctx)
await handlers2.session_start({ type: "session_start", reason: "startup" }, ctx)
const beacon = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: aaaaaaaaaa.bbbbbbbbbb.cccccccccc)"
const ev = () => ({ type: "tool_result", toolName: "mcp__s__watch", toolCallId: "c1", input: {}, isError: false, details: { server: "s", tool: "watch" },
  content: [{ type: "text", text: "Watching.\n\n" + beacon }] })
const second = await handlers2.tool_result(ev(), ctx)
const first = await handlers.tool_result(ev(), ctx)
await new Promise((r) => setTimeout(r, 400))
const lines = existsSync(process.env.FAKE_BRIDGE_DIR + "/in") ? readFileSync(process.env.FAKE_BRIDGE_DIR + "/in", "utf8").trim().split("\n") : []
await handlers.session_shutdown({}, ctx)
await handlers2.session_shutdown({}, ctx)
console.log(JSON.stringify({ first: first?.content?.map((c) => c.text).join("\n"), second: second === undefined, bridged: lines.length }))
`

// The package runs the connector from its platform package (making it
// executable), pins the default Wakeshift where the bridge reads it,
// removes the extension file an earlier install wrote (only that one), and
// acts once when both are loaded.
func TestPiPackageRuns(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir, home := piPackageLayout(t)
	agent := filepath.Join(home, "agent")
	os.MkdirAll(filepath.Join(agent, "extensions"), 0o700)
	old := filepath.Join(agent, "extensions", "wakeshift.ts")
	os.WriteFile(old, []byte(PiExtension("/x/wakeshift")), 0o600)
	theirs := filepath.Join(agent, "extensions", "other.ts")
	os.WriteFile(theirs, []byte("export default function () {}"), 0o600)
	r, out := runPiPackage(t, node, dir, home)
	if !strings.Contains(r.First, "Wakeshift: connected") || strings.Contains(r.First, "wakeshift-beacon") || !r.Second || r.Bridged < 1 {
		t.Fatalf("run: %s", out)
	}
	plat := filepath.Join(dir, "node_modules", "@wakeshift", "connect-"+npmPlatform())
	if st, err := os.Stat(filepath.Join(plat, "bin", "wakeshift")); err != nil || st.Mode()&0o111 == 0 {
		t.Fatal("the connector wasn't made executable")
	}
	var cfg struct {
		Issuers     []string `json:"issuers"`
		InstalledAt string   `json:"installed_at"`
	}
	b, _ := os.ReadFile(filepath.Join(home, ".wakeshift", "pi", "config.json"))
	if json.Unmarshal(b, &cfg) != nil || len(cfg.Issuers) != 1 || cfg.Issuers[0] != "https://app.wakeshift.ai" || cfg.InstalledAt == "" {
		t.Fatalf("config: %s", b)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the installer's extension file is still there")
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Fatal("removed someone else's extension")
	}
}

// A config file the package can't read as a JSON object is left as it is,
// and the package connects nothing (it still takes beacon lines out).
func TestPiPackageLeavesABadConfig(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir, home := piPackageLayout(t)
	cfg := filepath.Join(home, ".wakeshift", "pi", "config.json")
	os.MkdirAll(filepath.Dir(cfg), 0o700)
	for _, bad := range []string{"{not json", "null", "[]"} {
		os.WriteFile(cfg, []byte(bad), 0o600)
		r, out := runPiPackage(t, node, dir, home)
		if strings.Contains(r.First, "connected") || strings.Contains(r.First, "wakeshift-beacon: a") || r.Bridged != 0 {
			t.Fatalf("%s: %s", bad, out)
		}
		if b, _ := os.ReadFile(cfg); string(b) != bad {
			t.Fatalf("%s: rewritten to %s", bad, b)
		}
	}
}

// piPackageLayout lays the package out as npm installs it: the extension in
// node_modules/@wakeshift/pi, this platform's connector (the fake bridge,
// not executable, as a tarball may leave it) beside it.
func piPackageLayout(t *testing.T) (dir, home string) {
	t.Helper()
	dir = t.TempDir()
	plat := filepath.Join(dir, "node_modules", "@wakeshift", "connect-"+npmPlatform())
	os.MkdirAll(filepath.Join(plat, "bin"), 0o755)
	os.WriteFile(filepath.Join(plat, "package.json"), []byte(`{"name":"@wakeshift/connect-`+npmPlatform()+`","version":"0.0.0"}`), 0o644)
	os.WriteFile(filepath.Join(plat, "bin", "wakeshift"), []byte(piFakeBridge), 0o644)
	own := filepath.Join(dir, "node_modules", "@wakeshift", "pi")
	os.MkdirAll(own, 0o755)
	os.WriteFile(filepath.Join(own, "package.json"), []byte(`{"name":"@wakeshift/pi","version":"0.0.0","type":"module"}`), 0o644)
	os.WriteFile(filepath.Join(own, "index.js"), []byte(PiPackage()), 0o644)
	os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(piPackageDriver), 0o600)
	return dir, filepath.Join(dir, "home")
}

type piPackageRun struct {
	First   string
	Second  bool
	Bridged int
}

func runPiPackage(t *testing.T, node, dir, home string) (piPackageRun, string) {
	t.Helper()
	os.Remove(filepath.Join(dir, "in"))
	cmd := exec.Command(node, "driver.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FAKE_BRIDGE_DIR="+dir, "WAKESHIFT_HOME="+filepath.Join(home, ".wakeshift"), "PI_CODING_AGENT_DIR="+filepath.Join(home, "agent"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var r piPackageRun
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return r, string(out)
}
