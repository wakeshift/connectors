# Wakeshift for Claude Code

[Wakeshift](https://wakeshift.ai) lets the apps you connect to Claude Code notify it. When something your session is waiting for happens, such as a reply in a chat, a finished job or a new email, Wakeshift wakes the session, even while it's idle, and tells it what happened. This plugin adds the hooks that receive those messages in Claude Code.

It does nothing until an app that uses Wakeshift connects a conversation. An app connects a conversation when the agent calls one of the app's tools that subscribes to an event, and the tool's result carries a signed note from Wakeshift. Conversations no app has connected send nothing and receive nothing.

## Install

```sh
claude plugin install wakeshift --marketplace wakeshift/agent-plugins
```

On Claude Code before 2.1.292:

```sh
claude plugin marketplace add wakeshift/agent-plugins
claude plugin install wakeshift@wakeshift
```

Then type `/reload-plugins` in a running session, or start a new one. Usually you don't type this yourself: the first time a Wakeshift app needs the plugin, its tool result offers the command, and your agent asks you before it runs it.

The plugin has no settings. The repository's `main` branch trusts Wakeshift's service, `https://app.wakeshift.ai`, and only that.

## What it does

The plugin has four hooks. Each runs `bin/wakeshift`, a short POSIX shell script in this folder that you can read, which runs Wakeshift's connector:

| Hook | When | What it does |
| --- | --- | --- |
| `PostToolUse`, MCP tools only (`mcp__.*`), in the background | After the agent calls an MCP tool | Looks for a note signed by Wakeshift in the tool's result. If there is one, it connects the conversation to Wakeshift. Results without one are ignored. |
| `SessionStart` | When a session starts | Adds one line of context saying the plugin is installed, so apps don't offer to install it again. Makes no network request. |
| `SessionStart` and `Stop`, in the background | When a session starts, and when Claude finishes a turn | For a connected conversation, waits for its messages and wakes the session when one arrives. For any other conversation, it exits within two minutes. |

Only the session-start context hook runs before Claude Code goes on, and it works locally, apart from the one-time download below. If anything goes wrong, the hooks exit without an error and do nothing.

The connector is [open source](https://github.com/wakeshift/connectors) (Apache 2.0). It's built for macOS and Linux (WSL included), on arm64 and x86_64. Native Windows isn't supported: Claude Code there reports that the hooks can't start.

## Network

- **Once, on first use:** `bin/wakeshift` downloads the connector for your platform from Wakeshift's signed GitHub release, `https://github.com/wakeshift/connectors/releases/download/<version>/wakeshift-<os>-<arch>`. It checks the download against the SHA-256 in this plugin's `SHA256SUMS` and installs it to `~/.wakeshift/bin/wakeshift`. A download that doesn't match is deleted and nothing runs. The version and the release's address are in `wakeshift.env`. A later version of this plugin can pin a newer connector, which is downloaded the same way. A connector already there that's as new or newer (from another Wakeshift plugin, or Wakeshift's installer) is used as it is.
- **For connected conversations only:** the connector talks to Wakeshift (`https://app.wakeshift.ai`). It fetches Wakeshift's public signing keys, exchanges the signed note for a link to the conversation's messages, waits on that link and confirms each message it delivers. It sends the IDs Claude Code gives the session and the tool call, and its own version. It never sends your prompts, the agent's replies, tool results or files.

Nothing else is contacted. There's no telemetry.

## What it stores and changes on your computer

- `~/.wakeshift/bin/wakeshift`: the connector.
- `~/.wakeshift/claude-code/`: which Wakeshift it trusts, Wakeshift's public keys, a link (a secret token) for each connected conversation, and a log of when each wait started and ended, with no content or tokens.
- `~/.claude/settings.json`: one permission rule, so Claude Code runs Wakeshift's wait command, `~/.wakeshift/bin/wakeshift wait`, without asking each time. If you agree to an app's request to skip approvals for some of its tools, a rule for exactly those tools. The file is backed up before each change (`settings.json.before-wakeshift-<time>`).
- If you used Wakeshift's earlier installer, it removes the hooks that installer added to `~/.claude/settings.json`, so they don't run twice.

Messages are shown in the session and never written to disk.

## Turn it off or uninstall

To pause it, disable the plugin: `claude plugin disable wakeshift@wakeshift`.

To turn the hooks off for one run of Claude Code without changing the plugin, start it with `WAKESHIFT_DISABLED=1` (or `true`) in its environment: `WAKESHIFT_DISABLED=1 claude`. Every hook then exits at once and does nothing: no network request, no download, no output. Conversations that were connected keep their link, so their messages reach them again in a session started without the variable. A hook that was already running when you set it, such as a wait from an earlier session, carries on until it ends. Any other value, or none, leaves the hooks on.

To remove everything, in this order (the plugin sets the connector up again on its next hook while it's installed):

1. `claude plugin uninstall wakeshift@wakeshift`, then `/reload-plugins` in running sessions. `claude plugin marketplace remove wakeshift` also removes Wakeshift's marketplace (Claude Code clears the plugin's cached copy itself later).
2. `~/.wakeshift/bin/wakeshift uninstall --harness claude-code` removes the permission rules it added and `~/.wakeshift/claude-code`. Before each change to `~/.claude/settings.json` it keeps a copy beside it, `settings.json.before-wakeshift-<time>`: delete those once you don't need them.
3. If no other agent on this computer uses Wakeshift, delete `~/.wakeshift`.

## Privacy, security and support

- Privacy policy: https://wakeshift.ai/privacy
- Terms: https://wakeshift.ai/terms
- Docs: https://wakeshift.ai/docs/agents
- Questions and bugs: https://github.com/wakeshift/agent-plugins/issues or support@wakeshift.ai
- Report a security problem privately to support@wakeshift.ai with "Security" in the subject, not in an issue ([SECURITY](https://github.com/wakeshift/agent-plugins/blob/main/SECURITY.md)).

## License

Apache License 2.0 ([LICENSE](https://github.com/wakeshift/agent-plugins/blob/main/LICENSE)).
