package connector

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Claude Code (2.1.280, verified). Its MCP client sends
// servers the tool call's ID (_meta["claudecode/toolUseId"]) but not the
// session's, and its PostToolUse hooks get the result without _meta; the
// result's _meta is in the transcript ("mcpMeta"), written just after the
// hook starts. So:
//
//   - PostToolUse (async, MCP tools): find the result's _meta in the
//     transcript by tool_use_id, verify the beacon (bound to that call),
//     redeem it with the session ID.
//   - SessionStart and Stop (asyncRewake): wait on the session's link; on a
//     message, write it to stderr and exit 2, which wakes the idle session
//     (or reaches a busy one between tool calls). One waiter per Claude
//     process (CLAUDE_PID): a newer one replaces it, which also covers
//     /clear (a new session ID in the same process).

const claudeName = "claude-code"

func init() {
	register(Spec{Name: claudeName, Format: claudeFormat{}, Installer: claudeInstaller{},
		When:       "as soon as they arrive, even while the session is idle",
		RewakeWait: 23 * time.Hour, // under the hook's timeout (86400s)
		Rule:       ClaudeRule,
		Tools:      &claudeTools,
	})
}

// claudeTools: mcp__<server>__<tool> in the user's settings, an exact
// rule for one tool of one server (mcp__<server> alone, or with __*, would
// be every tool: never written). The server's name is checked against
// ~/.claude.json (the user's and every project's servers) and the
// conversation folder's .mcp.json.
var claudeTools = ToolRules{
	Path: func() string { return filepath.Join(ClaudeConfigDir(), "settings.json") },
	Rule: func(server, tool string) string { return "mcp__" + server + "__" + tool },
	Configured: func(server string, dirs []string) ([]string, error) {
		l, err := mcpEntries(claudeJSON(), server, false, func(top *Object) []*Object {
			var list []*Object
			if o := childObject(top, "mcpServers"); o != nil {
				list = append(list, o)
			}
			if projects := childObject(top, "projects"); projects != nil {
				for _, k := range projects.keys {
					if p, err := projects.Child(k); err == nil {
						if o := childObject(p, "mcpServers"); o != nil {
							list = append(list, o)
						}
					}
				}
			}
			return list
		})
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			if !filepath.IsAbs(d) {
				continue
			}
			p, err := mcpEntries(filepath.Join(d, ".mcp.json"), server, false, func(top *Object) []*Object {
				if o := childObject(top, "mcpServers"); o != nil {
					return []*Object{o}
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			l = append(l, p...)
		}
		return l, nil
	},
}

// claudeJSON is Claude Code's own state file, where `claude mcp add`
// keeps the user's and each project's servers.
func claudeJSON() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

type claudeFormat struct{}

type claudeInput struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	StopHookActive bool            `json:"stop_hook_active"`
	Cwd            string          `json:"cwd"`
	MCPServer      struct {
		Name string `json:"name"`
	} `json:"mcp_server"`
}

func (claudeFormat) Parse(b []byte) (Input, error) {
	var c claudeInput
	if err := json.Unmarshal(b, &c); err != nil {
		return Input{}, err
	}
	in := Input{Conversation: c.SessionID, Continued: c.StopHookActive}
	switch c.HookEventName {
	case "PostToolUse":
		in.Event, in.ToolName, in.ToolCallID, in.ToolResult = ToolResult, c.ToolName, c.ToolUseID, c.ToolResponse
		in.Server = c.MCPServer.Name
		if in.Server == "" {
			in.Server = MCPServerOf(c.ToolName)
		}
		in.MCPTool = MCPToolOf(c.ToolName)
		if c.Cwd != "" {
			in.Dirs = []string{c.Cwd}
		}
		// The result's own _meta is in the transcript, unless the response
		// has it (a beacon line in its text, which anyone's words can be,
		// doesn't stand in for it).
		if _, at := FindBeacon(c.ToolResponse); at != InResultMeta && c.ToolUseID != "" {
			if meta := claudeTranscriptMeta(c.TranscriptPath, c.SessionID, c.ToolUseID, 3*time.Second); meta != nil {
				in.ToolResult = meta
			}
		}
	case "Stop", "SessionStart":
		// Waiters: the installer runs these with --rewake (asyncRewake).
		in.Event = Stop
		in.transcript = c.TranscriptPath
	}
	return in, nil
}

// LastBeacon: the newest tool result in the session's transcript whose
// own _meta (the line's mcpMeta) carries a beacon, and the tool it called.
func (claudeFormat) LastBeacon(in Input) (FoundBeacon, bool) {
	path := in.transcript
	if path == "" || !filepath.IsAbs(path) || filepath.Ext(path) != ".jsonl" {
		return FoundBeacon{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return FoundBeacon{}, false
	}
	defer f.Close()
	names := map[string]string{} // tool_use ID → tool name
	var found FoundBeacon
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && json.Valid(line) {
			var l struct {
				SessionID string `json:"sessionId"`
				Message   struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
				MCPMeta json.RawMessage `json:"mcpMeta"`
			}
			if json.Unmarshal(line, &l) == nil && l.SessionID == in.Conversation {
				var parts []struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					Name      string `json:"name"`
					ToolUseID string `json:"tool_use_id"`
				}
				json.Unmarshal(l.Message.Content, &parts)
				for _, p := range parts {
					switch {
					case p.Type == "tool_use" && p.ID != "":
						names[p.ID] = p.Name
					case p.Type == "tool_result" && p.ToolUseID != "" && l.MCPMeta != nil:
						if tok, at := FindBeacon(l.MCPMeta); tok != "" && at == InResultMeta {
							found = FoundBeacon{Token: tok, Call: p.ToolUseID}
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if found.Token == "" {
		return FoundBeacon{}, false
	}
	found.ToolName = names[found.Call]
	return found, found.ToolName != ""
}

func (claudeFormat) Context(in Input, text string) (Output, bool) {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "additionalContext": text}})
	return Output{Stdout: append(b, '\n')}, true
}

// LateContext: PostToolUse runs async, and Claude Code shows an async
// hook's additionalContext only at the next user prompt, so the
// "connected" note also comes with the next wake.
func (claudeFormat) LateContext() bool { return true }

// Continue: exit 2 with the text on stderr. For an asyncRewake hook that
// wakes the session; for a plain Stop hook it continues it.
func (claudeFormat) Continue(in Input, text string) (Output, bool) {
	return Output{Stderr: []byte(text + "\n"), ExitCode: 2}, true
}

// claudeTranscriptMeta finds the result's _meta for a tool call in the
// session's transcript (Claude Code writes it as the line's mcpMeta just
// after PostToolUse hooks start), waiting up to wait for it to appear. Only
// a line of this session for this call counts.
func claudeTranscriptMeta(path, session, call string, wait time.Duration) json.RawMessage {
	if path == "" || !filepath.IsAbs(path) || filepath.Ext(path) != ".jsonl" {
		return nil
	}
	deadline := time.Now().Add(wait)
	for {
		if m := scanTranscript(path, session, call); m != nil {
			return m
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func scanTranscript(path, session, call string) json.RawMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	// The newest lines are at the end, but transcripts are small enough to
	// read through.
	r := bufio.NewReaderSize(f, 1<<16)
	var found json.RawMessage
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && json.Valid(line) {
			var l struct {
				SessionID string `json:"sessionId"`
				Message   struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
				MCPMeta json.RawMessage `json:"mcpMeta"`
			}
			if json.Unmarshal(line, &l) == nil && l.SessionID == session && l.MCPMeta != nil {
				var parts []struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id"`
				}
				json.Unmarshal(l.Message.Content, &parts)
				for _, p := range parts {
					if p.Type == "tool_result" && p.ToolUseID == call {
						found = l.MCPMeta // {"_meta": {...}, "structuredContent": ...}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return found
			}
			return nil
		}
	}
}

// ClaudeConfigDir is Claude Code's config directory (CLAUDE_CONFIG_DIR,
// default ~/.claude).
func ClaudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// ClaudeUnattended: a -p (print) session, which can't be woken while idle
// (its asyncRewake hooks run as plain blocking hooks), so waiters exit at
// once there.
func ClaudeUnattended() bool { return os.Getenv("CLAUDE_CODE_SESSION_ATTENDED") == "0" }

type claudeInstaller struct{}

func (claudeInstaller) path() string { return filepath.Join(ClaudeConfigDir(), "settings.json") }

func (claudeInstaller) entries(bin string) []entry {
	isOur := func(item json.RawMessage) bool { return commandIn(item, bin, claudeName) }
	waiter := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": HookCommand(bin, claudeName, "--rewake"),
		"asyncRewake": true, "timeout": 86400}}}
	return []entry{
		{event: "PostToolUse", isOur: isOur, json: map[string]any{"matcher": "mcp__.*", "hooks": []any{map[string]any{"type": "command",
			"command": HookCommand(bin, claudeName), "async": true, "timeout": 30}}}},
		{event: "SessionStart", isOur: isOur, json: waiter},
		{event: "Stop", isOur: isOur, json: waiter},
	}
}

func (i claudeInstaller) Install(bin string, out io.Writer) error {
	changed, backup, err := editHooks(i.path(), "hooks", i.entries(bin), false)
	if err != nil {
		return err
	}
	reportEdit(out, i.path(), changed, backup, false)
	return nil
}

func (i claudeInstaller) Uninstall(bin string, out io.Writer) error {
	changed, backup, err := editHooks(i.path(), "hooks", i.entries(bin), true)
	if err != nil {
		return err
	}
	reportEdit(out, i.path(), changed, backup, true)
	return nil
}
