package connector

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCopilot makes the hook run under a Copilot process with args (nil:
// none found) and the managed-policy files managed (path → content; a nil
// content is a file that can't be read).
func fakeCopilot(t *testing.T, args []string, managed map[string][]byte, mdm bool) {
	t.Helper()
	oldArgs, oldPaths := copilotProcArgs, copilotManagedPaths
	t.Cleanup(func() { copilotProcArgs, copilotManagedPaths = oldArgs, oldPaths })
	copilotProcArgs = func() ([]string, error) {
		if args == nil {
			return nil, errCopilotUnknown
		}
		return args, nil
	}
	dir := t.TempDir()
	var files []string
	for name, b := range managed {
		p := filepath.Join(dir, name)
		if b == nil {
			os.Mkdir(p, 0o700) // reading a directory fails
		} else {
			os.WriteFile(p, b, 0o600)
		}
		files = append(files, p)
	}
	files = append(files, filepath.Join(dir, "absent.json")) // missing: no rules
	copilotManagedPaths = func() ([]string, bool) { return files, mdm }
}

// The permissionRequest hook gives no answer while the user denies the
// wait command (or every shell command) with --deny-tool, while their
// organization's managed rules deny or ask for it, and while it can't
// tell: Copilot then applies the rule, or asks, as it would have.
func TestCopilotStepsAsideWhenDenied(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".wakeshift", "bin", "wakeshift")
	os.MkdirAll(filepath.Dir(bin), 0o700)
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700)
	wait := WaitCommand(bin)
	if wait == "" {
		t.Skipf("the temp directory %s can't hold a wait command", home)
	}
	if err := ownedByUser(bin); err != nil {
		t.Skipf("the temp directory can't hold the user's own binary: %v", err)
	}
	h := specHook(t, "copilot", newFakeWakeshift(t), newFakePoller())
	h.WaitCommand = wait
	ask := func() string { return runAs(h, "permissionRequest", copilotAsks("bash", wait)) }
	const allow = `{"behavior":"allow"}`

	for _, args := range [][]string{
		{"copilot", "--deny-tool", "shell"},
		{"copilot", "--deny-tool=shell"},
		{"copilot", "--deny-tool", "Shell"},
		{"copilot", "--deny-tool", "shell()"},
		{"copilot", "--deny-tool", "shell(" + wait + ")"},
		{"copilot", "--deny-tool=shell(" + bin + ")"},
		{"copilot", "--deny-tool", "shell(" + bin + ":*)"},
		{"copilot", "--deny-tool", "shell(" + filepath.Dir(bin) + ":*)"},
		{"copilot", "--deny-tool", "shell(wakeshift)"},
		{"copilot", "--deny-tool", "shell(wakeshift wait)"},
		{"copilot", "--deny-tool", "shell(~/.wakeshift/bin/wakeshift)"},
		{"copilot", "--deny-tool", "write", "shell(git push)", "shell", "-p", "hi"},
		{"copilot", "--deny-tool", "write,shell(git push:*),shell(wakeshift:*)"},
		{"copilot", "--allow-all-tools", "--deny-tool", "shell(" + wait},
		{"copilot", "--model", "x", "--deny-tool=shell", "--allow-tool", "shell(" + wait + ")"},
		{"copilot", "--acp"},
	} {
		fakeCopilot(t, args, nil, false)
		if out := ask(); out != "" {
			t.Errorf("%q: answered %q", args, out)
		}
	}
	// Rules that don't cover the wait command leave it approved.
	for _, args := range [][]string{
		{"copilot"},
		{"copilot", "--deny-tool", "write", "shell(git push)", "shell(rm:*)", "MyMCP(tool)", "url(https://example.com)"},
		{"copilot", "--deny-tool=shell(git:*),write(.env)", "-p", "shell"},
		{"copilot", "--deny-url", "shell", "--allow-tool", "shell"},
	} {
		fakeCopilot(t, args, nil, false)
		if out := ask(); strings.TrimSpace(out) != allow {
			t.Errorf("%q: answered %q", args, out)
		}
	}
	// No Copilot process found (or its arguments unreadable): no answer.
	fakeCopilot(t, nil, nil, false)
	if out := ask(); out != "" {
		t.Errorf("no Copilot process: answered %q", out)
	}

	// Managed rules: the device file has them at the top, a cached server
	// response under "response" after a comment line.
	for name, doc := range map[string]string{
		"device deny":       `{"permissions":{"deny":["shell"]}}`,
		"device ask":        `{"permissions":{"ask":["shell(` + wait + `)"]}}`,
		"device odd rule":   `{"permissions":{"deny":[{"kind":"shell"}]}}`,
		"device odd list":   `{"permissions":{"deny":"shell"}}`,
		"device odd shape":  `{"permissions":["shell"]}`,
		"server deny":       "// Disposable cache for enterprise managed settings, safe to delete. Managed automatically.\n" + `{"schemaVersion":1,"response":{"permissions":{"deny":["shell(wakeshift:*)"]}}}`,
		"server ask":        "// cache\n" + `{"response":{"settings":{"permissions":{"ask":["bash"]}}}}`,
		"not JSON":          `{"permissions":`,
		"unreadable (nil)":  "",
		"comment, no JSON":  "// only a comment",
		"deny, comma rules": `{"permissions":{"deny":["write,shell"]}}`,
	} {
		files := map[string][]byte{"m.json": []byte(doc)}
		if doc == "" {
			files = map[string][]byte{"m.json": nil}
		}
		fakeCopilot(t, []string{"copilot"}, files, false)
		if out := ask(); out != "" {
			t.Errorf("managed %s: answered %q", name, out)
		}
	}
	for name, doc := range map[string]string{
		"no policy":   "// cache\n" + `{"schemaVersion":1,"response":null,"ignoredPaths":[]}`,
		"other rules": `{"permissions":{"deny":["shell(git push)","write"],"allow":["shell"],"disableBypassPermissionsMode":"disable"}}`,
	} {
		fakeCopilot(t, []string{"copilot"}, map[string][]byte{"m.json": []byte(doc)}, false)
		if out := ask(); strings.TrimSpace(out) != allow {
			t.Errorf("managed %s: answered %q", name, out)
		}
	}
	// An MDM profile for Copilot, which the hook can't read: no answer.
	fakeCopilot(t, []string{"copilot"}, nil, true)
	if out := ask(); out != "" {
		t.Errorf("MDM: answered %q", out)
	}
}

