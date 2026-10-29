// Package harnesses is what Wakeshift's core knows about each agent
// harness: how to recognise it (the MCP clientInfo name it sends, else a
// _meta key or User-Agent token only it sends: Recognise), how its
// conversations are woken, how a tool result's beacon reaches its hook and
// is bound to the conversation, and what to tell its agent about installing
// Wakeshift's hooks (the install command and the harness's own trust step).
// The core uses it to write each harness's install note. Every entry was
// checked against the harness itself, or its docs and source where noted.
package harnesses

import (
	"regexp"
	"slices"
	"strings"
)

// Wake is how Wakeshift's messages reach a harness's conversation.
type Wake string

const (
	// Monitor: the agent watches its session's stream link itself (Claude
	// Code's Monitor tool). Nothing to install; at least once, confirmed by
	// keepalives.
	Monitor Wake = "monitor"
	// Connector: a local process starts a turn in the conversation (Codex's
	// app-server).
	Connector Wake = "connector"
	// Rewake: an async hook waits on the session's link and wakes the
	// conversation even while it's idle (Claude Code's asyncRewake).
	Rewake Wake = "rewake"
	// Plugin: a plugin in the harness holds the link and starts a turn
	// (opencode's promptAsync), even while it's idle.
	Plugin Wake = "plugin"
	// StopHook: a stop hook listens for a while when a turn ends and
	// continues the conversation with what arrives. Between turns only.
	StopHook Wake = "stop-hook"
	// Events: the harness subscribes to the app's MCP event for the
	// conversation, and Wakeshift POSTs its messages to the harness's
	// webhook (ChatGPT; internal/mcpevents). Nothing to install.
	Events Wake = "mcp-events"
	// Background: the agent starts a command in the background that prints
	// or exits when a message arrives, and the harness wakes it for that,
	// even while idle (Cursor's Shell with notify_on_output, Antigravity's
	// run_command, Copilot CLI's async bash). Nothing to install;
	// BackgroundNote says how.
	Background Wake = "background"
	// Foreground: the agent waits for messages with a command it runs in
	// the foreground (the stream's one-shot wait), blocked on it until a
	// message arrives or the user interrupts it, then runs it again (Grok
	// Bot, which a background command can't wake). ForegroundNote says how.
	Foreground Wake = "foreground"
	// Pull: nothing can wake it; the agent sees messages when it next uses
	// the app's tools.
	Pull Wake = "pull"
)

// Binding is how a tool result's beacon is tied to the conversation.
type Binding string

const (
	// ByConversation: the app learns the conversation's ID from the
	// tools/call _meta (ConversationMeta), so the beacon names it.
	ByConversation Binding = "conversation"
	// ByCall: the app learns the tool call's ID (CallMeta) and the hook
	// sees the same ID, so the beacon names the call.
	ByCall Binding = "call"
	// Unbound: neither; the harness hands the result only to the calling
	// conversation's hook, which tells Wakeshift its conversation when it
	// redeems (beacon.Binding.Unbound).
	Unbound Binding = "unbound"
)

// Carrier is where in a tool result the beacon goes.
type Carrier string

const (
	// Meta: _meta["wakeshift/connector"]; the model never sees it.
	Meta Carrier = "meta"
	// Text: a line of the result's text (connector.BeaconText), for
	// harnesses whose hooks see only text. StripsText says whether the
	// hook takes it out before the model sees it.
	Text Carrier = "text"
)

