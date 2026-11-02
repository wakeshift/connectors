package agentplugins

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConnector is a stand-in for the connector: it says its version, logs
// how it was run (and its stdin, for a hook) to $LOG, and for a hook prints
// what a harness reads.
func fakeConnector(version string) []byte {
	return []byte(`#!/bin/sh
case "$1" in
version) echo "` + version + `" ;;
install) echo "install $*" >>"$LOG"; [ -n "${FAIL_INSTALL:-}" ] && { echo "boom" >&2; exit 1; }; mkdir -p "$WAKESHIFT_HOME/$3"; echo "$5" >"$WAKESHIFT_HOME/$3/plugin-issuer" ;;
*) echo "run $* stdin=$(cat)" >>"$LOG"; echo '{"ok":true}' ;;
esac
`)
}

// platform is this machine's build name, as the launcher works it out.
func platform(t *testing.T) string {
	t.Helper()
	arch := map[string]string{"arm64": "arm64", "amd64": "amd64"}[runtime.GOARCH]
	if arch == "" || runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no connector build for this platform")
	}
	return "wakeshift-" + runtime.GOOS + "-" + arch
}

type release struct {
	srv   *httptest.Server
	hits  atomic.Int32
	mu    sync.Mutex
	files map[string][]byte
}

func newRelease(t *testing.T) *release {
	r := &release{files: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		time.Sleep(100 * time.Millisecond) // long enough for hooks that start together to meet at the lock
		r.mu.Lock()
		b, ok := r.files[strings.TrimPrefix(req.URL.Path, "/")]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *release) put(name string, b []byte) {
	r.mu.Lock()
	r.files[name] = b
	r.mu.Unlock()
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// lane is one plugin install: a rendered marketplace and a home.
type lane struct {
	t      *testing.T
	shell  string
	plugin string // plugins/<harness> in the rendered marketplace
	home   string // WAKESHIFT_HOME
	log    string
	env    []string
}

func newLane(t *testing.T, shell string, r *release, version string, served []byte) *lane {
	t.Helper()
	name := platform(t)
	sums := map[string]string{}
	for _, n := range Platforms {
		sums[n] = strings.Repeat("0", 64)
	}
	sums[name] = hexSum(served)
	r.put(name, served)
	dir := t.TempDir()
	if _, err := Render(dir, "https://app.wakeshift.ai", map[string]Pin{
		"codex":   {Version: version, URL: r.srv.URL, Sums: sums},
		"copilot": {Version: version, URL: r.srv.URL, Sums: sums},
	}); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), ".wakeshift")
	l := &lane{t: t, shell: shell, plugin: filepath.Join(dir, "plugins", "codex"), home: home, log: filepath.Join(t.TempDir(), "log")}
	l.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(home), "WAKESHIFT_HOME=" + home, "LOG=" + l.log}
	return l
}

// run runs the plugin's launcher as Codex runs its hooks.
func (l *lane) run(stdin string, args ...string) (stdout, stderr string) {
	l.t.Helper()
	cmd := exec.Command(l.shell, append([]string{filepath.Join(l.plugin, "bin", "wakeshift")}, args...)...)
	cmd.Env = l.env
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Run(); err != nil {
		l.t.Fatalf("the launcher failed (it never should): %v\n%s", err, e.String())
	}
	return o.String(), e.String()
}

func (l *lane) logged() string {
	b, _ := os.ReadFile(l.log)
	return string(b)
}

func (l *lane) bin() string { return filepath.Join(l.home, "bin", "wakeshift") }

func shells(t *testing.T) []string {
	var l []string
	for _, s := range []string{"sh", "dash", "bash"} {
		if p, err := exec.LookPath(s); err == nil {
			l = append(l, p)
		}
	}
	return l
}

func eachShell(t *testing.T, f func(t *testing.T, shell string)) {
	for _, s := range shells(t) {
		t.Run(filepath.Base(s), func(t *testing.T) { f(t, s) })
	}
}

