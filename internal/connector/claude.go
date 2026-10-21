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

// Claude Code (2.1.280, verified; docs/harnesses.md). Its MCP client sends
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
	})
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
		if BeaconIn(c.ToolResponse) == "" && c.ToolUseID != "" {
			if meta := claudeTranscriptMeta(c.TranscriptPath, c.SessionID, c.ToolUseID, 3*time.Second); meta != nil {
				in.ToolResult = meta
			}
		}
	case "Stop", "SessionStart":
		// Waiters: the installer runs these with --rewake (asyncRewake).
		in.Event = Stop
	}
	return in, nil
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
