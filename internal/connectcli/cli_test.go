package connectcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	if code != 0 || !strings.Contains(out, "codex resume --last") || !strings.Contains(out, "Removed the earlier connector binary") {
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
	// Capy (a webhook watch it registers) and Muse (a command it starts):
	// nothing to install (Muse's finishes with each batch: one-shot).
	if out, _ := run("wakeshift", "harnesses"); !strings.Contains(out, "retired: Google replaced Gemini CLI with Antigravity CLI (agy)") ||
		!strings.Contains(out, "Capy (agent-webhook: nothing to install)") || !strings.Contains(out, "Muse (one-shot: nothing to install)") {
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

// Under claude -p with a text prompt (unattended, no streaming input on
// Claude's command line), where Claude Code would run the waiters as
// blocking hooks, every hook of Wakeshift's does nothing (#1272): no mark,
// no redeem or link, no note, no claim, no waiter. The session behaves as
// one without the plugin. Claude's process here is this test's parent,
// read for real (CLAUDE_PID): its command line has no streaming input.
func TestClaudeHooksDoNothingUnderPrintMode(t *testing.T) {
	home := homes(t)
	if out, code := run("wakeshift", "install", "--harness", "claude-code", "--issuer", "https://app.wakeshift.ai", "--yes"); code != 0 {
		t.Fatalf("install: %d %s", code, out)
	}
	dir := filepath.Join(home, ".wakeshift", "claude-code")
	tree := func() string {
		var l []string
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil {
				l = append(l, fmt.Sprintf("%s %d", p, info.Size()))
			}
			return nil
		})
		return strings.Join(l, "\n")
	}
	const session = "5e551011-0000-4000-8000-0000000012f3"
	start := `{"session_id":"` + session + `","transcript_path":"/x.jsonl","hook_event_name":"SessionStart","source":"startup"}`
	hook := func(in string, extra ...string) (string, int, time.Duration) {
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		began := time.Now()
		code := Run(ctx, append([]string{"wakeshift", "hook", "--harness", "claude-code"}, extra...), strings.NewReader(in), &out, &out)
		return out.String(), code, time.Since(began)
	}
	// Attended, the session-context hook gives the mark.
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "")
	if out, _, _ := hook(start, "--event", connector.ClaudeSessionContextEvent); !strings.Contains(out, "pass harness=") {
		t.Fatalf("attended: no mark: %q", out)
	}
	before := tree()

	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "0")
	t.Setenv("CLAUDE_PID", strconv.Itoa(os.Getppid()))
	if mode := connector.ClaudeSessionMode(); mode != connector.ClaudeOff {
		t.Fatalf("this test's parent read as %v", mode)
	}
	post := `{"session_id":"` + session + `","transcript_path":"/x.jsonl","hook_event_name":"PostToolUse","tool_name":"mcp__ci__watch","tool_use_id":"toolu_1",` +
		`"tool_response":{"content":[{"type":"text","text":"ok"}],"_meta":{"wakeshift/connector":"eyJhbGciOiJFZERTQSJ9.e30.c2ln"}}}`
	pre := `{"session_id":"` + session + `","transcript_path":"/x.jsonl","hook_event_name":"PreToolUse","tool_name":"mcp__ci__watch","tool_use_id":"toolu_1","tool_input":{}}`
	sub := `{"session_id":"` + session + `","transcript_path":"/x.jsonl","hook_event_name":"SubagentStart","agent_id":"a1"}`
	stop := `{"session_id":"` + session + `","transcript_path":"/x.jsonl","hook_event_name":"Stop"}`
	for name, tc := range map[string]struct {
		in    string
		extra []string
	}{
		"session-context":     {start, []string{"--event", connector.ClaudeSessionContextEvent}},
		"PostToolUse":         {post, nil},
		"PreToolUse claim":    {pre, []string{"--event", connector.ClaudeClaimEvent}},
		"SubagentStart":       {sub, []string{"--event", connector.ClaudeSubagentContextEvent}},
		"SessionStart waiter": {start, []string{"--rewake"}},
		"Stop waiter":         {stop, []string{"--rewake"}},
	} {
		out, code, took := hook(tc.in, tc.extra...)
		if code != 0 || out != "" || took > 3*time.Second {
			t.Fatalf("%s: exit %d %q after %v", name, code, out, took)
		}
	}
	if after := tree(); after != before {
		t.Fatalf("a hook under claude -p wrote to Wakeshift's directory:\nbefore\n%s\nafter\n%s", before, after)
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

// install --plugin (what the plugins' launcher runs): trusts the issuer and
// writes no hooks, says nothing about a trust step (the launcher logs it),
// still asks before trusting a new issuer without --yes, and takes out the
// hooks a curl install wrote.
func TestInstallPlugin(t *testing.T) {
	home := homes(t)
	if out, code := run("wakeshift", "install", "--harness", "codex", "--issuer", "https://app.wakeshift.ai", "--plugin"); code != 1 || !strings.Contains(out, "needs your confirmation") {
		t.Fatalf("without --yes: %d %s", code, out)
	}
	if out, code := run("wakeshift", "install", "--harness", "codex", "--issuer", "https://app.wakeshift.ai", "--yes"); code != 0 {
		t.Fatalf("curl install: %d %s", code, out)
	}
	out, code := run("wakeshift", "install", "--harness", "codex", "--issuer", "https://app.wakeshift.ai", "--yes", "--plugin")
	if code != 0 || strings.Contains(out, "Next: ") || strings.Contains(out, AfterInstall) || !strings.Contains(out, "Removed the hooks an earlier Wakeshift install wrote") {
		t.Fatalf("--plugin: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); err == nil {
		t.Fatal("hooks.json left")
	}
	if b, err := os.ReadFile(filepath.Join(home, ".wakeshift", "codex", connector.PluginIssuerFile)); err != nil || string(b) != "https://app.wakeshift.ai\n" {
		t.Fatalf("plugin-issuer: %q %v", b, err)
	}
	if out, code := run("wakeshift", "install", "--harness", "notepad", "--issuer", "https://app.wakeshift.ai", "--yes", "--plugin"); code != 1 || !strings.Contains(out, "unknown harness") {
		t.Fatalf("unknown: %d %s", code, out)
	}
}