func TestCopilotRuleSplit(t *testing.T) {
	got := splitCopilotRules(" write, shell(git push, x), ,MyMCP(a,b) ")
	if want := []string{"write", "shell(git push, x)", "MyMCP(a,b)"}; !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestParseProcArgs2(t *testing.T) {
	b := binary.LittleEndian.AppendUint32(nil, 3)
	b = append(b, "/opt/x/copilot\x00\x00\x00\x00copilot\x00--deny-tool\x00shell(a b)\x00HOME=/h\x00"...)
	got, err := parseProcArgs2(b)
	if err != nil || !slices.Equal(got, []string{"copilot", "--deny-tool", "shell(a b)"}) {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := parseProcArgs2(b[:12]); err == nil {
		t.Fatal("short buffer parsed")
	}
}

// procArgs reads a real process's arguments, spaces and all.
func TestProcArgs(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no procArgs here")
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 10", "sh", "--deny-tool", "shell(a b)")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	want := []string{"/bin/sh", "-c", "sleep 10", "sh", "--deny-tool", "shell(a b)"}
	var got []string
	var err error
	for i := 0; i < 50; i++ { // until the child has exec'd
		if got, err = procArgs(cmd.Process.Pid); err == nil && slices.Equal(got, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("got %q, %v", got, err)
}

// The real lookup finds Copilot's command line past a shell and another
// process in between: this test binary runs as "copilot" with deny flags,
// which runs /usr/bin/time, which runs sh, which runs the probe.
func TestCopilotProcArgsFindsCopilot(t *testing.T) {
	switch os.Getenv("WAKESHIFT_TEST_COPILOT") {
	case "copilot":
		cmd := exec.Command("/usr/bin/time", "/bin/sh", "-c", `"$0" -test.run='^TestCopilotProcArgsFindsCopilot$'; :`, os.Args[0])
		cmd.Env = append(os.Environ(), "WAKESHIFT_TEST_COPILOT=probe")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		return
	case "probe":
		args, err := copilotProcArgs()
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(os.Getenv("WAKESHIFT_TEST_OUT"), []byte(strings.Join(args, "\x00")), 0o600)
		return
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no procArgs here")
	}
	if _, err := os.Stat("/usr/bin/time"); err != nil {
		t.Skip("no /usr/bin/time")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	copilot, out := filepath.Join(dir, "copilot"), filepath.Join(dir, "out")
	if err := os.Symlink(self, copilot); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(copilot, "-test.run=^TestCopilotProcArgsFindsCopilot$", "--", "--deny-tool", "shell(a b)")
	cmd.Env = append(os.Environ(), "WAKESHIFT_TEST_COPILOT=copilot", "WAKESHIFT_TEST_OUT="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(string(b), "\x00")
	if want := []string{copilot, "-test.run=^TestCopilotProcArgsFindsCopilot$", "--", "--deny-tool", "shell(a b)"}; !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	if rules, _ := copilotFlagRules(got[1:]); !slices.Equal(rules, []string{"shell(a b)"}) {
		t.Fatalf("rules %q", rules)
	}
}

// Copilot's managed-policy files: the cached server policy's directory is
// listed, and one that can't be listed means the hook can't tell.
func TestCopilotManagedPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	cache := filepath.Join(home, ".cache")
	if runtime.GOOS == "darwin" {
		cache = filepath.Join(home, "Library", "Caches")
	}
	files, unknown := copilotManagedPaths()
	if unknown {
		t.Fatal("no cache: unknown")
	}
	dir := filepath.Join(cache, "copilot", "managed-settings")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "a.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(dir, "a.lock"), nil, 0o600)
	files, unknown = copilotManagedPaths()
	if unknown || !slices.Contains(files, filepath.Join(dir, "a.json")) || slices.Contains(files, filepath.Join(dir, "a.lock")) {
		t.Fatalf("got %q, %v", files, unknown)
	}
	if os.Getuid() == 0 {
		return // root lists any directory
	}
	os.Chmod(dir, 0o000)
	defer os.Chmod(dir, 0o700)
	if _, unknown = copilotManagedPaths(); !unknown {
		t.Fatal("an unlistable cache wasn't unknown")
	}
}

// Under no Copilot process (here, go test), the real lookup can't tell.
func TestCopilotProcArgsOutsideCopilot(t *testing.T) {
	if args, err := copilotProcArgs(); err == nil {
		t.Skipf("go test runs under Copilot here: %q", args[0])
	} else if !errors.Is(err, errCopilotUnknown) {
		t.Fatalf("got %v", err)
	}
	if denied, _ := copilotDeniesWait("/Users/ada/.wakeshift/bin/wakeshift wait"); !denied {
		t.Fatal("approved outside Copilot")
	}
}
