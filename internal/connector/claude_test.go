package connector

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Claude Code's real PostToolUse input for an MCP tool (2.1.280): the result
// without _meta (the structured content as a string).
const claudePostToolUse = `{"session_id":"c95dca59-8b63-4ebb-a70e-8d69fde72b92","transcript_path":"TRANSCRIPT","cwd":"/private/tmp/proj","prompt_id":"1924d3a0","permission_mode":"default","hook_event_name":"PostToolUse","tool_name":"mcp__wsprobe__probe","tool_input":{"note":"hello"},"tool_response":"{\"ok\":true}","tool_use_id":"toolu_bdcfcf09e3814e0591d5","duration_ms":2,"mcp_server":{"name":"wsprobe","source":"user"}}`

// The transcript line Claude Code writes for that result.
const claudeTranscriptLine = `{"parentUuid":"bd78","isSidechain":false,"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_bdcfcf09e3814e0591d5","type":"tool_result","content":"{\"ok\":true}"}]},"toolUseResult":"{\"ok\":true}","mcpMeta":{"_meta":{"wakeshift/connector":"BEACON"},"structuredContent":{"ok":true}},"sessionId":"c95dca59-8b63-4ebb-a70e-8d69fde72b92","version":"2.1.280"}`

const claudeStop = `{"session_id":"c95dca59-8b63-4ebb-a70e-8d69fde72b92","transcript_path":"/x.jsonl","cwd":"/p","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"done","background_tasks":[],"session_crons":[]}`

func TestClaudeFormat(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "s.jsonl")
	// Another session's line for the same call ID doesn't count.
	other := strings.Replace(strings.Replace(claudeTranscriptLine, "c95dca59-8b63", "00000000-8b63", 1), "BEACON", "OTHER", 1)
	os.WriteFile(tr, []byte(`{"type":"summary"}`+"\n"+other+"\n"), 0o600)
	go func() { // written just after the hook starts, as Claude Code does
		time.Sleep(150 * time.Millisecond)
		f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
		f.WriteString(claudeTranscriptLine + "\n")
		f.Close()
	}()
	in, err := claudeFormat{}.Parse([]byte(strings.Replace(claudePostToolUse, "TRANSCRIPT", tr, 1)))
	if err != nil || in.Event != ToolResult || in.Conversation != "c95dca59-8b63-4ebb-a70e-8d69fde72b92" || in.ToolCallID != "toolu_bdcfcf09e3814e0591d5" {
		t.Fatalf("%+v %v", in, err)
	}
	if got := BeaconIn(in.ToolResult); got != "BEACON" {
		t.Fatalf("beacon %q from %s", got, in.ToolResult)
	}
	// A relative or non-transcript path isn't read.
	in, _ = claudeFormat{}.Parse([]byte(strings.Replace(claudePostToolUse, "TRANSCRIPT", "s.jsonl", 1)))
	if BeaconIn(in.ToolResult) != "" {
		t.Fatal("read a relative path")
	}

	in, _ = claudeFormat{}.Parse([]byte(claudeStop))
	if in.Event != Stop || in.Conversation == "" {
		t.Fatalf("stop %+v", in)
	}
	out, _ := claudeFormat{}.Continue(in, "hello")
	if out.ExitCode != 2 || string(out.Stderr) != "hello\n" || len(out.Stdout) != 0 {
		t.Fatalf("continue %+v", out)
	}
	out, _ = claudeFormat{}.Context(in, "note")
	var c struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if json.Unmarshal(out.Stdout, &c) != nil || c.HookSpecificOutput.HookEventName != "PostToolUse" || c.HookSpecificOutput.AdditionalContext != "note" {
		t.Fatalf("context %s", out.Stdout)
	}
}

const claudeSettings = `{
  "model": "opus",
  "hooks": {
    "PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "lint.sh"}]}],
    "Notification": [{"hooks": [{"type": "command", "command": "say hi"}]}]
  },
  "permissions": {"allow": ["Bash(ls)"]}
}`

