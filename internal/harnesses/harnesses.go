// Package harnesses is what Wakeshift's core knows about each agent
// harness: how to recognize it (the MCP clientInfo name it sends, else a
// _meta key or User-Agent token only it sends: Recognize), how its
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
	"sync/atomic"
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
	// run_command, Copilot CLI's async bash, Capy's terminal on its VM).
	// Nothing to install; BackgroundNote says how.
	Background Wake = "background"
	// OneShot: the agent starts a command in the background that waits once
	// for messages, prints them and finishes, and the harness wakes it
	// because the command finished (Muse: a finished background task's
	// output starts a new turn). The agent acts on what it printed, then
	// runs the same command again to keep listening. The command confirms
	// what it printed before it finishes (MuseLoop), so running it again
	// moves past that with no cursor in the agent's hands. Its ZeroInstall
	// is Background (the agent starts the command itself, nothing
	// installed); BackgroundNote says how.
	OneShot Wake = "one-shot"
	// Webhook: the agent makes a URL that wakes its conversation when it's
	// POSTed to (Capy's incoming_webhook watch on its own thread), and
	// registers it with Wakeshift, which POSTs each message there
	// (internal/agenthooks). Nothing to install, no process or awake
	// machine: the harness wakes the conversation even while it's idle.
	// CapyWebhookNote says how.
	Webhook Wake = "agent-webhook"
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
	// Shared are the names among Clients that other clients send too
	// (Hermes Agent before 0.22.5 sent the MCP Python SDK's default,
	// "mcp"). A call recognized only by one is Unsure. A harness with any
	// names its agents after the client's own name (AgentID), whichever of
	// its names it sends, so a conversation keeps its agent (and its
	// session key) across the harness's versions.
	Shared []string
	// ClientPrefix, if set, is a start of client names only it sends,
	// compared case-insensitively, followed by names that vary (Claude
	// Cowork's plugin servers: "local-agent-mode-plugin:<plugin>:<server>").
	// ForClient takes such a name as one of its Clients that others don't
	// send; its agents are named after the client's own name if it has
	// Shared ones (NamedByClient).
	ClientPrefix string
	// Unsure, set with Shared, is the condition its notes start with ("If
	// you're …:") when a call was recognized only by a Shared name: the
	// agent learns whether the note is for it. A call from one of its own
	// names isn't Unsure (Surely).
	Unsure string
	// Fingerprint is a tools/call _meta key (dotted as ConversationMeta)
	// that only this harness sends, with a string value: how a call is
	// recognized when the server never saw initialize (a stateless MCP
	// server). "" if none.
	Fingerprint string
	// UserAgents are product tokens (the part before "/") only this
	// harness puts in its MCP requests' HTTP User-Agent, compared
	// case-insensitively: recognition when there's neither a client name
	// nor a Fingerprint.
	UserAgents []string
	// SharedUserAgents are product tokens its MCP requests carry that
	// other clients' carry too (Hermes Agent's python-httpx2, the MCP
	// Python SDK 2.x's HTTP client): a call with no client name at all (a
	// stateless MCP server, which never sees initialize, on a protocol
	// before 2026-07-28) and nothing else that names a harness is
	// recognized by one as Unsure, as by a Shared client name.
	SharedUserAgents []string
	// Exact: recognized only by one of its Clients, exactly. Not by its
	// Name or ID as a client name, nor by a variant of either ("muse-…"),
	// which AgentID, Recognize and ExactClient otherwise take as it: its
	// one client name is specific, and other products' names may start
	// with its ID (Muse: "muse-custom-connector", and any "muse-…" tool).
	Exact bool
	Wake  Wake
	// Fallback, if set, is how it's woken when its Wake isn't set up (Capy:
	// a thread that hasn't registered a webhook watch runs the one-shot
	// command, OneShot).
	Fallback Wake
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
	// KitWord, if set, is what the harness calls what Wakeshift installs for
	// it, in place of "plugin" or "hooks" (pi: "extension"): Kit says
	// "Wakeshift's extension for pi".
	KitWord string
	// Unload, if set, is what the user does after removing what Wakeshift
	// installed so the harness stops running it ("type /reload in pi"), and
	// InstallerFile the file Wakeshift's installer writes in place of a
	// Package: UninstallNote says how to remove either (unloadNote).
	Unload        string
	InstallerFile string
	// Repo, if set, is the public repository (owner/repo) the harness's own
	// plugin installer takes Wakeshift's plugin from (Hermes: `hermes plugins
	// install`): the install offers give that command (OfferCommand) rather
	// than Wakeshift's installer, and the update offers the same command
	// forced over the installed plugin (UpdateCommand), which replaces it
	// however it was installed.
	Repo string
	// Marketplace, if set, is how the harness's own plugin commands install
	// Wakeshift's plugin from github.com/wakeshift/agent-plugins
	// (marketplace.go): the offers from a Wakeshift with a marketplace
	// branch give those commands and its texts (Route), others Wakeshift's
	// installer.
	Marketplace *Marketplace
	// Package, if set, is the npm package of Wakeshift's plugin that the
	// harness's own plugin command installs (opencode: `opencode plugin
	// @wakeshift/opencode@<version> --global --force`, PackageCommand with
	// {spec} the package at a version). Once this Wakeshift knows the version
	// to offer (SetPackageVersion: production's from npm, another's from its
	// configuration), the install and update offers give that command, and
	// PackageChanges in place of Changes; until then, InstallCommand.
	// PackageIssuer, if set, is the command PackageCommand runs after for a
	// Wakeshift other than production, whose issuer the package trusts by
	// default, with {origin} this Wakeshift (OpenClaw keeps the setting
	// before the plugin is installed, so the plugin never loads without it).
	//
	// PackageUpdateChanges, if set, is what the update offer says it does
	// (UpdateNote), for a package that isn't a plugin installed with the
	// harness's own command (Cursor: ConnectPackage, run with npx).
	Package              string
	PackageCommand       string
	PackageChanges       string
	PackageIssuer        string
	PackageUpdateChanges string
	// Uninstall, if set, is the command that removes the plugin a Package
	// offer installed and undoes what it changed (OpenClaw: the plugin's own
	// reset, then OpenClaw's uninstall, which leaves hooks.* alone).
	Uninstall string
	// routed: Route gave h its Marketplace's texts (the offer is the plugin).
	routed bool
	// client, set by WithClient, is the version of the harness's client
	// the call came from ("" if it didn't say): the offers give the
	// commands that version runs (Marketplace.Since).
	client string
	// Allow, if set, is a command OfferCommand (and UpdateCommand) runs after a Repo install to
	// turn on a setting the install needs, which its Changes disclose (Hermes:
	// allow_gateway_injection, without which the plugin starts no turn in
	// any Hermes chat: Hermes refuses its turns in `hermes chat`
	// conversations, and the plugin holds API server ones until it's on).
	Allow string
	// Changes says what the install changes, for the user.
	Changes string
	// AskAlso, if set, is what the agent also tells the user in the same
	// question (OpenClaw: its hooks endpoint on a gateway that listens
	// beyond this computer), so one yes covers the whole install; AgreeEnv
	// is the variable the agent's install command then sets, the
	// installer's record of that yes (without it the installer stops and
	// says to ask, a second question), and Agree the command a Package
	// offer runs first instead, the record of that yes where the plugin
	// reads it (without it the plugin turns nothing on and says to ask).
	AskAlso  string
	AgreeEnv string
	Agree    string
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
	// Resume is what the user does after Trust ("" if nothing), and never
	// so the agent can call the app's tool again (that tool may have side
	// effects: a result never asks for a repeat). Claude Code's plugin: a
	// message, whose turn's end runs the waiter that connects the
	// conversation from the transcript (connector.installedSince). The
	// background harnesses: a message, so the agent starts its command
	// again after the restart that loads the hooks. Elsewhere the hooks
	// connect the conversation from the next result of the app's tools
	// the agent calls anyway.
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
	// ConnectsAfter, if set, says when the install connects the
	// conversation from the result that offered it, which the hooks read
	// from the harness's own record of it (a late redeem, within the hour:
	// Codex's rollout), so no result of the app's tools is needed for it.
	ConnectsAfter string
	// Review: the harness holds new hooks until the user reviews them
	// (Codex's review when it starts), so a hook that's installed may still need Trust.
	Review bool
	// Blocker, if set, is why Wakeshift can't wake it yet.
	Blocker string
	// ReplacedBy, if set, is the harness that replaced this retired one
	// (Gemini CLI: Antigravity CLI). Wakeshift recognizes it only to say
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
	// never recognized by itself, only claimed by a call recognized as
	// Family (Refine).
	Family string
	// Tasks: with nothing installed, a call whose client takes MCP tasks
	// (protocol 2026-07-28 with the tasks extension, which the app's SDK
	// reports) is woken by Wakeshift's wait tool answered with a task
	// (TaskNote) instead of ZeroInstall (Claude Code: instead of Monitor).
	// The switch for the task path, per harness.
	Tasks bool
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
		// Codex 0.164 and later ask to trust a plugin's hooks again whenever
		// any file in it changes, so the marketplace re-pins Codex's plugin
		// only when it must (plugins/agent-plugins).
		Marketplace: &Marketplace{
			Install:     "codex plugin marketplace add {source} && codex plugin add wakeshift@wakeshift",
			Update:      "codex plugin marketplace add {source} && codex plugin marketplace upgrade wakeshift && codex plugin add wakeshift@wakeshift",
			Remove:      "codex plugin remove wakeshift@wakeshift",
			RefSep:      "@",
			Changes:     "installs Wakeshift's plugin for Codex with Codex's own plugin command, from github.com/wakeshift/agent-plugins; its hooks run Wakeshift's connector, which the plugin downloads into ~/.wakeshift/bin and checks against the SHA-256 it carries",
			Run:         "It writes ~/.codex/config.toml and downloads the plugin, which the sandbox blocks: run it with escalated permissions, so the user approves it.",
			AfterUpdate: "Quit Codex (/quit) and start it again in this conversation with `codex resume --last`: Codex asks to trust the plugin's hooks again after each update, so choose \"Trust all and continue\".",
		},
		Changes: "adds Wakeshift's hook to ~/.codex/hooks.json, keeping a backup",
		Run:     "It needs network access and writes ~/.wakeshift/bin and ~/.codex/hooks.json, which the sandbox blocks: run it with escalated permissions, so the user approves it.",
		Trust:   "Quit Codex (/quit), start it again in this conversation with `codex resume --last`, and trust the Wakeshift hooks when Codex asks (\"Trust all and continue\").",
		// Codex runs its SessionStart hook when the resumed thread's first turn
		// starts, not when the window opens (wakebench S8, October 28: the
		// connector re-armed only at the next prompt), so a restart needs one.
		Restart:     "After a later restart, send any message in this thread once it's open again, so it reconnects.",
		AfterUpdate: "Codex keeps trusting the hooks it trusted, since their command stays the same; if Codex asks the next time it starts (an update can add one, or a hook stops running), choose \"Trust all and continue\".",
		When:        "as soon as they arrive, or once its turn ends if it's busy, while the thread is open in Codex",
		NoteAt:      "right after this result",
		// The hook reads the offer's beacon from the thread's rollout
		// (codexconnector.lateRedeem) when the user's next message starts
		// the resumed thread's turn.
		ConnectsAfter: "when the user sends their next message after the restart",
		Review:        true,
		Verified:      "0.166.0",
	},
	{
		ID: "claude-code", Name: "Claude Code", Claim: "claude-code", Aliases: []string{"claude", "claude-cli"}, Clients: []string{"claude-code"}, UserAgents: []string{"claude-code"},
		Wake: Rewake, ZeroInstall: Monitor, Tasks: true, Ack: "at least once, acknowledged when the hook hands the message over",
		Binding: ByCall, Carrier: Meta, CallMeta: "claudecode/toolUseId", Fingerprint: "claudecode/toolUseId", Installable: true,
		// Wakeshift's plugin (plugins/agent-plugins). It has no options:
		// its launcher trusts the Wakeshift its marketplace ref was
		// rendered for, so it works installed as another plugin's
		// dependency, which sets none, and the install passes none. (Until
		// November 8, 2026 it passed `--config issuer=<origin>`, for a
		// stale marketplace clone holding the plugin from before that
		// option went; on the current plugin it only warned "declares no
		// userConfig options", which agents read as a failed setting. Such a
		// clone's plugin, installed without it, runs no hook ("Plugin option
		// "issuer" isn't set") rather than another Wakeshift's, and the
		// update below moves it on.) The update installs again (a no-op, or
		// the plugin in place of the installer's hooks), then refreshes the
		// marketplace, since `claude plugin update` doesn't, and updates.
		// `--marketplace` came in 2.1.292: an older client (or one that
		// doesn't say its version) adds the marketplace first, a no-op when
		// it's there already.
		Marketplace: &Marketplace{
			Install:       "claude plugin install wakeshift --marketplace {source}",
			Update:        "claude plugin install wakeshift --marketplace {source} && claude plugin marketplace update wakeshift && claude plugin update wakeshift@wakeshift",
			Since:         "2.1.292",
			BeforeInstall: "claude plugin marketplace add {source} && claude plugin install wakeshift@wakeshift",
			BeforeUpdate:  "claude plugin marketplace add {source} && claude plugin install wakeshift@wakeshift && claude plugin marketplace update wakeshift && claude plugin update wakeshift@wakeshift",
			Remove:        "claude plugin uninstall wakeshift@wakeshift",
			RefSep:        "#",
			Changes:       "installs Wakeshift's plugin for Claude Code from github.com/wakeshift/agent-plugins with Claude Code's own plugin installer; its hooks fetch Wakeshift's connector into ~/.wakeshift/bin, checking its SHA-256, and it adds one permission rule to ~/.claude/settings.json so Claude Code runs Wakeshift's wait command (~/.wakeshift/bin/wakeshift wait) without asking each time",
			Trust:         "There's nothing to approve, but Claude Code loads a newly installed plugin only when a session starts: type /reload-plugins here (or restart with claude --continue).",
			Resume:        "Then send any message here (\"done\" is enough): the plugin connects this conversation once I've replied.",
			AfterUpdate:   "Type /reload-plugins here (or restart Claude Code), so it runs the new version.",
			// What's out of date may be the hooks Wakeshift's installer
			// wrote, not the plugin: the plugin's first hook takes those
			// out (InstallPlugin) and keeps the conversations' links, so
			// the update needs no uninstall first, which would drop them.
			UpdateChanges: "it updates Wakeshift's plugin for Claude Code with Claude Code's own plugin command, or installs it in place of the hooks Wakeshift's installer wrote to ~/.claude/settings.json, and keeps their settings and connected conversations",
			Reload:        true,
		},
		Changes: "adds four hooks to ~/.claude/settings.json and pre-approves Wakeshift's own wait command there (~/.wakeshift/bin/wakeshift wait), so Claude Code runs it without asking, keeping a backup",
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
		// Claude Cowork, the Claude desktop app's Cowork tab. Its connectors
		// (remote MCP servers) are called from Anthropic's servers with
		// clientInfo "claude-ai", which claude.ai's web chat (and the app's
		// own chats) send too, so the name is Shared and its notes say what
		// to do with a Monitor tool and what without one (Unsure); a local
		// MCP server in a Cowork plugin sees
		// "local-agent-mode-plugin:<plugin>:<server>" (ClientPrefix; the
		// spike's probe, November 7, 2026). The model has named itself
		// "claude-ai" and "Claude app (claude.ai)" in the harness argument
		// (Aliases). Cowork runs Claude Code, with its Monitor tool, and
		// Monitor output wakes an idle task (November 7, 2026: a 3-tick
		// command woke it three times); plugin hooks don't run in a task,
		// and its MCP client takes no tasks, so the Monitor is the only wake
		// path. Its shell runs in a VM behind the workspace's network
		// allowlist (Settings > Capabilities), which may block the core's
		// host: the notes have the agent check first and tell the user.
		ID: "claude-cowork", Name: "Claude Cowork", Aliases: []string{"cowork", "claude-ai", "claude-app"},
		Clients: []string{"claude-ai"}, Shared: []string{"claude-ai"}, ClientPrefix: "local-agent-mode-plugin:",
		Unsure: coworkMonitor,
		Wake:   Monitor, Ack: "at least once, confirmed by the Monitor's next round",
		When: "as soon as they arrive, even while the task is idle, while Cowork is open and its Monitor runs",
	},
	{
		ID: "cursor", Name: "Cursor", Claim: "cursor", ClaimHint: "local CLI or IDE", Aliases: []string{"cursor-agent", "cursor-cli", "cursor-ide", "cursor-local"}, Clients: []string{"Cursor", "cursor-vscode"}, UserAgents: []string{"Cursor"},
		Wake: StopHook, ZeroInstall: Background, Ack: "at least once, acknowledged when the stop hook hands the message over",
		Binding: Unbound, Carrier: Text, StripsText: true, Installable: true, ConversationEnv: "CURSOR_CONVERSATION_ID",
		// The offers give `npx -y @wakeshift/connect@<version> cursor` once
		// this Wakeshift knows a published version (SetPackageVersion), the
		// version pinned so npx never reuses an older copy it cached.
		Package:              ConnectPackage,
		PackageCommand:       "npx -y {spec} cursor",
		PackageChanges:       "adds three hooks to ~/.cursor/hooks.json, keeping a backup, with Wakeshift's npm package " + ConnectPackage + ", which also puts Wakeshift's connector in ~/.wakeshift/bin",
		PackageUpdateChanges: "it runs Wakeshift's npm package " + ConnectPackage + " at its new version, which replaces Wakeshift's connector and keeps their settings and connected conversations",
		Changes:              "adds three hooks to ~/.cursor/hooks.json, keeping a backup",
		ToolRules:            true,
		Trust:                "Restart cursor-agent to load them (a new chat isn't enough); the Cursor app reloads them by itself. There's nothing to approve for hooks in your home folder.",
		Resume:               "Then reopen this chat (cursor-agent --continue) and send any message (\"done\" is enough), so I can start listening again.",
		Restart:              "After any later restart, reopen this chat and send any message, so I can start listening again.",
		AfterUpdate:          "There's nothing to approve or restart: the hooks run the new version from their next call.",
		When:                 "if they come within a minute after the agent finishes a turn, otherwise at the start of the next one (not in cursor-agent -p)",
		NoteAt:               "right after this result",
		Verified:             "cursor-agent 2026.09.23",
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
		Marketplace: &Marketplace{
			Install: "copilot plugin marketplace add {source} && copilot plugin install wakeshift@wakeshift",
			// A curl install has no plugin to update: install it then.
			Update:  "copilot plugin update wakeshift || (copilot plugin marketplace add {source}; copilot plugin install wakeshift@wakeshift)",
			Remove:  "copilot plugin uninstall wakeshift",
			RefSep:  "#",
			Changes: "installs Wakeshift's plugin for Copilot CLI with Copilot's own plugin installer, from github.com/wakeshift/agent-plugins, which adds its hooks; they download Wakeshift's connector into ~/.wakeshift/bin, checking its SHA-256",
			Trust: "There's nothing to approve for the plugin's hooks. Restart copilot to load them. " +
				"Once they connect this conversation, the command that wakes the agent while it's idle starts without asking: they approve Wakeshift's wait command, and only that command.",
			AfterUpdate: "There's nothing to approve. Restart copilot so it loads the new plugin, then reopen this session (copilot --continue).",
		},
		Trust: "There's nothing to approve for hooks in your home folder. Restart copilot to load them (/env lists them). " +
			"Once they connect this conversation, the command that wakes the agent while it's idle starts without asking: they approve Wakeshift's wait command, and only that command.",
		Resume:      "Then reopen this session (copilot --continue) and send any message (\"done\" is enough), so I can start listening again.",
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
		Package:        "@wakeshift/opencode",
		PackageCommand: "opencode plugin {spec} --global --force",
		PackageChanges: "installs Wakeshift's plugin for opencode, the npm package @wakeshift/opencode, with opencode's own plugin command: opencode adds it to the plugin list in ~/.config/opencode/opencode.json, keeping their other settings, and downloads it with Wakeshift's connector for this computer (the plugin also removes ~/.config/opencode/plugins/wakeshift.ts, if an earlier install wrote it)",
		Changes:        "writes one plugin file, ~/.config/opencode/plugins/wakeshift.ts",
		Trust:          "Quit and restart opencode to load the Wakeshift plugin (opencode doesn't ask to approve plugins).",
		Resume:         "Then reopen this session (opencode --continue). Once it's connected, opencode reconnects it by itself whenever it starts.",
		AfterUpdate:    "Quit and restart opencode so it loads the new version.",
		When:           "as soon as they arrive, even while the session is idle, as long as opencode is open (not in opencode run)",
		NoteAt:         "right after this result",
		Verified:       "1.18.7",
	},
	{
		// pi (earendil-works/pi, the terminal coding agent) calls MCP
		// servers with clientInfo "pi" and its version, protocol
		// 2025-11-25, User-Agent "undici" (Node's default: no rule) and
		// tools/call _meta of progressToken only (1.5.0, checked against a
		// probe server). Exact: "pi-…" client names may be other products.
		// Its MCP client drops a result's _meta before extensions see it,
		// so the beacon is the result's last line, which Wakeshift's
		// extension takes out; the extension sets conversation_id to the
		// session's ID (PI_SESSION_ID in pi's bash tool) and harness to
		// "pi" on the app's tools that declare them, so the beacon names
		// the session (internal/connector, pi.go).
		ID: "pi", Name: "pi", Claim: "pi", Aliases: []string{"pi-coding-agent", "pi-agent"}, Clients: []string{"pi"}, Exact: true,
		Wake: Plugin, Ack: "at least once, acknowledged when the turn that carries the message starts",
		Binding: Unbound, Carrier: Text, StripsText: true, Installable: true, ConversationEnv: "PI_SESSION_ID",
		KitWord: "extension",
		// pi's own package command, once this Wakeshift knows a published
		// version (SetPackageVersion): it adds the package to the packages
		// list in pi's settings and installs it with npm, which also picks
		// the connector for the computer (an optional dependency). Installing
		// another version replaces the entry (pi keys packages by name), so
		// the update is the same command. Until a version is known, the
		// offer is Wakeshift's installer, which writes the same extension as
		// a file of its own.
		Package:        "@wakeshift/pi",
		PackageCommand: "pi install npm:{spec}",
		PackageChanges: "installs the npm package @wakeshift/pi with pi's own package command: pi adds it to the packages list in ~/.pi/agent/settings.json, keeping their other settings, and downloads it with Wakeshift's connector for this computer (the extension also removes ~/.pi/agent/extensions/wakeshift.ts, if an earlier install wrote it)",
		Uninstall:      "pi remove npm:@wakeshift/pi",
		Unload:         "type /reload in pi",
		InstallerFile:  "~/.pi/agent/extensions/wakeshift.ts",
		Changes:        "writes one extension file, ~/.pi/agent/extensions/wakeshift.ts, and puts Wakeshift's connector, which the extension runs, in ~/.wakeshift/bin, checking its SHA-256",
		Trust:          "There's nothing to approve in pi: type /reload in pi (or restart it with pi --continue) so it loads Wakeshift's extension.",
		// Its connected sessions reconnect by themselves (the extension, at
		// session_start), so a later restart needs nothing (Restart "").
		Resume:      "Once it has connected this conversation, it reconnects by itself whenever pi starts or reloads.",
		AfterUpdate: "There's nothing to approve: type /reload in pi (or restart it with pi --continue) so it loads the new version.",
		When:        "as soon as they arrive, even while the session is idle, or once its turn ends if it's busy, as long as pi is open (not in pi -p)",
		NoteAt:      "right after this result",
		Verified:    "1.5.0",
	},
	{
		ID: "antigravity", Name: "Antigravity CLI", Claim: "antigravity", Aliases: []string{"agy", "antigravity-cli"}, Clients: []string{"antigravity-client"},
		Wake: StopHook, ZeroInstall: Background, Ack: "at least once, acknowledged when the stop hook hands the message over",
		Binding: ByConversation, Carrier: Text, ConversationMeta: "antigravity.google/conversation_id", Fingerprint: "antigravity.google/conversation_id", Installable: true,
		// agy installs only from a git URL whose root is the plugin, so the
		// plugin is a repository of its own (plugins/agent-plugins/standalone);
		// installing it again replaces the installed copy.
		Marketplace: &Marketplace{
			Install: "agy plugin install {source}",
			Update:  "agy plugin install {source}",
			Remove:  "agy plugin uninstall wakeshift",
			RefSep:  "/tree/",
			Repo:    "https://github.com/wakeshift/antigravity-plugin",
			// agy loads a plugin when it starts: a running agy keeps the hooks it
			// loaded, so an update takes a restart, as the install does.
			AfterUpdate: "There's nothing to approve. Restart agy and reopen this conversation (agy --continue), so it loads the new plugin.",
			Changes:     "installs Wakeshift's plugin with agy's own plugin installer, from github.com/wakeshift/antigravity-plugin, into ~/.gemini/config/plugins/wakeshift; its hooks run Wakeshift's connector, which the plugin downloads into ~/.wakeshift/bin and checks against the SHA-256 it carries, and take out a \"wakeshift\" entry an earlier install added to ~/.gemini/config/hooks.json",
		},
		Changes:     "adds a \"wakeshift\" entry to ~/.gemini/config/hooks.json, keeping a backup",
		Trust:       "There's nothing to approve for hooks in your home folder. Restart agy to load them (/hooks lists them).",
		Resume:      "Then reopen this conversation (agy --continue) and send any message (\"done\" is enough), so I can start listening again.",
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
		// From npm once a release has published it (SetPackageVersion):
		// npm checks the package's integrity, and the plugin itself refuses
		// to turn hooks on beyond loopback without the user's yes (Agree), so
		// nothing here detects the bind. --force: npm isn't a source OpenClaw
		// trusts without asking, and it replaces an install however it was
		// made (openclaw.sh's included); --accept-capabilities: nothing can
		// prompt; enable: a reinstall after an uninstall stays disabled.
		Package:        "@wakeshift/openclaw",
		PackageCommand: "openclaw plugins install npm:{spec} --force --accept-capabilities && openclaw plugins enable wakeshift --accept-capabilities",
		PackageIssuer:  `openclaw config set plugins.entries.wakeshift.config.issuer '"{origin}"' --strict-json`,
		PackageChanges: "installs Wakeshift's plugin, the npm package @wakeshift/openclaw, with OpenClaw's own openclaw plugins install, and enables it, trusting this Wakeshift. When it connects a conversation, the plugin turns on OpenClaw's hooks endpoint with a token of its own, allowing it to start turns only in the conversations that connect through Wakeshift; hooks settings OpenClaw already has are only added to, and `openclaw wakeshift reset` undoes this",
		Uninstall:      "openclaw wakeshift reset && openclaw plugins uninstall wakeshift --force",
		// One question, not two: a gateway in Docker usually listens beyond
		// this computer, and the installer would otherwise stop to ask about
		// it (wakebench, October 26, 2026: 5 of 5 asked twice; with this, 0).
		AskAlso:     "if the OpenClaw gateway listens beyond this computer (in Docker it usually does), OpenClaw's hooks endpoint shares that listener, so it's reachable there too, guarded by the plugin's token; a firewall or proxy rule that blocks /hooks/ from other machines keeps it local.",
		AgreeEnv:    "WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1",
		Agree:       "openclaw config set plugins.entries.wakeshift.config.allowNetworkHooks true --strict-json",
		Run:         "Run it yourself with your exec tool, on the machine (or in the container) where the OpenClaw gateway runs.",
		Trust:       "There's nothing to approve in OpenClaw, and no restart.",
		Resume:      "The plugin starts with my next turn. Once it's connected, it reconnects by itself whenever the gateway starts.",
		AfterUpdate: "There's nothing to approve again, and no restart. The new plugin starts with your next turn, so ask them to send any reply first.",
		When:        "as soon as they arrive, even while the session is idle, as long as the OpenClaw gateway runs",
		NoteAt:      "right after this result (or, when OpenClaw runs the turn in Codex, with the result of your next call to this app's tools)",
		Verified:    "2026.10.4",
	},
	{
		// clientInfo "hermes-agent" with Hermes's version (0.22.5): Hermes
		// sets it itself now. Earlier versions sent "mcp" 0.1.0, the MCP
		// Python SDK's default, which other Python clients send too. The
		// User-Agent, python-httpx2/2.12.0, is the SDK's either way. Its
		// plugin also passes the harness argument to every Wakeshift app's
		// tool (plugins/hermes, fill_args), for an app that never sees
		// the client name (a stateless server on a protocol before
		// 2026-07-28).
		ID: "hermes", Name: "Hermes Agent", Claim: "hermes", Aliases: []string{"hermes-agent"},
		Clients: []string{"hermes-agent", "mcp"}, Shared: []string{"mcp"}, Unsure: "you're Hermes Agent (Nous Research)",
		// A stateless server on a protocol before 2026-07-28 (the
		// quickstart's createMcpHandler, Shipyard in wakebench's realistic
		// tier) never sees Hermes's client name, and before its plugin is
		// installed nothing else in the call names it unless the model
		// passes harness itself: wakebench, November 7, 2026, 5 of 6
		// conversations got "Wakeshift can't wake this agent" and never
		// heard of the install. The User-Agent is the MCP Python SDK 2.x's
		// (other Python clients send it too), so it's Unsure.
		SharedUserAgents: []string{"python-httpx2"},
		Wake:             Plugin, Ack: "at least once, acknowledged once Hermes accepts the run",
		Binding: Unbound, Carrier: Meta, Installable: true,
		Repo:    "wakeshift/hermes-plugin",
		Changes: "installs Wakeshift's plugin from its public repository, github.com/wakeshift/hermes-plugin, with Hermes's own plugin installer, into $HERMES_HOME/plugins/wakeshift, enables it, and lets it start turns in your Hermes chats (Hermes's allow_gateway_injection setting for it, without which the plugin wakes no Hermes chat, through the API server or `hermes chat`); it starts them only in conversations connected to Wakeshift",
		Allow:   "hermes config set plugins.entries.wakeshift.allow_gateway_injection true",
		// The command a user types is `hermes …`; only the agent's own run
		// may need another form (wakebench, November 2, 2026: the bare
		// command failed with exit 127 in the image's terminal tool, and the
		// agent then gave the user its fallback to type).
		Run: "It runs where Hermes runs (inside its container, if it's in one). In your question, quote the command as it is, starting with `hermes`: that's what the user types where Hermes runs (in Docker, after `docker exec -u hermes <container>`). " +
			"To run it yourself, first check `command -v hermes`: if your terminal doesn't have it (Hermes's Docker image doesn't put it on the PATH), run each `hermes` in the command as `\"$HERMES_PYTHON\" -m hermes_cli.main` instead (Hermes sets HERMES_PYTHON for its tools). Whatever you ran, every command you give the user starts with `hermes`, so `hermes chat --continue` rather than that form. " +
			"Hermes may hold the command for approval, and in a chat through Hermes's API server nobody can approve it. If Hermes blocks the command, running it fails, or you can't run commands, tell them so in your reply and give them the command to run there, starting with `hermes`: never end your reply without it. " +
			"If its output warns \"custom (unreviewed) source\", tell them in one line: Wakeshift's plugin isn't in Hermes's catalog yet, so Hermes installed it from Wakeshift's public repository, github.com/wakeshift/hermes-plugin. " +
			"If its output says \"Gateway reloaded plugins\" and that the plugin is active in the running gateway now, Hermes loaded it already: skip the restart and resume below, and just ask them to send any message.",
		Trust:       "There's nothing to approve in Hermes after the install, but Hermes loads plugins only when a chat starts or is resumed, so Wakeshift can't wake this chat until they resume it: through Hermes's gateway or API server, restart the gateway and keep sending to this session as before; in `hermes chat`, exit and run `hermes chat --continue`, or `hermes chat --tui --continue` to resume it in Hermes's TUI, which shows each woken turn as it runs (the classic `hermes chat` runs a woken turn without showing it, until Hermes adds that). A new chat works too. If the plugin isn't enabled, first run `hermes plugins enable wakeshift` where Hermes runs.",
		Restart:     "After a later restart of hermes chat, send any message once you've resumed it, so it reconnects (conversations through Hermes's gateway reconnect by themselves).",
		AfterUpdate: "There's nothing to approve again, but Hermes loads plugins only when a chat starts or is resumed: ask them to resume this chat (exit and run `hermes chat --continue`, or restart Hermes's gateway) or start a new one, so it loads the new plugin.",
		When:        "as soon as they arrive, even while it's idle, as long as Hermes is running (conversations through Hermes's API server or CLI)",
		NoteAt:      "right after this result",
		Verified:    "0.22.5",
	},
	{
		// ChatGPT (and other OpenAI-hosted MCP clients) send clientInfo
		// "openai-mcp" 1.0.0 and User-Agent openai-mcp/1.0.0, per OpenAI's
		// developer forum (not captured here: no ChatGPT sign-in), and
		// _meta openai/session (an anonymized conversation ID; the Apps SDK
		// reference). It's woken by MCP Events in Work chats and dots, for
		// the app's own event types (pkg/wakeshift Attach, the TypeScript
		// SDK's attach): ChatGPT subscribes natively and starts a run
		// for each occurrence. What a conversation subscribes to with the
		// app's own tools waits for its next tool call (appapi
		// chatgptResult). openai/session only recognizes it.
		ID: "chatgpt", Name: "ChatGPT", Claim: "chatgpt", Clients: []string{"openai-mcp"}, UserAgents: []string{"openai-mcp"}, Fingerprint: "openai/session",
		Wake: Events, Ack: "at least once, acknowledged when ChatGPT accepts the webhook (not when the run reads it)",
		When: "as soon as they arrive, as a new run in the chat (Work chats and dots), for the app's own events it subscribes to",
	},
	{
		// Capy (capy.ai), a cloud coding-agent platform. Its backend calls
		// remote MCP servers with clientInfo "capy" 2 and User-Agent "capy",
		// capabilities {} (no tasks, elicitation, events or roots), first a
		// server/discover on 2026-07-28 with no session, then an initialize
		// on 2025-06-18; tools/call carries no _meta and no thread ID (only
		// traceparent and b3 headers). Checked with a probe server on
		// November 5, 2026. Each thread runs on its own VM, which sleeps
		// about 2 minutes after the thread goes idle (killing any listener),
		// and keeping it awake costs the user and lasts 6 hours at most. But
		// an agent can create a watch on its own thread (capy.subscribe with
		// an incoming_webhook source, until at most 30 days out) whose URL
		// wakes the thread on each POST, even idle, with no process or awake
		// machine. So it's woken by an agent-hosted webhook (Webhook,
		// internal/agenthooks): the subscribing result asks it to create the
		// watch and register the URL with a registration link. A thread that
		// doesn't (Fallback) runs the one-shot command (CapyLoop), whose
		// finishing wakes it: Capy wakes a thread when a command it waits on
		// finishes, not when one prints. Nothing to install.
		ID: "capy", Name: "Capy", Clients: []string{"capy"}, UserAgents: []string{"capy"},
		Wake: Webhook, Fallback: OneShot, ZeroInstall: Background,
		Ack:  "at least once, acknowledged when Capy accepts the webhook (202 accepted or duplicate), not when the agent reads it; with the fallback command, by the command itself once it has printed the message",
		When: "within seconds, even while the thread is idle, through its webhook watch once it's registered (else while its fallback command runs)",
	},
	{
		// Muse, Meta's agent (meta.ai). Its MCP connector, run from Meta's
		// servers (egress 2606:54c1:…), sends clientInfo
		// "muse-custom-connector" 1 on protocol 2025-03-26 with
		// capabilities {} and User-Agent Python-urllib/3.12 (generic: no
		// rule), and a new initialize before every tool call; tools/call
		// carries no _meta and no thread ID. Remote MCP with OAuth and
		// dynamic client registration. Checked with a probe server on
		// November 5, 2026. It can't hold a stream open, but it runs
		// background tasks on its sandbox VM, and a finished task's output
		// starts a new turn in the conversation (about 14s from a publish
		// to the turn, twice; an earlier session woken 3 of 3 times by a
		// one-shot wait). So it's woken by a one-shot command (MuseLoop):
		// it waits, prints what arrived, confirms it and finishes. Nothing
		// to install. Exact: only that client name, not "muse-…".
		ID: "muse", Name: "Muse", Clients: []string{"muse-custom-connector"}, Exact: true,
		Wake: OneShot, ZeroInstall: Background, Ack: "at least once to the command, acknowledged by the command itself once it has printed the message, just before it finishes (not when the agent reads it)",
		When: "within seconds, even while the conversation is idle, while its background command runs; what arrives while the agent is acting comes with the command's next run",
	},
	// Retired: recognized only to tell its agent what replaced it.
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
		if p := strings.ToLower(h.ClientPrefix); p != "" && strings.HasPrefix(name, p) && len(name) > len(p) {
			return h, true
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
	if h, ok := ForClient(base); ok && Surely(h, base).Unsure == "" { // a generic name others send too doesn't count tagged
		return h, true
	}
	for _, h := range all {
		if h.Unsure == "" && !h.Exact && (strings.EqualFold(base, h.Name) || strings.EqualFold(base, h.ID)) {
			return h, true
		}
	}
	return Harness{}, false
}

