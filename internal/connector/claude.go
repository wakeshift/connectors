package connector

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/harnesses"
)

// Claude Code (2.1.280, verified). Its MCP client sends
// servers the tool call's ID (_meta["claudecode/toolUseId"]) but not the
// session's, and its PostToolUse hooks get the result without _meta; the
// result's _meta is in the transcript ("mcpMeta"), written after the hook
// starts: about 1-2.5s after on 2.1.321, sometimes over 4s
// (claudeMetaWait). So:
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
	AgentID        string          `json:"agent_id"` // a subagent's call (the Agent tool)
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
			// A subagent's call: transcript_path is the session's, but the
			// result's line is in the subagent's own transcript.
			paths := []string{c.TranscriptPath}
			if c.AgentID != "" {
				if sub := claudeSubagentTranscript(c.TranscriptPath, c.SessionID, c.AgentID); sub != "" {
					paths = []string{sub, c.TranscriptPath}
				}
			}
			read := func() json.RawMessage { return claudeTranscriptMeta(paths, c.SessionID, c.ToolUseID, claudeMetaWait) }
			if RedeemByCallEnabled() {
				// Redeem by call (callredeem.go): the hook redeems by the call's ID
				// first, and reads the transcript only if it can't.
				in.lateMeta = read
			} else if meta := read(); meta != nil {
				in.ToolResult = meta
			}
		}
	case "Stop", "SessionStart":
		// Waiters: the installer runs these with --rewake (asyncRewake).
		in.Event = Stop
		in.transcript = c.TranscriptPath
		in.turnEnded = c.HookEventName == "Stop"
	}
	return in, nil
}

// claudeID is a session or subagent ID as Claude Code gives it (a UUID;
// agent_id "a5998067ef883574a"), checked before it names a file.
var claudeID = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

// claudeSubagentTranscript is where Claude Code writes a subagent's lines
// (its calls, their results and their mcpMeta), whose hooks get the
// session's transcript_path and session_id with the subagent's agent_id:
// <the session transcript's folder>/<session>/subagents/agent-<agent_id>.jsonl
// (observed with 2.1.280 and 2.1.321). Its lines carry the
// session's sessionId. "" if the path or an ID doesn't fit.
func claudeSubagentTranscript(path, session, agent string) string {
	if path == "" || !filepath.IsAbs(path) || filepath.Ext(path) != ".jsonl" || !claudeID.MatchString(session) || !claudeID.MatchString(agent) {
		return ""
	}
	return filepath.Join(filepath.Dir(path), session, "subagents", "agent-"+agent+".jsonl")
}

// claudeTranscripts is the session's transcript and its subagents'
// (claudeSubagentTranscript), for a waiter, whose hook input names no
// subagent: a beacon in a subagent's result connects the session too.
// nil if the session's path doesn't fit.
func claudeTranscripts(path, session string) []string {
	if path == "" || !filepath.IsAbs(path) || filepath.Ext(path) != ".jsonl" {
		return nil
	}
	paths := []string{path}
	if !claudeID.MatchString(session) {
		return paths
	}
	dir := filepath.Join(filepath.Dir(path), session, "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return paths
	}
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, "agent-") && strings.HasSuffix(n, ".jsonl") {
			paths = append(paths, filepath.Join(dir, n))
		}
	}
	return paths
}

// newerLine: a line read after another (later in its file, or in a file
// read later) is the newer one unless both have a timestamp and it was
// written earlier: a line without one keeps the reading order.
func newerLine(at, than time.Time) bool {
	return at.IsZero() || than.IsZero() || !at.Before(than)
}