func TestClaudeInstall(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(claudeSettings), 0o644)
	spec, _ := Lookup("claude-code")
	const bin = "/Users/ada/.wakeshift/bin/wakeshift"
	var out bytes.Buffer
	if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	if !has(out.String(), "Added the Wakeshift hooks", ".before-wakeshift-") {
		t.Fatalf("%s", out.String())
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	if !has(s, `"lint.sh"`, `"say hi"`, `"Bash(ls)"`, `"asyncRewake": true`, `"timeout": 86400`, `"rewakeSummary": "Woken by Wakeshift"`, `"rewakeMessage": "Wakeshift woke this session (not the user)."`, `"async": true`, `"matcher": "mcp__.*"`,
		bin+" hook --harness claude-code --rewake") || strings.Index(s, `"model"`) > strings.Index(s, `"hooks"`) || strings.Index(s, `"hooks"`) > strings.Index(s, `"permissions"`) {
		t.Fatalf("settings: %s", s)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
	var f struct {
		Hooks map[string][]json.RawMessage
	}
	json.Unmarshal(b, &f)
	// SessionStart: the waiter and the hook that adds the mark's context.
	if len(f.Hooks["PostToolUse"]) != 2 || len(f.Hooks["Stop"]) != 1 || len(f.Hooks["SessionStart"]) != 2 ||
		!has(s, `"command": "`+bin+` hook --harness claude-code --event session-context"`, `"timeout": 60`) {
		t.Fatalf("hooks %v", f.Hooks)
	}
	if LoadSettings("claude-code").Issuers[0] != "https://app.wakeshift.ai" {
		t.Fatal("issuer not trusted")
	}
	// Twice: no change, no backup.
	out.Reset()
	spec.Install(bin, "https://app.wakeshift.ai", &out)
	if b2, _ := os.ReadFile(path); !bytes.Equal(b, b2) || !has(out.String(), "already") {
		t.Fatalf("twice: %s", out.String())
	}
	// Uninstall leaves everything else as it was.
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	f.Hooks = nil
	json.Unmarshal(b, &f)
	if strings.Contains(string(b), "wakeshift") || len(f.Hooks["PostToolUse"]) != 1 || f.Hooks["Stop"] != nil || f.Hooks["SessionStart"] != nil || !has(string(b), `"lint.sh"`, `"say hi"`, `"model"`) {
		t.Fatalf("after uninstall: %s", b)
	}
}

// No settings file: one is made; invalid JSON is refused and left alone.
func TestClaudeInstallFiles(t *testing.T) {
	home := setHome(t)
	spec, _ := Lookup("claude-code")
	if err := spec.Install("/bin/my tools/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !has(string(b), `'/bin/my tools/wakeshift' hook --harness claude-code`) {
		t.Fatalf("%s", b)
	}
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte("{oops"), 0o600)
	if err := spec.Install("/bin/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{}); err == nil {
		t.Fatal("installed over invalid JSON")
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); string(b) != "{oops" {
		t.Fatalf("changed: %s", b)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "elsewhere"))
	spec.Install("/bin/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{})
	if _, err := os.Stat(filepath.Join(home, "elsewhere", "settings.json")); err != nil {
		t.Fatal("CLAUDE_CONFIG_DIR ignored")
	}
	if err := spec.Install("/bin/wakeshift", "http://evil.example", &bytes.Buffer{}); err == nil {
		t.Fatal("a plain-http issuer")
	}
}