// First run: fetch the pinned connector, check it, install it, trust the
// plugin's issuer, then run the hook with its stdin; only the connector
// writes to stdout. A warm run needs no network.
func TestLauncherFirstRunThenWarm(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		r := newRelease(t)
		l := newLane(t, shell, r, "2026.11.02.1912+abc1234", fakeConnector("2026.11.02.1912+abc1234"))
		out, errOut := l.run(`{"hook":1}`, "hook", "--harness", "codex")
		if out != "{\"ok\":true}\n" || errOut != "" {
			t.Fatalf("stdout %q stderr %q", out, errOut)
		}
		if st, err := os.Stat(l.bin()); err != nil || st.Mode().Perm() != 0o755 {
			t.Fatalf("installed: %v %v", st, err)
		}
		want := "install install --harness codex --issuer https://app.wakeshift.ai --yes --plugin\nrun hook --harness codex stdin={\"hook\":1}\n"
		if got := l.logged(); got != want {
			t.Fatalf("log:\n%s\nwant:\n%s", got, want)
		}
		if left, _ := filepath.Glob(filepath.Join(l.home, "bin", ".*")); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
		r.srv.Close()
		if out, errOut := l.run("", "hook", "--harness", "codex"); out != "{\"ok\":true}\n" || errOut != "" {
			t.Fatalf("warm: %q %q", out, errOut)
		}
		if strings.Count(l.logged(), "--plugin") != 1 {
			t.Fatalf("installed twice: %s", l.logged())
		}
	})
}

// A download that doesn't match the plugin's SHA-256 is refused: nothing is
// installed, the hook does nothing, and it says why on stderr.
func TestLauncherRefusesChecksumMismatch(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		r := newRelease(t)
		l := newLane(t, shell, r, "2026.11.02.1912", fakeConnector("2026.11.02.1912"))
		r.put(platform(t), fakeConnector("2026.11.02.1912")[1:]) // served differs from pinned
		out, errOut := l.run("{}", "hook", "--harness", "codex")
		if out != "" || !strings.Contains(errOut, "didn't match the SHA-256 this plugin carries; nothing was installed") {
			t.Fatalf("stdout %q stderr %q", out, errOut)
		}
		if _, err := os.Stat(l.bin()); err == nil {
			t.Fatal("installed anyway")
		}
		if l.logged() != "" {
			t.Fatalf("ran: %s", l.logged())
		}
		if left, _ := filepath.Glob(filepath.Join(l.home, "bin", ".*")); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
	})
}

// An older connector is replaced; a newer one (from another plugin, or the
// curl installer) is kept.
func TestLauncherVersions(t *testing.T) {
	sh := shells(t)[0]
	r := newRelease(t)
	l := newLane(t, sh, r, "2026.11.02.1912+abc", fakeConnector("2026.11.02.1912+abc"))
	os.MkdirAll(filepath.Dir(l.bin()), 0o700)
	for _, c := range []struct {
		have     string
		replaced bool
	}{
		{"2026.11.01.0900+old", true},
		{"2026.10.30.2359", true},
		{"dev", true},
		{"2026.11.02.1912+other", false},
		{"2026.11.10.0000+new", false},
		{"2027.1.1.0", false},
	} {
		os.WriteFile(l.bin(), fakeConnector(c.have), 0o755)
		l.run("", "hook", "--harness", "codex")
		b, _ := os.ReadFile(l.bin())
		if got := bytes.Contains(b, []byte("2026.11.02.1912+abc")); got != c.replaced {
			t.Errorf("%s: replaced %v, want %v", c.have, got, c.replaced)
		}
	}
}

// A connector whose version is older than the pin (a mislabeled release)
// isn't installed, or every hook would fetch it again.
func TestLauncherRefusesOlderDownload(t *testing.T) {
	r := newRelease(t)
	l := newLane(t, shells(t)[0], r, "2026.11.02.1912", fakeConnector("2026.11.01.0000"))
	_, errOut := l.run("", "hook", "--harness", "codex")
	if !strings.Contains(errOut, "older than the 2026.11.02.1912 this plugin pins") {
		t.Fatalf("stderr %q", errOut)
	}
	if _, err := os.Stat(l.bin()); err == nil {
		t.Fatal("installed")
	}
}

