# Wakeshift for Cursor

Wakeshift wakes your Cursor agent when the apps it watches send something: a new chat message, a webhook, a move in a game. Your agent subscribes from the app's own tools, and new messages reach the conversation within seconds, even while it's idle, so it can act on them without you checking in.

This plugin adds Wakeshift's hooks to Cursor (the app and `cursor-agent`). You don't need it to start: an agent that subscribes to an app is woken by a command it runs in the background. The hooks also deliver messages when a turn ends and confirm each one, so nothing is missed between turns.

## Install

- **From the Cursor Marketplace:** open **Customize**, find **Wakeshift** and choose **Install**.
- **Without the Marketplace:** run `npx -y @wakeshift/connect cursor` (Node 18 or later), which runs the newest release and adds the same hooks to `~/.cursor/hooks.json`. When an app first asks your agent to subscribe, the agent offers the same command at the version Wakeshift currently offers (`npx -y @wakeshift/connect@<version> cursor`).

Then restart `cursor-agent` (the Cursor app picks up the hooks by itself), reopen the chat with `cursor-agent --continue` and send any message, so the agent connects the conversation. A note that starts with `Wakeshift: connected` confirms it.

There's nothing to configure. The plugin works with Wakeshift at https://app.wakeshift.ai.

## What it does

The plugin has four hooks:

- **sessionStart** makes sure Wakeshift's connector is ready.
- **afterMCPExecution** and **postToolUse** (MCP tools only) read the one-time code a Wakeshift app puts in its tool result, connect the conversation to that app, and remove the code from what the model sees.
- **stop** waits up to a minute after a turn ends and hands over any new message as the next prompt.

Each hook runs Wakeshift's connector, `~/.wakeshift/bin/wakeshift`. The first time a hook runs, the plugin downloads the connector from the signed release of [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors/releases) and checks it against the SHA-256 in this plugin's `SHA256SUMS`. If the download doesn't match, nothing is installed and the hooks do nothing. After that, hooks run without the network until the plugin pins a newer connector.

The connector keeps its settings and connected conversations in `~/.wakeshift/cursor`. If an earlier install added Wakeshift's hooks to `~/.cursor/hooks.json`, the plugin removes them the first time it runs, keeping your own hooks and a backup of the file.

## Privacy and security

- Apart from downloading the connector from GitHub, the plugin talks only to Wakeshift, and acts only on codes signed by the Wakeshift it trusts.
- It reads the results of MCP tools to find Wakeshift's code. It doesn't send your prompts, code or files anywhere.
- It never runs a command an app sends. Messages reach the agent as text marked as outside data, not as your instructions.

Read the [Connector Terms](https://wakeshift.ai/connector-terms) and the [Privacy Policy](https://wakeshift.ai/privacy). Report a security problem privately to support@wakeshift.ai.

## Turn it off

To turn the hooks off without uninstalling, start Cursor or `cursor-agent` with `WAKESHIFT_DISABLED=1` (or `true`) in its environment. Every hook then exits at once and does nothing: no network request, no download, no output. Conversations that were connected keep their link, so their messages reach them again once Cursor runs without the variable. Any other value, or none, leaves the hooks on.

## Uninstall

Uninstall Wakeshift from **Customize**, then run:

```sh
~/.wakeshift/bin/wakeshift uninstall --harness cursor
```

It removes the connector's settings and connected conversations, and any hooks an install from npx added.

## Source

The plugin is open source under the Apache License 2.0, and so is the connector it runs: its source and signed release builds are at [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors). Docs: [wakeshift.ai/docs/agents](https://wakeshift.ai/docs/agents).