// Harness is one agent harness.
type Harness struct {
	ID   string // as in `wakeshift install --harness ID`
	Name string // for people
	// Clients are the MCP clientInfo names it sends in initialize
	// (compared case-insensitively).
	Clients []string
	// Unsure, if set, is the condition its notes start with ("If you're
	// …:"), for a harness whose client name others send too (Hermes Agent
	// sends the MCP Python SDK's default, "mcp"): the agent learns
	// whether the note is for it, and its agent isn't named after it.
	Unsure string
	// Fingerprint is a tools/call _meta key (dotted as ConversationMeta)
	// that only this harness sends, with a string value: how a call is
	// recognised when the server never saw initialize (a stateless MCP
	// server). "" if none.
	Fingerprint string
	// UserAgents are product tokens (the part before "/") only this
	// harness puts in its MCP requests' HTTP User-Agent, compared
	// case-insensitively: recognition when there's neither a client name
	// nor a Fingerprint.
	UserAgents []string
	Wake       Wake
	// ZeroInstall, if set, is how it's woken with nothing installed (Claude
	// Code: Monitor); installing is then an upgrade.
	ZeroInstall Wake
	// Ack is the delivery guarantee its wake path gives.
	Ack     string
	Binding Binding
	Carrier Carrier
	// StripsText: with the Text carrier, its hook removes the beacon from
	// what the model sees.
	StripsText bool
	// ConversationMeta, CallMeta: the tools/call _meta keys that carry the
	// conversation's or the call's ID ("" if none). Dots name nested
	// objects (x-codex-turn-metadata.thread_id) unless the key itself has
	// them (antigravity.google/conversation_id).
	ConversationMeta string
	CallMeta         string
	// ConversationEnv, if set, is the environment variable the agent reads
	// its conversation's ID from (Cursor's CURSOR_CONVERSATION_ID), for a
	// harness that tells only the agent: it passes the ID back in the
	// subscribe tool's conversation_id argument.
	ConversationEnv string
	// Installable: `wakeshift install --harness ID` sets it up, or with
	// Plugin, the harness's own plugin command installs Wakeshift's plugin
	// for it (plugins/ID).
	Installable bool
	Plugin      bool
	// Changes says what the install changes, for the user.
	Changes string
	// AskAlso, if set, is what the agent also tells the user in the same
	// question (OpenClaw: its hooks endpoint on a gateway that listens
	// beyond this computer), so one yes covers the whole install; AgreeEnv
	// is the variable the agent's install command then sets, the
	// installer's record of that yes (without it the installer stops and
	// says to ask, a second question).
	AskAlso  string
	AgreeEnv string
	// ToolRules: the harness has exact per-tool rules for MCP tools that
	// Wakeshift's hooks can write (Claude Code, Cursor CLI), so an install
	// offer from an app can also offer to pre-approve the app's own tools
	// (WithTools).
	ToolRules bool
	// allowTools, set by WithTools, is the variable the install command
	// sets for the installer: the app's tools the user agreed to allow.
	allowTools string
	// Run, if set, is how the agent runs the install command in this
	// harness (Codex's sandbox blocks it unless it's escalated).
	Run string
	// AskTool, if set, is the harness's tool for asking the user to pick
	// an option (Claude Code's AskUserQuestion): the agent asks with it,
	// and runs the install only if the user picks yes.
	AskTool string
	// UserShell, if set, is the prefix that runs a command typed in the
	// harness as the user (Claude Code's "!"): the way to install when
	// the agent's own run of the command is refused.
	UserShell string
	// Trust is the step the user takes in the harness after installing
	// ("" if none), as the installer prints it and the agent says it.
	Trust string
	// Resume is what the user does after Trust so the conversation connects
	// the first time ("" if nothing): a message, which gives the agent the
	// turn in which it calls the app's tool again (the hooks connect a
	// conversation only from a result that comes after the install). Empty
	// where the agent calls it again in the same turn (Claude Code).
	Resume string
	// Restart is what the user does after a later restart of the harness so
	// the conversation is woken again ("" if nothing): where nothing
	// installed runs when the harness starts or a conversation is resumed
	// that can start listening again (the background command needs the
	// agent to start it). Empty where the hooks or plugin reconnect a
	// connected conversation by themselves (Claude Code's SessionStart,
	// opencode's and OpenClaw's plugins at load; Codex's SessionStart runs
	// only when a resumed thread's first turn starts).
	Restart string
	// AfterUpdate is what the user does in the harness after an update
	// (the install command run again over an install): an update keeps
	// their settings and connected conversations, so it's usually nothing.
	AfterUpdate string
	// When says when messages reach a conversation once it's set up.
	When string
	// NoteAt says where the hook's "Wakeshift: connected" note appears
	// after it connects a conversation ("right after this result", or "at
	// the start of your next turn" for hooks that can't add context at
	// once).
	NoteAt string
	// Review: the harness holds new hooks until the user reviews them
	// (Codex's /hooks), so a hook that's installed may still need Trust.
	Review bool
	// Blocker, if set, is why Wakeshift can't wake it yet.
	Blocker string
	// ReplacedBy, if set, is the harness that replaced this retired one
	// (Gemini CLI: Antigravity CLI). Wakeshift recognises it only to say
	// so: no install, no beacon.
	ReplacedBy string
	// Verified is the version checked against the real binary; "" means
	// the entry is from docs and source only.
	Verified string
	// Claim, if set, is the name the agent gives for it in the harness
	// argument (ArgDescription lists it, with ClaimHint after it in
	// parentheses); Aliases are other names that mean it (Claimed).
	Claim     string
	ClaimHint string
	Aliases   []string
	// Family, if set, is the harness whose MCP traffic this one's is
	// (Grok Bot's and Cursor's cloud agents' calls are Cursor's): it's
	// never recognised by itself, only claimed by a call recognised as
	// Family (Refine).
	Family string
	// Embeds, if set, marks the calls this harness makes through another
	// harness's MCP client (OpenClaw running its turns in Codex's
	// app-server): a tools/call _meta key (dotted as ConversationMeta) and
	// the value it has then. Such a call is this harness's, whatever its
	// client name, and its beacon goes in _meta (Embedded).
	Embeds *Embedding
}

// Embedding is a _meta key and value (Harness.Embeds).
type Embedding struct{ Meta, Value string }

// Embedded reports whether a call came through another harness's MCP
// client that h runs (Harness.Embeds): that client keeps _meta from the
// model, so the beacon goes there whatever h.Carrier says.
func Embedded(h Harness, meta map[string]any) bool {
	return h.Embeds != nil && metaString(meta, h.Embeds.Meta) == h.Embeds.Value
}

// CarrierFor is where a call's beacon goes: _meta for an Embedded call,
// else h.Carrier.
func CarrierFor(h Harness, meta map[string]any) Carrier {
	if Embedded(h, meta) {
		return Meta
	}
	return h.Carrier
}

