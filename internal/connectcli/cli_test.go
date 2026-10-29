package connectcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/connector"
)

func homes(t *testing.T) string {
	t.Helper()
	// No terminal in tests: an install that would ask is refused.
	confirm := Confirm
	Confirm = func(string) (bool, error) { return false, errors.New("no terminal") }
	t.Cleanup(func() { Confirm = confirm })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WAKESHIFT_HOME", filepath.Join(home, ".wakeshift"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	for _, k := range []string{"XDG_CONFIG_HOME", "CLAUDE_CONFIG_DIR", "COPILOT_HOME", "CURSOR_CONFIG_DIR"} {
		t.Setenv(k, "")
	}
	return home
}

func run(args ...string) (string, int) {
	var out bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &out)
	return out.String(), code
}

// A live install from before (wakeshift-codex's `hook` in hooks.json) keeps
// working until it's reinstalled; reinstalling replaces its hook (and its
// binary) with this one's, keeping everything else.
func TestCodexReinstall(t *testing.T) {
	home := homes(t)
	oldBin := filepath.Join(home, ".wakeshift", "bin", "wakeshift-codex")
	os.MkdirAll(filepath.Dir(oldBin), 0o700)
	os.WriteFile(oldBin, []byte("old"), 0o755)
	hooks := filepath.Join(home, ".codex", "hooks.json")
	os.MkdirAll(filepath.Dir(hooks), 0o700)
	os.WriteFile(hooks, []byte(`{"hooks":{"PostToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"notify.sh"}]},`+
		`{"matcher":"^mcp__","hooks":[{"type":"command","command":"`+oldBin+` hook"}]}],"Stop":[{"hooks":[{"type":"command","command":"notify.sh"}]}]}}`), 0o600)

	// The old command still runs Codex's hook (nothing on stdin: nothing to do).
	if out, code := run("wakeshift-codex", "hook"); code != 0 || out != "" {
		t.Fatalf("old hook: %d %q", code, out)
	}
	out, code := run("wakeshift", "install", "--harness", "codex", "--issuer", "https://app.wakeshift.ai", "--yes")
	if code != 0 || !strings.Contains(out, "/hooks and press t") || !strings.Contains(out, "Removed the earlier connector binary") {
		t.Fatalf("%d %s", code, out)
	}
	b, _ := os.ReadFile(hooks)
	s := string(b)
	if strings.Contains(s, "wakeshift-codex hook") || strings.Count(s, "hook --harness codex") != 3 || !strings.Contains(s, "notify.sh") ||
		strings.Index(s, `"PostToolUse"`) > strings.Index(s, `"Stop"`) {
		t.Fatalf("hooks.json: %s", s)
	}
	if _, err := os.Stat(oldBin); !os.IsNotExist(err) {
		t.Fatal("old binary left")
	}
	if out, code := run("wakeshift", "install", "--harness", "codex", "--issuer", "https://app.wakeshift.ai", "--yes"); code != 0 || !strings.Contains(out, "already") {
		t.Fatalf("twice: %d %s", code, out)
	}
	if out, code := run("wakeshift", "uninstall", "--harness", "codex"); code != 0 || !strings.Contains(out, "Removed") {
		t.Fatalf("uninstall: %s", out)
	}
	if b, _ := os.ReadFile(hooks); strings.Contains(string(b), "--harness") || !strings.Contains(string(b), "notify.sh") {
		t.Fatalf("after uninstall: %s", b)
	}
}

