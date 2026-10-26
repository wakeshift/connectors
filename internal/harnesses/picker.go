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
				"Claude Code asks you to approve a background monitor. Approve it, and {each} wakes the agent within seconds, even when it's idle.",
				"Nothing to install. If the agent offers to install Wakeshift's hooks, they're optional.",
			},
		},
		{
			ID: "codex", Name: "Codex", Group: "main",
			Steps:  []PickerStep{run("codex mcp add {name} --url {url}")},
			SignIn: "If it asks you to sign in, run codex mcp login {name}.",
			Wakes: []string{
				"The first time, the agent offers a one-time install of Wakeshift's connector for Codex. Say yes, then type /hooks in Codex and press t to trust it.",
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
				"{Each} wakes the agent within seconds while Cowork is open, even when it's idle. Nothing to install.",
			},
		},
		{
			ID: "cursor", Name: "Cursor", Group: "main",
			Steps: []PickerStep{
				{Kind: "cursor", Text: "Add to Cursor"},
				{Kind: "file", Label: "Or add it to ~/.cursor/mcp.json (the IDE and cursor-agent both read it)", Text: "{\n  \"mcpServers\": {\n    \"{name}\": {\n      \"url\": \"{url}\"\n    }\n  }\n}"},
			},
			Wakes: []string{
				"The agent starts a background command that waits (Cursor may ask you to allow it). {Each} wakes the agent within seconds, even when it's idle. Nothing to install.",
				"Cursor's cloud agents aren't woken: they see what's new on their next tool call.",
			},
		},
		{
			ID: "opencode", Name: "opencode", Group: "main",
			Steps: []PickerStep{run("opencode mcp add {name} --url {url}")},
			Wakes: []string{
				"The agent offers a one-time install of Wakeshift's plugin for opencode. Say yes, and restart opencode.",
				"After that, {each} wakes the agent, even when it's idle.",
			},
		},
		{
			ID: "antigravity", Name: "Antigravity", Group: "more",
			Steps: []PickerStep{run("agy mcp add {name} {url}")},
			Wakes: []string{
				"The agent starts a background command that waits (agy may ask you to allow it). {Each} wakes the agent within seconds, even when it's idle. Nothing to install.",
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
			ID: "hermes", Name: "Hermes Agent", Group: "more",
			Steps: []PickerStep{
				{Kind: "file", Label: "Add to $HERMES_HOME/config.yaml (~/.hermes/config.yaml unless it's set)", Text: "mcp_servers:\n  {name}:\n    url: {url}"},
				{Kind: "note", Text: "Or run hermes mcp add {name} --url {url} where Hermes runs. In a running chat, /reload-mcp picks it up."},
			},
			Wakes: []string{
				"The agent offers a one-time install of Wakeshift's plugin for Hermes. Say yes (Hermes may ask you to approve the command once), then send any reply so the plugin starts.",
				"After that, {each} wakes the agent within a second, even when it's idle, as long as Hermes runs.",
			},
		},
		{
			ID: "openclaw", Name: "OpenClaw", Group: "more",
			Steps: []PickerStep{
				run("openclaw mcp add {name} --url {url} --transport streamable-http"),
				{Kind: "note", Text: "Run it where the OpenClaw gateway runs (in Docker, inside its container). New MCP servers are used from the next turn."},
			},
			Wakes: []string{
				"The agent offers a one-time install of Wakeshift's plugin for OpenClaw. Say yes, then send any reply so the plugin starts. If your gateway listens beyond this machine, it asks you to confirm first.",
				"After that, {each} wakes the agent within seconds, even when it's idle, as long as the gateway runs.",
			},
		},
		{
			ID: "copilot", Name: "Copilot CLI", Group: "more",
			Steps: []PickerStep{run("copilot mcp add --transport http {name} {url}")},
			Wakes: []string{
				"The agent starts a background command that waits. {Each} wakes the agent within seconds, even when it's idle. Nothing to install.",
				"Copilot asks you to approve that command each time the agent goes back to waiting, so each wake needs one \"Yes\". To skip that, run copilot with --allow-all-tools, which approves every tool, not just this one.",
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