var all = []Harness{
	{
		ID: "codex", Name: "Codex", Claim: "codex", Aliases: []string{"codex-cli", "openai-codex"}, Clients: []string{"codex-mcp-client"}, UserAgents: []string{"codex-mcp-client"},
		Wake: Connector, Ack: "at least once, acknowledged when the turn starts", Binding: ByConversation, Carrier: Meta,
		ConversationMeta: "x-codex-turn-metadata.thread_id", Fingerprint: "x-codex-turn-metadata.thread_id", Installable: true,
		Changes: "adds Wakeshift's hook to ~/.codex/hooks.json, keeping a backup",
		Run:     "It needs network access and writes ~/.wakeshift/bin and ~/.codex/hooks.json, which the sandbox blocks: run it with escalated permissions, so the user approves it.",
		Trust:   "In Codex, type /hooks and press t to trust the Wakeshift hooks (or restart Codex and choose Trust).",
		Resume:  "Then send any message here (\"done\" is enough), so I can connect this conversation.",
		// Codex runs its SessionStart hook when the resumed thread's first turn
		// starts, not when the window opens (wakebench S8, October 28: the
		// connector re-armed only at the next prompt), so a restart needs one.
		Restart:     "After a later restart, send any message in this thread once it's open again, so I can reconnect it.",
		AfterUpdate: "Codex keeps trusting the hooks it trusted, since their command stays the same; if Codex asks (an update can add one, or a hook stops running), type /hooks and press t.",
		When:        "as soon as they arrive, or once its turn ends if it's busy, while the thread is open in Codex",
		NoteAt:      "right after this result",
		Review:      true,
		Verified:    "0.163.0",
	},
	{
		ID: "claude-code", Name: "Claude Code", Claim: "claude-code", Aliases: []string{"claude", "claude-cli"}, Clients: []string{"claude-code"}, UserAgents: []string{"claude-code"},
		Wake: Rewake, ZeroInstall: Monitor, Ack: "at least once, acknowledged when the hook hands the message over",
		Binding: ByCall, Carrier: Meta, CallMeta: "claudecode/toolUseId", Fingerprint: "claudecode/toolUseId", Installable: true,
		Changes: "adds three hooks to ~/.claude/settings.json and pre-approves Wakeshift's own wait command there (~/.wakeshift/bin/wakeshift wait), so Claude Code runs it without asking, keeping a backup",
		AskTool: "AskUserQuestion", UserShell: "!", ToolRules: true,
		Trust:       "There's nothing to approve. Claude Code picks the hooks up in running sessions; in a folder Claude Code hasn't trusted yet, choose \"Yes, I trust this folder\" when it asks, since hooks don't run until then.",
		AfterUpdate: "There's nothing to approve again: the hooks run the new version from their next call.",
		When:        "as soon as they arrive, even while the session is idle (not in claude -p)",
		// Its async hook's own note reaches the model only at the next
		// user prompt, so the core says it too: as the Monitor's last line
		// (until=hooks), else with the next tool result or wake.
		NoteAt:   "as the Monitor's last line, or with your next tool result or wake",
		Verified: "2.1.280",
	},
	{
		ID: "cursor", Name: "Cursor", Claim: "cursor", ClaimHint: "local CLI or IDE", Aliases: []string{"cursor-agent", "cursor-cli", "cursor-ide", "cursor-local"}, Clients: []string{"Cursor", "cursor-vscode"}, UserAgents: []string{"Cursor"},
		Wake: StopHook, ZeroInstall: Background, Ack: "at least once, acknowledged when the stop hook hands the message over",
		Binding: Unbound, Carrier: Text, StripsText: true, Installable: true, ConversationEnv: "CURSOR_CONVERSATION_ID",
		Changes:     "adds three hooks to ~/.cursor/hooks.json, keeping a backup",
		ToolRules:   true,
		Trust:       "Restart cursor-agent to load them (a new chat isn't enough); the Cursor app reloads them by itself. There's nothing to approve for hooks in your home folder.",
		Resume:      "Then reopen this chat (cursor-agent --continue) and send any message (\"done\" is enough), so I can connect it and start listening again.",
		Restart:     "After any later restart, reopen this chat and send any message, so I can start listening again.",
		AfterUpdate: "There's nothing to approve or restart: the hooks run the new version from their next call.",
		When:        "if they come within a minute after the agent finishes a turn, otherwise at the start of the next one (not in cursor-agent -p)",
		NoteAt:      "right after this result",
		Verified:    "cursor-agent 2026.09.23",
	},
	{
		// Cursor's cloud agents call MCP servers as Cursor does; their
		// CURSOR_CONVERSATION_ID (bc-…) is the agent's ID.
		ID: "cursor-cloud", Name: "Cursor cloud agent", Claim: "cursor-cloud", ClaimHint: "Cursor cloud agent",
		Aliases: []string{"cursor-cloud-agent", "cursor-background-agent"}, Family: "cursor",
		Wake: Pull, ConversationEnv: "CURSOR_CONVERSATION_ID",
		Blocker: "nothing can start a Cursor cloud agent's next turn yet (that needs a bridge to Cursor's cloud)",
	},
	{
		// Grok Bot's MCP traffic is Cursor's: clientInfo Cursor 1.0.0,
		// User-Agent Cursor/1.0.0, from AWS us-east-1, no _meta beyond
		// progressToken (checked 2026-10-19). Only its agent can tell.
		ID: "grok-bot", Name: "Grok Bot", Claim: "grok-bot", Aliases: []string{"grok", "grokbot"}, Family: "cursor",
		Wake: Foreground, ConversationEnv: "CURSOR_CONVERSATION_ID",
		Blocker: "a command in the background can't wake Grok Bot, so it waits in the foreground (webhook delivery comes later)",
	},
	{
		ID: "copilot", Name: "GitHub Copilot CLI", Claim: "copilot-cli", Aliases: []string{"copilot", "github-copilot"}, Clients: []string{"copilot-cli"}, UserAgents: []string{"copilot-cli"},
		Wake: StopHook, ZeroInstall: Background, Ack: "at least once, acknowledged when the stop hook hands the message over",
		Binding: ByConversation, Carrier: Text, StripsText: true, ConversationMeta: "wakeshift/conversation", Fingerprint: "wakeshift/conversation", Installable: true,
		Changes: "writes one hooks file, ~/.copilot/hooks/wakeshift.json",
		Trust: "There's nothing to approve for hooks in your home folder. Restart copilot to load them (/env lists them). " +
			"The background command that wakes the agent while it's idle still needs one \"Yes\" each time it starts (once per wake), unless copilot runs with --allow-all-tools (which approves every tool, not just this one).",
		Resume:      "Then reopen this session (copilot --continue) and send any message (\"done\" is enough), so I can connect it and start listening again.",
		Restart:     "After any later restart, reopen this session and send any message, so I can start listening again.",
		AfterUpdate: "There's nothing to approve or restart: the hooks run the new version from their next call.",
		When:        "when the agent finishes a turn; while it's idle, they wait for the next one",
		NoteAt:      "right after this result",
		Verified:    "1.0.101",
	},
	{
		ID: "opencode", Name: "opencode", Claim: "opencode", Aliases: []string{"open-code"}, Clients: []string{"opencode"}, UserAgents: []string{"opencode"},
		Wake: Plugin, Ack: "at least once, acknowledged once opencode stores the message",
		Binding: Unbound, Carrier: Meta, Installable: true,
		Changes:     "writes one plugin file, ~/.config/opencode/plugins/wakeshift.ts",
		Trust:       "Quit and restart opencode to load the Wakeshift plugin (opencode doesn't ask to approve plugins).",
		Resume:      "Then reopen this session (opencode --continue) and send any message (\"done\" is enough), so I can connect it. After that, opencode reconnects it by itself whenever it starts.",
		AfterUpdate: "Quit and restart opencode so it loads the new plugin.",
		When:        "as soon as they arrive, even while the session is idle, as long as opencode is open (not in opencode run)",
		NoteAt:      "right after this result",
		Verified:    "1.18.7",
	},
	{
		ID: "antigravity", Name: "Antigravity CLI", Claim: "antigravity", Aliases: []string{"agy", "antigravity-cli"}, Clients: []string{"antigravity-client"},
		Wake: StopHook, ZeroInstall: Background, Ack: "at least once, acknowledged when the stop hook hands the message over",
		Binding: ByConversation, Carrier: Text, ConversationMeta: "antigravity.google/conversation_id", Fingerprint: "antigravity.google/conversation_id", Installable: true,
		Changes:     "adds a \"wakeshift\" entry to ~/.gemini/config/hooks.json, keeping a backup",
		Trust:       "There's nothing to approve for hooks in your home folder. Restart agy to load them (/hooks lists them).",
		Resume:      "Then reopen this conversation (agy --continue) and send any message (\"done\" is enough), so I can connect it and start listening again.",
		Restart:     "After any later restart, reopen this conversation and send any message, so I can start listening again.",
		AfterUpdate: "There's nothing to approve or restart: the hooks run the new version from their next call.",
		When:        "if they come within a minute after the agent finishes a turn, otherwise at the start of the next one",
		NoteAt:      "at the start of your next turn",
		Verified:    "1.4.0",
	},
	{
		// OpenClaw's own runtime calls MCP servers as openclaw-bundle-mcp,
		// with no _meta and User-Agent "undici" (Node's default, so no rule);
		// when it runs a turn in Codex's app-server, Codex's client calls them
		// and marks the thread as OpenClaw's.
		ID: "openclaw", Name: "OpenClaw", Claim: "openclaw", ClaimHint: "even when it runs your turns in Codex", Aliases: []string{"open-claw"}, Clients: []string{"openclaw-bundle-mcp"},
		Embeds: &Embedding{Meta: "x-codex-turn-metadata.thread_source", Value: "openclaw"},
		Wake:   Plugin, Ack: "at least once, acknowledged once the turn it wakes has finished",
		Binding: Unbound, Carrier: Text, StripsText: true, Installable: true, Plugin: true,
		Changes: "installs the Wakeshift plugin with openclaw plugins install and sets the Wakeshift it trusts. When it connects a conversation, the plugin turns on OpenClaw's hooks endpoint with a token of its own, allowing it to start turns only in the conversations that connect through Wakeshift; hooks settings OpenClaw already has are only added to, and its uninstaller undoes this",
		// One question, not two: a gateway in Docker usually listens beyond
		// this computer, and the installer would otherwise stop to ask about
		// it (wakebench, October 26, 2026: 5 of 5 asked twice; with this, 0).
		AskAlso:     "if the OpenClaw gateway listens beyond this computer (in Docker it usually does), OpenClaw's hooks endpoint shares that listener, so it's reachable there too, guarded by the plugin's token; a firewall or proxy rule that blocks /hooks/ from other machines keeps it local.",
		AgreeEnv:    "WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1",
		Run:         "Run it yourself with your exec tool, on the machine (or in the container) where the OpenClaw gateway runs.",
		Trust:       "There's nothing to approve in OpenClaw, and no restart.",
		Resume:      "The plugin starts with my next turn, so send any reply (\"done\" is enough), and I'll connect this conversation. After that, it reconnects by itself whenever the gateway starts.",
		AfterUpdate: "There's nothing to approve again, and no restart. The new plugin starts with your next turn, so ask them to send any reply first.",
		When:        "as soon as they arrive, even while the session is idle, as long as the OpenClaw gateway runs",
		NoteAt:      "right after this result (or, when OpenClaw runs the turn in Codex, with the result of your next call to this app's tools)",
		Verified:    "2026.10.4",
	},
	{
		// clientInfo "mcp" 0.1.0 and User-Agent python-httpx2/2.12.0 are the
		// MCP Python SDK's defaults, which other Python clients send too.
		ID: "hermes", Name: "Hermes Agent", Claim: "hermes", Aliases: []string{"hermes-agent"}, Clients: []string{"mcp"}, Unsure: "you're Hermes Agent (Nous Research)",
		Wake: Plugin, Ack: "at least once, acknowledged once Hermes accepts the run",
		Binding: Unbound, Carrier: Meta, Installable: true,
		Changes:     "adds Wakeshift's plugin, $HERMES_HOME/plugins/wakeshift, and enables it",
		Run:         "Run it where Hermes runs (inside its container, if it's in one). If Hermes blocks the command, give it to the user to run there.",
		Trust:       "There's nothing to approve in Hermes, but Hermes loads plugins only when a chat starts or is resumed, so Wakeshift can't wake this chat until they resume it: exit and run `hermes chat --continue` (through Hermes's gateway or API server, restart the gateway and continue the session). A new chat works too. If the installer couldn't enable the plugin, first run `hermes plugins enable wakeshift` where Hermes runs.",
		Resume:      "Then send any message (\"done\" is enough), so I can connect it.",
		Restart:     "After a later restart of hermes chat, send any message once you've resumed it, so I can reconnect it (conversations through Hermes's gateway reconnect by themselves).",
		AfterUpdate: "There's nothing to approve again, but Hermes loads plugins only when a chat starts or is resumed: ask them to resume this chat (exit and run `hermes chat --continue`, or restart Hermes's gateway) or start a new one, so it loads the new plugin.",
		When:        "as soon as they arrive, even while it's idle, as long as Hermes is running (conversations through Hermes's API server or CLI)",
		NoteAt:      "right after this result",
		Verified:    "0.22.2",
	},
	{
		// ChatGPT (and other OpenAI-hosted MCP clients) send clientInfo
		// "openai-mcp" 1.0.0 and User-Agent openai-mcp/1.0.0, per OpenAI's
		// developer forum (not captured here: no ChatGPT sign-in), and
		// _meta openai/session (an anonymized conversation ID; the Apps SDK
		// reference). It's woken by MCP Events in Work chats and dots, for
		// the app's own event types (pkg/appkit Attach, the TypeScript
		// SDK's attach): ChatGPT subscribes natively and starts a run
		// for each occurrence. What a conversation subscribes to with the
		// app's own tools waits for its next tool call (appapi
		// chatgptResult). openai/session only recognises it.
		ID: "chatgpt", Name: "ChatGPT", Claim: "chatgpt", Clients: []string{"openai-mcp"}, UserAgents: []string{"openai-mcp"}, Fingerprint: "openai/session",
		Wake: Events, Ack: "at least once, acknowledged when ChatGPT accepts the webhook (not when the run reads it)",
		When: "as soon as they arrive, as a new run in the chat (Work chats and dots), for the app's own events it subscribes to",
	},
	// Retired: recognised only to tell its agent what replaced it.
	{
		ID: "gemini", Name: "Gemini CLI", Clients: []string{"gemini-cli-mcp-client"},
		Wake: Pull, ReplacedBy: "antigravity",
		Blocker: "Google replaced Gemini CLI with Antigravity CLI (agy), which Wakeshift can wake",
	},

	// Researched from docs and source only.
	{
		ID: "goose", Name: "Goose", Clients: []string{"goose-cli", "goose-desktop"},
		Wake: StopHook, Binding: ByConversation, ConversationMeta: "agent-session-id",
		Blocker: "Goose's PostToolUse hook gets no tool result and can't add context, so no beacon reaches it",
	},
	{
		ID: "cline", Name: "Cline", Clients: []string{"Cline"},
		Wake:    Pull,
		Blocker: "the VS Code extension has no stop hook or plugins; the CLI's plugins could wake a session, but it sends MCP servers no session ID",
	},
	{
		ID: "amp", Name: "Amp", Wake: Plugin,
		Blocker: "Amp's MCP client name and its plugin's tool.result shape aren't documented; a plugin could wake threads with appendUserMessage",
	},
	{
		ID: "devin", Name: "Devin (Windsurf)",
		Wake:    StopHook,
		Blocker: "Cascade hooks can't add context or continue; Devin CLI and Devin Local could use a stop-hook loop with a text beacon, unverified",
	},
	{
		ID: "vscode", Name: "VS Code (Copilot agent mode)", Clients: []string{"Visual Studio Code", "Visual Studio Code - Insiders"},
		Wake: StopHook, Binding: ByConversation, Carrier: Text, ConversationMeta: "vscode.conversationId",
		Blocker: "not built: its Stop hook can continue a session and its PostToolUse hook sees the result's text, so it could use the text-beacon stop-hook path (unverified)",
	},
}

