package connector

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/harnesses"
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
//   - SessionStart (--event session-context; the plugin's, and the
//     installer's since it wrote one): add context telling the model the
//     mark (internal/pluginmark) to pass as the harness argument of an app's
//     tools, so the core leaves the install question and the Monitor out of
//     the subscribing result. A PreToolUse hook can't set it: Claude Code's
//     auto mode refuses a call whose input a hook changed after its review
//     (2.1.316). The waiter below can't add it: an asyncRewake hook's output
//     is read only when it exits, with a message.
//   - SessionStart and Stop (asyncRewake): wait on the session's link; on a
//     message, write it to stderr and exit 2, which wakes the idle session
//     (or reaches a busy one between tool calls). One waiter per Claude
//     process (CLAUDE_PID): a newer one replaces it, which also covers
//     /clear (a new session ID in the same process).

const claudeName = "claude-code"

// How Claude Code presents a waiter's wake (its asyncRewake hook's
// rewakeSummary and rewakeMessage, read from the hook's settings since
// 2.1.280 at least). Without them the terminal labels each wake "Stop hook
// feedback", which reads like an error, and the model's system reminder
// starts `Stop hook blocking error from command "…":`.
const (
	// ClaudeRewakeSummary is the line the terminal shows for the wake.
	ClaudeRewakeSummary = "Woken by Wakeshift"
	// ClaudeRewakeMessage starts the system reminder the model gets,
	// before the hook's text (the headline, then the framed messages).
	ClaudeRewakeMessage = "Wakeshift woke this session (not the user)."
)

