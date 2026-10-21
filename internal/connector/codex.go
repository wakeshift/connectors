package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Codex: its hook and per-thread connector are internal/codexconnector
// (the hook reads the beacon from _meta and starts a process that drives
// Codex's app-server); here is only its installer, so every harness
// installs the same way. Its hook is one PostToolUse entry in
// ~/.codex/hooks.json. Codex's trust covers the command string, so the
// command is fixed (updating the binary doesn't ask again), and a changed
// command (the old `wakeshift-codex hook`) is trusted again in /hooks.

const codexName = "codex"

func init() {
	register(Spec{Name: codexName, Installer: codexInstaller{}, When: "as soon as they arrive, while the thread is open in Codex"})
}

// CodexHome is Codex's directory (CODEX_HOME, default ~/.codex).
func CodexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// CodexHookMatcher is the MCP tools the hook runs after: all of them (it
// exits at once without a beacon), so any Wakeshift app's result can
// restart the connector.
const CodexHookMatcher = "^mcp__"

type codexInstaller struct{}

func (codexInstaller) path() string { return filepath.Join(CodexHome(), "hooks.json") }

func (codexInstaller) entries(bin string) []entry {
	return []entry{{event: "PostToolUse",
		isOur: func(item json.RawMessage) bool { return commandIn(item, bin, codexName) },
		json:  map[string]any{"matcher": CodexHookMatcher, "hooks": []any{map[string]any{"type": "command", "command": HookCommand(bin, codexName)}}}}}
}

// Install adds the hook, replacing one an earlier install added (the
// wakeshift-codex binary's `hook`, or another binary path), and removes
// that earlier binary.
func (i codexInstaller) Install(bin string, out io.Writer) error {
	changed, backup, err := editHooks(i.path(), "hooks", i.entries(bin), false)
	if err != nil {
		return err
	}
	reportEdit(out, i.path(), changed, backup, false)
	old := filepath.Join(Home(), "bin", "wakeshift-codex")
	if old != bin && exists(old) && os.Remove(old) == nil {
		fmt.Fprintf(out, "Removed the earlier connector binary, %s.\n", old)
	}
	return nil
}

func (i codexInstaller) Uninstall(bin string, out io.Writer) error {
	changed, backup, err := editHooks(i.path(), "hooks", i.entries(bin), true)
	if err != nil {
		return err
	}
	reportEdit(out, i.path(), changed, backup, true)
	if changed {
		fmt.Fprintln(out, "Running connectors stop when their thread's window closes.")
	}
	return nil
}