// All lists the harnesses, in a stable order.
func All() []Harness { return append([]Harness(nil), all...) }

// Get returns a harness by ID.
func Get(id string) (Harness, bool) {
	for _, h := range all {
		if h.ID == id {
			return h, true
		}
	}
	return Harness{}, false
}

// ForClient returns the harness that sends this MCP clientInfo name.
func ForClient(name string) (Harness, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Harness{}, false
	}
	for _, h := range all {
		for _, c := range h.Clients {
			if strings.ToLower(c) == name {
				return h, true
			}
		}
	}
	return Harness{}, false
}

// ExactClient is the harness a client name names exactly: one of its
// client names, its name or its ID, ignoring case and a trailing tag in
// parentheses. A stateless server sees only the name a client registered
// with, which isn't always its clientInfo name and often carries a tag
// ("OpenCode", "opencode (wakebench)", "Claude Code (wakeshift)"). A
// variant ("cursor-nightly") isn't exact: it may be another product.
func ExactClient(name string) (Harness, bool) {
	if h, ok := ForClient(name); ok {
		return h, true
	}
	base := strings.TrimSpace(name)
	if i := strings.Index(base, "("); i > 0 && strings.HasSuffix(base, ")") {
		base = strings.TrimSpace(base[:i])
	}
	if base == "" {
		return Harness{}, false
	}
	if h, ok := ForClient(base); ok && h.Unsure == "" { // a generic name others send too doesn't count tagged
		return h, true
	}
	for _, h := range all {
		if h.Unsure == "" && (strings.EqualFold(base, h.Name) || strings.EqualFold(base, h.ID)) {
			return h, true
		}
	}
	return Harness{}, false
}