// Each harness installs, says its trust step, and uninstalls.
func TestInstallEach(t *testing.T) {
	home := homes(t)
	for harness, file := range map[string]string{
		"codex":       ".codex/hooks.json",
		"claude-code": ".claude/settings.json",
		"opencode":    ".config/opencode/plugins/wakeshift.ts",
		"cursor":      ".cursor/hooks.json",
		"copilot":     ".copilot/hooks/wakeshift.json",
		"antigravity": ".gemini/config/hooks.json",
	} {
		out, code := run("wakeshift", "install", "--harness", harness, "--issuer", "https://app.wakeshift.ai", "--yes")
		if code != 0 || !strings.Contains(out, "Next: ") {
			t.Fatalf("%s: %d %s", harness, code, out)
		}
		// Last, where an agent that ran it reads it: call the tool again.
		if !strings.HasSuffix(out, AfterInstall+"\n") {
			t.Fatalf("%s: the install doesn't end with the call-again line: %s", harness, out)
		}
		if _, err := os.Stat(filepath.Join(home, file)); err != nil {
			t.Fatalf("%s: %v", harness, err)
		}
		// cursor-agent loads new hooks only when it restarts.
		if harness == "cursor" && (!strings.Contains(out, "Next: Restart cursor-agent") || strings.Contains(out, "Start a new chat")) {
			t.Fatalf("cursor's next step: %s", out)
		}
		if out, code := run("wakeshift", "uninstall", "--harness", harness); code != 0 {
			t.Fatalf("%s uninstall: %s", harness, out)
		}
	}
	if out, code := run("wakeshift", "install", "--harness", "notepad", "--issuer", "https://app.wakeshift.ai", "--yes"); code != 1 || !strings.Contains(out, "unknown harness") {
		t.Fatalf("unknown: %d %s", code, out)
	}
	if out, code := run("wakeshift", "install", "--issuer", "https://app.wakeshift.ai", "--yes"); code != 1 || !strings.Contains(out, "--harness is required") {
		t.Fatalf("no harness: %d %s", code, out)
	}
	if _, code := run("wakeshift", "install", "--harness", "claude-code"); code != 1 {
		t.Fatal("no issuer")
	}
	// Gemini CLI is retired: nothing to install, and the list says why.
	if out, code := run("wakeshift", "install", "--harness", "gemini", "--issuer", "https://app.wakeshift.ai", "--yes"); code != 1 || !strings.Contains(out, "unknown harness") {
		t.Fatalf("gemini: %d %s", code, out)
	}
	if out, _ := run("wakeshift", "harnesses"); !strings.Contains(out, "retired: Google replaced Gemini CLI with Antigravity CLI (agy)") {
		t.Fatalf("harnesses: %s", out)
	}
	// An unknown harness's hook still never fails the harness.
	if _, code := run("wakeshift", "hook", "--harness", "notepad"); code != 0 {
		t.Fatal("hook for an unknown harness failed")
	}
}

// After uninstall, the hooks a running session read at startup (Claude Code
// keeps them) do nothing, and so don't make the harness's directory again.
func TestHookAfterUninstall(t *testing.T) {
	home := homes(t)
	t.Setenv("CLAUDE_PID", "4242")
	for harness, inputs := range map[string][]string{
		"claude-code": {`{"session_id":"s","hook_event_name":"Stop"}`,
			`{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"mcp__agentchat__join","tool_response":"{}"}`},
		"codex": {`{"session_id":"01a18699-4ee4-77e0-abdb-9ace6e215367","hook_event_name":"UserPromptSubmit"}`},
	} {
		if out, code := run("wakeshift", "install", "--harness", harness, "--issuer", "https://app.wakeshift.ai", "--yes"); code != 0 {
			t.Fatalf("%s: %d %s", harness, code, out)
		}
		if out, code := run("wakeshift", "uninstall", "--harness", harness); code != 0 {
			t.Fatalf("%s: %d %s", harness, code, out)
		}
		dir := filepath.Join(home, ".wakeshift", harness)
		if _, err := os.Lstat(dir); err == nil {
			t.Fatalf("%s: uninstall left %s", harness, dir)
		}
		for _, in := range inputs {
			args := []string{"wakeshift", "hook", "--harness", harness}
			if strings.Contains(in, "Stop") {
				args = append(args, "--rewake")
			}
			var out bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			code := Run(ctx, args, strings.NewReader(in), &out, &out)
			cancel()
			if code != 0 || out.Len() != 0 {
				t.Fatalf("%s: %d %q", harness, code, out.String())
			}
			if _, err := os.Lstat(dir); err == nil {
				t.Fatalf("%s: a hook after uninstall made %s again", harness, dir)
			}
		}
	}
}

// Claude Code's waiter does nothing in claude -p, where it can't wake.
func TestClaudeWaiterUnattended(t *testing.T) {
	homes(t)
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "0")
	var out bytes.Buffer
	code := Run(context.Background(), []string{"wakeshift", "hook", "--harness", "claude-code", "--rewake"},
		strings.NewReader(`{"session_id":"s","hook_event_name":"Stop"}`), &out, &out)
	if code != 0 || out.Len() != 0 {
		t.Fatalf("%d %q", code, out.String())
	}
}