// Via is the signal a call's harness was recognized by.
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

// Recognize is the harness a tool call came from and how it was told: by
// the client name if that names a harness (a stated client always wins:
// _meta and the User-Agent never override it, except the _meta that says
// another harness runs that client, Harness.Embeds), else by a Fingerprint key
// in the call's _meta, else by a product token in the User-Agent (one only
// it sends, else, for a call with no client name, one it shares with other
// clients, which leaves it Unsure: SharedUserAgents). A
// stateless MCP server (a new server per request) never sees initialize,
// so it has no client name; the other two come with every call. Every
// signal is self-asserted, the client name included: recognizing a harness
// by one grants nothing that stating its client name doesn't.
func Recognize(client string, meta map[string]any, userAgent string) (Harness, Via, bool) {
	for _, h := range all {
		if Embedded(h, meta) { // the stated client is the one it runs
			return h, ByMeta, true
		}
	}
	if h, ok := ForClient(client); ok {
		return Surely(h, client), ByClient, true
	}
	if strings.TrimSpace(client) != "" {
		if h, ok := Get(AgentID(client)); ok && !h.Exact {
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
		// A client that stated a name isn't one of these: Hermes Agent
		// states "hermes-agent" (or, before 0.22.5, "mcp").
		if strings.TrimSpace(client) == "" {
			for _, h := range all {
				for _, p := range h.SharedUserAgents {
					if slices.Contains(products, strings.ToLower(p)) && h.Unsure != "" {
						return h, ByUserAgent, true
					}
				}
			}
		}
	}
	return Harness{}, "", false
}

// Surely is h as a call from client is recognized: a harness with a client
// name others send too (Unsure) isn't Unsure when the call sent one of its
// own names that isn't Shared (Hermes Agent's "hermes-agent"), so its
// notes don't hedge and a claim can't override it (Refine). h as it is
// otherwise.
func Surely(h Harness, client string) Harness {
	c := strings.TrimSpace(client)
	if h.Unsure == "" || c == "" || containsFold(h.Shared, c) || !containsFold(h.Clients, c) {
		return h
	}
	h.Unsure = ""
	return h
}

// NamedByClient reports whether h's agents are named after the client's
// own name (AgentID) rather than h's ID: a harness with a client name
// others send too (Harness.Shared).
func NamedByClient(h Harness) bool { return len(h.Shared) > 0 }

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
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
// copilot) or a variant of its ID ("claude-code-vscode" → claude-code),
// except for an Exact harness, which only its client names map to (a
// "muse-…" client isn't Muse's); else the name itself as an ID ("My
// Client" → my-client). IDs are 2-40
// lowercase letters, digits and dashes; "agent" if nothing's left. A
// harness with a client name others send too (NamedByClient: Hermes
// Agent's "mcp") keeps the client's own name as the ID, whichever of its
// names it sends ("mcp", "hermes-agent").
func AgentID(client string) string {
	if h, ok := ForClient(client); ok && !NamedByClient(h) {
		return h.ID
	}
	id := slug(client)
	for _, h := range all {
		if NamedByClient(h) || h.Exact {
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

// installerURL, when set, is where install commands fetch the scripts
// from: a published release of the installers (WAKESHIFT_INSTALLER_URL;
// docs/dependencies.md, "Signed installers").
var installerURL atomic.Pointer[string]

// SetInstallerURL makes install commands fetch each script, <harness>.sh,
// from u instead of the Wakeshift's own /install. The scripts there must
// pin this Wakeshift as their issuer, so only the Wakeshift the release was
// built for sets it. "" goes back to /install.
func SetInstallerURL(u string) {
	u = strings.TrimSuffix(u, "/")
	installerURL.Store(&u)
}

// ScriptURL is where a harness's install script is fetched from: the
// installers' release (SetInstallerURL), else the Wakeshift at base.
func ScriptURL(h Harness, base string) string {
	if u := installerURL.Load(); u != nil && *u != "" {
		return *u + "/" + h.ID + ".sh"
	}
	return strings.TrimSuffix(base, "/") + "/install/" + h.ID + ".sh"
}

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
	return "curl -fsSL " + ScriptURL(h, base) + sh
}

// pluginIssuer is the Wakeshift a plugin installed from its public
// repository trusts when nothing says otherwise (plugins/hermes,
// DEFAULT_ISSUER).
const pluginIssuer = ProductionIssuer

// ConnectPackage is Wakeshift's npm package that installs the connector and
// its hooks for a harness (`npx -y @wakeshift/connect <harness>`,
// plugins/connect).
const ConnectPackage = "@wakeshift/connect"

// ProductionIssuer is production Wakeshift's address: the issuer the
// plugins and npm packages it releases trust by default.
const ProductionIssuer = "https://app.wakeshift.ai"

// originRE is a Wakeshift's address that can go into a shell command and a
// YAML list as it is.
var originRE = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

// OfferCommand is the command an install offer gives for a harness: for
// one with a Repo, its own plugin installer (`hermes plugins install
// wakeshift/hermes-plugin --enable`) and its Allow command, preceded, for a Wakeshift other than
// the one the plugin trusts by default, by the setting that makes it trust
// the Wakeshift at base instead; otherwise InstallCommand.
func OfferCommand(h Harness, base string) string { return repoCommand(h, base, false) }

// UpdateCommand is the command an update offer gives for a harness:
// OfferCommand with the plugin installer's --force for one with a Repo,
// otherwise InstallCommand. Hermes 0.22.5's `hermes plugins update` can't
// update a plugin Wakeshift's installer wrote (it has no .git), but
// `hermes plugins install --force` replaces the plugin however it was
// installed: over its own install from the same repository it keeps the
// plugin directory's untracked files; over Wakeshift's installer's it drops
// config.json (the Wakeshift it pinned), which the command's issuers
// setting replaces for a Wakeshift other than the default. Hermes's
// settings and the plugin's state ($HERMES_HOME/wakeshift) are outside the
// directory and stay.
func UpdateCommand(h Harness, base string) string { return repoCommand(h, base, true) }

func repoCommand(h Harness, base string, force bool) string {
	if spec := PackageSpec(h); spec != "" {
		cmd := strings.Replace(h.PackageCommand, "{spec}", spec, 1)
		if h.allowTools != "" {
			// The package runs Wakeshift's installer, which reads it
			// (WithTools).
			cmd = h.allowTools + " " + cmd
		}
		origin := strings.TrimSuffix(base, "/")
		if h.PackageIssuer == "" || origin == pluginIssuer || origin == "" {
			return cmd
		}
		if originRE.MatchString(origin) {
			return strings.Replace(h.PackageIssuer, "{origin}", origin, 1) + " && " + cmd
		}
	}
	if m := h.Marketplace; m != nil {
		tmpl := m.Install
		if force {
			tmpl = m.Update
		}
		if m.Since != "" && !atLeast(h.client, m.Since) {
			tmpl = m.BeforeInstall
			if force {
				tmpl = m.BeforeUpdate
			}
		}
		if c := marketplaceCommand(h, base, tmpl); c != "" {
			return c
		}
	}
	if h.Repo == "" || !h.Installable {
		return InstallCommand(h, base)
	}
	cmd := h.ID + " plugins install " + h.Repo + " --enable"
	if force {
		cmd += " --force"
	}
	if h.Allow != "" {
		cmd += " && " + h.Allow
	}
	origin := strings.TrimSuffix(base, "/")
	if origin == pluginIssuer || origin == "" {
		return cmd
	}
	if !originRE.MatchString(origin) {
		return InstallCommand(h, base)
	}
	// The issuer first: Hermes 0.22.5's install reloads a running gateway's
	// plugins at once, and the plugin reads the setting when it loads
	// (`hermes config set` keeps it for a plugin not installed yet, and the
	// install keeps it).
	return h.ID + ` config set plugins.entries.wakeshift.settings.issuers '["` + origin + `"]' && ` + cmd
}

// packageVersions is the version of each Package to offer, by harness ID
// (SetPackageVersion).
var packageVersions atomic.Pointer[map[string]string]

// packageVersionRE is a version npm takes that can go into a shell command
// as it is: X.Y.Z, with an optional prerelease.
var packageVersionRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// ValidPackageVersion reports whether v is a version SetPackageVersion takes.
func ValidPackageVersion(v string) bool { return len(v) <= 64 && packageVersionRE.MatchString(v) }

// SetPackageVersion makes the install and update offers for the harness
// with that ID give its Package at version v (PackageCommand). The package
// must trust this Wakeshift (production's releases trust production; a
// package for another is packed and published for it). "" (or a version
// that isn't one) goes back to InstallCommand.
func SetPackageVersion(id, v string) {
	for {
		old := packageVersions.Load()
		next := map[string]string{}
		if old != nil {
			for k, x := range *old {
				next[k] = x
			}
		}
		if ValidPackageVersion(v) {
			next[id] = v
		} else {
			delete(next, id)
		}
		if packageVersions.CompareAndSwap(old, &next) {
			return
		}
	}
}

// PackageSpec is h's Package at the version SetPackageVersion set ("" if
// it has none, or none is set).
func PackageSpec(h Harness) string {
	if h.Package == "" || h.PackageCommand == "" || !h.Installable {
		return ""
	}
	m := packageVersions.Load()
	if m == nil || (*m)[h.ID] == "" {
		return ""
	}
	return h.Package + "@" + (*m)[h.ID]
}

// changes is what the install offer tells the user it changes: its
// package's, when the offer gives the package's command.
func changes(h Harness) string {
	if h.PackageChanges != "" && PackageSpec(h) != "" {
		return h.PackageChanges
	}
	return h.Changes
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
	also := "; it also lets " + h.Name + " run " + name + "'s " + ToolList(o.Tools) + " " + plural(len(o.Tools), "tool") + " without asking each time (no other tools)"
	h.Changes += also
	if h.PackageChanges != "" {
		h.PackageChanges += also
	}
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

// unwoken is what a conversation nothing delivers to is told. It never
// says to call the tool again: the app's tool may have side effects (send
// a message, start a paid job), and the app's own tools are how the agent
// checks, as without Wakeshift.
const unwoken = "Wakeshift can't wake this conversation yet."

// connectsLater says how a conversation connects once the user has set an
// install up: from the next result of the app's tools that the agent
// calls anyway, never from a call made for it (the tool that subscribed
// may have side effects).
func connectsLater(h Harness) string {
	if h.ConnectsAfter != "" {
		return "You don't need to call this app's tools again for the install: " + Kit(h) + " " + connects(h) + " this conversation from this result " + h.ConnectsAfter +
			", and a note \"" + Connected + "\" then appears (after an hour, it connects from the next result of this app's tools that you call anyway)."
	}
	at := strings.Replace(h.NoteAt, "this result", "that result", 1)
	return "You don't need to call this app's tools again for the install: " + Kit(h) + " " + connects(h) + " this conversation from the next result of this app's tools that you call anyway, and a note \"" + Connected + "\" then appears " + at + "."
}

// InstallNote is what an agent in this harness should tell its user
// while nothing delivers Wakeshift's messages to its conversation: whether
// and how to install Wakeshift's hooks and the harness's trust step, or that
// it can't be woken yet (for a retired harness: at all, and what replaced
// it).
func InstallNote(h Harness, base string) string {
	h = Route(h, base)
	return unsure(h, installNote(h, OfferCommand(h, base)))
}

// unsure is a note for a harness recognized by a client name others send
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
	if h.Wake == Webhook {
		// Capy: a webhook watch on the thread, and nothing to install.
		return "You can be woken now, within seconds even while idle, through a webhook watch on this thread (subscribing says how)."
	}
	if h.ZeroInstall == Background && !h.Installable {
		// Muse: the command is the only way, and there's nothing to install.
		return "You can be woken now, within seconds even while idle, by a command you start in the background (subscribing gives it)."
	}
	if h.Wake == Monitor && !h.Installable {
		// Claude Cowork: a Monitor the agent starts, and nothing to install.
		return "You can be woken now, within seconds even while idle, by a Monitor you start (subscribing gives its command)."
	}
	if !h.Installable {
		return CantWake(h.Name)
	}
	if h.ZeroInstall == Monitor {
		return pluginFirst(h, "", install, false, "start the Monitor tool on your stream link, as this result says.")
	}
	if h.ZeroInstall == Background {
		return "You can be woken now, within seconds even while idle, by a command you start in the background (subscribing gives it). " + backgroundUpgrade(h, install)
	}
	install = agreed(h, install)
	ask := unwoken + " Ask the user now whether to install " + Kit(h) + " (it " + changes(h) + "), so new messages reach this conversation " + h.When + ". " +
		askAlso(h) + askShape(h, install) + " " + declined + " "
	if h.AskTool != "" {
		ask = unwoken + " " + Kit(h) + " " + delivers(h) + " new messages to this conversation " + h.When + ". " + askWith(h, install) + " "
	}
	if h.Run != "" {
		ask += h.Run + " "
	}
	if t := TrustNote(h); t != "" {
		ask += "Then tell them: " + t + " "
	}
	return ask + connectsLater(h)
}

// Kit is what an install offer for h (as Route presents it) installs, for
// people: "Wakeshift's plugin for Codex" where the offer is a plugin (the
// harness's own plugin command, an npm plugin, Hermes's repository, or a
// plugin file), "Wakeshift's package for Cursor" for Cursor's npm package
// (npx), and "Wakeshift's hooks for …" where it's a plain hooks install.
func Kit(h Harness) string { return "Wakeshift's " + kit(h) + " for " + h.Name }

// KitFor is Kit for h's offers from the Wakeshift at base.
func KitFor(h Harness, base string) string { return Kit(Route(h, base)) }

func kit(h Harness) string {
	switch {
	case h.KitWord != "":
		return h.KitWord
	case h.routed, h.Wake == Plugin, h.Plugin, h.Repo != "" && h.Installable:
		return "plugin"
	case PackageSpec(h) != "" && h.Package == ConnectPackage:
		return "package"
	}
	return "hooks"
}

// pick is one or many, agreeing with Kit (hooks are many).
func pick(h Harness, one, many string) string {
	if kit(h) == "hooks" {
		return many
	}
	return one
}

func delivers(h Harness) string { return pick(h, "delivers", "deliver") }
func connects(h Harness) string { return pick(h, "connects", "connect") }
func itThem(h Harness) string   { return pick(h, "it", "them") }
func itThey(h Harness) string   { return pick(h, "it", "they") }

// askShape is how the agent asks about an install and runs it: a real
// yes/no question that quotes the exact command (the user sees what will
// run before they agree), options in a neutral order, and on a yes the
// command as given, with nothing looked up first (wakebench, November 2,
// 2026: Codex's question left the command out, Copilot's only offered to
// give it, and Codex's agent read its docs skill and searched the web
// before running it).
func askShape(h Harness, install string) string {
	return "Ask it as a yes/no question that quotes this exact command, for example: \"Install " + Kit(h) + "? It runs `" + install + "`.\" " +
		"If you offer options, put yes first and don't mark either as recommended. " +
		"Only if they say yes, run exactly that command right away: it's complete as given, so don't read docs or search for it first."
}

// declined is what the agent tells the user after a no where nothing else
// wakes the conversation (wakebench, November 2, 2026: opencode's agent
// said it would "keep polling", which nothing does).
const declined = "If they say no, install nothing, and tell them you won't be notified here when something arrives, so they'll need to ask you to check."

// installedAlready is the exception an install note ends with: the hooks
// are installed if their note appears.
func installedAlready(h Harness) string {
	n := "Skip this if a note \"" + Connected + "\" appears " + h.NoteAt + ": " + Kit(h) + " " + pick(h, "is", "are") + " installed already."
	if h.Review && h.Trust != "" {
		n += " If the user installed " + itThem(h) + " but there's no such note, only the step in " + h.Name + " is needed."
	}
	return n
}

// CantWake is what a conversation in a harness Wakeshift can't wake (name
// "" for one it doesn't know) is told: one neutral line, so the agent
// doesn't promise the user a notification. Nothing to do: the app's own
// tools are how it checks, as without Wakeshift.
func CantWake(name string) string {
	who := "this agent"
	if name != "" {
		who = name + " conversations"
	}
	return "Wakeshift can't wake " + who + ", so nothing will notify you here when something arrives."
}

// RetiredNote is what an agent in a retired harness (one with ReplacedBy)
// is told: it can't be woken, and what replaced it ("" for any other).
func RetiredNote(h Harness) string {
	if h.ReplacedBy == "" {
		return ""
	}
	return "Wakeshift can't wake " + h.Name + " conversations: " + h.Blocker + "."
}

// AskYes and AskNo are the options an agent with an AskTool offers for the
// install (AskYes for harness h, as Route presents it), in that order.
func AskYes(h Harness) string { return "Yes, install " + Kit(h) }

const AskNo = "No, don't install anything"

// askWith is how an agent in a harness with an AskTool asks about the
// install: the tool, its two options, the command only on yes, and with a
// UserShell, the user running it themselves if the agent's run is refused.
func askWith(h Harness, install string) string {
	no := "nothing notifies this conversation; I check only when you ask"
	if h.ZeroInstall == Monitor {
		no = "messages still wake this conversation through the Monitor"
	}
	return askWithNo(h, install, no)
}

// askWithNo is askWith with the "no" option's description given. The
// question itself quotes the command too, and the options come yes first
// with neither marked as recommended (wakebench, November 2, 2026: models
// put "No" first and preselected it, or labelled it "(Recommended)").
func askWithNo(h Harness, install, no string) string {
	n := "Ask them with your " + h.AskTool + " tool, quoting the exact command in the question itself, for example: \"Install " + Kit(h) + "? It runs `" + install + "`.\" " +
		"Give two options, in this order, and mark neither as recommended: \"" + AskYes(h) + "\" (description: runs `" + install + "`, " + changes(h) + ") " +
		"and \"" + AskNo + "\" (description: " + no + "). " +
		"Run `" + install + "` only if they pick yes, right away and as given (don't look anything up first)"
	if h.UserShell != "" {
		n += "; if running it is refused (auto mode's classifier or a permission denial), give them the command to type themselves, as it is: `" + h.UserShell + " " + install + "` (the " + h.UserShell + " prefix runs it as the user)"
	}
	return n + "."
}

// Plugin first (Claude Code): a conversation that subscribes while nothing
// wakes it asks the user about Wakeshift's plugin before it starts what
// wakes it with nothing installed (the Monitor, or the MCP-tasks wait), and
// starts that only after a no or an install that didn't happen. In manual
// permission mode the Monitor's approval shows its whole wait loop with
// Claude Code's "Quoted text in this command can't be checked" warning, the
// scariest screen in any flow (wakebench, November 2, 2026), so a user who
// says yes never sees it. Nothing is lost while the agent asks: new
// messages are held for the conversation until the plugin connects it or
// the fallback starts.

// waker is what wakes a Claude Code conversation with nothing installed:
// the MCP-tasks wait (tasks) or the Monitor.
func waker(tasks bool) string {
	if tasks {
		return WaitTool
	}
	return "the Monitor"
}

// PluginFirstNote is what a conversation in a harness with an AskTool and a
// zero-install wake path (Claude Code) is told when it first subscribes
// while nothing wakes it, for the app named app: that subscribing alone
// won't reach it, and one question to the user, with three answers: be
// notified while this session is open (fallback, the zero-install path:
// what to start, and how), in every session (Wakeshift's plugin, as the
// Wakeshift at base offers it, then connect without starting anything
// else), or not at all. Nothing runs before the answer. tasks: the
// zero-install path is the MCP-tasks wait, not the Monitor.
func PluginFirstNote(h Harness, base, app string, tasks bool, fallback string) string {
	return pluginFirst(Route(h, base), app, OfferCommand(h, base), tasks, fallback)
}

// The three answers to the plugin-first question, in the order offered.
const (
	AskSession = "Notify me while this session is open"
	AskEvery   = "Notify me in every session"
	AskNone    = "Don't notify me"
)

// sessionOnly is the description of AskSession: what runs, for this
// session only, with nothing installed.
func sessionOnly(tasks bool) string {
	if tasks {
		return "waits on this app's own " + WaitTool + " tool for this session only; nothing is installed"
	}
	return "starts a background listener (the Monitor) for this session only; nothing is installed"
}

func pluginFirst(h Harness, app, install string, tasks bool, fallback string) string {
	if !h.Installable || h.AskTool == "" || install == "" {
		return fallback
	}
	svc := "the service " + app + " uses to notify agents"
	if app == "" {
		app, svc = "This app", "the service it uses to notify agents"
	}
	w := waker(tasks)
	// A fact first, then one question with every way to be notified, and
	// nothing to run before the answer. In the realistic bench tier
	// (wakebench R1-R5, November 2-3, 2026), Claude Code set aside a result
	// that told it to start a background command against an external URL,
	// or to offer "a third-party plugin" that "would edit your settings",
	// as an unrequested or injected instruction, believing the app's own
	// watch would tell it anyway ("Shipyard's own watch should notify me").
	// Saying first that subscribing alone won't reach the conversation, and
	// offering the listener, the plugin and nothing as the user's choice,
	// took it from 0/10 to asking or listening in nearly every run.
	n := "Subscribed, but that alone won't tell you when something arrives: " + app + " delivers its notifications through Wakeshift, " + svc + ", and they reach this conversation only once the user picks how. " +
		"The user asked to hear about it, so ask them now; nothing runs until they answer, and new messages are held for this conversation until then, so none are lost. " +
		"Ask them one question with your " + h.AskTool + " tool, \"How should I notify you here when it arrives, even while I'm idle?\", with three options, in this order, none marked as recommended: " +
		"\"" + AskSession + "\" (description: " + sessionOnly(tasks) + "), " +
		"\"" + AskEvery + "\" (description: runs `" + install + "` (" + Kit(h) + "), which " + changes(h) + "), " +
		"and \"" + AskNone + "\" (description: nothing runs; I check only when you ask). " +
		"Only if they pick \"" + AskEvery + "\", run `" + install + "` right away and as given (don't look anything up first)"
	if h.UserShell != "" {
		n += "; if running it is refused (auto mode's classifier or a permission denial), give them the command to type themselves, as it is: `" + h.UserShell + " " + install + "` (the " + h.UserShell + " prefix runs it as the user)"
	}
	n += ". If it installs, nothing else needs to run: don't start " + w + ", and don't call this app's tools again for it. "
	if t := TrustNote(h); t != "" {
		n += "In your reply to the user, tell them this: " + t + " "
	}
	n += selfConnects(h)
	n += " If they pick \"" + AskNone + "\", start nothing."
	return n + " If they pick \"" + AskSession + "\", or the plugin doesn't install (it fails, or it's refused and they don't run it themselves), use " + w + ": " + fallback
}

// selfConnects says how Claude Code's plugin (or Wakeshift's hooks)
// connect the conversation after the install with no call from the agent:
// the waiter that runs when a turn ends finds the result that offered the
// install in the transcript and connects from it (connector.installedSince,
// a late redeem), and the first wake after says so. The install never
// needs a repeat call: the tool that subscribed may have side effects.
func selfConnects(h Harness) string {
	when := "when this turn ends"
	if reloads(h) {
		when = "once it's loaded and the user has sent a message"
	}
	return Kit(h) + " then " + connects(h) + " this conversation by " + pick(h, "itself", "themselves") + " " + when + ", and " + pick(h, "its", "their") + " first wake starts with \"" + Connected + "\"."
}

// PluginConnectingNote is what such a conversation is told on a result
// while the result of its call just before is still connecting the plugin
// (its hooks haven't redeemed that result's beacon yet; they do within
// seconds): don't fall back yet, call again in a moment (wakebench,
// November 2, 2026: an agent that called twice at once got no "connected"
// and started the Monitor, which in manual mode means its approval on the
// happy path).
func PluginConnectingNote(h Harness, base string, tasks bool) string {
	h = Route(h, base)
	return "Your previous call's result is connecting " + Kit(h) + " to this conversation, which takes a few seconds. Don't start " + waker(tasks) + "."
}

// PluginPendingNote is what such a conversation is told on a later result
// while nothing wakes it: if the user installed the plugin, this result
// connects it (confirmNote), and otherwise again, the zero-install path's
// instructions for an agent told them before.
func PluginPendingNote(h Harness, base string, tasks bool, again string) string {
	h = Route(h, base)
	if !h.Installable || h.AskTool == "" {
		return again
	}
	w := waker(tasks)
	loaded, notYet := "", ""
	if reloads(h) {
		loaded = " and the user has loaded it (/reload-plugins, or a restart)"
		notYet = " If you installed it but they haven't loaded it yet, don't start " + w + " either: ask them again to, and to send any message after."
	}
	return "If you installed " + Kit(h) + " in this conversation" + loaded + ", don't start " + w + ": " + itThey(h) + " " + connects(h) + " this conversation by " + pick(h, "itself", "themselves") + ", and " + pick(h, "its", "their") + " first wake starts with \"" + Connected + "\"." +
		notYet + " If the user chose not to be notified (\"" + AskNone + "\"), start nothing. Otherwise (they chose this session only, or " + itThey(h) + " " + pick(h, "isn't", "aren't") + " installed): " + again
}

// PluginReadyNote is what subscribing tells a conversation whose call
// Wakeshift's plugin marked (internal/pluginmark: it's installed, and trusts
// this Wakeshift) before its hooks have connected it: they connect it from
// this result when the turn ends (Claude Code), or right after it (Codex), so there's nothing to ask, install or
// start (no Monitor: in manual mode it would ask the user to approve it).
// hooks: the mark is from the hooks Wakeshift's installer wrote, not the
// plugin (pluginmark.Mark.Hooks), which connect it the same way.
func PluginReadyNote(h Harness, app string, hooks bool) string {
	what, it := "Wakeshift's plugin for "+h.Name+" is", "it connects"
	if hooks {
		what, it = "Wakeshift's hooks for "+h.Name+" are", "they connect"
	}
	if h.Wake == Connector {
		// Codex: its PostToolUse hook connects the thread from this
		// result's beacon right away, and says so in a note after it.
		return "Subscribed. " + what + " installed here, so " + app + "'s notifications reach this conversation by themselves, even while you're idle: " +
			it + " this conversation from this result, and a note starting \"" + Connected + "\" follows it. " +
			"Nothing else needs to run: don't run a background command, don't install anything, and don't ask the user how to be notified; just tell them you'll let them know."
	}
	return "Subscribed. " + what + " installed here, so " + app + "'s notifications reach this conversation by themselves, even while you're idle: " +
		it + " this conversation when this turn ends, and new messages are held for it until then. " +
		"Nothing else needs to run: don't start a Monitor or a background command, don't install anything, and don't ask the user how to be notified; just tell them you'll let them know."
}

// reloads reports whether h's offer (Route) is a plugin the user must load
// in the running session first (Marketplace.Reload).
func reloads(h Harness) bool { return h.routed && h.Marketplace != nil && h.Marketplace.Reload }

// SubscribeNote is what a conversation that just asked to be woken is
// told while Wakeshift's hooks haven't connected it: that the hooks say so
// in a note if they're installed, and otherwise how to install them.
func SubscribeNote(h Harness, base string) string {
	h = Route(h, base)
	return SubscribeNoteFor(h, OfferCommand(h, base))
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
func RepeatNote(h Harness, base string) string {
	return RepeatNoteFor(Route(h, base), OfferCommand(h, base))
}

// RepeatNoteFor is RepeatNote with the install command given. It reads
// true whether or not this very result connects the conversation: the
// "connected" note comes after the result (wakebench, November 2, 2026:
// Codex's result said its hooks hadn't connected, right above the note
// that they had).
func RepeatNoteFor(h Harness, install string) string {
	if !h.Installable {
		return unsure(h, installNote(h, install))
	}
	ask := "If you haven't asked the user yet whether to install " + itThem(h) + " (it " + changes(h) + "), ask now. " + askAlso(h) + askShape(h, agreed(h, install)) + " If they agreed and it hasn't run, run it." + sep(h.Run)
	if h.AskTool != "" {
		ask = "If you haven't asked the user yet whether to install " + itThem(h) + ", ask now. " + askWith(h, install)
	}
	return unsureShort(h, "If "+Kit(h)+" "+pick(h, "is", "are")+" installed, "+itThey(h)+" "+connects(h)+" this conversation from this result, and a note \""+Connected+"\" appears "+h.NoteAt+": "+
		"then "+itThey(h)+" "+delivers(h)+" new messages, and the rest of this doesn't apply. Until then, nothing wakes this conversation. "+ask+
		" Don't ask again if they said no.")
}

// Asked once per conversation (vendor bench, November 6, 2026): a
// conversation that polls the app's status tool, which subscribes too, got
// the whole ask again on every call, and Claude Code read the copies as a
// repeated injected request and set them aside. So the ask comes in full
// once per conversation, and a later subscribing call that finds nothing
// waking the conversation yet gets one sentence: it's subscribed, what
// still has to happen, and where the ask is (the earlier result), never
// the ask again.

// AskedNote is what a conversation in h is told when it subscribes again
// while nothing wakes it, having been given the install ask (SubscribeNote)
// in an earlier result: one sentence that points back to that ask, and
// that this result connects the conversation if the install happened.
// The install command isn't repeated: the earlier result has it.
func AskedNote(h Harness, base string) string {
	h = Route(h, base)
	if !h.Installable {
		return unsure(h, installNote(h, OfferCommand(h, base)))
	}
	return unsureShort(h, "Subscribed, but nothing wakes this conversation until "+Kit(h)+" "+connects(h)+" it: if you haven't asked the user yet whether to install "+itThem(h)+", ask now, as an earlier result of this app's tools says; "+
		"if they agreed and it hasn't run, run the command that result gives; if they said no, don't ask again in this conversation. "+
		"Once "+itThey(h)+" "+pick(h, "is", "are")+" installed, "+itThey(h)+" "+connects(h)+" this conversation from this result, and a note \""+Connected+"\" appears "+h.NoteAt+".")
}

// PluginAskedNote is AskedNote for a harness with an AskTool and a
// zero-install wake path (Claude Code), whose first result asked the user
// one question with three answers (PluginFirstNote): while nothing the
// user picked runs yet (no plugin connected it, and its fallback never
// started), a later subscribing call is pointed back to that question.
func PluginAskedNote(h Harness, base, app string) string {
	h = Route(h, base)
	return "Subscribed. " + app + "'s notifications reach this conversation the way the user picks in the question an earlier result of this app's tools had you ask (\"" + AskSession + "\", \"" + AskEvery + "\" or \"" + AskNone + "\"); " +
		"until that runs, nothing wakes it, and new messages are held for it. If you haven't asked them yet, ask now, as that result says. " +
		"Once they've answered, do what that result says for their answer, and don't ask again in this conversation. If " + Kit(h) + " " + pick(h, "was", "were") + " installed, start nothing: " + strings.Replace(selfConnects(h), " then ", " ", 1)
}

// MonitorRunningNote is what subscribing says while the conversation's
// Wakeshift Monitor runs: nothing to start.
func MonitorRunningNote() string {
	return "Your Wakeshift Monitor is already running; this subscription's messages arrive on it too. If it has stopped, start it again with the same command as before."
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
	// A Package offer (the harness's own command): the record of the yes first.
	if h.Agree != "" && strings.HasPrefix(install, h.ID+" ") {
		return h.Agree + " && " + install
	}
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