// When the plugin's issuer changes (another ref of the marketplace), the
// connector is told to trust it.
func TestLauncherIssuerChange(t *testing.T) {
	r := newRelease(t)
	l := newLane(t, shells(t)[0], r, "2026.11.02.1912", fakeConnector("2026.11.02.1912"))
	l.run("", "hook", "--harness", "codex")
	if _, err := Render(filepath.Dir(filepath.Dir(l.plugin)), "https://app.staging.wakeshift.ai", nil); err != nil {
		t.Fatal(err)
	}
	l.run("", "hook", "--harness", "codex")
	if got := l.logged(); strings.Count(got, "--plugin") != 2 || !strings.Contains(got, "--issuer https://app.staging.wakeshift.ai --yes --plugin") {
		t.Fatalf("log: %s", got)
	}
	// prepare only makes sure: no hook runs.
	before := l.logged()
	if out, errOut := l.run("", "prepare"); out != "" || errOut != "" || l.logged() != before {
		t.Fatalf("prepare: %q %q %s", out, errOut, l.logged())
	}
}

// A failed trust is said, and tried again on the next run.
func TestLauncherInstallFails(t *testing.T) {
	r := newRelease(t)
	l := newLane(t, shells(t)[0], r, "2026.11.02.1912", fakeConnector("2026.11.02.1912"))
	l.env = append(l.env, "FAIL_INSTALL=1")
	out, errOut := l.run("", "hook", "--harness", "codex")
	if out != "" || !strings.Contains(errOut, "couldn't set up Wakeshift for codex: boom") {
		t.Fatalf("%q %q", out, errOut)
	}
	l.env = l.env[:len(l.env)-1]
	if out, _ := l.run("", "hook", "--harness", "codex"); out == "" {
		t.Fatal("not tried again")
	}
}

// Hooks that start together fetch once, under the lock; a lock left by a
// hook that was killed is taken over.
func TestLauncherLock(t *testing.T) {
	r := newRelease(t)
	l := newLane(t, shells(t)[0], r, "2026.11.02.1912", fakeConnector("2026.11.02.1912"))
	var wg sync.WaitGroup
	outs := make([]string, 6)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], _ = l.run("", "hook", "--harness", "codex")
		}()
	}
	wg.Wait()
	for i, o := range outs {
		if o != "{\"ok\":true}\n" {
			t.Fatalf("hook %d: %q", i, o)
		}
	}
	if n := r.hits.Load(); n != 1 {
		t.Fatalf("%d downloads", n)
	}
	if strings.Count(l.logged(), "--plugin") != 1 {
		t.Fatalf("trusted more than once: %s", l.logged())
	}

	// A stale lock: its holder (a process that has exited) is gone.
	os.Remove(l.bin())
	dead := exec.Command("true")
	dead.Run()
	lock := filepath.Join(l.home, "bin", ".lock")
	os.Mkdir(lock, 0o700)
	os.WriteFile(filepath.Join(lock, "pid"), []byte(fmt.Sprintf("%d\n", dead.Process.Pid)), 0o600)
	if out, errOut := l.run("", "hook", "--harness", "codex"); out == "" {
		t.Fatalf("stale lock not taken over: %q", errOut)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("lock left")
	}
}

// Only https release URLs, but for this machine's.
func TestLauncherRefusesPlainHTTP(t *testing.T) {
	r := newRelease(t)
	l := newLane(t, shells(t)[0], r, "2026.11.02.1912", fakeConnector("2026.11.02.1912"))
	env := filepath.Join(l.plugin, "wakeshift.env")
	b, _ := os.ReadFile(env)
	os.WriteFile(env, bytes.ReplaceAll(b, []byte(r.srv.URL), []byte("http://example.com")), 0o644)
	if _, errOut := l.run("", "hook", "--harness", "codex"); !strings.Contains(errOut, "isn't https") {
		t.Fatalf("%q", errOut)
	}
	if r.hits.Load() != 0 {
		t.Fatal("fetched")
	}
}

// A plugin with no connector pinned yet, or no wakeshift.env, does nothing
// and says so.
func TestLauncherUnpinned(t *testing.T) {
	dir := t.TempDir()
	if _, err := Render(dir, "https://app.wakeshift.ai", nil); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(dir, "plugins", "copilot", "bin", "wakeshift")
	cmd := exec.Command("sh", launcher, "hook", "--harness", "copilot")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "pins no connector yet") {
		t.Fatalf("%v %s", err, out)
	}
	os.Remove(filepath.Join(dir, "plugins", "copilot", "wakeshift.env"))
	out, err = exec.Command("sh", launcher).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "no wakeshift.env") {
		t.Fatalf("%v %s", err, out)
	}
}