// Trusting a new issuer asks first (unless --yes, which the install script
// from that Wakeshift passes); an issuer already trusted doesn't ask again.
func TestInstallAsksBeforeTrustingAnIssuer(t *testing.T) {
	homes(t)
	var asked []string
	answer := false
	Confirm = func(q string) (bool, error) { asked = append(asked, q); return answer, nil }
	out, code := run("wakeshift", "install", "--harness", "claude-code", "--issuer", "https://evil.example")
	if code != 1 || !strings.Contains(out, "wasn't trusted") || len(asked) != 1 || !strings.Contains(asked[0], "https://evil.example") {
		t.Fatalf("declined: %d %s %q", code, out, asked)
	}
	if got := connector.LoadSettings("claude-code").Issuers; len(got) != 0 {
		t.Fatalf("trusted after declining: %v", got)
	}
	answer = true
	if out, code := run("wakeshift", "install", "--harness", "claude-code", "--issuer", "https://app.wakeshift.ai"); code != 0 || len(asked) != 2 {
		t.Fatalf("accepted: %d %s", code, out)
	}
	// Already trusted: reinstalling doesn't ask.
	if out, code := run("wakeshift", "install", "--harness", "claude-code", "--issuer", "https://app.wakeshift.ai/"); code != 0 || len(asked) != 2 {
		t.Fatalf("reinstall: %d %s %d", code, out, len(asked))
	}
	// No terminal to ask on: refused, saying how.
	Confirm = func(string) (bool, error) { return false, errors.New("no terminal") }
	if out, code := run("wakeshift", "install", "--harness", "claude-code", "--issuer", "https://other.example"); code != 1 || !strings.Contains(out, "--yes") {
		t.Fatalf("no terminal: %d %s", code, out)
	}
	if out, code := run("wakeshift", "install", "--harness", "claude-code", "--issuer", "https://other.example", "--yes"); code != 0 {
		t.Fatalf("--yes: %d %s", code, out)
	}
}

// `wakeshift wait` takes no arguments (a prefix rule's extra words change
// nothing), won't write into a file a redirect named, and without a
// connected conversation says so in a hub.closed line.
func TestWaitCommandLine(t *testing.T) {
	homes(t)
	for _, k := range []string{"CURSOR_CONVERSATION_ID", "ANTIGRAVITY_CONVERSATION_ID", "CLAUDE_CODE_SESSION_ID"} {
		t.Setenv(k, "")
	}
	for _, extra := range [][]string{{"--harness", "cursor"}, {"x"}, {"https://evil.example/v1/stream/wait?link=x"}, {"-h"}} {
		out, code := run(append([]string{"wakeshift", "wait"}, extra...)...)
		if code != 2 || !strings.Contains(out, connector.ErrWaitArgs.Error()) {
			t.Fatalf("%q: %d %q", extra, code, out)
		}
	}
	out, code := run("wakeshift", "wait")
	if code != 1 || !strings.HasPrefix(out, `{"type":"hub.closed","reason":"not_connected"`) {
		t.Fatalf("%d %q", code, out)
	}

	// Redirected into a file outside the temporary directories: refused,
	// nothing written to it.
	dir, err := os.MkdirTemp(".", "waitout-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f, _ := os.Create(filepath.Join(dir, "zshrc"))
	defer f.Close()
	var errs bytes.Buffer
	if code := Run(context.Background(), []string{"wakeshift", "wait"}, strings.NewReader(""), f, &errs); code != 2 ||
		!strings.Contains(errs.String(), connector.ErrWaitRedirected.Error()) {
		t.Fatalf("%d %q", code, errs.String())
	}
	if st, _ := f.Stat(); st.Size() != 0 {
		t.Fatal("wrote into the file")
	}
	// stderr redirected too: refused without a word.
	if code := Run(context.Background(), []string{"wakeshift", "wait"}, strings.NewReader(""), &errs, f); code != 2 {
		t.Fatalf("%d", code)
	}
	if st, _ := f.Stat(); st.Size() != 0 {
		t.Fatal("wrote into the file")
	}
}