// Via is the signal a call's harness was recognised by.
type Via string

const (
	// ByClient: the client name it states (initialize's clientInfo.name,
	// or the name it registered with).
	ByClient Via = "client"
	// ByMeta: a key only it puts in the tools/call _meta (Fingerprint).
	ByMeta Via = "meta"
	// ByUserAgent: a product token only it puts in the HTTP User-Agent
	// (UserAgents).
	ByUserAgent Via = "user_agent"
)

// Recognise is the harness a tool call came from and how it was told: by
// the client name if that names a harness (a stated client always wins:
// _meta and the User-Agent never override it, except the _meta that says
// another harness runs that client, Harness.Embeds), else by a Fingerprint key
// in the call's _meta, else by a product token in the User-Agent. A
// stateless MCP server (a new server per request) never sees initialize,
// so it has no client name; the other two come with every call. Every
// signal is self-asserted, the client name included: recognising a harness
// by one grants nothing that stating its client name doesn't.
func Recognise(client string, meta map[string]any, userAgent string) (Harness, Via, bool) {
	for _, h := range all {
		if Embedded(h, meta) { // the stated client is the one it runs
			return h, ByMeta, true
		}
	}
	if h, ok := ForClient(client); ok {
		return h, ByClient, true
	}
	if strings.TrimSpace(client) != "" {
		if h, ok := Get(AgentID(client)); ok {
			return h, ByClient, true
		}
	}
	for _, h := range all {
		if h.Fingerprint != "" && metaString(meta, h.Fingerprint) != "" {
			return h, ByMeta, true
		}
	}
	if products := uaProducts(userAgent); len(products) > 0 {
		for _, h := range all {
			for _, p := range h.UserAgents {
				if slices.Contains(products, strings.ToLower(p)) {
					return h, ByUserAgent, true
				}
			}
		}
	}
	return Harness{}, "", false
}

