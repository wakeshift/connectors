package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Cursor (cursor-agent 2026.09.23, verified; the IDE reads the same file,
// from source and docs). User hooks are in
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
		// No Rule (CursorRule): cursor-agent matches Shell rules on the
		// command with its redirects taken off, and lets an allowlisted
		// command redirect into workspace and temporary files, so the rule
		// would let `… wait > <workspace file>` run unasked; the shell
		// empties the file before `wakeshift wait` can refuse.
		Wait:  true,
		Tools: &cursorTools,
	})
}

// cursorTools: Mcp(<server>:<tool>) in cli-config.json. Cursor matches
// both parts as globs, case-insensitively, so names with no glob
// characters (ValidToolName) match exactly that server's tool. The
// server's name comes from afterMCPExecution (mcp_server_name, the name
// its rules match), checked against ~/.cursor/mcp.json and the
// workspace's .cursor/mcp.json, by the URL the call went to.
var cursorTools = ToolRules{
	Path: func() string {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".cursor", "cli-config.json")
	},
	Rule:      func(server, tool string) string { return "Mcp(" + server + ":" + tool + ")" },
	MustExist: true,
	Configured: func(server string, dirs []string) ([]string, error) {
		home, _ := os.UserHomeDir()
		files := []string{filepath.Join(home, ".cursor", "mcp.json")}
		for _, d := range dirs {
			if filepath.IsAbs(d) {
				files = append(files, filepath.Join(d, ".cursor", "mcp.json"))
			}
		}
		var l []string
		for _, f := range files {
			e, err := mcpEntries(f, server, true, func(top *Object) []*Object {
				if o := childObject(top, "mcpServers"); o != nil {
					return []*Object{o}
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			l = append(l, e...)
		}
		return l, nil
	},
}

type cursorFormat struct{}

// CallServers: Cursor's postToolUse names the tool, not its server;
// afterMCPExecution, which runs just before it for the same call, names
// both (cursor-agent 2026.10.16).
func (cursorFormat) CallServers() bool { return true }

type cursorInput struct {
	ConversationID string   `json:"conversation_id"`
	HookEventName  string   `json:"hook_event_name"`
	ToolName       string   `json:"tool_name"`
	ToolOutput     string   `json:"tool_output"`
	ToolUseID      string   `json:"tool_use_id"`
	LoopCount      int      `json:"loop_count"`
	ServerName     string   `json:"mcp_server_name"`
	ServerURL      string   `json:"mcp_server_url"`
	Cwd            string   `json:"cwd"`
	WorkspaceRoots []string `json:"workspace_roots"`
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
		// MCP:<tool>: Cursor names the tool, not its server.
		in.Event, in.ToolName, in.MCPTool, in.ToolCallID = ToolResult, c.ToolName, strings.TrimPrefix(c.ToolName, "MCP:"), c.ToolUseID
		in.ToolResult = json.RawMessage(c.ToolOutput) // the result as JSON text
		if !json.Valid(in.ToolResult) {
			in.ToolResult, _ = json.Marshal(c.ToolOutput)
		}
		if c.Cwd != "" {
			in.Dirs = append(in.Dirs, c.Cwd)
		}
		for _, d := range c.WorkspaceRoots {
			if !slices.Contains(in.Dirs, d) {
				in.Dirs = append(in.Dirs, d)
			}
		}
	case "afterMCPExecution":
		in.Event, in.ToolName, in.ToolCallID, in.Server, in.ServerURL = CallDone, c.ToolName, c.ToolUseID, c.ServerName, c.ServerURL
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
		// Records each MCP call's server, for its postToolUse (the server's
		// name for tool rules, and pinning its beacons to one app).
		{event: "afterMCPExecution", isOur: isOur, json: map[string]any{"command": HookCommand(bin, cursorName), "timeout": 10}},
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

// TextOnly: its hooks get a result's content as JSON text, never _meta.
func (cursorFormat) TextOnly() bool { return true }
