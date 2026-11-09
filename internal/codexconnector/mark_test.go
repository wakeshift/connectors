package codexconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wakeshift/connectors/internal/connector"
	"github.com/wakeshift/connectors/internal/pluginmark"
)

// withoutMark is a hook's context without the mark's note (markNote).
func withoutMark(ctx string) string {
	var keep []string
	for _, p := range strings.Split(ctx, "\n\n") {
		if !strings.HasPrefix(p, "Wakeshift's plugin is installed in this Codex") && !strings.HasPrefix(p, "Wakeshift's hooks are installed in this Codex") {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n\n")
}

// At SessionStart, which Codex runs as the thread's first turn starts, the
// hook tells the model the mark to pass as an app's harness argument
// (internal/pluginmark): the plugin's when its install left the issuer's
// file, the installer's hooks' otherwise (only while their PostToolUse hook
// is in hooks.json). Not at UserPromptSubmit, not for a
// thread off Codex's daemon (nothing could wake it), and not with no
// Wakeshift trusted.
func TestHookMark(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	const issuer = "https://app.wakeshift.ai"
	run := func(h *Hook, event, th string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"session_id": th, "hook_event_name": event, "source": "startup"})
		var out bytes.Buffer
		h.Run(context.Background(), bytes.NewReader(b), &out)
		if out.Len() == 0 {
			return ""
		}
		var m struct {
			HookSpecificOutput struct{ HookEventName, AdditionalContext string }
		}
		if err := json.Unmarshal(out.Bytes(), &m); err != nil || m.HookSpecificOutput.HookEventName != event {
			t.Fatalf("hook output %q", out.String())
		}
		return m.HookSpecificOutput.AdditionalContext
	}
	markIn := func(ctx string) (string, pluginmark.Mark) {
		t.Helper()
		i := strings.Index(ctx, `pass harness="`)
		if i < 0 {
			t.Fatalf("no mark: %q", ctx)
		}
		rest := ctx[i+len(`pass harness="`):]
		v := rest[:strings.Index(rest, `"`)]
		claim, m, ok := pluginmark.Split(v)
		if !ok || claim != "codex" || !m.TrustsIssuer(issuer) || strings.Contains(v, "wakeshift.ai") {
			t.Fatalf("mark %q", v)
		}
		return v, m
	}
	newHook := func(plugin bool, issuers ...string) *Hook {
		dir := t.TempDir()
		if plugin {
			os.WriteFile(filepath.Join(dir, connector.PluginIssuerFile), []byte(issuer+"\n"), 0o600)
		}
		return &Hook{Settings: Settings{Dir: dir, Issuers: issuers}, Socket: fakeAppServer(t, thread),
			Start: func(string, string, string, string, bool) error { return nil }}
	}

	ctx := run(newHook(true, issuer), EventSessionStart, thread)
	if !strings.HasPrefix(ctx, "Wakeshift's plugin is installed in this Codex session") {
		t.Fatalf("plugin: %q", ctx)
	}
	if _, m := markIn(ctx); m.Hooks {
		t.Fatalf("the plugin's mark says install=hooks: %q", ctx)
	}
	// The installer's hooks mark only while their PostToolUse hook, which
	// redeems the beacon, is still in hooks.json.
	if got := run(newHook(false, issuer), EventSessionStart, thread); got != "" {
		t.Fatalf("installer hooks without PostToolUse: %q", got)
	}
	hooksJSON := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/x/.wakeshift/bin/wakeshift hook --harness codex"}]}]}}`
	os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json"), []byte(hooksJSON), 0o600)
	if got := run(newHook(false, issuer), EventSessionStart, thread); got != "" {
		t.Fatalf("installer hooks, SessionStart only: %q", got)
	}
	hooksJSON = `{"hooks":{"PostToolUse":[{"matcher":"^mcp__","hooks":[{"type":"command","command":"/x/.wakeshift/bin/wakeshift hook --harness codex"}]}]}}`
	os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json"), []byte(hooksJSON), 0o600)
	ctx = run(newHook(false, issuer), EventSessionStart, thread)
	if !strings.HasPrefix(ctx, "Wakeshift's hooks are installed in this Codex session") {
		t.Fatalf("hooks: %q", ctx)
	}
	if _, m := markIn(ctx); !m.Hooks {
		t.Fatalf("the installer's mark lacks install=hooks: %q", ctx)
	}
	if got := run(newHook(true, issuer), EventUserPromptSubmit, thread); got != "" {
		t.Fatalf("UserPromptSubmit: %q", got)
	}
	if got := run(newHook(true, issuer), EventSessionStart, "01a14bd7-0000-7b70-a723-0abb9f38118e"); got != "" {
		t.Fatalf("a thread off the daemon: %q", got)
	}
	if got := run(newHook(true), EventSessionStart, thread); got != "" {
		t.Fatalf("no Wakeshift trusted: %q", got)
	}
}