func init() {
	register(Spec{Name: claudeName, Format: claudeFormat{}, Installer: claudeInstaller{},
		When:       "as soon as they arrive, even while the session is idle",
		RewakeWait: 23 * time.Hour, // under the hook's timeout (86400s)
		Rule:       ClaudeRule,
		Tools:      &claudeTools,
		Headline:   true,
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

// LastPromise: the newest result of an MCP tool (mcp__<server>__<tool>) in
// the session's transcript whose text starts a paragraph with the core's
// "the plugin (or hooks) connects this conversation" lead
// (harnesses.PluginReadyLead), and when it was written.
func (claudeFormat) LastPromise(in Input) (Promise, bool) {
	path := in.transcript
	if path == "" || !filepath.IsAbs(path) || filepath.Ext(path) != ".jsonl" {
		return Promise{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return Promise{}, false
	}
	defer f.Close()
	h, _ := harnesses.Get(claudeName)
	leads := []string{harnesses.PluginReadyLead(h, false), harnesses.PluginReadyLead(h, true)}
	promised := func(text string) bool {
		for _, para := range strings.Split(text, "\n\n") {
			for _, lead := range leads {
				if strings.HasPrefix(strings.TrimSpace(para), lead) {
					return true
				}
			}
		}
		return false
	}
	names := map[string]string{} // tool_use ID → tool name
	// Results that promised, in transcript order: a result can come before
	// its tool_use line, so they're checked against names at the end, as
	// FindBeacon does.
	var promises []Promise
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && json.Valid(line) {
			var l struct {
				SessionID string    `json:"sessionId"`
				Timestamp time.Time `json:"timestamp"`
				Message   struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &l) == nil && l.SessionID == in.Conversation {
				var parts []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					ToolUseID string          `json:"tool_use_id"`
					Content   json.RawMessage `json:"content"`
				}
				json.Unmarshal(l.Message.Content, &parts)
				for _, p := range parts {
					switch {
					case p.Type == "tool_use" && p.ID != "":
						names[p.ID] = p.Name
					case p.Type == "tool_result" && p.ToolUseID != "" && promised(claudeResultText(p.Content)):
						promises = append(promises, Promise{Call: p.ToolUseID, At: l.Timestamp})
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	for i := len(promises) - 1; i >= 0; i-- {
		if strings.HasPrefix(names[promises[i].Call], "mcp__") {
			return promises[i], true
		}
	}
	return Promise{}, false
}

// claudeResultText is a transcript tool_result's text: its content as a
// string, or its text items joined.
func claudeResultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(content, &items)
	var texts []string
	for _, it := range items {
		if it.Type == "text" {
			texts = append(texts, it.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// ClaudeSessionContextEvent is the --event of the plugin's SessionStart
// hook that adds the mark's context (SessionContext).
const ClaudeSessionContextEvent = "session-context"

// ParseEvent: the plugin's session-context hook names its event; any
// other hook is read as Parse reads it.
func (f claudeFormat) ParseEvent(event string, b []byte) (Input, error) {
	in, err := f.Parse(b)
	if err != nil || event != ClaudeSessionContextEvent {
		return in, err
	}
	var c claudeInput
	json.Unmarshal(b, &c)
	if c.HookEventName != "SessionStart" {
		return Input{}, nil
	}
	return Input{Event: SessionContext, Conversation: c.SessionID}, nil
}

// ClaudeMarkNote is the context the plugin's SessionStart hook adds, with
// the mark (%s) to pass.
const ClaudeMarkNote = "Wakeshift's plugin is installed in this Claude Code session, so apps that notify through Wakeshift can wake this conversation by themselves, even while it's idle. " +
	"When you call an app's tool that takes a `harness` argument, pass harness=%q (exactly this): it tells the app the plugin is here, so its result won't ask you to set notifications up."

// ClaudeHooksMarkNote is ClaudeMarkNote for the hooks Wakeshift's
// installer wrote to ~/.claude/settings.json (no plugin).
const ClaudeHooksMarkNote = "Wakeshift's hooks are installed in this Claude Code session, so apps that notify through Wakeshift can wake this conversation by themselves, even while it's idle. " +
	"When you call an app's tool that takes a `harness` argument, pass harness=%q (exactly this): it tells the app the hooks are here, so its result won't ask you to set notifications up."

// SessionContext is the SessionStart output that adds the mark's note
// (the hooks' wording when hooks: the installer's, not the plugin's), if
// there's a mark (none under claude -p, where nothing wakes the session).
func (claudeFormat) SessionContext(mark string, hooks bool) (Output, bool) {
	if mark == "" || ClaudeUnattended() {
		return Output{}, false
	}
	note := ClaudeMarkNote
	if hooks {
		note = ClaudeHooksMarkNote
	}
	b, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": fmt.Sprintf(note, mark)}})
	if err != nil {
		return Output{}, false
	}
	return Output{Stdout: append(b, '\n')}, true
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
	// SessionStart holds two of ours: the waiter, and the hook that adds
	// the mark's context (claudeContextEntry).
	isWaiter := func(item json.RawMessage) bool { return isOur(item) && !claudeContextHook(item) }
	waiter := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": HookCommand(bin, claudeName, "--rewake"),
		"asyncRewake": true, "timeout": 86400, "rewakeSummary": ClaudeRewakeSummary, "rewakeMessage": ClaudeRewakeMessage}}}
	return []entry{
		{event: "PostToolUse", isOur: isOur, json: map[string]any{"matcher": "mcp__.*", "hooks": []any{map[string]any{"type": "command",
			"command": HookCommand(bin, claudeName), "async": true, "timeout": 30}}}},
		{event: "SessionStart", isOur: isWaiter, json: waiter},
		{event: "Stop", isOur: isOur, json: waiter},
		claudeContextEntry(bin),
	}
}

// claudeContextEntry is the SessionStart hook that tells the model the
// mark to pass (SessionContext), as the plugin's own: plain, not async, so
// its context is there before the session's first prompt.
func claudeContextEntry(bin string) entry {
	return entry{event: "SessionStart", isOur: func(item json.RawMessage) bool { return commandIn(item, bin, claudeName) && claudeContextHook(item) },
		json: map[string]any{"hooks": []any{map[string]any{"type": "command", "command": HookCommand(bin, claudeName, "--event", ClaudeSessionContextEvent), "timeout": 60}}}}
}

// claudeContextHook reports whether a hook entry (or a group of them under
// "hooks") runs a session-context hook.
func claudeContextHook(item json.RawMessage) bool {
	var e struct {
		Command string `json:"command"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if json.Unmarshal(item, &e) != nil {
		return false
	}
	is := func(c string) bool { return strings.HasSuffix(c, " --event "+ClaudeSessionContextEvent) }
	if is(e.Command) {
		return true
	}
	for _, h := range e.Hooks {
		if is(h.Command) {
			return true
		}
	}
	return false
}

// claudeContextAdded, in Claude Code's directory under Wakeshift's home,
// records that UpgradeClaudeHooks added the session-context hook, so it
// does that once: a user who takes it out again keeps it out.
const claudeContextAdded = "context-hook-added"

// UpgradeClaudeHooks brings an install of Wakeshift's hooks in Claude
// Code's user settings from before the session-context hook up to date,
// when a newer connector runs its waiter (bin: the binary its hooks run,
// which another harness's install or Wakeshift's plugin may have
// replaced): it adds that hook, so the sessions that start from then on
// pass the mark and aren't asked to set notifications up. Only for the
// installer's hooks running bin (its waiter is in the file), never a
// plugin install's (whose own hook adds the context), and once. It never
// fails the hook: what it can't do, it leaves.
func UpgradeClaudeHooks(bin string) {
	set := LoadSettings(claudeName)
	if !set.Installed() || exists(filepath.Join(set.Dir, PluginIssuerFile)) || exists(filepath.Join(set.Dir, claudeContextAdded)) {
		return
	}
	unlock, err := lockFile(filepath.Join(set.Dir, ".upgrade.lock"))
	if err != nil {
		return
	}
	defer unlock()
	if exists(filepath.Join(set.Dir, claudeContextAdded)) {
		return
	}
	i := claudeInstaller{}
	f, err := readHookFile(i.path(), "hooks")
	if err != nil {
		return
	}
	starts, err := f.list("SessionStart")
	if err != nil {
		return
	}
	waiter := HookCommand(bin, claudeName, "--rewake")
	installed, context := false, claudeContextEntry(bin)
	for _, item := range starts {
		if context.isOur(item) { // there already (an install wrote it)
			os.WriteFile(filepath.Join(set.Dir, claudeContextAdded), nil, 0o600)
			return
		}
		var e struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		}
		if json.Unmarshal(item, &e) == nil {
			for _, h := range e.Hooks {
				installed = installed || h.Command == waiter
			}
		}
	}
	if !installed {
		return
	}
	if _, _, err := editHooks(i.path(), "hooks", []entry{context}, false); err != nil {
		return
	}
	os.WriteFile(filepath.Join(set.Dir, claudeContextAdded), nil, 0o600)
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