// The plugin's session-context hook (SessionStart) adds the mark for the
// model to pass as an app's harness argument, not under claude -p; any other
// event named session-context adds nothing.
func TestClaudeSessionContext(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "")
	in, err := claudeFormat{}.ParseEvent(ClaudeSessionContextEvent, []byte(`{"session_id":"s1","transcript_path":"/x.jsonl","hook_event_name":"SessionStart","source":"startup"}`))
	if err != nil || in.Event != SessionContext || in.Conversation != "s1" {
		t.Fatalf("parse: %+v %v", in, err)
	}
	if in, _ := (claudeFormat{}).ParseEvent(ClaudeSessionContextEvent, []byte(claudeStop)); in.Event != OtherEvent {
		t.Fatalf("a Stop read as the session's context: %+v", in)
	}
	if in, _ := (claudeFormat{}).ParseEvent("", []byte(claudeStop)); in.Event != Stop {
		t.Fatalf("no event: %+v", in)
	}
	const mark = "claude-code; wakeshift-plugin=1.0; trusts=0123456789abcdef"
	for _, hooks := range []bool{false, true} {
		out, ok := claudeFormat{}.SessionContext(mark, hooks)
		var o struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		what := "Wakeshift's plugin is installed"
		if hooks {
			what = "Wakeshift's hooks are installed"
		}
		if !ok || json.Unmarshal(out.Stdout, &o) != nil || o.HookSpecificOutput.HookEventName != "SessionStart" ||
			!strings.Contains(o.HookSpecificOutput.AdditionalContext, `pass harness="`+mark+`"`) || !strings.HasPrefix(o.HookSpecificOutput.AdditionalContext, what) {
			t.Fatalf("context (hooks %v): %s %v", hooks, out.Stdout, ok)
		}
	}
	if _, ok := (claudeFormat{}).SessionContext("", true); ok {
		t.Error("context with no mark")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "0")
	if _, ok := (claudeFormat{}).SessionContext(mark, false); ok {
		t.Error("context under claude -p")
	}
}

// oldClaudeSettings is ~/.claude/settings.json as Wakeshift's installer
// wrote it before the session-context hook: three hooks.
func oldClaudeSettings(bin string) string {
	return `{
  "model": "opus",
  "hooks": {
    "PostToolUse": [{"matcher": "mcp__.*", "hooks": [{"type": "command", "command": "` + bin + ` hook --harness claude-code", "async": true, "timeout": 30}]}],
    "SessionStart": [{"hooks": [{"type": "command", "command": "` + bin + ` hook --harness claude-code --rewake", "asyncRewake": true, "timeout": 86400, "rewakeSummary": "Woken by Wakeshift", "rewakeMessage": "Wakeshift woke this session (not the user)."}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "` + bin + ` hook --harness claude-code --rewake", "asyncRewake": true, "timeout": 86400, "rewakeSummary": "Woken by Wakeshift", "rewakeMessage": "Wakeshift woke this session (not the user)."}]}]
  }
}`
}

