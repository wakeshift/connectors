package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cursor (cursor-agent 2026.09.23, verified; the IDE reads the same file,
// from source and docs; docs/harnesses.md). User hooks are in
// ~/.cursor/hooks.json (always under the home directory), which must say
// "version": 1 or Cursor ignores the whole file. Cursor gives hooks an MCP
// result's text only (no _meta) and tells MCP servers neither the
// conversation nor the call, so:
//
//   - postToolUse (MCP tools): the beacon comes in the result's text
//     (BeaconText), unbound; verify and redeem it, take it out of what the
//     model sees (updated_mcp_tool_output), and say "connected"
//     (additional_context).
//   - stop: listen for a while; on messages, {"followup_message": …}
//     continues the conversation. Interactive sessions (the CLI's TUI and
//     the IDE) only: cursor-agent -p runs no stop hooks.

const cursorName = "cursor"

func init() {
	register(Spec{Name: cursorName, Format: cursorFormat{}, Installer: cursorInstaller{},
		When:     "if they come within a minute after the agent finishes a turn, otherwise at the start of the next one",
		StopWait: 60 * time.Second, // the hook's timeout is 90s; Cursor shows "Working" meanwhile
		Unbound:  true,
	})
}

type cursorFormat struct{}

type cursorInput struct {
	ConversationID string `json:"conversation_id"`
	HookEventName  string `json:"hook_event_name"`
	ToolName       string `json:"tool_name"`
	ToolOutput     string `json:"tool_output"`
	ToolUseID      string `json:"tool_use_id"`
	LoopCount      int    `json:"loop_count"`
}

func (cursorFormat) Parse(b []byte) (Input, error) {
	var c cursorInput
	if err := json.Unmarshal(b, &c); err != nil {
		return Input{}, err
	}
	in := Input{Conversation: c.ConversationID, Continued: c.LoopCount > 0, raw: b}
	switch c.HookEventName {
	case "postToolUse":
		if !strings.HasPrefix(c.ToolName, "MCP:") {
			return in, nil
		}
		in.Event, in.ToolName, in.ToolCallID = ToolResult, c.ToolName, c.ToolUseID
		in.ToolResult = json.RawMessage(c.ToolOutput) // the result as JSON text
		if !json.Valid(in.ToolResult) {
			in.ToolResult, _ = json.Marshal(c.ToolOutput)
		}
	case "stop":
		in.Event = Stop
	}
	return in, nil
}

func (f cursorFormat) Context(in Input, text string) (Output, bool) {
	out := map[string]any{}
	if text != "" {
		out["additional_context"] = text
	}
	if s, ok := f.stripped(in); ok {
		out["updated_mcp_tool_output"] = s
	}
	if len(out) == 0 {
		return Output{}, false
	}
	b, _ := json.Marshal(out)
	return Output{Stdout: append(b, '\n')}, true
}

func (f cursorFormat) Strip(in Input) (Output, bool) { return f.Context(in, "") }

// stripped is the result's text without its beacon line, if it had one.
func (cursorFormat) stripped(in Input) (string, bool) {
	var c cursorInput
	if json.Unmarshal(in.raw, &c) != nil || in.Event != ToolResult || !strings.Contains(c.ToolOutput, "wakeshift-beacon:") {
		return "", false
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal([]byte(c.ToolOutput), &r) != nil {
		return StripBeaconText(c.ToolOutput), true
	}
	var parts []string
	for _, p := range r.Content {
		if p.Type == "text" {
			if t := StripBeaconText(p.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n"), true
}

func (cursorFormat) Continue(in Input, text string) (Output, bool) {
	b, _ := json.Marshal(map[string]any{"followup_message": text})
	return Output{Stdout: append(b, '\n')}, true
}

type cursorInstaller struct{}

// Cursor reads user hooks from ~/.cursor/hooks.json whatever
// CURSOR_CONFIG_DIR says.
func (cursorInstaller) path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cursor", "hooks.json")
}

func (cursorInstaller) entries(bin string) []entry {
	isOur := func(item json.RawMessage) bool { return commandIn(item, bin, cursorName) }
	return []entry{
		{event: "postToolUse", isOur: isOur, json: map[string]any{"command": HookCommand(bin, cursorName), "matcher": "^MCP:", "timeout": 20}},
		{event: "stop", isOur: isOur, json: map[string]any{"command": HookCommand(bin, cursorName), "timeout": 90, "loop_limit": nil}},
	}
}

func (i cursorInstaller) Install(bin string, out io.Writer) error {
	path := i.path()
	// Without "version": 1 Cursor ignores the file: say so if other hooks
	// in it will start running.
	top, err := ReadJSONFile(path)
	if err != nil {
		return err
	}
	_, hasVersion := top.Get("version")
	others := 0
	if hooks, err := top.Child("hooks"); err == nil {
		for _, k := range hooks.keys {
			l, _ := hooks.List(k)
			for _, item := range l {
				if !commandIn(item, bin, cursorName) {
					others++
				}
			}
		}
	}
	changed, backup, err := editHooks(path, "hooks", i.entries(bin), false, func(top *Object) bool {
		if _, ok := top.Get("version"); ok {
			return false
		}
		top.Set("version", json.RawMessage(`1`))
		return true
	})
	if err != nil {
		return err
	}
	reportEdit(out, path, changed, backup, false)
	if changed && !hasVersion && others > 0 {
		fmt.Fprintf(out, "Note: %s had no \"version\": 1, so Cursor wasn't running its %d other hooks; with it added, Cursor runs them too. The backup has the file as it was.\n", path, others)
	}
	return nil
}

// onlyDefaults: what's left is at most the "version": 1 install adds.
func (cursorInstaller) onlyDefaults(o *Object) bool {
	for _, k := range o.keys {
		if v, _ := o.Get(k); k != "version" || !jsonEqual(v, json.RawMessage(`1`)) {
			return false
		}
	}
	return true
}

func (i cursorInstaller) Uninstall(bin string, out io.Writer) error {
	changed, backup, err := editHooks(i.path(), "hooks", i.entries(bin), true)
	if err != nil {
		return err
	}
	reportEdit(out, i.path(), changed, backup, true)
	return nil
}
