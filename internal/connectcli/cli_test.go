package connectcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func homes(t *testing.T) string {
	t.Helper()
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
	out, code := run("wakeshift", "install", "--harness", "codex", "--issuer", "https://app.wakeshift.ai")
	if code != 0 || !strings.Contains(out, "/hooks and press t") || !strings.Contains(out, "Removed the earlier connector binary") {
		t.Fatalf("%d %s", code, out)
	}
	b, _ := os.ReadFile(hooks)
	s := string(b)
	if strings.Contains(s, "wakeshift-codex hook") || strings.Count(s, "hook --harness codex") != 1 || !strings.Contains(s, "notify.sh") ||
		strings.Index(s, `"PostToolUse"`) > strings.Index(s, `"Stop"`) {
		t.Fatalf("hooks.json: %s", s)
	}
	if _, err := os.Stat(oldBin); !os.IsNotExist(err) {
		t.Fatal("old binary left")
	}
	if out, code := run("wakeshift", "install", "--harness", "codex", "--issuer", "https://app.wakeshift.ai"); code != 0 || !strings.Contains(out, "already") {
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
		out, code := run("wakeshift", "install", "--harness", harness, "--issuer", "https://app.wakeshift.ai")
		if code != 0 || !strings.Contains(out, "Next: ") {
			t.Fatalf("%s: %d %s", harness, code, out)
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
	if out, code := run("wakeshift", "install", "--harness", "notepad", "--issuer", "https://app.wakeshift.ai"); code != 1 || !strings.Contains(out, "unknown harness") {
		t.Fatalf("unknown: %d %s", code, out)
	}
	if out, code := run("wakeshift", "install", "--issuer", "https://app.wakeshift.ai"); code != 1 || !strings.Contains(out, "--harness is required") {
		t.Fatalf("no harness: %d %s", code, out)
	}
	if _, code := run("wakeshift", "install", "--harness", "claude-code"); code != 1 {
		t.Fatal("no issuer")
	}
	// Gemini CLI is retired: nothing to install, and the list says why.
	if out, code := run("wakeshift", "install", "--harness", "gemini", "--issuer", "https://app.wakeshift.ai"); code != 1 || !strings.Contains(out, "unknown harness") {
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
