package harnesses

import (
	"bytes"
	"encoding/json"
)

// What the apps' "connect your agent" pickers say about each agent
// (apps/agentpicker renders it; `make agent-picker` regenerates the copies).
// The commands were checked against each harness's own CLI. Public copy: what happens and what to do, never how the
// listener works.
//
// Text may hold placeholders the picker fills per app:
//
//	{name}  the server's name in the agent's config (e.g. "webhooks")
//	{url}   the app's MCP URL
//	{each}  what wakes the agent, mid-sentence ("each post")
//	{Each}  the same, starting a sentence ("Each post")
//	{auth}  what to pick for a connector's authentication ("OAuth", or
//	        "No authentication" for an app without sign-in)
//	{hermesauth}      " --auth oauth" for `hermes mcp add` ("" without
//	                  sign-in: Hermes's gateway can't start OAuth itself)
//	{hermesauthyaml}  the same as a config.yaml line ("\n    auth: oauth")

// PickerStep is one thing the user does to add the app to an agent.
type PickerStep struct {
	// Kind: "run" (a command), "file" (a config file's text), "url" (the
	// MCP URL to paste), "note" (a sentence), "cursor" (Cursor's install
	// link, which the picker builds from the URL).
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
	Text  string `json:"text,omitempty"`
}

// PickerAgent is one agent in the picker.
type PickerAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Group: "main" (tiers 1 and 2, shown first) or "more" (behind "More
	// agents").
	Group string       `json:"group"`
	Steps []PickerStep `json:"steps"`
	// SignIn is how to start sign-in in this agent when the app needs the
	// user to sign in ("" when the agent opens it by itself).
	SignIn string `json:"signIn,omitempty"`
	// Wakes says, in plain words, whether it's woken while idle and what
	// to install or approve.
	Wakes []string `json:"wakes"`
}