// uaProducts are a User-Agent's product names, lowercase, without versions
// or comments: "claude-code/2.1 (external, cli) node/22" is claude-code
// and node. Only the first 500 bytes are read.
func uaProducts(ua string) []string {
	if len(ua) > 500 {
		ua = ua[:500]
	}
	var out []string
	depth := 0
	for _, f := range strings.Fields(ua) {
		if depth > 0 || strings.HasPrefix(f, "(") {
			depth = max(0, depth+strings.Count(f, "(")-strings.Count(f, ")"))
			continue
		}
		if name, _, _ := strings.Cut(f, "/"); name != "" {
			out = append(out, strings.ToLower(name))
		}
	}
	return out
}

// AgentID is the agent a client's conversations are named as, from its MCP
// clientInfo name (or the name it registered with): the harness's ID when
// it's one Wakeshift knows, by its client name ("copilot-cli" → copilot,
// "antigravity-client" → antigravity),
// its name ("Claude Code (wakeshift)" → claude-code, "Copilot CLI" →
// copilot) or a variant of its ID ("claude-code-vscode" → claude-code);
// else the name itself as an ID ("My Client" → my-client). IDs are 2-40
// lowercase letters, digits and dashes; "agent" if nothing's left. A client
// name others send too (Unsure: Hermes Agent's "mcp") keeps its own ID.
func AgentID(client string) string {
	if h, ok := ForClient(client); ok && h.Unsure == "" {
		return h.ID
	}
	id := slug(client)
	for _, h := range all {
		if h.Unsure != "" {
			continue
		}
		if id == h.ID || strings.HasPrefix(id, h.ID+"-") {
			return h.ID
		}
		for _, c := range h.Clients {
			if id == slug(c) {
				return h.ID
			}
		}
	}
	return id
}