// eachTranscriptLine calls fn with each whole JSON line of each file.
func eachTranscriptLine(paths []string, fn func(line []byte)) {
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		r := bufio.NewReaderSize(f, 1<<16)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 && json.Valid(line) {
				fn(line)
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
}

// LastBeacon: the newest tool result in the session's transcript, or a
// subagent's, whose own _meta (the line's mcpMeta) carries a beacon, and
// the tool it called.
func (claudeFormat) LastBeacon(in Input) (FoundBeacon, bool) {
	paths := claudeTranscripts(in.transcript, in.Conversation)
	if paths == nil {
		return FoundBeacon{}, false
	}
	names := map[string]string{} // tool_use ID → tool name
	var found FoundBeacon
	var foundAt time.Time
	eachTranscriptLine(paths, func(line []byte) {
		var l struct {
			SessionID string    `json:"sessionId"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			MCPMeta json.RawMessage `json:"mcpMeta"`
		}
		if json.Unmarshal(line, &l) != nil || l.SessionID != in.Conversation {
			return
		}
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
				// The newest: later in its file, and written no earlier
				// than the one found so far in any file (newerLine).
				if tok, at := FindBeacon(l.MCPMeta); tok != "" && at == InResultMeta && newerLine(l.Timestamp, foundAt) {
					found, foundAt = FoundBeacon{Token: tok, Call: p.ToolUseID}, l.Timestamp
				}
			}
		}
	})
	if found.Token == "" {
		return FoundBeacon{}, false
	}
	found.ToolName = names[found.Call]
	return found, found.ToolName != ""
}

// LastPromise: the newest result of an MCP tool (mcp__<server>__<tool>) in
// the session's transcript, or a subagent's, whose text starts a paragraph
// with the core's "the plugin (or hooks) connects this conversation" lead
// (harnesses.PluginReadyLead), and when it was written.
func (claudeFormat) LastPromise(in Input) (Promise, bool) {
	paths := claudeTranscripts(in.transcript, in.Conversation)
	if paths == nil {
		return Promise{}, false
	}
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
	// Results that promised: a result can come before its tool_use line,
	// so they're checked against names at the end, as FindBeacon does.
	var promises []Promise
	eachTranscriptLine(paths, func(line []byte) {
		var l struct {
			SessionID string    `json:"sessionId"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil || l.SessionID != in.Conversation {
			return
		}
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
	})
	var newest Promise
	ok := false
	for _, p := range promises { // in file order: a later one written no earlier wins
		if strings.HasPrefix(names[p.Call], "mcp__") && (!ok || newerLine(p.At, newest.At)) {
			newest, ok = p, true
		}
	}
	return newest, ok
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
	if event == ClaudeSubagentContextEvent {
		// The plugin's SubagentStart hook (redeem by call): the mark for
		// the subagent's context, which doesn't get SessionStart's.
		var c claudeInput
		if err := json.Unmarshal(b, &c); err != nil || c.HookEventName != "SubagentStart" || !RedeemByCallEnabled() {
			return Input{}, err
		}
		return Input{Event: SessionContext, Conversation: c.SessionID, contextEvent: "SubagentStart"}, nil
	}
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
// there's a mark (none under claude -p with text input, where nothing
// wakes the session: ClaudeSessionMode).
func (f claudeFormat) SessionContext(mark string, hooks bool) (Output, bool) {
	return f.ContextFor("SessionStart", mark, hooks)
}

// ContextFor is SessionContext for the hook event that adds it
// (SessionStart, or SubagentStart for a subagent's own context).
func (claudeFormat) ContextFor(event, mark string, hooks bool) (Output, bool) {
	if mark == "" || ClaudeSessionMode() == ClaudeOff {
		return Output{}, false
	}
	note := ClaudeMarkNote
	if hooks {
		note = ClaudeHooksMarkNote
	}
	b, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": fmt.Sprintf(note, mark)}})
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

// claudeMetaWait is how long the PostToolUse hook waits for the result's
// line to reach the transcript. Claude Code 2.1.321 writes it once the
// model's next request is under way: usually within a few seconds of the
// hook starting, occasionally later, and past 3s now and then with some
// models. A hook that waited only 3s gave up on a result written later:
// no redeem, so the conversation was never woken. The hook is async, so the
// wait holds up nothing; it stays under the hook's timeout (30s: the plugin's
// hooks.json, claudeInstaller). A result missed all the same is redeemed
// by the conversation's waiter (missedRedeem).
var claudeMetaWait = 25 * time.Second

// claudeTranscriptMeta finds the result's _meta for a tool call in the
// session's transcripts (paths: a subagent's, then the session's; Claude
// Code writes it as the line's mcpMeta after
// PostToolUse hooks start), waiting up to wait for it to appear. Only a
// line of this session for this call counts. A result line without
// mcpMeta (a tool whose result has no _meta or structured content) ends
// the wait: nil.
func claudeTranscriptMeta(paths []string, session, call string, wait time.Duration) json.RawMessage {
	var scans []*transcriptScan
	for _, path := range paths {
		if path != "" && filepath.IsAbs(path) && filepath.Ext(path) == ".jsonl" {
			scans = append(scans, &transcriptScan{path: path, session: session, call: call})
		}
	}
	if len(scans) == 0 {
		return nil
	}
	start := time.Now()
	for {
		for _, sc := range scans {
			if m, seen := sc.next(); m != nil || seen {
				return m
			}
		}
		if time.Since(start) > wait {
			return nil
		}
		// Often while the line usually comes, then less.
		if time.Since(start) < 5*time.Second {
			time.Sleep(50 * time.Millisecond)
		} else {
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// transcriptScan reads the session's transcript for the call's result (its
// line's mcpMeta, and whether the result's line, a "user" line, is there
// at all: the request copies Claude Code writes before it,
// "api-request-blob", don't count) a piece at a time: each next reads only
// what was appended since the last one (a line not finished yet is read
// again next time).
type transcriptScan struct {
	path, session, call string
	off                 int64 // the end of the last whole line read
	found               json.RawMessage
	seen                bool
}

func (s *transcriptScan) next() (json.RawMessage, bool) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() < s.off {
		s.off, s.found, s.seen = 0, nil, false // replaced: read it again
	}
	if _, err := f.Seek(s.off, io.SeekStart); err != nil {
		return nil, false
	}
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err == nil {
			s.off += int64(len(line))
		}
		if len(line) > 0 && json.Valid(line) {
			s.read(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return s.found, s.seen
			}
			return nil, false
		}
	}
}

func (s *transcriptScan) read(line []byte) {
	var l struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		MCPMeta json.RawMessage `json:"mcpMeta"`
	}
	if json.Unmarshal(line, &l) != nil || l.SessionID != s.session || (l.MCPMeta == nil && l.Type != "user") {
		return
	}
	var parts []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
	}
	json.Unmarshal(l.Message.Content, &parts)
	for _, p := range parts {
		if p.Type == "tool_result" && p.ToolUseID == s.call {
			s.seen = s.seen || l.Type == "user"
			if l.MCPMeta != nil {
				s.found = l.MCPMeta // {"_meta": {...}, "structuredContent": ...}
			}
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
		// Redeem by call (callredeem.go): the claim, synchronous and short,
		// and the mark for subagents.
		{event: "PreToolUse", isOur: isOur, json: map[string]any{"matcher": "mcp__.*", "hooks": []any{map[string]any{"type": "command",
			"command": HookCommand(bin, claudeName, "--event", ClaudeClaimEvent), "timeout": 5}}}},
		{event: "SubagentStart", isOur: isOur, json: map[string]any{"hooks": []any{map[string]any{"type": "command",
			"command": HookCommand(bin, claudeName, "--event", ClaudeSubagentContextEvent), "timeout": 10}}}},
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