// Picker is every agent, in the order the picker shows them: tier 1, then
// tier 2 (the main row), then tier 3 and any other MCP client (behind "More
// agents"). Tiers: bench/tiers.yaml.
func Picker() []PickerAgent {
	run := func(cmd string) PickerStep { return PickerStep{Kind: "run", Label: "Run", Text: cmd} }
	return []PickerAgent{
		{
			ID: "claude-code", Name: "Claude Code", Group: "main",
			Steps:  []PickerStep{run("claude mcp add --transport http {name} {url}")},
			SignIn: "If it doesn't open a browser to sign in, type /mcp in Claude Code, choose {name}, then Authenticate.",
			Wakes: []string{
				"The agent first asks whether to install Wakeshift's plugin for Claude Code. Say yes, then type /reload-plugins and send any message so it connects. Then {each} wakes the agent within seconds, even when it's idle, with nothing more to approve.",
				"If you say no, Claude Code starts a background monitor instead (in manual permission mode, it asks you to approve it), with nothing installed. Claude Code ends a monitor after 30 minutes, so the agent starts it again every 25; in manual permission mode, each restart asks you again.",
			},
		},
		{
			ID: "codex", Name: "Codex", Group: "main",
			Steps:  []PickerStep{run("codex mcp add {name} --url {url}")},
			SignIn: "If it asks you to sign in, run codex mcp login {name}.",
			Wakes: []string{
				"The first time, the agent offers to install Wakeshift's plugin for Codex with Codex's own plugin command. Say yes, then restart Codex with codex resume --last, choose Trust all and continue, and send any message so it connects.",
				"After that, {each} wakes the agent within seconds, even when it's idle.",
			},
		},
		{
			ID: "claude-cowork", Name: "Claude Cowork", Group: "main",
			Steps: []PickerStep{
				{Kind: "note", Text: "In the Claude app, open Settings, then Connectors, choose Add custom connector, and paste this URL."},
				{Kind: "url", Label: "Connector URL", Text: "{url}"},
				{Kind: "note", Text: "Then choose Connect."},
			},
			Wakes: []string{
				"The agent starts a background listener in the Cowork task. {Each} then wakes it within seconds while Cowork is open, even when it's idle. Nothing to install.",
				"If the agent says it can't reach Wakeshift, allow app.wakeshift.ai in the Claude app under Settings > Capabilities (on Team and Enterprise plans, an owner does it under Organization settings > Capabilities), then tell the agent.",
				"In a Claude chat outside Cowork, the agent isn't woken: it sees what's new the next time it uses the app's tools.",
			},
		},
		{
			ID: "cursor", Name: "Cursor", Group: "main",
			Steps: []PickerStep{
				{Kind: "cursor", Text: "Add to Cursor"},
				{Kind: "file", Label: "Or add it to ~/.cursor/mcp.json (the IDE and cursor-agent both read it)", Text: "{\n  \"mcpServers\": {\n    \"{name}\": {\n      \"url\": \"{url}\"\n    }\n  }\n}"},
			},
			Wakes: []string{
				"The agent starts a background command that waits (Cursor may ask you to allow it). {Each} wakes the agent within seconds, even when it's idle. Nothing to install. The agent also asks once whether to install Wakeshift's package for Cursor, which is optional.",
				"Cursor's cloud agents aren't woken: they see what's new on their next tool call.",
			},
		},
		{
			ID: "opencode", Name: "opencode", Group: "main",
			Steps: []PickerStep{run("opencode mcp add {name} --url {url}")},
			Wakes: []string{
				"The agent offers a one-time install of Wakeshift's plugin for opencode. Say yes, restart opencode (opencode --continue reopens the session), and ask the agent something that uses this app, which connects it.",
				"After that, {each} wakes the agent, even when it's idle.",
			},
		},
		{
			ID: "hermes", Name: "Hermes Agent", Group: "main",
			Steps: []PickerStep{
				run("hermes mcp add {name} --url {url}{hermesauth}"),
				{Kind: "note", Text: "Run it where Hermes runs (in Docker, inside its container); if it asks whether the server requires authentication, answer no. In a running chat, /reload-mcp picks it up, or start a new chat."},
				{Kind: "file", Label: "Or add it to $HERMES_HOME/config.yaml (~/.hermes/config.yaml unless it's set)", Text: "mcp_servers:\n  {name}:\n    url: {url}{hermesauthyaml}"},
			},
			Wakes: []string{
				"The agent offers a one-time install of Wakeshift's plugin for Hermes. Say yes (Hermes may ask you to approve the command once). If the install says the running gateway loaded the plugin, just send any message. Otherwise Hermes loads it when a chat starts or is resumed, so resume the chat (exit and run hermes chat --continue, or restart the gateway) or start a new one, and send any message.",
				"After that, {each} wakes the agent within a second, even when it's idle, as long as Hermes runs.",
				"Chat in Hermes's TUI (hermes --tui) to see each woken turn: the classic hermes chat runs it without showing it.",
			},
		},
		{
			ID: "pi", Name: "pi", Group: "main",
			Steps: []PickerStep{
				run("pi mcp add {name} --url {url}"),
				{Kind: "note", Text: "In a pi session that's already running, type /reload to pick it up."},
			},
			SignIn: "If it says the server needs sign-in, type /mcp in pi, choose {name}, then Sign in (or run pi mcp login {name}).",
			Wakes: []string{
				"The agent offers a one-time install of Wakeshift's extension for pi. Say yes, then type /reload in pi and ask the agent something that uses this app, which connects it.",
				"After that, {each} wakes the agent within seconds, even when it's idle, as long as pi is open. While it's busy, it gets {each} once its turn ends.",
			},
		},
		{
			ID: "antigravity", Name: "Antigravity", Group: "more",
			Steps: []PickerStep{run("agy mcp add {name} {url}")},
			Wakes: []string{
				"The agent starts a background command that waits (agy may ask you to allow it). {Each} wakes the agent within seconds, even when it's idle. Nothing to install. The agent also asks once whether to install Wakeshift's plugin for Antigravity, which is optional.",
			},
		},
		{
			ID: "grok-bot", Name: "Grok Bot", Group: "more",
			Steps: []PickerStep{
				{Kind: "note", Text: "Ask your bot to add an MCP server named {name} with this URL."},
				{Kind: "url", Label: "MCP URL", Text: "{url}"},
				{Kind: "note", Text: "Then start a new chat with the bot."},
			},
			Wakes: []string{
				"The first time, the agent sets up wake-ups for the bot: it runs three commands Wakeshift gives it, which install Wakeshift's connector on the bot's computer and make the bot's signing key, then creates a routine named \"Wakeshift wake-ups\" with a webhook trigger and gives you a one-time link. On that page, paste the routine's webhook URL and key from its settings.",
				"After that, {each} starts a run in the bot, even when it's idle. Each run counts toward your Grok usage, so Wakeshift starts at most one every 20 seconds, carrying everything waiting.",
				"Until then, or if you'd rather not set it up, the bot waits in the foreground, and talking to it interrupts the wait.",
			},
		},
		{
			ID: "chatgpt", Name: "ChatGPT", Group: "more",
			Steps: []PickerStep{
				{Kind: "note", Text: "In ChatGPT, open Settings, then Apps & Connectors, then Advanced settings, and turn on Developer mode. Back in Apps & Connectors, choose Create and paste this URL."},
				{Kind: "url", Label: "Connector URL", Text: "{url}"},
				{Kind: "note", Text: "For authentication, choose {auth}."},
			},
			Wakes: []string{
				"Open a Work chat (ChatGPT Business or Enterprise; on desktop, choose Cloud), turn on the connector, and ask ChatGPT to subscribe.",
				"{Each} then starts a new ChatGPT run. A run takes longer to start than other agents' wakes.",
				"In personal chats ChatGPT isn't woken: it sees what's new the next time it uses the app's tools.",
			},
		},
		{
			ID: "openclaw", Name: "OpenClaw", Group: "more",
			Steps: []PickerStep{
				// Not `openclaw mcp add`: since 2026.11 it refuses while the
				// gateway runs (it needs the gateway stopped), while `config
				// set` goes through a running gateway, which applies it
				// without a restart, and writes the file when it's stopped
				// (#1109).
				run(`openclaw config set mcp.servers.{name} '{"url":"{url}","transport":"streamable-http"}' --strict-json`),
				{Kind: "note", Text: "Run it where the OpenClaw gateway runs (in Docker, inside its container). It works with the gateway running, and new MCP servers are used from the next turn."},
			},
			Wakes: []string{
				"The agent offers a one-time install of Wakeshift's plugin for OpenClaw. Say yes. The plugin connects the conversation the next time the agent uses this app, so ask it something that does. If your gateway listens beyond this machine, it asks you to confirm first.",
				"After that, {each} wakes the agent within seconds, even when it's idle, as long as the gateway runs.",
			},
		},
		{
			ID: "copilot", Name: "Copilot CLI", Group: "more",
			Steps: []PickerStep{run("copilot mcp add --transport http {name} {url}")},
			Wakes: []string{
				"The agent starts a background command that waits. {Each} wakes the agent within seconds, even when it's idle. Nothing to install. The agent also asks once whether to install Wakeshift's plugin for Copilot CLI, which is optional.",
				"Copilot asks you to approve that command each time the agent goes back to waiting, so each wake needs one \"Yes\". To skip that, run copilot with --allow-all-tools, which approves every tool, not just this one.",
			},
		},
		{
			ID: "capy", Name: "Capy", Group: "more",
			Steps: []PickerStep{
				{Kind: "note", Text: "In Capy, open Settings, then MCP & plugins, add a server, choose Streamable HTTP, and paste this URL."},
				{Kind: "url", Label: "Server URL", Text: "{url}"},
				{Kind: "note", Text: "Capy detects whether it needs sign-in. Choose Connect if it asks."},
			},
			Wakes: []string{
				"The first time the agent subscribes, it creates a webhook watch on its own thread and registers it with Wakeshift, with one command. Nothing to install or approve.",
				"After that, {each} wakes the thread, even when it's idle and its machine is asleep, until the watch ends (at most 30 days). The next time the agent subscribes after that, it sets up a new one.",
				"If it can't set up the watch, it waits on a command in the background instead, and {each} wakes it when the command finishes.",
			},
		},
		{
			ID: "muse", Name: "Muse", Group: "more",
			Steps: []PickerStep{
				{Kind: "note", Text: "In Muse, add a custom connector and paste this URL."},
				{Kind: "url", Label: "Connector URL", Text: "{url}"},
			},
			Wakes: []string{
				"The agent starts a command that waits as a background task in Muse. {Each} ends the task, which wakes the agent in about 15 seconds, even when it's idle; the agent acts on it, then starts the command again. Nothing to install.",
				"What arrives while the agent is acting comes as soon as it starts the command again.",
			},
		},
		{
			ID: "other", Name: "Any other MCP client", Group: "more",
			Steps: []PickerStep{{Kind: "url", Label: "MCP URL (Streamable HTTP)", Text: "{url}"}},
			Wakes: []string{
				"It sees what's new on its next call to the app's tools.",
			},
		},
	}
}

// PickerJSON is Picker as the JSON the apps embed (apps/agentpicker).
func PickerJSON() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(Picker()); err != nil {
		panic(err)
	}
	return b.Bytes()
}