// slug is a name as an agent ID: lowercase letters, digits and dashes, up
// to 40 characters, without anything in parentheses ("agent" if fewer
// than 2 are left).
func slug(name string) string {
	name = strings.ToLower(name)
	if i := strings.Index(name, "("); i > 0 {
		name = name[:i]
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.TrimSpace(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	id := strings.Trim(b.String(), "-")
	if len(id) > 40 {
		id = strings.Trim(id[:40], "-")
	}
	if len(id) < 2 {
		return "agent"
	}
	return id
}

// toShell is how InstallCommand runs the script it downloads.
const toShell = " | sh"

// InstallCommand is the command that installs Wakeshift's hooks for a
// harness from the Wakeshift at base ("" if it has none).
func InstallCommand(h Harness, base string) string {
	if !h.Installable {
		return ""
	}
	sh := toShell
	if h.allowTools != "" {
		sh = " | " + h.allowTools + " sh"
	}
	return "curl -fsSL " + strings.TrimSuffix(base, "/") + "/install/" + h.ID + ".sh" + sh
}

// AllowToolsEnv is the variable an install command sets for the installer
// when the user agreed to pre-approve an app's tools in the same question:
// <app>:<tool>,<tool>,… (internal/connector, Spec.AllowTools).
const AllowToolsEnv = "WAKESHIFT_ALLOW_TOOLS"

// ToolsOffer is an app's own MCP tools that the agent's program can run
// without asking each time, as the app's manifest names them
// (apps.Manifest.PreapproveTools).
type ToolsOffer struct {
	App, AppName string
	Tools        []string
}

// toolNameRE: what apps.Manifest.Validate allows (checked again here, since
// the names go into a shell command).
var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var appIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)

// WithTools is h with the install offer extended to o's tools, for a
// harness with exact per-tool rules (ToolRules): its Changes say so, by
// name, and its install command (InstallCommand) passes them to the
// installer (AllowToolsEnv), so the user's one yes covers both. h as it
// is otherwise, or if o names no tool or anything a rule can't hold.
func WithTools(h Harness, o ToolsOffer) Harness {
	if !h.ToolRules || !h.Installable || len(o.Tools) == 0 || !appIDRE.MatchString(o.App) {
		return h
	}
	for _, t := range o.Tools {
		if !toolNameRE.MatchString(t) || strings.Contains(t, "__") {
			return h
		}
	}
	name := o.AppName
	if name == "" {
		name = o.App
	}
	h.Changes += "; it also lets " + h.Name + " run " + name + "'s " + ToolList(o.Tools) + " " + plural(len(o.Tools), "tool") + " without asking each time (no other tools)"
	h.allowTools = AllowToolsEnv + "=" + o.App + ":" + strings.Join(o.Tools, ",")
	return h
}

// ToolList names tools for the user: "history", "history and post",
// "history, list_channels and post".
func ToolList(tools []string) string {
	switch len(tools) {
	case 0:
		return ""
	case 1:
		return tools[0]
	}
	return strings.Join(tools[:len(tools)-1], ", ") + " and " + tools[len(tools)-1]
}

func plural(n int, s string) string {
	if n == 1 {
		return s
	}
	return s + "s"
}

// Connected is the exact text a hook's note starts with once it connects a
// conversation (internal/connector, internal/codexconnector), and what
// agents are told to look for. No other note starts with it.
const Connected = "Wakeshift: connected"

// unwoken is what a conversation nothing delivers to is told.
const unwoken = "Wakeshift can't wake this conversation yet: new messages are held until you call this tool again."

// InstallAgree is the agreement line for the wakeshift program and its
// connectors (Connector Terms Section 3.1): every offer to install or update
// them has the agent show it to the user, word for word, as it asks
// (ShowAgree). It is written once, here.
const InstallAgree = "By installing, you agree to Wakeshift's Connector Terms (https://wakeshift.ai/connector-terms) and Privacy Policy (https://wakeshift.ai/privacy)."

// ShowAgree tells the agent to show InstallAgree when it asks.
const ShowAgree = "When you ask, show them this line, word for word: \"" + InstallAgree + "\""

// InstallNote is what an agent in this harness should tell its user
// while nothing delivers Wakeshift's messages to its conversation: whether
// and how to install Wakeshift's hooks and the harness's trust step, or that
// it can't be woken yet (for a retired harness: at all, and what replaced
// it).
func InstallNote(h Harness, base string) string {
	return unsure(h, installNote(h, InstallCommand(h, base)))
}

// unsure is a note for a harness recognised by a client name others send
// too (Unsure): for it if the condition holds, and otherwise CantWake.
func unsure(h Harness, note string) string {
	if h.Unsure == "" {
		return note
	}
	return "If " + h.Unsure + ": " + note + " If not: " + CantWake("")
}

// unsureShort is unsure without what other clients should expect, for a
// note they've been told before (RepeatNote).
func unsureShort(h Harness, note string) string {
	if h.Unsure == "" {
		return note
	}
	return "If " + h.Unsure + ": " + note
}

func installNote(h Harness, install string) string {
	if h.ReplacedBy != "" {
		return RetiredNote(h)
	}
	if !h.Installable {
		return CantWake(h.Name)
	}
	if h.ZeroInstall == Monitor {
		return "You can be woken now with the Monitor tool on your stream link (it's restarted about once an hour). " + upgradeNote(h, install)
	}
	if h.ZeroInstall == Background {
		return "You can be woken now, within seconds even while idle, by a command you start in the background (subscribing gives it). " + backgroundUpgrade(h, install)
	}
	install = agreed(h, install)
	ask := unwoken + " Ask the user now whether to install Wakeshift's hooks for " + h.Name + " (it " + h.Changes + "), so new messages reach this conversation " + h.When + ". " +
		askAlso(h) + ShowAgree + " Only if they agree, run `" + install + "`. "
	if h.AskTool != "" {
		ask = unwoken + " Wakeshift's hooks for " + h.Name + " deliver new messages to this conversation " + h.When + ". " + askWith(h, install) + " "
	}
	if h.Run != "" {
		ask += h.Run + " "
	}
	if t := TrustNote(h); t != "" {
		ask += "Then tell them: " + t + " "
	}
	return ask + "Then call this tool again: a note \"" + Connected + "\" confirms it."
}

// installedAlready is the exception an install note ends with: the hooks
// are installed if their note appears.
func installedAlready(h Harness) string {
	n := "Skip this if a note \"" + Connected + "\" appears " + h.NoteAt + ": the hooks are installed already."
	if h.Review && h.Trust != "" {
		n += " If the user installed them but there's no such note, only the step in " + h.Name + " is needed."
	}
	return n
}

// CantWake is what a conversation in a harness Wakeshift can't wake (name
// "" for one it doesn't know) is told: messages are held, and calling the
// same tool again shows them.
func CantWake(name string) string {
	who := "this agent"
	if name != "" {
		who = name + " conversations"
	}
	return "Wakeshift can't wake " + who + " yet, so new messages are held for you: call this tool again to check for them."
}

// RetiredNote is what an agent in a retired harness (one with ReplacedBy)
// is told: it can't be woken, and what replaced it ("" for any other).
func RetiredNote(h Harness) string {
	if h.ReplacedBy == "" {
		return ""
	}
	return "Wakeshift can't wake " + h.Name + " conversations: " + h.Blocker + ". " +
		"New messages are held for you here: call this tool again to check for them."
}

// AskYes and AskNo are the options an agent with an AskTool offers for the
// install (AskYes for harness h).
func AskYes(h Harness) string { return "Yes, install Wakeshift's hooks for " + h.Name }

const AskNo = "No, don't install them"

// askWith is how an agent in a harness with an AskTool asks about the
// install: the tool, its two options, the command only on yes, and with a
// UserShell, the user running it themselves if the agent's run is refused.
func askWith(h Harness, install string) string {
	no := "messages wait until you call this tool again"
	if h.ZeroInstall == Monitor {
		no = "messages still wake this conversation through the Monitor"
	}
	n := "Ask them with your " + h.AskTool + " tool, with two options: \"" + AskYes(h) + "\" (description: runs `" + install + "`, " + h.Changes + ") " +
		"and \"" + AskNo + "\" (description: " + no + "). " +
		"Put this line in the question, word for word: \"" + InstallAgree + "\" " +
		"Run `" + install + "` only if they pick yes"
	if h.UserShell != "" {
		n += "; if running it is refused (auto mode or a permission denial), ask them to run it themselves by typing `" + h.UserShell + " " + install + "` (the " + h.UserShell + " prefix runs it as the user)"
	}
	return n + "."
}

// UpgradeNote, for a harness that's woken without installing anything
// (Claude Code's Monitor), offers Wakeshift's hooks as the better path.
func UpgradeNote(h Harness, base string) string { return upgradeNote(h, InstallCommand(h, base)) }

func upgradeNote(h Harness, install string) string {
	if !h.Installable {
		return ""
	}
	var n string
	if h.AskTool != "" {
		n = "For delivery without a Monitor, the user can install Wakeshift's hooks for " + h.Name + ". Start the Monitor first either way: it wakes you until the hooks connect. " + askWith(h, install)
	} else {
		n = "For delivery without a Monitor, the user can install Wakeshift's hooks for " + h.Name + " (it " + h.Changes + "). " +
			"Ask them first. " + ShowAgree + " Only if they agree, run `" + install + "`."
	}
	n += " After that, call this tool again right away, in the same turn and without waiting for the user: the hooks work in this session already, but they connect this conversation only from a result that comes after the install, so until you call it they can't wake you. Once they connect it, a note \"" + Connected + "\" appears " + h.NoteAt + ", and they deliver instead of the Monitor."
	if t := TrustNote(h); t != "" {
		// Last, for the reply after that call: placed before the call, an
		// agent hurrying to call again skipped it (wakebench S6).
		n += " Then, in your reply to the user, also tell them this: " + t
	}
	return n
}

// SubscribeNote is what a conversation that just asked to be woken is
// told while Wakeshift's hooks haven't connected it: that the hooks say so
// in a note if they're installed, and otherwise how to install them.
func SubscribeNote(h Harness, base string) string {
	return SubscribeNoteFor(h, InstallCommand(h, base))
}

// SubscribeNoteFor is SubscribeNote with the install command given.
func SubscribeNoteFor(h Harness, install string) string {
	if !h.Installable {
		return unsure(h, installNote(h, install))
	}
	n := installNote(h, install)
	if h.ZeroInstall == "" {
		n += " " + installedAlready(h)
	}
	return unsure(h, n)
}

// RepeatNote is SubscribeNote for a conversation that's been told it once
// already (it subscribed before): short, and without asking the user again.
func RepeatNote(h Harness, base string) string { return RepeatNoteFor(h, InstallCommand(h, base)) }

// RepeatNoteFor is RepeatNote with the install command given.
func RepeatNoteFor(h Harness, install string) string {
	if !h.Installable {
		return unsure(h, installNote(h, install))
	}
	ask := "If you haven't asked the user yet whether to install them (it " + h.Changes + "), ask now. " + askAlso(h) + ShowAgree + " If they agreed and it hasn't run, run `" + agreed(h, install) + "`." + sep(h.Run)
	if h.AskTool != "" {
		ask = "If you haven't asked the user yet whether to install them, ask now. " + askWith(h, install)
	}
	return unsureShort(h, "Wakeshift's hooks for "+h.Name+" haven't connected this conversation yet: messages wait until you call this tool again. "+
		"If they're installed, a note \""+Connected+"\" appears "+h.NoteAt+". "+ask+
		" Don't ask again if they said no.")
}

// askAlso is the sentence that has the agent tell the user h.AskAlso in
// the same question ("" if there's nothing more to tell).
func askAlso(h Harness) string {
	if h.AskAlso == "" {
		return ""
	}
	return "In the same question, tell them: " + h.AskAlso + " "
}

// agreed is the agent's install command for h once the user agreed to
// everything they were asked (h.AgreeEnv set for the installer).
// The command is InstallCommand's script and shell, with the variable set
// for the shell; TestAgreed keeps every harness's in step with it.
func agreed(h Harness, install string) string {
	sh := toShell
	if h.allowTools != "" {
		sh = " | " + h.allowTools + " sh"
	}
	script, ok := strings.CutSuffix(install, sh)
	if h.AgreeEnv == "" || !ok {
		return install
	}
	return script + strings.TrimSuffix(sh, "sh") + h.AgreeEnv + " sh"
}

// sep is s after a space ("" if s is).
// TrustNote is what the agent tells the user after the install: the
// harness's trust step, what then connects this conversation (a message,
// where the agent needs a turn to call the app's tool again), and what to
// do after a later restart, only where nothing reconnects it by itself
// ("" if there's nothing to tell).
func TrustNote(h Harness) string {
	return strings.TrimSpace(h.Trust + sep(h.Resume) + sep(h.Restart))
}

func sep(s string) string {
	if s == "" {
		return ""
	}
	return " " + s
}

// Conversation returns the conversation ID a harness put in a tools/call
// _meta ("" if it sends none).
func Conversation(h Harness, meta map[string]any) string {
	return metaString(meta, h.ConversationMeta)
}

// Call returns the tool call ID a harness put in a tools/call _meta.
func Call(h Harness, meta map[string]any) string {
	return metaString(meta, h.CallMeta)
}

func metaString(meta map[string]any, key string) string {
	if key == "" || meta == nil {
		return ""
	}
	if s, ok := meta[key].(string); ok {
		return limit(s)
	}
	cur := meta
	parts := strings.Split(key, ".")
	for i, p := range parts {
		if i == len(parts)-1 {
			s, _ := cur[p].(string)
			return limit(s)
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			return ""
		}
		cur = next
	}
	return ""
}

func limit(s string) string {
	if len(s) > 200 {
		return ""
	}
	return s
}