// An install of the hooks from before the session-context hook, whose
// waiter a newer connector runs (another harness's install or the plugin
// replaced the binary they run), gets that hook added, once, so the
// sessions that start from then on pass the mark; installing again does
// the same. Never a plugin install, nor hooks that run another binary.
func TestUpgradeClaudeHooks(t *testing.T) {
	const bin = "/Users/ada/.wakeshift/bin/wakeshift"
	setup := func(t *testing.T) (settings string, set Settings) {
		home := setHome(t)
		settings = filepath.Join(home, ".claude", "settings.json")
		os.MkdirAll(filepath.Dir(settings), 0o700)
		os.WriteFile(settings, []byte(oldClaudeSettings(bin)), 0o600)
		set = LoadSettings("claude-code")
		os.MkdirAll(set.Dir, 0o700)
		set.Trust("https://app.wakeshift.ai")
		set.Save()
		return settings, set
	}
	starts := func(t *testing.T, path string) []json.RawMessage {
		t.Helper()
		b, _ := os.ReadFile(path)
		var f struct{ Hooks map[string][]json.RawMessage }
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		return f.Hooks["SessionStart"]
	}

	t.Run("old install", func(t *testing.T) {
		settings, _ := setup(t)
		UpgradeClaudeHooks(bin)
		l := starts(t, settings)
		if len(l) != 2 || !claudeContextHook(l[1]) || !strings.Contains(string(l[1]), bin+" hook --harness claude-code --event session-context") {
			t.Fatalf("SessionStart: %s", l)
		}
		b, _ := os.ReadFile(settings)
		if !has(string(b), `"model": "opus"`, "--rewake") || len(backupsOf(settings)) != 1 {
			t.Fatalf("settings: %s", b)
		}
		// Once: taken out again, it stays out.
		spec, _ := Lookup("claude-code")
		os.WriteFile(settings, []byte(oldClaudeSettings(bin)), 0o600)
		UpgradeClaudeHooks(bin)
		if l := starts(t, settings); len(l) != 1 {
			t.Fatalf("added twice: %s", l)
		}
		// Installing again writes it, and twice makes no second one.
		spec.Install(bin, "https://app.wakeshift.ai", &bytes.Buffer{})
		spec.Install(bin, "https://app.wakeshift.ai", &bytes.Buffer{})
		if l := starts(t, settings); len(l) != 2 {
			t.Fatalf("install: %s", l)
		}
	})
	t.Run("already there", func(t *testing.T) {
		settings, _ := setup(t)
		spec, _ := Lookup("claude-code")
		spec.Install(bin, "https://app.wakeshift.ai", &bytes.Buffer{})
		before, _ := os.ReadFile(settings)
		UpgradeClaudeHooks(bin)
		if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
			t.Fatalf("changed: %s", after)
		}
	})
	t.Run("plugin", func(t *testing.T) {
		settings, set := setup(t)
		os.WriteFile(filepath.Join(set.Dir, PluginIssuerFile), []byte("https://app.wakeshift.ai\n"), 0o600)
		UpgradeClaudeHooks(bin)
		if l := starts(t, settings); len(l) != 1 {
			t.Fatalf("a plugin install's settings changed: %s", l)
		}
	})
	t.Run("another binary", func(t *testing.T) {
		settings, _ := setup(t)
		UpgradeClaudeHooks("/opt/other/wakeshift")
		if l := starts(t, settings); len(l) != 1 {
			t.Fatalf("hooks of another binary changed: %s", l)
		}
	})
	t.Run("not installed", func(t *testing.T) {
		settings, set := setup(t)
		os.RemoveAll(set.Dir)
		UpgradeClaudeHooks(bin)
		if l := starts(t, settings); len(l) != 1 {
			t.Fatalf("uninstalled, but changed: %s", l)
		}
	})
	t.Run("uninstall", func(t *testing.T) {
		settings, _ := setup(t)
		UpgradeClaudeHooks(bin)
		spec, _ := Lookup("claude-code")
		if err := spec.Uninstall(bin, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(settings); strings.Contains(string(b), "wakeshift") {
			t.Fatalf("after uninstall: %s", b)
		}
	})
}

// The session-context hook's mark says which install it's from: the
// installer's hooks (no plugin issuer file) mark install=hooks, with the
// hooks' wording; the plugin's doesn't.
func TestClaudeSessionContextHook(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "")
	setHome(t)
	spec, _ := Lookup("claude-code")
	if err := spec.Install("/bin/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		h := spec.NewHook(nil, nil)
		h.EventName = ClaudeSessionContextEvent
		var out, errOut bytes.Buffer
		h.Run(t.Context(), strings.NewReader(`{"session_id":"s1","transcript_path":"/x.jsonl","hook_event_name":"SessionStart","source":"startup"}`), &out, &errOut)
		return out.String()
	}
	out := run()
	if !has(out, "Wakeshift's hooks are installed", "; install=hooks") {
		t.Fatalf("hooks: %s", out)
	}
	set := LoadSettings("claude-code")
	os.WriteFile(filepath.Join(set.Dir, PluginIssuerFile), []byte("https://app.wakeshift.ai\n"), 0o600)
	out = run()
	if !has(out, "Wakeshift's plugin is installed", "wakeshift-plugin=") || strings.Contains(out, "install=hooks") {
		t.Fatalf("plugin: %s", out)
	}
}
