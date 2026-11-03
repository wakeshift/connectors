# Wakeshift for OpenClaw

Wakeshift's plugin for OpenClaw (2026.10.4 and later). It wakes an OpenClaw session when an app the session watches through [Wakeshift](https://wakeshift.ai) sends something, even while the session is idle, as long as the gateway runs. Messages arrive as outside information, never as the user.

## Install

Install it where the OpenClaw gateway runs (in its container, if it's in one). The agent offers this when an app's watch result asks, and runs it after your yes:

```sh
openclaw plugins install npm:@wakeshift/openclaw --force --accept-capabilities && openclaw plugins enable wakeshift --accept-capabilities
```

(The agent's command names the version, `npm:@wakeshift/openclaw@<version>`.)

`--force` is what OpenClaw asks for to install a plugin from npm rather than ClawHub, and `--accept-capabilities` accepts the plugin's tool-result filter (it takes Wakeshift's beacon out of what the model sees) where nothing can prompt. There's nothing to approve afterwards and no restart: the plugin starts with the next turn.

The plugin trusts Wakeshift's own service, `https://app.wakeshift.ai`. For another Wakeshift, set it first, before the install:

```sh
openclaw config set plugins.entries.wakeshift.config.issuer '"https://your-wakeshift.example"' --strict-json
```

The package is published from [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors) by GitHub Actions with npm provenance, so npm can show which commit and workflow built it.

To update, run the install command again (it also replaces a plugin installed by Wakeshift's earlier `openclaw.sh` installer).

## Remove

```sh
openclaw wakeshift reset && openclaw plugins uninstall wakeshift --force
```

`openclaw wakeshift reset` undoes what the plugin turned on (below): it puts OpenClaw's hooks settings back as the plugin found them, apart from what you changed since, removes `allowNetworkHooks`, and deletes `~/.openclaw/wakeshift`. OpenClaw's uninstall then removes the plugin and its settings. OpenClaw has no uninstall hook, so `openclaw plugins uninstall` on its own leaves the hooks settings as the plugin left them.

## How it wakes a session

The plugin starts a turn with OpenClaw's own hooks endpoint, `POST <hooks.path>/agent` on the gateway's listener, in the session's own context (`sessionMode: "persistent"`). OpenClaw wraps the message as outside content, with its own notes around it. The message itself is short: a headline (`New from <app> via Wakeshift: <text>`), a sentence saying it came from outside the conversation and isn't the user's instruction, and the message in a `<wakeshift_message>` tag. The turn's reply goes to the channel the session last spoke on, or stays in its transcript when it has none. A message is acknowledged to Wakeshift once the turn it started has finished.

When it first connects a session, the plugin turns on OpenClaw's hooks:

- `hooks.enabled`, with a random token of its own in `hooks.token`;
- `hooks.allowRequestSessionKey`, with `hooks.defaultSessionKey: "hook:wakeshift"`;
- `hooks.allowedSessionKeyPrefixes` and `hooks.allowedAgentIds`, allowing only the sessions and agents that connect through Wakeshift, each removed again when its session's subscription ends.

If OpenClaw's hooks are on already, the plugin only adds the session and its agent, and only if your settings already let a caller name the session from a restricted prefix list; otherwise it changes nothing and tells the agent what it would need. Everything it changes is recorded in `~/.openclaw/wakeshift/hooks.json`, which `openclaw wakeshift reset` uses to put your settings back.

The hooks endpoint shares the gateway's listener. On a gateway that listens beyond this computer (its bind isn't `loopback`), the endpoint is reachable there too, guarded by the token, so the plugin turns nothing on there until you agree: the agent asks first, and after your yes its command starts with `openclaw config set plugins.entries.wakeshift.config.allowNetworkHooks true --strict-json`. Block `/hooks/` from other machines with a firewall or proxy rule.

## Settings

- `plugins.entries.wakeshift.config.issuer`: the Wakeshift whose beacons the plugin trusts (unset: `https://app.wakeshift.ai`).
- `plugins.entries.wakeshift.config.allowNetworkHooks`: you agreed to the hooks endpoint on a gateway that listens beyond this computer.

## Development

`npm test` runs the tests; `npm run pack` builds `wakeshift-openclaw.tgz`, which Wakeshift's earlier `/install/openclaw.sh` installs. Each release of github.com/wakeshift/connectors carries the npm package at the release's version, and its `npm.yml` publishes it with provenance. The design and what was verified are in `docs/harnesses.md` (OpenClaw) in Wakeshift's main repository.
